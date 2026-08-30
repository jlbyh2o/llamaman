package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The swap (DESIGN section 12.2, section 15).
//
// Three properties are asserted structurally rather than by inspection, because
// section 19 names all three as things to re-check on every change under
// `update/`:
//
//   - **Every change to the installed binary is one rename() between two names
//     in one directory.** No copy over a live path, no cross-filesystem move, no
//     two-step install — which is what makes the power-loss rows of section
//     12.3's table one line each and what removes the EXDEV the previous design
//     would have hit on any host with a separate /var.
//   - **No step in the protocol stops a unit.** The Restarter interface has no
//     Stop on it to call, and the fake records every verb issued.
//   - **The privileged actors never open llamaman.db**, asserted by directory
//     diff after every run.

// applyOn runs the swap against a host with the test key.
func applyOn(t *testing.T, h *host, restart Restarter) (ApplyResult, error) {
	t.Helper()
	return Apply(context.Background(), ApplyOptions{
		Scope:   model.ScopeSystem,
		Layout:  h.layout,
		Keys:    h.keys,
		Restart: restart,
		GOARCH:  hostArch,
	})
}

// stageForApply is a host with a verifiable release staged and a marker naming
// it — the on-disk state section 12.1 step 6 leaves behind, which is exactly
// what the swap actor wakes up to.
//
// The fixture's `<prefix>` is passed through ApplyOptions.Layout rather than
// derived, for the reason Apply's own comment gives: a test binary's
// os.Executable() is the test binary, in a build cache no test should write to.
func stageForApply(t *testing.T, version string) (*host, release) {
	t.Helper()
	h := newHost(t)
	rel := stageRelease(t, h.layout.UpdateDir(), version, hostArch, "echo "+version)
	h.writeMarker(Marker{
		Format: MarkerFormat, SelfUpdateID: "01J8ZQ7X00000000000000TEST",
		FromVersion: "v1.1.0", TargetVersion: version,
		BinaryPath: h.layout.InstalledPath(), StagedAt: 1788012345678,
	})
	return h, rel
}

// TestApplyPerformsTheD2UserScopeSwap: in the D2 topology there is no oneshot,
// because "the daemon performs section 12.2's swap sequence itself and then
// exits normally" (section 12.1 step 7, section 5.2a item 2). Apply IS that
// sequence — one implementation, two callers — so it must run in user scope.
//
// What refuses in user scope is the `selfupdate-apply` ENTRY POINT, which is a
// statement about who was summoned rather than about what the sequence does;
// internal/cli asserts that half. Running the sequence here and refusing there
// is the difference between a `--user-units` host that can self-update and one
// that stages updates it can never apply.
//
// Restart is nil deliberately, and that is the second half of the topology's
// difference: the user-scope daemon exits and Restart=always starts the binary
// now on disk (D79). Nothing issues a restart of the unit this process is.
func TestApplyPerformsTheD2UserScopeSwap(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	rel := h.stage("v1.2.0")

	res, err := Apply(context.Background(), ApplyOptions{
		Scope: model.ScopeUser, Layout: h.layout, Keys: h.keys, GOARCH: hostArch,
	})
	if err != nil {
		t.Fatalf("the user-scope in-process swap refused: %v", err)
	}
	if res.Restarted {
		t.Error("the user-scope swap issued a restart; it must exit and let Restart=always act (D79)")
	}
	if got := mustRead(t, h.layout.InstalledPath()); !bytes.Equal(got, rel.binary) {
		t.Errorf("installed %q, want the tarball's binary %q", got, rel.binary)
	}
	if got := mustRead(t, h.layout.RetainedPath()); !bytes.Equal(got, h.installed) {
		t.Errorf("retained %q, want the replaced binary %q", got, h.installed)
	}
	h.assertNoDatabaseFiles()
}

