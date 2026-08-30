package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Release verification (DESIGN section 12.1 step 3, section 16.2 step 3).
//
// The property under test is the one section 12.1 states in italics: **a
// signature failure aborts hard**. It is never downgraded to a warning and never
// satisfied by "the sha256 matched" — a checksums file an attacker wrote agrees
// perfectly with a tarball the same attacker wrote.

// TestVerifyStaged is the table of everything that can be wrong with a staged
// release, and what each one must produce.
func TestVerifyStaged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// corrupt mutates the staged directory after a good release was written.
		corrupt func(t *testing.T, dir string, rel release)
		wantErr error
	}{
		{
			name: "a verifiable release",
		},
		{
			// The case the whole compiled-in key exists for: the bytes were
			// replaced AND the checksums file was rewritten to agree with them, so
			// only the signature can tell.
			name: "the tarball and its checksums were both replaced",
			corrupt: func(t *testing.T, dir string, rel release) {
				replaced := stageRelease(t, dir, "v1.2.0", hostArch, "echo hostile")
				_ = replaced
				// stageRelease re-signed with the test key, so put back a
				// signature that does NOT match the new checksums.
				writeFile(t, filepath.Join(dir, SignatureName),
					make([]byte, ed25519.SignatureSize))
			},
			wantErr: ErrSignature,
		},
		{
			name: "the signature is corrupt",
			corrupt: func(t *testing.T, dir string, rel release) {
				sig := mustRead(t, filepath.Join(dir, SignatureName))
				sig[0] ^= 0xff
				writeFile(t, filepath.Join(dir, SignatureName), sig)
			},
			wantErr: ErrSignature,
		},
		{
			name: "the signature is missing",
			corrupt: func(t *testing.T, dir string, rel release) {
				if err := os.Remove(filepath.Join(dir, SignatureName)); err != nil {
					t.Fatalf("remove the signature: %v", err)
				}
			},
		},
		{
			// The signature still verifies — the checksums file is untouched —
			// and the tarball is not the file it names.
			name: "the tarball does not match its digest",
			corrupt: func(t *testing.T, dir string, rel release) {
				writeFile(t, filepath.Join(dir, rel.tarball), []byte("not the tarball"))
			},
			wantErr: ErrDigest,
		},
		{
			name: "checksums.txt does not name this architecture's tarball",
			corrupt: func(t *testing.T, dir string, rel release) {
				body := mustRead(t, filepath.Join(dir, ChecksumsName))
				// Re-sign, so the failure is the missing NAME and not the
				// signature: the two must be distinguishable.
				renamed := []byte(string(body[:64]) + "  llamaman_v1.2.0_linux_riscv64.tar.gz\n")
				writeFile(t, filepath.Join(dir, ChecksumsName), renamed)
				priv, _ := testKey(t)
				writeFile(t, filepath.Join(dir, SignatureName), ed25519.Sign(priv, renamed))
			},
			wantErr: ErrDigest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			rel := stageRelease(t, dir, "v1.2.0", hostArch, "echo v1.2.0")
			if tc.corrupt != nil {
				tc.corrupt(t, dir, rel)
			}

			_, keys := testKey(t)
			_, err := VerifyStaged(dir, rel.tarball, keys)
			switch {
			case tc.wantErr == nil && tc.corrupt == nil:
				if err != nil {
					t.Fatalf("a verifiable release did not verify: %v", err)
				}
			case tc.wantErr == nil:
				if err == nil {
					t.Fatal("a corrupt release verified")
				}
			default:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
			}
		})
	}
}

// TestVerifyAcceptsBothSignatureEncodings: a release job that pipes the
// signature through `base64` and one that writes the raw 64 bytes both produce a
// release this binary can install. The alternative is a class of failure only
// the first host to update would ever discover.
func TestVerifyAcceptsBothSignatureEncodings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rel := stageRelease(t, dir, "v1.2.0", hostArch, "echo v1.2.0")
	_, keys := testKey(t)

	raw := mustRead(t, filepath.Join(dir, SignatureName))
	if _, err := VerifyStaged(dir, rel.tarball, keys); err != nil {
		t.Fatalf("raw signature: %v", err)
	}

	writeFile(t, filepath.Join(dir, SignatureName),
		[]byte(base64.StdEncoding.EncodeToString(raw)+"\n"))
	if _, err := VerifyStaged(dir, rel.tarball, keys); err != nil {
		t.Fatalf("base64 signature: %v", err)
	}
}

