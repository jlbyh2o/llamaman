package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/settings"
)

// TestRedactScrubsAKnownValueEverywhere is the redaction mechanism itself,
// isolated from the rest of the bundle: every byte-occurrence of a value
// handed to Redact disappears from every file, regardless of which section
// wrote it.
func TestRedactScrubsAKnownValueEverywhere(t *testing.T) {
	t.Parallel()

	const token = "hf_super-secret-value-nobody-should-see"
	files := []File{
		{Name: "a.json", Content: []byte(`{"note":"token is ` + token + ` here"}`)},
		{Name: "b.log", Content: []byte("line one\nleaked: " + token + "\nline three\n")},
		{Name: "c.txt", Content: []byte("nothing sensitive here")},
	}

	out := Redact(files, []string{token})

	for _, f := range out {
		if strings.Contains(string(f.Content), token) {
			t.Errorf("%s still contains the raw value after Redact: %s", f.Name, f.Content)
		}
	}
	if !strings.Contains(string(out[2].Content), "nothing sensitive here") {
		t.Error("Redact altered a file that had nothing to scrub")
	}
}

// TestRedactShapeBasedSafetyNet: even a value Redact was never TOLD about is
// caught if it has the shape of one of the credentials this project handles —
// the second line of defense for a value that leaked into a journal line or a
// build log without ever passing through the secrets service.
func TestRedactShapeBasedSafetyNet(t *testing.T) {
	t.Parallel()

	leaked := "ghp_" + strings.Repeat("a", 36)
	files := []File{{Name: "journal/llamaman.service.log", Content: []byte("auth failed with " + leaked)}}

	out := Redact(files, nil)
	if strings.Contains(string(out[0].Content), leaked) {
		t.Errorf("a GitHub-token-shaped value survived Redact with no known values: %s", out[0].Content)
	}
}

// TestWriteTarGzRoundTrips: what Build produces can be read back with the
// standard library alone — nothing this design's own reader could invent a
// dependency on.
func TestWriteTarGzRoundTrips(t *testing.T) {
	t.Parallel()

	files := []File{
		{Name: "manifest.json", Content: []byte(`{"a":1}`)},
		{Name: "units/llamaman.service", Content: []byte("[Unit]\n")},
	}
	var buf bytes.Buffer
	if err := WriteTarGz(&buf, files, time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("WriteTarGz: %v", err)
	}

	got := readTarGz(t, buf.Bytes())
	for _, f := range files {
		content, ok := got[f.Name]
		if !ok {
			t.Fatalf("%s is missing from the archive", f.Name)
		}
		if string(content) != string(f.Content) {
			t.Errorf("%s round-tripped as %q, want %q", f.Name, content, f.Content)
		}
	}
}

func readTarGz(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	out := map[string][]byte{}
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

// fakeSecrets is a minimal SecretsService a test can seed without a database.
type fakeSecrets struct {
	values map[model.SecretName]string
}

func (f fakeSecrets) Get(_ context.Context, name model.SecretName) (string, error) {
	v, ok := f.values[name]
	if !ok {
		return "", secrets.ErrNotStored
	}
	return v, nil
}

func (f fakeSecrets) Info(_ context.Context, name model.SecretName) (secrets.Info, error) {
	v, ok := f.values[name]
	if !ok {
		return secrets.Info{Name: name}, nil
	}
	return secrets.Info{Name: name, Present: true, Hint: secrets.Hint(v)}, nil
}

// TestBuildDegradesGracefullyWithNoDatabase: a fresh host with no database, no
// systemd and no journal access still gets a complete bundle, with every
// DB-backed section saying it was skipped rather than being silently absent.
func TestBuildDegradesGracefullyWithNoDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files, err := Build(ctx, Options{
		Now:        time.Unix(1700000000, 0).UTC(),
		DoctorJSON: []byte(`{"checks":[]}`),
		Registry:   settings.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{
		"manifest.json", "REDACTION.txt", "doctor.json", "settings.json",
		"secrets.json", "units/drift.json", "journal/README.txt",
		"build-logs/README.txt", "versions.json", "schema.json",
		"jobs_summary.json", "instances_summary.json",
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
		if len(f.Content) == 0 {
			t.Errorf("%s is empty", f.Name)
		}
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("the bundle has no %q; a section that cannot run must say so, not vanish", w)
		}
	}

	for _, name := range []string{"schema.json", "jobs_summary.json", "instances_summary.json"} {
		if !strings.Contains(string(fileContent(files, name)), "skipped") {
			t.Errorf("%s does not report itself skipped with no database: %s", name, fileContent(files, name))
		}
	}
}

// TestBuildScrubsASecretItFetchedItself: Build's own scrub pass, driven by
// its Secrets field, removes a value the fixture reports as stored — proving
// the pipeline end to end without a database.
func TestBuildScrubsASecretItFetchedItself(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const token = "hf_fake-token-for-tests-only-0123456789"
	files, err := Build(ctx, Options{
		Now:        time.Unix(1700000000, 0).UTC(),
		DoctorJSON: []byte(`{"checks":[]}`),
		Registry:   settings.NewRegistry(),
		Secrets:    fakeSecrets{values: map[model.SecretName]string{model.SecretHFToken: token}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), token) {
			t.Fatalf("%s contains the raw token after Build", f.Name)
		}
	}
	secretsJSON := string(fileContent(files, "secrets.json"))
	if !strings.Contains(secretsJSON, `"present": true`) || !strings.Contains(secretsJSON, "hf_f") {
		t.Errorf("secrets.json does not show the hint for a stored token: %s", secretsJSON)
	}
}

func fileContent(files []File, name string) []byte {
	for _, f := range files {
		if f.Name == name {
			return f.Content
		}
	}
	return nil
}
