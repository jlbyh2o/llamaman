package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The AES-GCM box and its key file (DESIGN sections 1 and 2.2).
//
// # What this protects against, and what it does not (D46)
//
// The `secrets` table holds ciphertext; the key is one file beside the database.
// Anything that can read the database can very likely read the file next to it,
// so this is NOT protection against an attacker with the service identity. What
// it IS protection against is every way a database travels WITHOUT its
// directory, and those are numerous and routine in this design: a `db-backups/`
// snapshot (§12.1), a `VACUUM INTO` copy, a `llamaman diagnostics` bundle, a
// file someone copies off a host to look at a schema question. A Hugging Face
// token in the clear in any of those is a token that has left the machine.
//
// The design states that limit rather than dressing it up, and this file is
// written to keep it exactly that size: the key never leaves the process, the
// plaintext is never written anywhere but the caller's memory, and nothing here
// logs either.

// KeyFileName is the key file's name inside the state directory (§2.2's
// "the key is <state_dir>/secret.key (0600)").
const KeyFileName = "secret.key"

// KeyBytes is the AES-256 key length. AES-256-GCM is the box: an AEAD, so a
// tampered ciphertext fails to open rather than decrypting to garbage.
const KeyBytes = 32

// ErrKeyMode means the key file's permissions are wider than 0600.
//
// It is a refusal rather than a repair. A key file that is group- or
// world-readable has been readable by something else for as long as it has been
// that way, and silently chmod-ing it would hide the fact that it happened
// rather than fix its consequence — which is that the key should be considered
// disclosed. The daemon says so and lets a human decide.
var ErrKeyMode = errors.New("secrets: the key file is readable by more than its owner")

// Key is the sealed-box key. The zero value is unusable; LoadOrCreateKey and
// NewKey are the constructors.
type Key struct {
	aead cipher.AEAD
}

// LoadOrCreateKey reads `<dir>/secret.key`, creating it on first boot.
//
// The create is `O_CREAT|O_EXCL|O_WRONLY` at 0600: exclusive so two daemons
// racing at first boot cannot each write a key and leave one of them holding
// ciphertext it cannot open, and 0600 for the reason §2.2 gives. The file holds
// hex rather than raw bytes so that a human who opens it sees something they can
// recognize as a key, and so a stray trailing newline from an editor is
// survivable.
func LoadOrCreateKey(dir string) (Key, error) {
	path := filepath.Join(dir, KeyFileName)

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		info, serr := os.Stat(path)
		if serr != nil {
			return Key{}, fmt.Errorf("secrets: stat %s: %w", path, serr)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return Key{}, fmt.Errorf("%w: %s is mode %#o, want 0600", ErrKeyMode, path, perm)
		}
		return parseKey(path, raw)
	case !errors.Is(err, fs.ErrNotExist):
		return Key{}, fmt.Errorf("secrets: read %s: %w", path, err)
	}

	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return Key{}, fmt.Errorf("secrets: generate a key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another process created it between the read and the create. Read
			// it rather than failing the boot: whoever won the race wrote a key
			// this one can use.
			return LoadOrCreateKey(dir)
		}
		return Key{}, fmt.Errorf("secrets: create %s: %w", path, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return Key{}, fmt.Errorf("secrets: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return Key{}, fmt.Errorf("secrets: close %s: %w", path, err)
	}
	return NewKey(key)
}

func parseKey(path string, raw []byte) (Key, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return Key{}, fmt.Errorf("secrets: %s does not hold a hex key: %w", path, err)
	}
	k, err := NewKey(key)
	if err != nil {
		return Key{}, fmt.Errorf("secrets: %s: %w", path, err)
	}
	return k, nil
}

// NewKey builds a Key from raw bytes, for a test and for LoadOrCreateKey.
func NewKey(key []byte) (Key, error) {
	if len(key) != KeyBytes {
		return Key{}, fmt.Errorf("secrets: a key is %d bytes, got %d", KeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Key{}, fmt.Errorf("secrets: build the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("secrets: build the AEAD: %w", err)
	}
	return Key{aead: aead}, nil
}

// Usable reports whether this Key was constructed.
func (k Key) Usable() bool { return k.aead != nil }

// Seal encrypts plaintext and returns the nonce and ciphertext the two `secrets`
// columns hold.
//
// The nonce is fresh random bytes per seal, which is what GCM requires: reusing
// one across two ciphertexts under the same key is the failure that breaks GCM
// outright. The secret's NAME is passed as additional authenticated data, so a
// row whose `name` was edited to swap the Hugging Face token for the GitHub one
// fails to open rather than opening as the wrong credential.
func (k Key) Seal(name string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if !k.Usable() {
		return nil, nil, errors.New("secrets: this key was never constructed")
	}
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secrets: generate a nonce: %w", err)
	}
	return nonce, k.aead.Seal(nil, nonce, plaintext, []byte(name)), nil
}

// Open decrypts what Seal produced. A failure is reported without the ciphertext
// or any part of it: an error message is the one part of a response that reliably
// ends up in a log.
func (k Key) Open(name string, nonce, ciphertext []byte) ([]byte, error) {
	if !k.Usable() {
		return nil, errors.New("secrets: this key was never constructed")
	}
	if len(nonce) != k.aead.NonceSize() {
		return nil, fmt.Errorf("secrets: the stored nonce for %s is %d bytes, want %d",
			name, len(nonce), k.aead.NonceSize())
	}
	out, err := k.aead.Open(nil, nonce, ciphertext, []byte(name))
	if err != nil {
		return nil, fmt.Errorf("secrets: %s could not be opened with this host's key "+
			"(the key file was replaced, or the row came from another host)", name)
	}
	return out, nil
}
