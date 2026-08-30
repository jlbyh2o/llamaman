package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Verifying a model (DESIGN section 3.7: "`202` — re-stat and optional sha256").
//
// The two halves are deliberately different in cost and in what they can prove.
// A re-stat is instant and answers "is it still there, and is it the size we
// recorded". A sha256 reads the whole file and answers "are these the bytes
// Hugging Face served" — which is the only check that catches a silent
// corruption, and which on a 40 GB quant takes minutes. So the digest is behind
// `hf.verify_checksums` and behind an explicit request, and the stat always runs.
//
// The digest is checkable at all only because of the cache layout: for an LFS
// object the blob's NAME is its sha256 hex (§7.2), so there is nothing to fetch
// and nothing to store — the expected value is already on disk as a file name.

// sha256Hex matches a blob name that is a sha256 digest, which is what makes it
// a checkable expectation. A blob whose name is a plain ETag — a small non-LFS
// file — is not one, and hashing it would produce a number with nothing to
// compare against.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify is `POST /api/v1/models/{id}/verify`.
func (s *Service) Verify(ctx context.Context, id string) (JobRef, error) {
	var ref JobRef
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.LocalModel(ctx, tx, id); err != nil {
			return err
		}
		var err error
		ref, err = s.enqueue(ctx, tx, jobs.EnqueueParams{
			Kind: model.JobModelVerify, DomainID: id,
		}, id)
		return err
	})
	return ref, err
}

// VerifyWorker runs `model_verify` jobs.
type VerifyWorker struct {
	svc *Service
	// checksums decides whether the digest half runs. The composition root
	// supplies a reader of `settings['hf.verify_checksums']`; a nil func means
	// stat-only, which is the safe default for a binary with no settings cache.
	checksums func(ctx context.Context) bool
}

// NewVerifyWorker builds the worker. checksums may be nil.
func NewVerifyWorker(s *Service, checksums func(ctx context.Context) bool) *VerifyWorker {
	return &VerifyWorker{svc: s, checksums: checksums}
}

// Kind is `model_verify`.
func (w *VerifyWorker) Kind() model.JobKind { return model.JobModelVerify }

// Run re-stats every file and, when checksums are on, hashes the ones whose blob
// name is a digest.
//
// The verdict is the model's state: every file present and correct is `ready`;
// any file gone is `missing`; any digest mismatch is `corrupt`. `missing` wins
// over `corrupt` when both apply, because "the disk is not there" is the
// diagnosis a user must act on first and re-hashing what remains would say
// nothing useful about it.
func (w *VerifyWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	return w.runFor(ctx, t.Job().SubjectID)
}

// runFor is Run's body without the lease, for the same reason ExecuteDelete is
// split out: `jobs.Task` can only be constructed by the queue, and what is worth
// asserting here is the verdict, not the bookkeeping.
func (w *VerifyWorker) runFor(ctx context.Context, id string) (jobs.Outcome, error) {
	s := w.svc

	var (
		m     model.LocalModel
		files []model.ModelFile
	)
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if m, err = s.store.LocalModel(ctx, tx, id); err != nil {
			return err
		}
		files, err = s.store.ModelFiles(ctx, tx, id)
		return err
	}); err != nil {
		return jobs.Outcome{}, err
	}

	digest := w.checksums != nil && w.checksums(ctx)
	verdict := model.ModelReady
	results := make([]fileVerdict, 0, len(files))
	var onDisk int64

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return jobs.Canceled(nil), nil
		}
		v := verifyFile(m, f, digest)
		results = append(results, v)
		onDisk += v.bytesOnDisk
		switch v.state {
		case model.FileMissing:
			verdict = model.ModelMissing
		case model.FileCorrupt:
			if verdict != model.ModelMissing {
				verdict = model.ModelCorrupt
			}
		}
	}
	if len(files) == 0 {
		// Nothing to verify. The catalog row exists and names no file, which is
		// the state of a model whose files were removed out from under it.
		verdict = model.ModelMissing
	}

	sink := &events.Sink{}
	out := jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		now := s.now().UnixMilli()
		for i, f := range files {
			f.State = results[i].state
			f.ChecksumVerified = results[i].hashed
			f.BytesOnDisk = results[i].bytesOnDisk
			f.SizeBytes = results[i].size
			f.UpdatedAt = now
			if err := s.store.UpsertModelFile(ctx, tx, f); err != nil {
				return err
			}
		}

		// A verify is also a re-measure. `bytes_on_disk` is what a delete
		// preview promises to free, and a file that was sparse when the scan
		// saw it and is fully allocated now would otherwise keep the old
		// number until the next full scan.
		row := m
		row.BytesOnDisk = onDisk
		row.LastVerifiedAt = &now
		row.UpdatedAt = now
		if _, err := s.store.UpdateLocalModel(ctx, tx, row); err != nil {
			return err
		}

		if m.State != verdict {
			if _, err := s.store.SetLocalModelState(ctx, tx, id, verdict, now); err != nil {
				return err
			}
			// `state` moved, so D69 applies.
			if err := s.recomputeFor(ctx, tx, id); err != nil {
				return err
			}
			from, to := string(m.State), string(verdict)
			return s.appendEvent(ctx, tx, model.Event{
				Level: levelFor(verdict), Category: model.CategoryModel, Actor: model.ActorAdmin,
				Action: "model_verified", SubjectID: &id, FromState: &from, ToState: &to,
				Message: "verified " + m.RepoID + " " + m.PrimaryFile,
			}, sink)
		}
		return nil
	})
	out.AfterCommit = func() { s.publish(sink) }
	return out, nil
}

type fileVerdict struct {
	state       model.ModelFileState
	hashed      bool
	size        int64
	bytesOnDisk int64
}

// verifyFile stats one file and optionally hashes it.
func verifyFile(m model.LocalModel, f model.ModelFile, digest bool) fileVerdict {
	path := filepath.Join(m.SnapshotDir, filepath.FromSlash(f.Filename))
	if f.LinkPath != nil && *f.LinkPath != "" {
		path = *f.LinkPath
	}

	st, err := os.Stat(path)
	if err != nil {
		return fileVerdict{state: model.FileMissing}
	}
	v := fileVerdict{state: model.FilePresent, size: st.Size()}
	v.bytesOnDisk = allocated(st)

	// A size that disagrees with what was recorded is enough on its own: the
	// digest cannot match either, and saying "the file is the wrong size" is a
	// better message than "the checksum failed".
	if f.SizeBytes > 0 && st.Size() != f.SizeBytes {
		v.state = model.FileCorrupt
		return v
	}
	if !digest || f.Etag == nil || !sha256Hex.MatchString(*f.Etag) {
		return v
	}

	sum, err := sha256File(path)
	if err != nil {
		return fileVerdict{state: model.FileMissing, size: st.Size(), bytesOnDisk: v.bytesOnDisk}
	}
	if sum != *f.Etag {
		v.state = model.FileCorrupt
		return v
	}
	v.hashed = true
	return v
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// allocated is the on-disk cost of one stat result, using the same
// `st_blocks × 512` rule the scan uses so the two never disagree about what a
// delete would free.
func allocated(st os.FileInfo) int64 {
	return cache.AllocatedBytes(st)
}

func levelFor(s model.ModelState) model.EventLevel {
	switch s {
	case model.ModelMissing, model.ModelCorrupt:
		return model.LevelWarn
	default:
		return model.LevelInfo
	}
}

// removeFile unlinks one path. It is a function rather than a call site so the
// stray delete of delete.go has one place to be audited.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