// TestApplyRefusesAnUnparsableScope is the other half of section 12.2's
// "a missing or unparsable `--scope` is a refusal, not a guess", at the level
// the sequence itself owns: a scope that is neither `system` nor `user` is not a
// topology this code knows how to be correct in.
func TestApplyRefusesAnUnparsableScope(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	h.stage("v1.2.0")
	_, err := Apply(context.Background(), ApplyOptions{
		Scope: model.SystemdScope("neither"), Layout: h.layout, Keys: h.keys, GOARCH: hostArch,
	})
	if err == nil {
		t.Fatal("an unparsable scope was accepted")
	}
	h.assertInstalledUnchanged()
	h.assertNoDatabaseFiles()
}

// TestApplyDefaultsToTheCompiledInKeys is the production wiring every other test
// in this file bypasses by passing its own KeySet.
//
// Section 12.2 step 0 re-verifies "against the compiled-in ed25519 key", and the
// root oneshot has no other key to offer — it never opens the database and has no
// configuration. A nil KeySet must therefore MEAN the embedded set, not an empty
// one: an empty KeySet makes VerifyChecksums fall through to ErrSignature for
// every genuine release, so a caller that simply omitted the field would refuse
// every update at preflight with a message that reads like a corrupt download.
//
// The embedded keys ship with their private halves discarded, so a release
// signed by this suite's key must NOT verify against them. The assertion is
// therefore that the failure is the signature — proof the embedded set was
// loaded and consulted — rather than that the swap succeeds.
func TestApplyDefaultsToTheCompiledInKeys(t *testing.T) {
	t.Parallel()

	h := newHost(t)
	h.stage("v1.2.0")

	_, err := Apply(context.Background(), ApplyOptions{
		Scope: model.ScopeSystem, Layout: h.layout, GOARCH: hostArch,
	})
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v, want ErrSignature from the compiled-in key set", err)
	}
	h.assertInstalledUnchanged()
	h.assertNoDatabaseFiles()
}

// TestApplyRefusalsTouchNothing is stop-point row 5, driven the five ways
// section 15 names: "--scope removed from the unit, `pending` truncated,
// `pending.binary_path` pointed elsewhere, `checksums.txt.sig` corrupted,
// `<prefix>` filled to ENOSPC".
//
// The ENOSPC row cannot be produced without filling a filesystem, so its
// stand-in is the check itself — asserted separately in TestCheckRoom below.
// Every other row is here, and every one asserts the same thing: **nothing
// written, nothing deleted, nothing stopped**.
func TestApplyRefusalsTouchNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		scope model.SystemdScope
		// break_ mutates the staged host into the refusal under test.
		break_ func(t *testing.T, h *host)
	}{
		{
			name:  "--scope was not rendered into the unit",
			scope: model.SystemdScope(""),
		},
		{
			name:  "pending is truncated",
			scope: model.ScopeSystem,
			break_: func(t *testing.T, h *host) {
				writeFile(t, h.layout.PendingPath(), []byte(`{"format":1,"from_ver`))
			},
		},
		{
			name:  "pending is absent",
			scope: model.ScopeSystem,
			break_: func(t *testing.T, h *host) {
				if err := os.Remove(h.layout.PendingPath()); err != nil {
					t.Fatalf("remove the marker: %v", err)
				}
			},
		},
		{
			name:  "pending.binary_path names another installation",
			scope: model.ScopeSystem,
			break_: func(t *testing.T, h *host) {
				m := Marker{
					Format: MarkerFormat, SelfUpdateID: "01J", FromVersion: "v1.1.0",
					TargetVersion: "v1.2.0", BinaryPath: "/opt/somewhere-else/llamaman",
					StagedAt: 1,
				}
				h.writeMarker(m)
			},
		},
		{
			name:  "checksums.txt.sig is corrupt",
			scope: model.ScopeSystem,
			break_: func(t *testing.T, h *host) {
				sig := mustRead(t, filepath.Join(h.layout.UpdateDir(), SignatureName))
				sig[0] ^= 0xff
				writeFile(t, filepath.Join(h.layout.UpdateDir(), SignatureName), sig)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHost(t)
			h.stage("v1.2.0")
			if tc.break_ != nil {
				tc.break_(t, h)
			}

			restart := &fakeRestarter{}
			_, err := Apply(context.Background(), ApplyOptions{
				Scope: tc.scope, Layout: h.layout, Keys: h.keys,
				Restart: restart, GOARCH: hostArch,
			})
			if err == nil {
				t.Fatal("the actor proceeded past a refusal")
			}

			h.assertInstalledUnchanged()
			h.assertNoDatabaseFiles()
			if exists(h.layout.RetainedPath()) {
				t.Error("a refusing actor retained a binary")
			}
			if exists(filepath.Join(h.layout.Prefix, installTmpName)) {
				t.Error("a refusing actor left a staged install behind")
			}
			if len(restart.restarted) != 0 {
				t.Errorf("a refusing actor issued %v; it must stop and start NOTHING",
					restart.restarted)
			}
			// The marker is left exactly as it was found — the actor never deletes
			// it, in any branch. Resolving it is the daemon's gate.
			if tc.name != "pending is absent" && !exists(h.layout.PendingPath()) {
				t.Error("a refusing actor deleted the marker")
			}
		})
	}
}

