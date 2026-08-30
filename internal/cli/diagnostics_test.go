package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// readBundle opens the gzip'd tar Diagnostics wrote and returns every member
// by name, so a test can assert on the bundle the same way a support engineer
// who received it would: by extracting it.
func readBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the bundle: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = content
	}
	return out
}

// TestDiagnosticsBundleContentsAndRedaction is D50's own contract end to end:
// every section §11.3 names is present, and a Hugging Face token seeded into
// this host's sealed secrets never appears in ANY file the bundle writes —
// not the obvious secrets.json (which never carries a value to begin with),
// but every other file too, since Build's scrub pass runs over the whole
// bundle rather than trusting each section to remember.
func TestDiagnosticsBundleContentsAndRedaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	const fakeToken = "hf_fake-token-for-tests-only-0123456789ABCDEF"
	seedFakeHFToken(t, ctx, st, stateDir, env, fakeToken)
	seedASetting(t, ctx, st, env)
	seedAJob(t, ctx, st, env)
	seedAnInstance(t, ctx, st, env)

	bundlePath := filepath.Join(t.TempDir(), "diagnostics.tar.gz")
	if err := Diagnostics(env, []string{"--out", bundlePath}); err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}

	// The completion message names the file and echoes the redaction note
	// (the task's own "section-11.3-consistent redaction note printed on
	// completion").
	if !strings.Contains(out.String(), bundlePath) {
		t.Errorf("stdout does not name the bundle it wrote: %s", out.String())
	}
	if !strings.Contains(out.String(), "no plaintext secret") {
		t.Errorf("stdout does not print the redaction note: %s", out.String())
	}

	files := readBundle(t, bundlePath)

	// --- Bundle contents inventory (D50, §11.3) -----------------------------
	want := []string{
		"manifest.json", "REDACTION.txt", "doctor.json", "settings.json",
		"secrets.json", "units/drift.json", "versions.json", "schema.json",
		"jobs_summary.json", "instances_summary.json",
	}
	for _, name := range want {
		if _, ok := files[name]; !ok {
			t.Errorf("the bundle has no %q", name)
		}
	}
	// The journal and build-log sections are always present in SOME form —
	// either captured content or the file explaining why not — never absent.
	hasPrefix := func(prefix string) bool {
		for name := range files {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	if !hasPrefix("journal/") {
		t.Error("the bundle has no journal/ section at all")
	}
	if !hasPrefix("build-logs/") {
		t.Error("the bundle has no build-logs/ section at all")
	}

	if strings.Contains(string(files["settings.json"]), `"7"`) && !strings.Contains(string(files["settings.json"]), `"hf.download_concurrency"`) {
		t.Error("the seeded setting override is not reflected in settings.json")
	}
	if !strings.Contains(string(files["jobs_summary.json"]), "model_download") {
		t.Errorf("jobs_summary.json does not mention the seeded job's kind: %s", files["jobs_summary.json"])
	}
	if !strings.Contains(string(files["instances_summary.json"]), "diag-test") {
		t.Errorf("instances_summary.json does not mention the seeded instance's name: %s", files["instances_summary.json"])
	}
	if !strings.Contains(string(files["secrets.json"]), `"present": true`) {
		t.Errorf("secrets.json does not report the stored token as present: %s", files["secrets.json"])
	}

	// --- Redaction proof -----------------------------------------------------
	if len(files) == 0 {
		t.Fatal("the bundle is empty")
	}
	for name, content := range files {
		if bytes.Contains(content, []byte(fakeToken)) {
			t.Errorf("%s contains the raw seeded token — redaction failed", name)
		}
	}
	// The hint IS expected somewhere (secrets.json), and it is not the full
	// token — proving the test would have caught a redaction that was too
	// broad (scrubbing everything, including the hint) as well as one that
	// was too narrow.
	hint := secrets.Hint(fakeToken)
	if !strings.Contains(string(files["secrets.json"]), hint) {
		t.Errorf("secrets.json does not carry the token's hint %q: %s", hint, files["secrets.json"])
	}
}

// TestDiagnosticsOnAnUninitializedHost: no database, no secrets, no state —
// the command still produces a complete, well-formed bundle rather than
// failing, and it creates nothing under the state directory (§11.3's rule for
// every root-invocable subcommand, restated here for diagnostics by name).
func TestDiagnosticsOnAnUninitializedHost(t *testing.T) {
	t.Parallel()

	env, out, _, stateDir := testEnv(t)
	bundlePath := filepath.Join(t.TempDir(), "diagnostics.tar.gz")

	if err := Diagnostics(env, []string{"--out", bundlePath}); err != nil {
		t.Fatalf("Diagnostics against a host that has never run: %v", err)
	}
	if !strings.Contains(out.String(), bundlePath) {
		t.Errorf("stdout does not name the bundle: %s", out.String())
	}

	if _, err := os.Stat(stateDir); err == nil {
		t.Error("Diagnostics created the state directory on a host that has never run")
	}

	files := readBundle(t, bundlePath)
	if !strings.Contains(string(files["schema.json"]), "skipped") {
		t.Errorf("schema.json does not report itself skipped with no database: %s", files["schema.json"])
	}
}

// TestDiagnosticsRequiresOut is the ordinary flag-usage guard.
func TestDiagnosticsRequiresOut(t *testing.T) {
	t.Parallel()
	env, _, errOut, _ := testEnv(t)

	if err := Diagnostics(env, nil); err == nil {
		t.Fatal("Diagnostics accepted a missing --out")
	}
	if !strings.Contains(errOut.String(), "--out is required") {
		t.Errorf("stderr does not explain the missing flag: %s", errOut.String())
	}
}

func seedFakeHFToken(t *testing.T, ctx context.Context, st *store.Store, stateDir string, env Env, token string) {
	t.Helper()
	key, err := secrets.LoadOrCreateKey(stateDir)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	svc, err := secrets.New(secrets.Config{Store: st, Key: key, Now: env.now})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	if err := svc.Put(ctx, model.SecretHFToken, token, secrets.Verdict{Valid: true, User: "diagnostics-test"}); err != nil {
		t.Fatalf("Put(hf_token): %v", err)
	}
}

func seedASetting(t *testing.T, ctx context.Context, st *store.Store, env Env) {
	t.Helper()
	if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return st.PutSetting(ctx, tx, model.Setting{
			Key: "hf.download_concurrency", Value: "7",
			UpdatedAt: env.now().UnixMilli(), UpdatedBy: model.UpdatedByAdmin,
		})
	}); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
}

