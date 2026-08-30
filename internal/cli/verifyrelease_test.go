package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
)

// The trust root is now a VALUE these tests pass in rather than a pair of
// package-level `var`s they swap out, and that is the point rather than a
// convenience: there is one key source in the product — the keys embedded from
// `internal/selfupdate/keys/*.pub`, which `POST /update/apply` and the root swap
// actor also verify against — so there is no second pair for a test to install.
// A test supplies its own KeySet to the function under test; nothing global
// moves, and the tests below are parallel-safe as a result.
func keySet(current, next ed25519.PublicKey) selfupdate.KeySet {
	var keys selfupdate.KeySet
	if current != nil {
		keys = append(keys, current)
	}
	if next != nil {
		keys = append(keys, next)
	}
	return keys
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return pub, priv
}

// releaseDir builds a directory shaped like a downloaded release: some payload
// files, a checksums.txt naming them, and a signature over its exact bytes.
type releaseDir struct {
	dir     string
	sumsRaw []byte
}

func newReleaseDir(t *testing.T, files map[string]string) *releaseDir {
	t.Helper()

	dir := t.TempDir()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// Deterministic order so the signed bytes are stable across runs.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	var sums bytes.Buffer
	for _, name := range names {
		body := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		sum := sha256.Sum256([]byte(body))
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return &releaseDir{dir: dir, sumsRaw: sums.Bytes()}
}

// sign writes checksums.txt and checksums.txt.sig. encode selects the on-disk
// signature encoding, since decodeSignature accepts three.
func (r *releaseDir) sign(t *testing.T, priv ed25519.PrivateKey, encode func([]byte) []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(r.dir, ChecksumsFileName), r.sumsRaw, 0o644); err != nil {
		t.Fatalf("writing checksums: %v", err)
	}
	sig := ed25519.Sign(priv, r.sumsRaw)
	if err := os.WriteFile(filepath.Join(r.dir, ChecksumsSigFileName), encode(sig), 0o644); err != nil {
		t.Fatalf("writing signature: %v", err)
	}
}

func sigBase64(sig []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func sigRaw(sig []byte) []byte { return sig }

func sigHex(sig []byte) []byte { return []byte(hex.EncodeToString(sig) + "\n") }

// TestVerifyReleaseAccepts is the happy path in its three shapes: the current
// key, the rotation key, and the three signature encodings.
func TestVerifyReleaseAccepts(t *testing.T) {
	pubA, privA := genKey(t)
	pubB, privB := genKey(t)

	tests := []struct {
		name     string
		current  ed25519.PublicKey
		next     ed25519.PublicKey
		signWith ed25519.PrivateKey
		encode   func([]byte) []byte
		wantKey  int
	}{
		{"current key, base64 signature", pubA, pubB, privA, sigBase64, 0},
		{"next key accepted during rotation", pubA, pubB, privB, sigBase64, 1},
		{"raw 64-byte signature", pubA, nil, privA, sigRaw, 0},
		{"hex signature", pubA, nil, privA, sigHex, 0},
		{"no next key configured", pubA, nil, privA, sigBase64, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseDir(t, map[string]string{
				"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload",
				"llamaman_v1.0.0_linux_arm64.tar.gz": "arm64 payload",
			})
			r.sign(t, tc.signWith, tc.encode)

			got, err := verifyRelease(r.dir, []string{"llamaman_v1.0.0_linux_amd64.tar.gz"},
				keySet(tc.current, tc.next))
			if err != nil {
				t.Fatalf("verifyRelease: %v", err)
			}
			if got.KeyIndex != tc.wantKey {
				t.Errorf("KeyIndex = %d, want %d", got.KeyIndex, tc.wantKey)
			}
			want := []string{
				"llamaman_v1.0.0_linux_amd64.tar.gz",
				"llamaman_v1.0.0_linux_arm64.tar.gz",
			}
			if diff := cmp.Diff(want, got.Verified); diff != "" {
				t.Errorf("Verified (-want +got):\n%s", diff)
			}
			if len(got.Skipped) != 0 {
				t.Errorf("Skipped = %v, want none", got.Skipped)
			}
		})
	}
}

