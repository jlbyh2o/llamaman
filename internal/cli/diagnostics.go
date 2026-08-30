package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/diagnostics"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// `llamaman diagnostics --out FILE` (DESIGN section 11.3, D50): a redacted
// gzip'd tar support bundle — doctor output, a sanitized settings dump, recent
// journal excerpts, unit render and drift status, the schema version and
// per-table row counts, job and instance state summaries, and a versions
// manifest. Never the database, never a plaintext secret.
//
// internal/diagnostics owns the bundle's shape and its redaction; this file
// owns wiring it to a real host: finding the database (never creating one —
// the same rule §11.3 states for `status` and `doctor`), the settings
// registry, the secrets service, the unit-render inputs and the journal
// reader.
//
// DESIGN section 3 (the REST API contract, §3.1–§3.15) names no
// `/system/diagnostics` route anywhere — §3.3's System table, the one section
// that could carry it, stops at `GET /system/capabilities` — so this command
// is CLI-only, exactly as `install-units` and `restore-db` are.

func Diagnostics(env Env, args []string) error {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	out := fs.String("out", "", "write the bundle to this path (required)")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman diagnostics --out FILE\n\n")
		fmt.Fprintf(env.Stderr, "Writes a redacted support bundle (gzip'd tar). Creates nothing under\n")
		fmt.Fprintf(env.Stderr, "the state directory: it only reads.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		fmt.Fprintf(env.Stderr, "llamaman diagnostics: --out is required\n")
		fs.Usage()
		return fmt.Errorf("--out is required")
	}

	ctx := context.Background()
	p := resolvePaths(env)

	// The doctor report first: it is the one section every bundle carries
	// regardless of what else this host can answer, and runDoctor already
	// implements §11.3's own "create nothing, open read-only, skip rather
	// than fail when the database is absent" rules — reusing it here is what
	// keeps this command from restating them a second way.
	doctorReport := runDoctor(ctx, env, false)
	doctorJSON, err := json.Marshal(doctorReport)
	if err != nil {
		return fmt.Errorf("llamaman diagnostics: render the doctor report: %w", err)
	}

	opt := diagnostics.Options{
		Now:           env.now().UTC(),
		DoctorJSON:    doctorJSON,
		Registry:      settings.NewRegistry(),
		DaemonVersion: buildinfo.Version,
		DaemonCommit:  buildinfo.Commit,
		DaemonChannel: buildinfo.Channel,
		Scope:         doctorScope(env),
		BuildLogDir:   filepath.Join(p.StateDir, source.LogsDirName, source.BuildDirName),
		JournalTail:   systemd.Tail,
	}

	// The database: opened read-only, and only when it already exists — the
	// same three-case rule checkDatabase applies for doctor, restated here
	// because this command owns its own DB lifetime rather than sharing
	// doctor's.
	access, _, cerr := classify(p)
	if cerr == nil && access == dbReadable {
		if st, oerr := store.OpenReadOnly(ctx, p.DBPath); oerr == nil {
			defer st.Close()
			opt.DB = st
			wireFromRuntimeInfo(ctx, st, &opt)
			opt.Secrets = openSecretsReadOnly(st, p.StateDir)
		}
	}
	if opt.Prefix == "" {
		opt.Prefix = fallbackPrefix()
	}

	files, berr := diagnostics.Build(ctx, opt)
	if berr != nil {
		return fmt.Errorf("llamaman diagnostics: %w", berr)
	}

	f, cerr2 := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if cerr2 != nil {
		return fmt.Errorf("llamaman diagnostics: create %s: %w", *out, cerr2)
	}
	werr := diagnostics.WriteTarGz(f, files, opt.Now)
	cerr3 := f.Close()
	if werr != nil {
		return fmt.Errorf("llamaman diagnostics: write %s: %w", *out, werr)
	}
	if cerr3 != nil {
		return fmt.Errorf("llamaman diagnostics: close %s: %w", *out, cerr3)
	}

	fmt.Fprintf(env.Stdout, "wrote %s (%d files)\n\n", *out, len(files))
	fmt.Fprintln(env.Stdout, diagnostics.RedactionNote)
	return nil
}