// TestApplySwapsAndRetains is the happy path of section 12.2 steps 1 through 4,
// with the D89 assertions section 15 names:
//
//   - `<prefix>/llamaman.prev` ends byte-identical to the binary that was
//     replaced;
//   - the actor installs a binary it extracted from the tarball IT re-verified
//     rather than the daemon's `update/llamaman.new` — the staged file is
//     asserted already unlinked at that point, which is the D89 (c) regression
//     test;
//   - the restart is issued, and it is the ONLY systemd verb.
func TestApplySwapsAndRetains(t *testing.T) {
	h, rel := stageForApply(t, "v1.2.0")

	// The daemon unlinked `update/llamaman.new` after its version probe, so it is
	// absent here — and the actor must not need it.
	if exists(h.layout.StagedBinaryPath()) {
		t.Fatal("the fixture left update/llamaman.new behind")
	}

	installedBefore := copyOf(t, h.layout.InstalledPath())
	restart := &fakeRestarter{}
	res, err := applyOn(t, h, restart)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := mustRead(t, h.layout.RetainedPath()); !bytes.Equal(got, installedBefore) {
		t.Error("<prefix>/llamaman.prev is not byte-identical to the binary it replaced")
	}
	if got := mustRead(t, h.layout.InstalledPath()); !bytes.Equal(got, rel.binary) {
		t.Error("the installed binary is not the one extracted from the re-verified tarball")
	}
	if res.RetainedSHA256 == res.InstalledSHA256 {
		t.Error("the retained and installed digests are the same; nothing was swapped")
	}
	if len(restart.restarted) != 1 || restart.restarted[0] != DaemonUnit {
		t.Errorf("systemd verbs issued: %v, want exactly one restart of %s",
			restart.restarted, DaemonUnit)
	}
	if exists(filepath.Join(h.layout.Prefix, installTmpName)) {
		t.Error("the staged install survived the rename")
	}
	h.assertNoDatabaseFiles()
}

// TestEveryBinaryChangeIsOneRenameInOneDirectory is section 19's second
// preservation property, asserted about the paths themselves: install, retain
// and revert are all renames between two names in `<prefix>`, so none of them
// can return EXDEV and none of them has an intermediate on-disk state.
func TestEveryBinaryChangeIsOneRenameInOneDirectory(t *testing.T) {
	t.Parallel()

	l := Layout{StateDir: "/var/lib/llamaman", Prefix: "/usr/local/bin"}
	paths := map[string]string{
		"the installed binary":  l.InstalledPath(),
		"the retained binary":   l.RetainedPath(),
		"the retain staging":    l.retainTmpPath(),
		"the install staging":   l.installTmpPath(),
		"the prefix itself + /": l.Prefix + "/x",
	}
	for name, path := range paths {
		if dir := filepath.Dir(path); dir != l.Prefix {
			t.Errorf("%s is in %s, not in <prefix> %s: a rename between them could return EXDEV",
				name, dir, l.Prefix)
		}
	}

	// And the staged release is deliberately NOT in <prefix>: it is owned by the
	// service identity, which is exactly why the actor extracts its own copy
	// rather than renaming that one into place (D89 (a) and (c)).
	if filepath.Dir(l.StagedBinaryPath()) == l.Prefix {
		t.Error("the staged binary is inside <prefix>, where the service identity could rewrite it")
	}
}

