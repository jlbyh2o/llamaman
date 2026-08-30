package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// testParams are deliberately the cheapest argon2id parameters that are still a
// real argon2id: production takes DefaultParams, and hashing at 64 MiB in every
// table row of every test — under the race detector — would make this package's
// suite take minutes for no additional coverage of the code under test.
func testParams() Params {
	return Params{Memory: 64, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()

	p := testParams()
	encoded, err := p.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("encoded hash = %q, want a PHC argon2id string", encoded)
	}

	cases := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"the password that was hashed", "correct horse battery staple", nil},
		{"a different password", "correct horse battery stapl", ErrPasswordMismatch},
		{"the empty password", "", ErrPasswordMismatch},
		{"a prefix of the password", "correct", ErrPasswordMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rehash, err := p.Verify(encoded, tc.password)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify = %v, want %v", err, tc.wantErr)
			}
			if err == nil && rehash {
				t.Error("a hash made with these exact parameters asked to be rehashed")
			}
		})
	}
}

// TestHashIsSaltedPerCall: two hashes of one password must differ, or the column
// would leak "these two installs share a password".
func TestHashIsSaltedPerCall(t *testing.T) {
	t.Parallel()

	p := testParams()
	a, err := p.Hash("same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := p.Hash("same password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of one password are identical; the salt is not per-call")
	}
}

// TestVerifyReportsRehash pins the upgrade path: a hash stored at a lower cost
// verifies, and says so, so a successful login can rehash at the current cost —
// the only moment the plaintext is available to do it.
func TestVerifyReportsRehash(t *testing.T) {
	t.Parallel()

	weak := Params{Memory: 64, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
	strong := Params{Memory: 128, Time: 2, Parallelism: 1, SaltLen: 8, KeyLen: 16}

	encoded, err := weak.Hash("hunter2hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	rehash, err := strong.Verify(encoded, "hunter2hunter2")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rehash {
		t.Error("a hash stored below the current cost did not ask to be rehashed")
	}

	rehash, err = weak.Verify(encoded, "hunter2hunter2")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rehash {
		t.Error("a hash stored at the current cost asked to be rehashed")
	}
}

// TestVerifyRejectsMalformedHashes: the stored hash is the one credential a human
// can edit with sqlite3, and a lenient parser that guessed a missing parameter
// would verify a password against a cost nobody chose.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	t.Parallel()

	b64 := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	cases := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"bcrypt", "$2y$10$abcdefghijklmnopqrstuv"},
		{"argon2i rather than argon2id", "$argon2i$v=19$m=64,t=1,p=1$" + b64 + "$" + b64},
		{"unsupported version", "$argon2id$v=16$m=64,t=1,p=1$" + b64 + "$" + b64},
		{"missing a parameter", "$argon2id$v=19$m=64,t=1$" + b64 + "$" + b64},
		{"unknown parameter", "$argon2id$v=19$m=64,t=1,q=1$" + b64 + "$" + b64},
		{"zero memory", "$argon2id$v=19$m=0,t=1,p=1$" + b64 + "$" + b64},
		{"unreadable salt", "$argon2id$v=19$m=64,t=1,p=1$!!!!$" + b64},
		{"no hash field", "$argon2id$v=19$m=64,t=1,p=1$" + b64},
	}

	p := testParams()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.Verify(tc.encoded, "anything at all"); err == nil {
				t.Fatal("a malformed hash verified")
			} else if errors.Is(err, ErrPasswordMismatch) {
				t.Fatalf("a malformed hash reported a mismatch rather than a parse error: %v", err)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		password string
		wantOK   bool
	}{
		{"empty", "", false},
		{"one short", strings.Repeat("a", MinPasswordLen-1), false},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLen), true},
		{"a long passphrase", strings.Repeat("correct horse ", 20), true},
		{"one over the maximum", strings.Repeat("a", MaxPasswordLen+1), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePassword(tc.password)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("ValidatePassword = %v, want nil", err)
				}
				return
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != model.CodePasswordInvalid {
				t.Fatalf("ValidatePassword = %v, want a %s error", err, model.CodePasswordInvalid)
			}
		})
	}
}

// TestBase58Encode pins the alphabet and the leading-zero rule. The alphabet is
// the reason base58 is specified at all: the token is read off a terminal and
// retyped, and 0/O/I/l are exactly what a human confuses.
func TestBase58Encode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"one zero byte", []byte{0}, "1"},
		{"three zero bytes", []byte{0, 0, 0}, "111"},
		{"one", []byte{1}, "2"},
		{"fifty-seven", []byte{57}, "z"},
		{"fifty-eight", []byte{58}, "21"},
		{"leading zero then a value", []byte{0, 1}, "12"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := base58Encode(tc.in); got != tc.want {
				t.Fatalf("base58Encode(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	for _, c := range base58Alphabet {
		if strings.ContainsRune("0OIl", c) {
			t.Fatalf("the alphabet contains %q, which is one of the four characters base58 omits", c)
		}
	}
	if len(base58Alphabet) != 58 {
		t.Fatalf("the alphabet has %d characters, want 58", len(base58Alphabet))
	}
}