// wireFromRuntimeInfo fills the unit-render inputs from the daemon's own
// recorded facts, when it has ever booted and recorded any. A host that has
// never booted leaves Identity empty, which is diagnostics.Build's signal to
// report installed units without attempting to render an expected copy.
func wireFromRuntimeInfo(ctx context.Context, st *store.Store, opt *diagnostics.Options) {
	var ri model.RuntimeInfo
	err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		ri, err = st.RuntimeInfo(ctx, tx)
		return err
	})
	if err != nil {
		return
	}

	if ri.SystemdScope != nil {
		opt.Scope = *ri.SystemdScope
	}
	if ri.ServiceUser != nil {
		opt.Identity = *ri.ServiceUser
	}
	if ri.ServiceGroup != nil {
		opt.IdentityGroup = *ri.ServiceGroup
	}
	if ri.BinaryPath != nil && *ri.BinaryPath != "" {
		opt.Prefix = filepath.Dir(*ri.BinaryPath)
	}
	if ri.UIPortFlag != nil {
		opt.Port = int(*ri.UIPortFlag)
	}
	if ri.PolkitUnitFiles != nil {
		opt.UnitFilesGrant = *ri.PolkitUnitFiles
	}
}

// fallbackPrefix is what a host that has never booted gets instead of the
// runtime-recorded install prefix: the directory this very binary runs from,
// which is the ordinary case for a hand-run `llamaman diagnostics` against a
// fresh install, and systemd.DefaultPrefix otherwise.
func fallbackPrefix() string {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" && dir != "." {
			return dir
		}
	}
	return systemd.DefaultPrefix
}

// openSecretsReadOnly builds a diagnostics.SecretsService against the key
// file WITHOUT ever creating one — §11.3's rule binds this command exactly as
// it binds `status` and `doctor`, and secrets.LoadOrCreateKey's create branch
// is only reachable when the file does not exist. Stat-ing first and refusing
// to call it at all when the file is absent is what keeps a root-run
// `diagnostics` against a host that has never booted from creating a
// root-owned secret.key the service identity could never use.
//
// It does not hand back a plain *secrets.Service: Service.Get stamps
// `last_used_at` on every read, through Store.Write — which PANICS on a Store
// from store.OpenReadOnly, whose write pool is nil by design (§11.3, the same
// property that keeps this command from creating a WAL sidecar). roSecrets.Get
// below opens the sealed value directly and never writes, which is also the
// more honest behavior for a diagnostic read: pulling a token into a support
// bundle's redaction pass is not "using" it.
func openSecretsReadOnly(st *store.Store, stateDir string) diagnostics.SecretsService {
	keyPath := filepath.Join(stateDir, secrets.KeyFileName)
	if _, err := os.Stat(keyPath); err != nil {
		return nil
	}
	key, err := secrets.LoadOrCreateKey(stateDir)
	if err != nil {
		return nil
	}
	svc, err := secrets.New(secrets.Config{Store: st, Key: key})
	if err != nil {
		return nil
	}
	return roSecrets{st: st, key: key, info: svc}
}

// roSecrets is a read-only diagnostics.SecretsService: Info delegates to a
// real secrets.Service (safe — Info never writes), and Get decrypts directly
// against the store and the key, bypassing Service.Get's write-on-read.
type roSecrets struct {
	st   *store.Store
	key  secrets.Key
	info *secrets.Service
}

func (r roSecrets) Info(ctx context.Context, name model.SecretName) (secrets.Info, error) {
	return r.info.Info(ctx, name)
}

func (r roSecrets) Get(ctx context.Context, name model.SecretName) (string, error) {
	var sec store.Secret
	err := r.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		sec, err = r.st.Secret(ctx, tx, name)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", secrets.ErrNotStored
	}
	if err != nil {
		return "", err
	}
	plain, err := r.key.Open(string(name), sec.Nonce, sec.Ciphertext)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
