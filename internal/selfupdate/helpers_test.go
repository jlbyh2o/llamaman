package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Shared fixtures for the section 12 suites.
//
// # The TEST signing key
//
// The release key compiled into the binary has no private half anywhere — it was
// generated with the private key discarded, so nothing can forge a signature
// against it, which is the fail-closed posture a trust root should have before
// its real key is published. So the tests sign with their OWN key, derived from
// a fixed seed below, and pass it in through the KeySet seam that both verifying
// callers already take.
//
// The seed is a literal byte pattern in a test file. It is not a credential:
// nothing outside this package's tests ever verifies against the key it
// produces, and a signature made with it does not verify against either embedded
// key — which the embedded-key test asserts.

// testSeed derives the test signing key. ed25519 seeds are 32 bytes; this one is
// the byte value of its own index, so the key is stable across runs and machines
// and the fixtures a test writes can be compared against golden bytes.
func testSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return seed
}

func testKey(t *testing.T) (ed25519.PrivateKey, KeySet) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(testSeed())
	return priv, KeySet{priv.Public().(ed25519.PublicKey)}
}

// release is a staged release on disk: a tarball, its checksums file and a
// signature over that file.
type release struct {
	dir     string
	tarball string
	// binary is the bytes the tarball's `llamaman` member carries, and what an
	// installed copy must end up byte-identical to.
	binary []byte
	sha256 string
}

// stageRelease writes a verifiable release into dir. `script` is the shell body
// of the fake `llamaman` binary the tarball carries — enough for the version
// probe of section 12.1 step 3 to run it.
func stageRelease(t *testing.T, dir, version, goarch, script string) release {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	binary := []byte("#!/bin/sh\n" + script + "\n")
	name := TarballName(version, goarch)
	tarballPath := filepath.Join(dir, name)
	writeTarball(t, tarballPath, binary)

	sum := sha256.Sum256(mustRead(t, tarballPath))
	digest := hex.EncodeToString(sum[:])
	checksums := []byte(digest + "  " + name + "\n")
	writeFile(t, filepath.Join(dir, ChecksumsName), checksums)

	priv, _ := testKey(t)
	writeFile(t, filepath.Join(dir, SignatureName), ed25519.Sign(priv, checksums))

	return release{dir: dir, tarball: name, binary: binary, sha256: digest}
}