// TestEmbeddedKeys asserts the two compiled-in keys parse and are distinct — the
// current key and the "next" key, so a rotation needs no flag day (section 12.1
// step 3).
//
// It also asserts the property that makes the shipped placeholder SAFE: a
// signature made with the test key does not verify against either embedded key.
// Until the release job's public key is committed, self-update refuses every
// tarball at the signature step rather than installing an unsigned one.
func TestEmbeddedKeys(t *testing.T) {
	t.Parallel()

	keys, err := EmbeddedKeys()
	if err != nil {
		t.Fatalf("EmbeddedKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("the binary embeds %d release keys, want 2 (current and next)", len(keys))
	}
	if string(keys[0]) == string(keys[1]) {
		t.Error("the current and next release keys are the same key, so a rotation would be a flag day")
	}

	// The ORDER is load-bearing, not incidental. EmbeddedKeys sorts by file
	// name, and two things read index 0 as "the current key": `verify-release`'s
	// report, which says `current` or `next (rotation)` so an operator can see a
	// rotation in progress, and release.yml's pre-build gate, which prints the
	// same distinction. A renamed key file would silently invert both.
	current, err := fs.ReadFile(keyFS, "keys/release-current.pub")
	if err != nil {
		t.Fatalf("reading the current key file: %v", err)
	}
	wantFirst, err := ParsePublicKey(current)
	if err != nil {
		t.Fatalf("parsing the current key file: %v", err)
	}
	if string(keys[0]) != string(wantFirst) {
		t.Error("keys[0] is not release-current.pub: `verify-release` would report a " +
			"rotation backwards, and release.yml's gate would name the wrong key")
	}

	priv, _ := testKey(t)
	message := []byte("checksums.txt\n")
	if err := VerifyChecksums(message, ed25519.Sign(priv, message), keys); !errors.Is(err, ErrSignature) {
		t.Errorf("a signature from a key that is not a release key verified: %v", err)
	}
}

// TestParsePublicKeyRejectsGarbage: the key format is deliberately dull, and
// every way of getting it wrong has to be a refusal rather than a key that
// verifies nothing and says nothing.
func TestParsePublicKeyRejectsGarbage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"comments only", "# a key file with no key\n\n"},
		{"not base64", "this is not base64!!\n"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("too short")) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePublicKey([]byte(tc.body)); err == nil {
				t.Error("a key file this malformed was accepted")
			}
		})
	}
}

// TestParseChecksums covers the `sha256sum` shapes a release job can produce.
func TestParseChecksums(t *testing.T) {
	t.Parallel()

	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := ParseChecksums([]byte(
		"# a comment\n" +
			digest + "  llamaman_v1.2.0_linux_amd64.tar.gz\n" +
			digest + " *llamaman_v1.2.0_linux_arm64.tar.gz\n\n"))
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	for _, name := range []string{
		"llamaman_v1.2.0_linux_amd64.tar.gz",
		"llamaman_v1.2.0_linux_arm64.tar.gz",
	} {
		if got[name] != digest {
			t.Errorf("%s: got %q, want %q", name, got[name], digest)
		}
	}

	if _, err := ParseChecksums([]byte("not a checksums file\n")); err == nil {
		t.Error("a malformed checksums file was accepted")
	}
}

// TestExtractBinaryTakesOnlyTheNamedMember: the tarball carries three files and
// the extractor takes exactly one, to a path the CALLER chose. That is the
// safety property — the archive contributes nothing to the destination — and it
// is what makes the root actor's extraction into `<prefix>` a swap rather than an
// arbitrary root-owned write.
func TestExtractBinaryTakesOnlyTheNamedMember(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rel := stageRelease(t, dir, "v1.2.0", hostArch, "echo v1.2.0")

	dest := filepath.Join(t.TempDir(), "extracted")
	digest, err := ExtractBinary(filepath.Join(dir, rel.tarball), dest, 0o755, rel.sha256)
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	if got := mustRead(t, dest); string(got) != string(rel.binary) {
		t.Errorf("extracted %q, want %q", got, rel.binary)
	}
	if digest != fileSHA(t, dest) {
		t.Error("ExtractBinary reported a digest that is not the extracted file's")
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("extracted mode %04o, want 0755", fi.Mode().Perm())
	}
}

