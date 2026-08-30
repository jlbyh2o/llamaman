package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"golang.org/x/crypto/argon2"
)

// argon2id password hashing (SPEC §4, DESIGN §2.2: "argon2id encoded string").
//
// The stored value is a PHC string:
//
//	$argon2id$v=19$m=65536,t=2,p=4$<b64 salt>$<b64 hash>
//
// carrying its own parameters, which is the whole reason the column is TEXT and
// not two BLOBs. A later release may raise the cost without a migration: verify
// reads the parameters out of the stored string, and Verify reports whether the
// hash it just accepted was made with weaker ones so the caller can rehash on the
// next successful login.

// Params are the argon2id cost parameters. The defaults follow the OWASP
// recommendation for argon2id (19 MiB, t=2, p=1) with the memory rounded up to
// 64 MiB, which a host that is about to run a multi-gigabyte language model can
// certainly spare and which a laptop-class attacker cannot parallelize away.
//
// They are a struct rather than constants because the tests hash hundreds of
// passwords under the race detector and must be able to ask for a cheap cost;
// production never constructs one and takes DefaultParams.
type Params struct {
	// Memory is the argon2id memory cost, in KiB.
	Memory uint32
	// Time is the number of passes.
	Time uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLen and KeyLen are in bytes.
	SaltLen int
	KeyLen  uint32
}

// DefaultParams is what a real daemon hashes with.
func DefaultParams() Params {
	p := uint8(4)
	if n := runtime.NumCPU(); n < 4 {
		p = uint8(n)
	}
	if p < 1 {
		p = 1
	}
	return Params{
		Memory:      64 * 1024,
		Time:        2,
		Parallelism: p,
		SaltLen:     16,
		KeyLen:      32,
	}
}

// The password rules. They are deliberately minimal: SPEC §4 asks for argon2id
// and a lockout, not for a composition policy, and §11.2 puts a strength METER in
// front of the user rather than a rejection. What is refused here is only what is
// unusable — an empty or trivially short password, and one long enough to be a
// denial of service against the hasher.
const (
	// MinPasswordLen is the shortest password this daemon will store.
	MinPasswordLen = 8
	// MaxPasswordLen bounds the input to the hasher. bcrypt's 72-byte limit
	// does not apply to argon2id, but an unbounded password is an unbounded
	// amount of work for an unauthenticated caller to ask for.
	MaxPasswordLen = 1024
)

// ErrPasswordMismatch is returned by Verify for a wrong password. It is a
// sentinel rather than a bool so a caller cannot accidentally treat an error as
// a successful verification.
var ErrPasswordMismatch = errors.New("auth: password does not match")

// ValidatePassword applies the two rules above and returns the API's
// `password_invalid` error when either fails.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLen:
		return model.Error{
			Code:    model.CodePasswordInvalid,
			Message: fmt.Sprintf("the password must be at least %d characters", MinPasswordLen),
			Details: map[string]any{"min_length": MinPasswordLen},
		}
	case len(password) > MaxPasswordLen:
		return model.Error{
			Code:    model.CodePasswordInvalid,
			Message: fmt.Sprintf("the password must be at most %d bytes", MaxPasswordLen),
			Details: map[string]any{"max_length": MaxPasswordLen},
		}
	}
	return nil
}

// Hash returns the PHC-encoded argon2id hash of password under p.
func (p Params) Hash(password string) (string, error) {
	p = p.withDefaults()
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	return p.encode(salt, p.key(password, salt)), nil
}

// Verify checks password against an encoded hash in constant time.
//
// The second result is "this hash was made with parameters weaker than p" —
// the signal to rehash. It is reported rather than acted on here because
// rehashing is a database write, and this file has no store.
func (p Params) Verify(encoded, password string) (rehash bool, err error) {
	stored, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := stored.key(password, salt)
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, ErrPasswordMismatch
	}
	return stored.weakerThan(p.withDefaults()), nil
}

func (p Params) key(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Parallelism, p.KeyLen)
}

func (p Params) withDefaults() Params {
	d := DefaultParams()
	if p.Memory == 0 {
		p.Memory = d.Memory
	}
	if p.Time == 0 {
		p.Time = d.Time
	}
	if p.Parallelism == 0 {
		p.Parallelism = d.Parallelism
	}
	if p.SaltLen == 0 {
		p.SaltLen = d.SaltLen
	}
	if p.KeyLen == 0 {
		p.KeyLen = d.KeyLen
	}
	return p
}

// weakerThan reports whether p costs less than want on any axis that matters.
func (p Params) weakerThan(want Params) bool {
	return p.Memory < want.Memory || p.Time < want.Time || p.KeyLen < want.KeyLen
}

const phcPrefix = "$argon2id$"

// phcVersion is argon2's version 19 (0x13), the only one golang.org/x/crypto
// implements and therefore the only one this encoder writes or accepts.
const phcVersion = argon2.Version

func (p Params) encode(salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		phcPrefix, phcVersion, p.Memory, p.Time, p.Parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decode parses a PHC string back into its parameters, salt and key.
//
// It is strict about every field. A stored hash is the one credential in this
// design that a human can edit with sqlite3, and a lenient parser that guessed a
// missing parameter would verify a password against a cost nobody chose.
func decode(encoded string) (Params, []byte, []byte, error) {
	fail := func(why string) (Params, []byte, []byte, error) {
		return Params{}, nil, nil, fmt.Errorf("auth: malformed argon2id hash: %s", why)
	}
	if !strings.HasPrefix(encoded, phcPrefix) {
		return fail("not an argon2id PHC string")
	}
	parts := strings.Split(encoded[len(phcPrefix):], "$")
	if len(parts) != 4 {
		return fail("wrong number of fields")
	}

	var version int
	if _, err := fmt.Sscanf(parts[0], "v=%d", &version); err != nil {
		return fail("unreadable version")
	}
	if version != phcVersion {
		return fail("unsupported argon2 version " + strconv.Itoa(version))
	}

	var p Params
	var mem, t, par uint64
	fields := strings.Split(parts[1], ",")
	if len(fields) != 3 {
		return fail("wrong number of parameters")
	}
	for _, f := range fields {
		name, value, ok := strings.Cut(f, "=")
		if !ok {
			return fail("unreadable parameter " + f)
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return fail("unreadable parameter " + name)
		}
		switch name {
		case "m":
			mem = n
		case "t":
			t = n
		case "p":
			par = n
		default:
			return fail("unknown parameter " + name)
		}
	}
	if mem == 0 || t == 0 || par == 0 || par > 255 {
		return fail("a parameter is out of range")
	}
	p.Memory, p.Time, p.Parallelism = uint32(mem), uint32(t), uint8(par)

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return fail("unreadable salt")
	}
	key, err := b64.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return fail("unreadable hash")
	}
	p.SaltLen, p.KeyLen = len(salt), uint32(len(key))
	return p, salt, key, nil
}