// TestSwapIsAtomicToAConcurrentReader is section 15's racing reader: "`<prefix>`
// never contains a partially written `llamaman` at any instant, by racing a
// reader that hashes it in a loop against the swap and accepting only the two
// known digests".
func TestSwapIsAtomicToAConcurrentReader(t *testing.T) {
	h, rel := stageForApply(t, "v1.2.0")

	before := fileSHA(t, h.layout.InstalledPath())
	afterWant := ""
	{
		// The digest the new binary will have, computed from the fixture's own
		// bytes rather than from the file under test.
		tmp := filepath.Join(t.TempDir(), "expected")
		writeFile(t, tmp, rel.binary)
		afterWant = fileSHA(t, tmp)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad []string
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sum, err := FileSHA256(h.layout.InstalledPath())
			if err != nil {
				// The file is never absent during a rename; an error here is a
				// finding, not a flake.
				mu.Lock()
				bad = append(bad, "unreadable: "+err.Error())
				mu.Unlock()
				continue
			}
			if sum != before && sum != afterWant {
				mu.Lock()
				bad = append(bad, "a third digest: "+sum)
				mu.Unlock()
			}
		}
	}()

	if _, err := applyOn(t, h, &fakeRestarter{}); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("Apply: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(bad) > 0 {
		t.Errorf("a reader saw <prefix>/llamaman in a state that is neither the old nor the "+
			"new binary: %v", bad)
	}
}

// TestApplyIsRerunnableAfterAPartialSwap is stop-point row 7: the actor was
// killed between steps 2 and 3, leaving `<prefix>/llamaman.new.tmp` behind.
// "`llamaman.new.tmp` is opened O_TRUNC by the next actor run, so it has a
// writer that reclaims it; nothing ever executes it."
func TestApplyIsRerunnableAfterAPartialSwap(t *testing.T) {
	h, rel := stageForApply(t, "v1.2.0")

	// The debris a killed actor leaves.
	writeFile(t, filepath.Join(h.layout.Prefix, installTmpName),
		[]byte("half an extracted binary"))
	writeFile(t, filepath.Join(h.layout.Prefix, retainTmpName),
		[]byte("half a retained binary"))

	if _, err := applyOn(t, h, &fakeRestarter{}); err != nil {
		t.Fatalf("Apply after a partial swap: %v", err)
	}
	if got := mustRead(t, h.layout.InstalledPath()); !bytes.Equal(got, rel.binary) {
		t.Error("the re-run installed the debris rather than the verified binary")
	}
	if exists(filepath.Join(h.layout.Prefix, installTmpName)) ||
		exists(filepath.Join(h.layout.Prefix, retainTmpName)) {
		t.Error("the re-run left its own staging files behind")
	}
}

// TestCheckRoom is step 0's last check, and the reason it is a check rather than
// a discovery: ENOSPC halfway through a swap is the one failure this sequence
// cannot make harmless.
func TestCheckRoom(t *testing.T) {
	t.Parallel()

	// A demand no filesystem can satisfy stands in for a full <prefix>.
	const impossible = int64(1) << 62
	if err := checkRoom(t.TempDir(), impossible); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("got %v, want ErrNoRoom", err)
	}
	if err := checkRoom(t.TempDir(), 1); err != nil {
		t.Fatalf("a one-byte demand was refused: %v", err)
	}
}