// TestExtractBinaryNamesAMissingMember: a tarball with no `llamaman` in it is a
// wrong asset, and the failure is named rather than mysterious.
func TestExtractBinaryNamesAMissingMember(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.tar.gz")
	writeTarballWithout(t, path)

	_, err := ExtractBinary(path, filepath.Join(dir, "out"), 0o755, fileSHA(t, path))
	if !errors.Is(err, ErrMemberMissing) {
		t.Fatalf("got %v, want ErrMemberMissing", err)
	}
}

// TestExtractBinaryRefusesBytesTheSignatureDidNotCover is the D89 privilege
// boundary, as a regression test.
//
// `<state_dir>/update` is owned by the unprivileged SERVICE IDENTITY and
// `<prefix>` is root's, so the root actor of section 12.2 must never install
// bytes it did not itself verify. An earlier version of this code verified the
// tarball by PATH at step 0 and then re-opened that same path at step 2, which
// let the service identity rename a different tarball over the verified name
// between the two opens and have root extract it, chown it 0755 root:root and
// rename it onto `<prefix>/llamaman` — arbitrary root code execution by the
// exact principal section 12.2 says the actor "must not trust".
//
// The fix is that ExtractBinary hashes the bytes IT reads, in the same pass that
// feeds the tar reader, against the digest the signed checksums.txt named. This
// test is that swap performed deliberately: the file at the path is no longer
// the file the signature covered, and the extractor must refuse having installed
// nothing.
func TestExtractBinaryRefusesBytesTheSignatureDidNotCover(t *testing.T) {
	t.Parallel()

	staging := t.TempDir()
	signed := stageRelease(t, staging, "v1.2.0", hostArch, "echo v1.2.0")
	tarball := filepath.Join(staging, signed.tarball)

	// The signed digest, read the way section 12.2 step 0 reads it — out of the
	// checksums file the ed25519 signature covers, not off the disk.
	_, keys := testKey(t)
	signedDigest, err := VerifyStaged(staging, signed.tarball, keys)
	if err != nil {
		t.Fatalf("the staged release did not verify: %v", err)
	}
	if signedDigest != signed.sha256 {
		t.Fatalf("VerifyStaged returned %s, want the signed digest %s", signedDigest, signed.sha256)
	}

	// The service identity swaps in its own tarball, under the verified name.
	// It is a perfectly well-formed archive carrying a perfectly good binary —
	// it is simply not the one anybody signed.
	attacker := t.TempDir()
	writeTarball(t, filepath.Join(attacker, "evil.tar.gz"), []byte("#!/bin/sh\necho pwned\n"))
	writeFile(t, tarball, mustRead(t, filepath.Join(attacker, "evil.tar.gz")))

	prefix := t.TempDir()
	dest := filepath.Join(prefix, "llamaman.new.tmp")
	if _, err := ExtractBinary(tarball, dest, 0o755, signedDigest); !errors.Is(err, ErrDigest) {
		t.Fatalf("got %v, want ErrDigest", err)
	}
	if exists(dest) {
		t.Error("the swapped tarball's binary was written to <prefix>; nothing must land there")
	}
	// And no debris either: a refusal leaves `<prefix>` exactly as it found it.
	entries, err := os.ReadDir(prefix)
	if err != nil {
		t.Fatalf("read %s: %v", prefix, err)
	}
	for _, e := range entries {
		t.Errorf("a refused extraction left %s behind in <prefix>", e.Name())
	}
}

// TestExtractBinaryRequiresTheVerifiedDigest: there is no "extract without
// checking" mode, because the default that would suggest itself — an empty
// digest meaning "do not check" — is exactly the hole the parameter closes.
func TestExtractBinaryRequiresTheVerifiedDigest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rel := stageRelease(t, dir, "v1.2.0", hostArch, "echo v1.2.0")
	_, err := ExtractBinary(filepath.Join(dir, rel.tarball),
		filepath.Join(t.TempDir(), "out"), 0o755, "")
	if !errors.Is(err, ErrExpectedDigestRequired) {
		t.Fatalf("got %v, want ErrExpectedDigestRequired", err)
	}
}
