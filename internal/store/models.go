package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// ModelRef is a `models` row as the INSTANCE service reads it (§2.6, §3.10a).
//
// It is deliberately not `ModelPaths` (§5.6's projection) and deliberately not a
// full row. The launcher needs one path and nothing else; the instance service
// needs that path plus the two GGUF fields D34's draft check compares —
// `tokenizer_model` and `n_vocab` — and whether the parse has happened at all,
// because §3.10a's answer is three-valued and "not parsed yet" is a supported
// state rather than an error. When internal/models grows its own row type this
// projection stays as it is: an instance form that had to construct a full
// catalog row to render a command line would be coupled to every column a
// download pipeline adds.
type ModelRef struct {
	ID string
	// SnapshotDir and PrimaryFile are the two halves of the one path llama.cpp
	// is given. PrimaryFile is shard 1 for a sharded set (§7.3).
	SnapshotDir string
	PrimaryFile string
	State       model.ModelState
	Kind        model.ModelKind
	// Parsed is `gguf_parsed_at IS NOT NULL`. The two fields below exist only
	// after a parse, which is exactly what makes §3.10a's `deferred` verdict
	// distinguishable from a mismatch.
	Parsed         bool
	TokenizerModel *string
	NVocab         *int64
}

// ModelRefsByID returns one entry per id that exists. A missing id is simply
// absent from the map, which is how a reference to a purged model is reported
// without an error type — the instance service renders no argv for it and says
// so in a warning, rather than refusing to show the instance at all.
func (s *Store) ModelRefsByID(ctx context.Context, tx Tx, ids []string) (map[string]ModelRef, error) {
	out := make(map[string]ModelRef, len(ids))
	args := distinctArgs(ids)
	if len(args) == 0 {
		return out, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, snapshot_dir, primary_file, state, kind,
		        gguf_parsed_at, tokenizer_model, n_vocab
		   FROM models WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("select models: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			m        ModelRef
			state    string
			kind     string
			parsedAt *int64
		)
		if err := rows.Scan(&m.ID, &m.SnapshotDir, &m.PrimaryFile, &state, &kind,
			&parsedAt, &m.TokenizerModel, &m.NVocab); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		m.State = model.ModelState(state)
		m.Kind = model.ModelKind(kind)
		m.Parsed = parsedAt != nil
		out[m.ID] = m
	}
	return out, rows.Err()
}

// distinctArgs drops empty and duplicate ids, so an IN list is never built with
// a placeholder that cannot match and never repeats one that can.
func distinctArgs(ids []string) []any {
	args := make([]any, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	return args
}