func seedAJob(t *testing.T, ctx context.Context, st *store.Store, env Env) {
	t.Helper()
	at := env.now().UnixMilli()
	if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return st.InsertJob(ctx, tx, model.Job{
			ID: store.NewID(env.now()), Kind: model.JobModelDownload,
			SubjectType: model.SubjectDownload, SubjectID: "dl-1",
			State: model.JobQueued, MaxAttempts: 1, RunAfter: at, CreatedAt: at,
		})
	}); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
}

func seedAnInstance(t *testing.T, ctx context.Context, st *store.Store, env Env) {
	t.Helper()
	at := env.now().UnixMilli()
	inst := model.Instance{
		ID: store.NewID(env.now()), Name: "diag-test",
		PublicPort: 18080, InternalPort: 18081,
		AuthMode: model.AuthToken, RestartPolicy: model.RestartOnFailure,
		RestartMax: 5, RestartWindowSec: 600, FlagsJSON: `{}`,
		ConfigHash: "hash-diag-test", DesiredState: model.DesiredStopped,
		DraftValidation: model.DraftOK, UnitName: "llamaman-instance@diag-test.service",
		Generation: 1, CreatedAt: at, UpdatedAt: at,
	}
	if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := st.InsertInstance(ctx, tx, inst); err != nil {
			return err
		}
		return st.InsertInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID: inst.ID, State: model.InstanceStopped,
			LastChangeAt: at, GPUAttribution: model.AttributionUnknown,
		})
	}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
}
