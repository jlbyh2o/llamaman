package selfupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Release verification (DESIGN section 12.1 step 3 and section 16.2 step 3).
//
// The release job produces three artifacts per tag: the tarballs, a
// `checksums.txt` naming each one's sha256, and `checksums.txt.sig`, an ed25519
// signature over the bytes of `checksums.txt` made with a key held in repository
// secrets. The PUBLIC half is committed and compiled into the binary, together
// with a "next" key so a rotation needs no flag day.
//
// The trust root is the compiled-in key and nothing else. Not TLS, not the
// provenance attestation section 16.2 also publishes, not GitHub's own asset
// digest: verification must work offline with no external tooling, and the one
// place a compromised download would be catastrophic is exactly here. **A
// signature failure aborts hard** — it is never downgraded to a warning and
// never satisfied by "the sha256 matched", because a checksums file an attacker
// wrote agrees with a tarball the same attacker wrote.
//
// This runs TWICE per update, and that is the point. The unprivileged daemon
// verifies what it downloaded, and then the root actor re-verifies the same
// bytes itself, because between the two there is a genuine privilege boundary
// and a file the service identity could rewrite after the first check (D89).

//go:embed keys/*.pub
var keyFS embed.FS

// KeySet is the set of public keys a signature may verify against: the current
// release key and the next one.
type KeySet []ed25519.PublicKey

// EmbeddedKeys parses the keys compiled into this binary, in file-name order so
// the result is stable and a test can name them.
//
// A key file is one base64 line of the 32 raw public-key bytes, with `#` comment
// lines and blank lines ignored — the format is deliberately dull, because the
// component that has to work when everything else is broken must not need a
// parser anyone could get wrong.
func EmbeddedKeys() (KeySet, error) {
	entries, err := fs.ReadDir(keyFS, "keys")
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read the embedded key directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pub") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var keys KeySet
	for _, name := range names {
		b, err := fs.ReadFile(keyFS, path.Join("keys", name))
		if err != nil {
			return nil, fmt.Errorf("selfupdate: read the embedded key %s: %w", name, err)
		}
		k, err := ParsePublicKey(b)
		if err != nil {
			return nil, fmt.Errorf("selfupdate: embedded key %s: %w", name, err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("selfupdate: no release public key is compiled into this binary")
	}
	return keys, nil
}

// ParsePublicKey reads one key file.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("not base64: %w", err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("an ed25519 public key is %d bytes, got %d",
				ed25519.PublicKeySize, len(raw))
		}
		return ed25519.PublicKey(raw), nil
	}
	return nil, errors.New("the file contains no key line")
}

// ErrSignature is the hard abort of section 12.1 step 3. It is its own sentinel
// because the two callers report it differently — the daemon fails the job with
// it, the root actor writes one structured journald line and exits non-zero —
// and because no code path anywhere may treat it as recoverable.
var ErrSignature = errors.New("selfupdate: checksums.txt does not verify against any compiled-in release key")

// ErrDigest is a tarball whose sha256 does not match `checksums.txt`.
var ErrDigest = errors.New("selfupdate: the tarball's sha256 does not match checksums.txt")

// VerifyChecksums checks `checksums.txt` against `checksums.txt.sig` and the
// given keys. It returns ErrSignature on failure, whichever key was tried.
func VerifyChecksums(checksums, signature []byte, keys KeySet) error {
	sig, err := decodeSignature(signature)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	for _, k := range keys {
		if ed25519.Verify(k, checksums, sig) {
			return nil
		}
	}
	return ErrSignature
}

// KeyFingerprints renders the keys a verification would have accepted, so a
// failure says which release line this binary was expecting rather than only
// that it was disappointed.
//
// Public keys are public: printing eight bytes of one leaks nothing and is what
// makes "signed with a key this binary does not carry" distinguishable from
// "corrupt download" in a bug report.
func KeyFingerprints(keys KeySet) string {
	if len(keys) == 0 {
		return "accepts no key"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		sum := sha256.Sum256(k)
		parts = append(parts, hex.EncodeToString(sum[:8]))
	}
	return "accepts key " + strings.Join(parts, " or ")
}

