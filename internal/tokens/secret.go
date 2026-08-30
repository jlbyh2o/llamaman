package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// The secret format (DESIGN section 9.3).
//
//	`lm_` + base58 of 32 crypto/rand bytes
//
// Three properties of that sentence are load-bearing:
//
//   - THIRTY-TWO RANDOM BYTES is why D37 hashes with sha256 rather than argon2id.
//     A 256-bit uniformly random secret has no dictionary to attack, so a slow
//     hash buys nothing — and it would cost ~100 ms on every inference request,
//     on the hot path of the thing this daemon exists to serve. The admin
//     PASSWORD, which is human-chosen and therefore guessable, keeps argon2id.
//   - BASE58 rather than base64 so the secret survives a double-click, a URL, a
//     shell without quotes and a hand transcription: no `+`, no `/`, no `=`, and
//     no character pair a human confuses (0/O, l/I).
//   - The `lm_` PREFIX so a leaked secret is greppable, both by its owner and by
//     the scanners that watch public repositories for exactly this shape.

// Prefix is the literal every minted secret starts with.
const Prefix = "lm_"

// SecretBytes is how much entropy a secret carries.
const SecretBytes = 32

// prefixChars is how much of the secret `api_tokens.prefix` keeps in the clear:
// section 2.9's "`lm_` plus the first six characters".
const prefixChars = 6

// base58Alphabet is Bitcoin's ordering. The omissions are the point: no 0, O, I
// or l.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// NewSecret mints one secret from r, or from crypto/rand when r is nil.
//
// A short read is an error rather than a shorter secret. There is no such thing
// as a token that is nearly random enough, and the one caller that could be
// tempted to continue — a test with a deterministic reader — is exactly the
// caller that must be told.
func NewSecret(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, SecretBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("tokens: read %d random bytes: %w", SecretBytes, err)
	}
	return Prefix + base58Encode(buf), nil
}

// Hash is D37's at-rest form: the hex sha256 of the WHOLE secret, `lm_` included.
//
// Hashing the whole string rather than the body is deliberate. `token_hash` is
// UNIQUE and is the gateway's index, and hashing exactly the bytes a client
// presents means verification never has to parse, slice or normalize a
// credential before it can look it up — which is the kind of pre-processing that
// grows a padding-oracle-shaped bug.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix is what `api_tokens.prefix` stores in the clear: `lm_` plus the
// first six characters of the body. It is enough to recognize which token a
// journal line is about and nowhere near enough to authenticate with.
//
// A value that is not a plausible secret comes back as much of itself as there
// is, which only a malformed credential can produce and which is never stored.
func DisplayPrefix(secret string) string {
	body := strings.TrimPrefix(secret, Prefix)
	if len(body) > prefixChars {
		body = body[:prefixChars]
	}
	return Prefix + body
}

// base58Encode renders bytes in base58, preserving leading zero bytes as leading
// '1's — the standard convention, and the reason the encoding is not simply a
// big.Int conversion.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}

	n := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	mod := new(big.Int)

	// 32 bytes is at most 44 base58 characters; the extra room costs nothing and
	// removes a reallocation from a path that runs on every mint.
	out := make([]byte, 0, len(b)*137/100+1)
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for range zeros {
		out = append(out, base58Alphabet[0])
	}

	// The digits came out least-significant first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