// TestVerifyReleaseSkipsAbsent is the installer's actual shape: checksums.txt
// lists both architectures and only one tarball was downloaded. The absent one
// is SKIPPED, not a failure — and the one that is there still has to verify.
func TestVerifyReleaseSkipsAbsent(t *testing.T) {
	t.Parallel()

	pub, priv := genKey(t)

	r := newReleaseDir(t, map[string]string{
		"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload",
		"llamaman_v1.0.0_linux_arm64.tar.gz": "arm64 payload",
	})
	r.sign(t, priv, sigBase64)

	if err := os.Remove(filepath.Join(r.dir, "llamaman_v1.0.0_linux_arm64.tar.gz")); err != nil {
		t.Fatalf("removing arm64 tarball: %v", err)
	}

	got, err := verifyRelease(r.dir, []string{"llamaman_v1.0.0_linux_amd64.tar.gz"}, keySet(pub, nil))
	if err != nil {
		t.Fatalf("verifyRelease: %v", err)
	}
	if diff := cmp.Diff([]string{"llamaman_v1.0.0_linux_amd64.tar.gz"}, got.Verified); diff != "" {
		t.Errorf("Verified (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"llamaman_v1.0.0_linux_arm64.tar.gz"}, got.Skipped); diff != "" {
		t.Errorf("Skipped (-want +got):\n%s", diff)
	}
}

// TestVerifyReleaseRefuses is the refusal table. Every row is a way a download
// can be wrong, and each must produce a distinguishable message rather than a
// generic failure — "wrong key" and "corrupt tarball" send an operator to very
// different places.
func TestVerifyReleaseRefuses(t *testing.T) {
	goodFiles := map[string]string{"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload"}

	tests := []struct {
		name string
		// setup returns the directory to verify and the trust root to verify it
		// against — a value rather than a global, because there is only one key
		// source in the product and a test cannot install a second.
		setup   func(t *testing.T) (string, selfupdate.KeySet)
		require []string
		wantMsg string
	}{
		{
			name: "no key compiled in is a refusal, never a skip",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				_, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				return r.dir, nil
			},
			wantMsg: "no release signing key is compiled into this binary",
		},
		{
			name: "signed by a key this binary does not accept",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, _ := genKey(t)
				_, otherPriv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, otherPriv, sigBase64)
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "does not verify against the release signing key",
		},
		{
			name: "checksums.txt edited after signing",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				path := filepath.Join(r.dir, ChecksumsFileName)
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading checksums: %v", err)
				}
				edited := bytes.Replace(b, []byte("a"), []byte("b"), 1)
				if err := os.WriteFile(path, edited, 0o644); err != nil {
					t.Fatalf("writing checksums: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "does not verify against the release signing key",
		},
		{
			name: "payload does not match its signed digest",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				path := filepath.Join(r.dir, "llamaman_v1.0.0_linux_amd64.tar.gz")
				if err := os.WriteFile(path, []byte("swapped payload"), 0o644); err != nil {
					t.Fatalf("rewriting payload: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "sha256 mismatch",
		},
		{
			name: "truncated tarball",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				path := filepath.Join(r.dir, "llamaman_v1.0.0_linux_amd64.tar.gz")
				if err := os.WriteFile(path, []byte("amd64 pay"), 0o644); err != nil {
					t.Fatalf("truncating payload: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "sha256 mismatch",
		},
		{
			name: "a required file is not in this release at all",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				return r.dir, keySet(pub, nil)
			},
			require: []string{"llamaman_v1.0.0_linux_arm64.tar.gz"},
			wantMsg: "does not list",
		},
		{
			name: "a required file is listed but was not downloaded",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, map[string]string{
					"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload",
					"llamaman_v1.0.0_linux_arm64.tar.gz": "arm64 payload",
				})
				r.sign(t, priv, sigBase64)
				if err := os.Remove(filepath.Join(r.dir, "llamaman_v1.0.0_linux_arm64.tar.gz")); err != nil {
					t.Fatalf("removing arm64: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			require: []string{"llamaman_v1.0.0_linux_arm64.tar.gz"},
			wantMsg: "is required but was not found",
		},
		{
			name: "nothing at all was downloaded",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				if err := os.Remove(filepath.Join(r.dir, "llamaman_v1.0.0_linux_amd64.tar.gz")); err != nil {
					t.Fatalf("removing payload: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "nothing was verified",
		},
		{
			name: "checksums.txt absent",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, _ := genKey(t)
				return t.TempDir(), keySet(pub, nil)
			},
			wantMsg: "reading checksums.txt",
		},
		{
			name: "signature absent",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, _ := genKey(t)
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, ChecksumsFileName), []byte("x\n"), 0o644); err != nil {
					t.Fatalf("writing checksums: %v", err)
				}
				return dir, keySet(pub, nil)
			},
			wantMsg: "reading checksums.txt.sig",
		},
		{
			name: "signature is not 64 bytes in any encoding",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				r := newReleaseDir(t, goodFiles)
				r.sign(t, priv, sigBase64)
				path := filepath.Join(r.dir, ChecksumsSigFileName)
				if err := os.WriteFile(path, []byte("not a signature\n"), 0o644); err != nil {
					t.Fatalf("writing signature: %v", err)
				}
				return r.dir, keySet(pub, nil)
			},
			wantMsg: "is not a 64-byte ed25519 signature",
		},
		{
			name: "signed but empty checksums file",
			setup: func(t *testing.T) (string, selfupdate.KeySet) {
				pub, priv := genKey(t)
				dir := t.TempDir()
				body := []byte("# nothing here\n")
				if err := os.WriteFile(filepath.Join(dir, ChecksumsFileName), body, 0o644); err != nil {
					t.Fatalf("writing checksums: %v", err)
				}
				sig := ed25519.Sign(priv, body)
				if err := os.WriteFile(filepath.Join(dir, ChecksumsSigFileName), sigBase64(sig), 0o644); err != nil {
					t.Fatalf("writing signature: %v", err)
				}
				return dir, keySet(pub, nil)
			},
			wantMsg: "names no files",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, keys := tc.setup(t)
			_, err := verifyRelease(dir, tc.require, keys)
			if err == nil {
				t.Fatalf("verifyRelease succeeded, want an error containing %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestParseChecksums pins the `sha256sum` output format this design consumes,
// including the shapes that must be refused rather than interpreted.
//
// It drives selfupdate.ParseChecksums, which is the ONE parser: `install.sh`'s
// verification path and the self-update path read the same signed file, and a
// second implementation of "what does this line mean" is how the two ends of a
// trust boundary come to disagree. This file used to carry that second
// implementation, and the two had already drifted.
func TestParseChecksums(t *testing.T) {
	t.Parallel()

	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string
	}{
		{
			name: "two-space text mode",
			in:   digest + "  llamaman_v1.0.0_linux_amd64.tar.gz\n",
			want: map[string]string{"llamaman_v1.0.0_linux_amd64.tar.gz": digest},
		},
		{
			name: "binary-mode star marker",
			in:   digest + " *llamaman_v1.0.0_linux_arm64.tar.gz\n",
			want: map[string]string{"llamaman_v1.0.0_linux_arm64.tar.gz": digest},
		},
		{
			name: "uppercase digest is normalized",
			in:   strings.ToUpper(digest) + "  install.sh\n",
			want: map[string]string{"install.sh": digest},
		},
		{
			name: "blank lines, comments and CRLF",
			in:   "\n# a comment\r\n" + digest + "  install.sh\r\n\n",
			want: map[string]string{"install.sh": digest},
		},
		{
			name:    "short digest",
			in:      "abc  install.sh\n",
			wantErr: "is not a sha256",
		},
		{
			name:    "non-hex digest",
			in:      strings.Repeat("z", 64) + "  install.sh\n",
			wantErr: "is not hex",
		},
		{
			name:    "a path is refused, not joined",
			in:      digest + "  ../../etc/passwd\n",
			wantErr: "is not a plain file name",
		},
		{
			name:    "no separator",
			in:      digest + "\n",
			wantErr: "is not `<sha256>  <name>`",
		},
		{
			name:    "nothing but comments",
			in:      "# nothing here\n",
			wantErr: "names no files",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := selfupdate.ParseChecksums([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseChecksums succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChecksums: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("entries (-want +got):\n%s", diff)
			}
		})
	}
}

// TestVerifyReleaseCommand covers the flag surface: the positional argument is
// mandatory, --quiet suppresses the report, and a failure explains itself on
// stderr rather than only through the exit status.
func TestVerifyReleaseCommand(t *testing.T) {
	t.Parallel()

	pub, priv := genKey(t)
	keys := keySet(pub, nil)

	newEnv := func() (Env, *bytes.Buffer, *bytes.Buffer) {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		return Env{Stdout: out, Stderr: errOut}, out, errOut
	}

	t.Run("no directory", func(t *testing.T) {
		env, _, errOut := newEnv()
		if err := verifyReleaseCommand(env, nil, keys); err == nil {
			t.Fatal("VerifyRelease succeeded with no directory")
		}
		if !strings.Contains(errOut.String(), "exactly one directory") {
			t.Errorf("stderr = %q", errOut.String())
		}
	})

	t.Run("two directories", func(t *testing.T) {
		env, _, _ := newEnv()
		if err := verifyReleaseCommand(env, []string{"/a", "/b"}, keys); err == nil {
			t.Fatal("VerifyRelease succeeded with two directories")
		}
	})

	t.Run("reports the key and every file", func(t *testing.T) {
		r := newReleaseDir(t, map[string]string{
			"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload",
			"llamaman_v1.0.0_linux_arm64.tar.gz": "arm64 payload",
		})
		r.sign(t, priv, sigBase64)
		if err := os.Remove(filepath.Join(r.dir, "llamaman_v1.0.0_linux_arm64.tar.gz")); err != nil {
			t.Fatalf("removing arm64: %v", err)
		}

		env, out, _ := newEnv()
		args := []string{"--require", "llamaman_v1.0.0_linux_amd64.tar.gz", r.dir}
		if err := verifyReleaseCommand(env, args, keys); err != nil {
			t.Fatalf("VerifyRelease: %v", err)
		}
		got := out.String()
		for _, want := range []string{
			"signature ok (current release key)",
			"sha256 ok  llamaman_v1.0.0_linux_amd64.tar.gz",
			"skipped    llamaman_v1.0.0_linux_arm64.tar.gz",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("stdout = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("quiet prints nothing on success", func(t *testing.T) {
		r := newReleaseDir(t, map[string]string{"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload"})
		r.sign(t, priv, sigBase64)

		env, out, _ := newEnv()
		if err := verifyReleaseCommand(env, []string{"--quiet", r.dir}, keys); err != nil {
			t.Fatalf("VerifyRelease: %v", err)
		}
		if out.String() != "" {
			t.Errorf("stdout = %q, want empty", out.String())
		}
	})

	t.Run("failure explains itself on stderr", func(t *testing.T) {
		_, otherPriv := genKey(t)
		r := newReleaseDir(t, map[string]string{"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload"})
		r.sign(t, otherPriv, sigBase64)

		env, _, errOut := newEnv()
		if err := verifyReleaseCommand(env, []string{r.dir}, keys); err == nil {
			t.Fatal("VerifyRelease succeeded against a foreign signature")
		}
		if !strings.Contains(errOut.String(), "llamaman verify-release:") {
			t.Errorf("stderr = %q, want the llamaman: prefix section 13 requires", errOut.String())
		}
	})
}

// TestVerifyReleaseCreatesNothing is §11.3's blanket rule for a root-invocable
// subcommand, asserted where it is cheapest: verify-release must leave the
// directory it was pointed at byte-for-byte as it found it, and must not create
// anything beside it.
func TestVerifyReleaseCreatesNothing(t *testing.T) {
	t.Parallel()

	pub, priv := genKey(t)

	r := newReleaseDir(t, map[string]string{"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload"})
	r.sign(t, priv, sigBase64)

	before := dirListing(t, r.dir)
	env := Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := verifyReleaseCommand(env, []string{r.dir}, keySet(pub, nil)); err != nil {
		t.Fatalf("verify-release: %v", err)
	}
	if diff := cmp.Diff(before, dirListing(t, r.dir)); diff != "" {
		t.Errorf("directory changed (-before +after):\n%s", diff)
	}
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		out = append(out, e.Name()+" "+info.Mode().String())
	}
	return out
}

// TestOneTrustRoot is the assertion whose absence let two of them exist.
//
// `install.sh` verifies a download by running `llamaman verify-release` (DESIGN
// section 13 step 3); `POST /update/apply` (section 12.1 step 3) and the root
// swap actor (section 12.2 step 0) verify through selfupdate.EmbeddedKeys().
// Those used to be two committed key pairs with two decoders and two parsers,
// and nothing — not the build, not the tests, not installer/README.md, not
// release.yml's pre-build gate — asserted they agreed. The consequence of
// generating the real keypair by the documented procedure would have been an
// installer that verified fine, a release job that published green, and a
// self-update that refused every tarball on every host forever.
//
// Two properties are asserted here, and together they are "one trust root":
// the command's key source IS the embedded set, and a signature either verifies
// for both callers or for neither.
func TestOneTrustRoot(t *testing.T) {
	t.Parallel()

	embedded, err := selfupdate.EmbeddedKeys()
	if err != nil {
		t.Fatalf("the embedded release keys do not load: %v", err)
	}
	root, err := releaseTrustRoot()
	if err != nil {
		t.Fatalf("releaseTrustRoot: %v", err)
	}
	if diff := cmp.Diff(embedded, root); diff != "" {
		t.Errorf("verify-release does not use the embedded key set (-embedded +verify-release):\n%s", diff)
	}

	// And the same bytes get the same verdict from both ends. The daemon's half
	// is selfupdate.VerifyChecksums; the installer's is verifyRelease.
	pub, priv := genKey(t)
	_, foreign := genKey(t)

	for _, tc := range []struct {
		name string
		sign ed25519.PrivateKey
		want bool
	}{
		{"a key both ends carry", priv, true},
		{"a key neither end carries", foreign, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseDir(t, map[string]string{
				"llamaman_v1.0.0_linux_amd64.tar.gz": "amd64 payload",
			})
			r.sign(t, tc.sign, sigBase64)
			keys := keySet(pub, nil)

			sums, err := os.ReadFile(filepath.Join(r.dir, ChecksumsFileName))
			if err != nil {
				t.Fatalf("reading checksums: %v", err)
			}
			sig, err := os.ReadFile(filepath.Join(r.dir, ChecksumsSigFileName))
			if err != nil {
				t.Fatalf("reading signature: %v", err)
			}

			daemonOK := selfupdate.VerifyChecksums(sums, sig, keys) == nil
			_, cliErr := verifyRelease(r.dir, nil, keys)
			cliOK := cliErr == nil

			if daemonOK != tc.want || cliOK != tc.want {
				t.Errorf("the two ends of the trust boundary disagree: "+
					"self-update accepted=%v, verify-release accepted=%v, want %v (%v)",
					daemonOK, cliOK, tc.want, cliErr)
			}
		})
	}
}