// decodeSignature accepts the 64-byte ed25519 signature raw (what
// `openssl pkeyutl -sign` and Go's ed25519.Sign both produce), base64-encoded
// (what a release job that pipes through `base64` produces — and what this
// project's own does), or hex.
//
// All three shapes have appeared in signing scripts; accepting them costs ten
// lines and removes a class of release-day failure that would only ever be
// discovered by the first host to update. There is no security value in being
// strict about an encoding when the bytes either verify or they do not, and a
// signature that fails to PARSE produces a very different bug report from one
// that fails to VERIFY.
func decodeSignature(b []byte) ([]byte, error) {
	if len(b) == ed25519.SignatureSize {
		return b, nil
	}
	text := strings.TrimSpace(string(b))
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil &&
		len(raw) == ed25519.SignatureSize {
		return raw, nil
	}
	if raw, err := hex.DecodeString(text); err == nil && len(raw) == ed25519.SignatureSize {
		return raw, nil
	}
	return nil, fmt.Errorf(
		"%s is not a %d-byte ed25519 signature (raw, base64 or hex); got %d bytes",
		SignatureName, ed25519.SignatureSize, len(b))
}

// ParseChecksums reads the `sha256sum`-style file the release job produces:
// `<64 hex>  <name>` per line, two spaces by convention and any run of blanks
// accepted. The map is keyed by name, because that is what both callers have —
// the asset name.
//
// Names must be PLAIN: a `/`, a `.` or a `..` is refused rather than reduced to
// its base name. The file is signed, so a path inside it is not an attack this
// check prevents — it is a release-pipeline bug this check declines to act on,
// and refusing is cheaper than reasoning about what a caller's filepath.Join
// would do with it.
func ParseChecksums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("selfupdate: %s line %d is not `<sha256>  <name>`: %q",
				ChecksumsName, i+1, line)
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != 64 {
			return nil, fmt.Errorf("selfupdate: %s line %d: %q is not a sha256",
				ChecksumsName, i+1, fields[0])
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("selfupdate: %s line %d: %q is not hex",
				ChecksumsName, i+1, fields[0])
		}
		// `sha256sum` writes a `*` before a name it read in binary mode.
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		switch {
		case name == "", name == "." || name == "..", strings.ContainsRune(name, '/'):
			return nil, fmt.Errorf("selfupdate: %s line %d: %q is not a plain file name",
				ChecksumsName, i+1, name)
		}
		out[name] = digest
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("selfupdate: %s names no files", ChecksumsName)
	}
	return out, nil
}

// FileSHA256 hashes a file in a streaming read, so a tarball is never held in
// memory in order to be verified.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("selfupdate: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("selfupdate: read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyStaged is the whole verification, over the three files in one directory,
// and it is the SAME function the daemon runs at section 12.1 step 3 and the
// root actor re-runs at section 12.2 step 0.
//
// One function rather than two is deliberate. The privilege boundary is what
// makes the second check necessary, but a second IMPLEMENTATION of it is how the
// two ends of a boundary come to disagree about what "verified" means.
//
// It returns the tarball's SIGNED sha256 — the digest `checksums.txt` names for
// it, not a digest of whatever is on disk right now — and that return value is
// load-bearing rather than a convenience. `<state_dir>/update` is owned by the
// unprivileged service identity, so between this check and the extraction that
// follows it, that identity can rename a different tarball over the verified
// name. Handing the signed digest forward is what lets ExtractBinary bind the
// bytes it actually reads to the bytes this function approved (D89: "a file the
// service identity could rewrite after verification is never the file that lands
// on `<prefix>`"), instead of the two agreeing only on a path.
func VerifyStaged(dir, tarball string, keys KeySet) (string, error) {
	checksums, err := os.ReadFile(path.Join(dir, ChecksumsName))
	if err != nil {
		return "", fmt.Errorf("selfupdate: read %s: %w", ChecksumsName, err)
	}
	sig, err := os.ReadFile(path.Join(dir, SignatureName))
	if err != nil {
		return "", fmt.Errorf("selfupdate: read %s: %w", SignatureName, err)
	}
	if err := VerifyChecksums(checksums, sig, keys); err != nil {
		return "", err
	}

	want, err := ParseChecksums(checksums)
	if err != nil {
		return "", err
	}
	expected, ok := want[tarball]
	if !ok {
		return "", fmt.Errorf("%w: checksums.txt does not name %s", ErrDigest, tarball)
	}
	got, err := FileSHA256(path.Join(dir, tarball))
	if err != nil {
		return "", err
	}
	if got != expected {
		return "", fmt.Errorf("%w: %s is %s, checksums.txt says %s", ErrDigest, tarball, got, expected)
	}
	return expected, nil
}