// writeTarball writes a gzip tarball whose one regular member is `llamaman`,
// which is the shape section 16.2 step 2 produces (binary + LICENSE + README —
// the other two are written too, so the extractor is exercised against an
// archive it has to skip members in).
func writeTarball(t *testing.T, path string, binary []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	members := []struct {
		name string
		mode int64
		body []byte
	}{
		{"LICENSE", 0o644, []byte("a license\n")},
		{BinaryName, 0o755, binary},
		{"README.md", 0o644, []byte("a readme\n")},
	}
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{
			Name: m.name, Mode: m.mode, Size: int64(len(m.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", m.name, err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatalf("tar body %s: %v", m.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	writeFile(t, path, buf.Bytes())
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	sum, err := FileSHA256(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return sum
}

// exists reports whether a path is there, which is what most of the stop-point
// assertions are phrased over.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// host is a whole simulated installation: a `<prefix>` and a `<state_dir>` in
// two separate directories, with an installed binary and, optionally, a staged
// release under `update/`.
type host struct {
	t      *testing.T
	layout Layout
	keys   KeySet
	// installed is the bytes of the binary at `<prefix>/llamaman` at the moment
	// the host was built, which every "nothing was touched" assertion compares
	// against.
	installed []byte
}

func newHost(t *testing.T) *host {
	t.Helper()
	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	stateDir := filepath.Join(root, "state")
	for _, dir := range []string{prefix, filepath.Join(stateDir, UpdateDirName)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	installed := []byte("#!/bin/sh\necho v1.1.0\n")
	writeFile(t, filepath.Join(prefix, BinaryName), installed)
	if err := os.Chmod(filepath.Join(prefix, BinaryName), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, keys := testKey(t)
	return &host{
		t:         t,
		layout:    Layout{StateDir: stateDir, Prefix: prefix},
		keys:      keys,
		installed: installed,
	}
}

// stage writes a verifiable release plus the marker that names it, which is the
// on-disk state section 12.1 step 6 leaves behind.
func (h *host) stage(version string) release {
	h.t.Helper()
	rel := stageRelease(h.t, h.layout.UpdateDir(), version, hostArch, "echo "+version)
	h.writeMarker(Marker{
		Format:        MarkerFormat,
		SelfUpdateID:  "01J8ZQ7X00000000000000TEST",
		FromVersion:   "v1.1.0",
		TargetVersion: version,
		BinaryPath:    h.layout.InstalledPath(),
		StagedAt:      1788012345678,
	})
	return rel
}

func (h *host) writeMarker(m Marker) {
	h.t.Helper()
	if err := WriteMarker(h.layout.PendingPath(), m); err != nil {
		h.t.Fatalf("write the marker: %v", err)
	}
}

// assertInstalledUnchanged is the assertion every refusal row of section 12.3's
// stop-point table shares: the actor "wrote nothing, deleted nothing, stopped
// nothing", so the installed binary is byte-for-byte what it found.
func (h *host) assertInstalledUnchanged() {
	h.t.Helper()
	got := mustRead(h.t, h.layout.InstalledPath())
	if !bytes.Equal(got, h.installed) {
		h.t.Errorf("the installed binary changed: got %q, want %q", got, h.installed)
	}
}

// assertNoDatabaseFiles is section 19's fifth preservation property, asserted by
// directory diff: **the privileged actors never open llamaman.db, not even
// read-only**, because a root-created `-wal`/`-shm` beside it is a database the
// service identity can never write again.
func (h *host) assertNoDatabaseFiles() {
	h.t.Helper()
	for _, name := range []string{"llamaman.db", "llamaman.db-wal", "llamaman.db-shm"} {
		if exists(filepath.Join(h.layout.StateDir, name)) {
			h.t.Errorf("%s exists: an actor opened the database (DESIGN section 11.3)", name)
		}
	}
}

// hostArch is the architecture the fixtures name their tarballs for. It is a
// constant rather than runtime.GOARCH so a fixture written on one machine is the
// same fixture on another; every caller passes it explicitly through GOARCH.
const hostArch = "amd64"

// fakeUnits satisfies UnitStater with a scripted answer, which is how the six
// rows of the judge's trigger truth table and the gate's branch 2 are driven: an
// ActiveState is a systemd state transition a fake has to be able to produce.
type fakeUnits struct {
	state string
	err   error
	calls int
}

func (f *fakeUnits) ActiveState(_ context.Context, _ string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.state, nil
}

// fakeJournal satisfies JournalTailer.
type fakeJournal struct{ text string }

func (f fakeJournal) Tail(_ context.Context, unit string, _ int) (string, error) {
	return unit + ": " + f.text, nil
}

// fakeUnitFiles satisfies UnitFacts for the guard's table-driven fixture.
type fakeUnitFiles map[string]UnitFile

func (f fakeUnitFiles) Unit(name string) (UnitFile, error) { return f[name], nil }

// gateFixture is a store plus a gate over a real temp directory.
type gateFixture struct {
	t     *testing.T
	store *store.Store
	gate  *Gate
	units *fakeUnits
	l     Layout
	now   time.Time
}

func newGateFixture(t *testing.T, version string) *gateFixture {
	t.Helper()
	stateDir := t.TempDir()
	st := openStore(t, stateDir)
	l := Layout{StateDir: stateDir, Prefix: filepath.Join(stateDir, "prefix")}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("create the update directory: %v", err)
	}

	now := time.Unix(1788012345, 0).UTC()
	units := &fakeUnits{state: "inactive"}
	g := NewGate(GateConfig{
		Store:   st,
		Layout:  l,
		Version: version,
		Units:   units,
		Journal: fakeJournal{text: "a journal line"},
		Now:     func() time.Time { return now },
	})
	return &gateFixture{t: t, store: st, gate: g, units: units, l: l, now: now}
}

// seedUpdate inserts a `self_updates` row and its paired job in one transaction,
// which is the pairing §2.3a fixes and every gate branch reads.
func (f *gateFixture) seedUpdate(id string, rowState model.SelfUpdateState,
	jobState model.JobState, to string) {

	f.t.Helper()
	f.seedLeasedUpdate(id, rowState, jobState, to, "")
}

// seedLeasedUpdate is seedUpdate with a LEASE OWNER on the job, which is the one
// fact that distinguishes "this daemon is applying the update right now" from
// "a boot that is gone left it behind" (§2.3). An empty owner leaves the column
// NULL, which is every other fixture in this file.
func (f *gateFixture) seedLeasedUpdate(id string, rowState model.SelfUpdateState,
	jobState model.JobState, to, leaseOwner string) {

	f.t.Helper()
	ctx := context.Background()
	err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := f.store.InsertSelfUpdate(ctx, tx, store.SelfUpdate{
			ID: id, FromVersion: "v1.1.0", ToVersion: to, Channel: "stable",
			State: rowState, CreatedAt: f.now.UnixMilli(),
		}); err != nil {
			return err
		}
		j := model.Job{
			ID:          "job-" + id,
			Kind:        model.JobSelfUpdate,
			SubjectType: model.SubjectSelfUpdate,
			SubjectID:   id,
			State:       jobState,
			Priority:    100,
			RunAfter:    f.now.UnixMilli(),
			MaxAttempts: 1,
			CreatedAt:   f.now.UnixMilli(),
		}
		if leaseOwner != "" {
			owner := leaseOwner
			expires := f.now.Add(time.Minute).UnixMilli()
			j.LeaseOwner, j.LeaseExpiresAt = &owner, &expires
		}
		return f.store.InsertJob(ctx, tx, j)
	})
	if err != nil {
		f.t.Fatalf("seed the self-update rows: %v", err)
	}
}

func (f *gateFixture) row(id string) store.SelfUpdate {
	f.t.Helper()
	var out store.SelfUpdate
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = f.store.SelfUpdate(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read self_update %s: %v", id, err)
	}
	return out
}

func (f *gateFixture) job(id string) model.Job {
	f.t.Helper()
	var out model.Job
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = f.store.Job(ctx, tx, "job-"+id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read job for %s: %v", id, err)
	}
	return out
}

func (f *gateFixture) notifications() []string {
	f.t.Helper()
	var codes []string
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT code FROM notifications ORDER BY at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				return err
			}
			codes = append(codes, code)
		}
		return rows.Err()
	})
	if err != nil {
		f.t.Fatalf("read the notifications: %v", err)
	}
	return codes
}

// writeMarker writes one into the fixture's update directory.
func (f *gateFixture) writeMarker(m Marker) {
	f.t.Helper()
	if err := WriteMarker(f.l.PendingPath(), m); err != nil {
		f.t.Fatalf("write the marker: %v", err)
	}
}

// scratch writes a file into `update/` that is not the marker, so that "the
// branch cleared the scratch" is an assertion about a file that was really there.
func (f *gateFixture) scratch(name string) string {
	f.t.Helper()
	path := filepath.Join(f.l.UpdateDir(), name)
	writeFile(f.t, path, []byte("debris"))
	return path
}

// copyOf is used by the actor tests to prove the retained binary is
// byte-identical to the one that was replaced.
func copyOf(t *testing.T, src string) []byte {
	t.Helper()
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	return b
}

// fakeRestarter records the one systemd verb the swap actor issues, so that
// "no step in the protocol stops a unit" can be asserted structurally: there is
// no Stop on this type to call, and the recorded verbs are checked.
type fakeRestarter struct{ restarted []string }

func (f *fakeRestarter) RestartNoBlock(_ context.Context, unit string) error {
	f.restarted = append(f.restarted, unit)
	return nil
}

// openStore is a migrated database in a directory the test owns. The gate is one
// of the components that cannot be tested against a fake store: its three
// branches and its closing pass are each ONE TRANSACTION over two tables, and a
// fake would be asserting against itself rather than against the pairing §2.3a
// fixes.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background(), store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate the fixture database: %v", err)
	}
	return st
}

func fmtVersions(from, to string) string { return fmt.Sprintf("%s -> %s", from, to) }

// writeTarballWithout writes a release tarball that carries everything EXCEPT
// the binary, which is the shape a wrong asset has.
func writeTarballWithout(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("a license\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "LICENSE", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	writeFile(t, path, buf.Bytes())
}
