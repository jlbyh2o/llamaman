package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

const hfToken = "hf_fake-token-for-tests-only" // dashes keep this unmatchable by real-token scanners

func newService(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	svc, err := New(Config{Store: st, Key: key, Now: func() time.Time {
		return time.Unix(1_800_000_000, 0).UTC()
	}})
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return svc, st, dir
}

func TestPutGetDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _, _ := newService(t)

	if _, err := svc.Get(ctx, model.SecretHFToken); !errors.Is(err, ErrNotStored) {
		t.Fatalf("Get before Put = %v, want ErrNotStored", err)
	}
	info, err := svc.Info(ctx, model.SecretHFToken)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Present {
		t.Error("Info reports a token on a host that has none")
	}

	if err := svc.Put(ctx, model.SecretHFToken, hfToken, Verdict{
		Valid: true, User: "someone", Scopes: []string{"read-repos"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := svc.Get(ctx, model.SecretHFToken)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != hfToken {
		t.Errorf("Get = %q, want the token that was stored", got)
	}

	info, err = svc.Info(ctx, model.SecretHFToken)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.Present || info.Valid == nil || !*info.Valid {
		t.Errorf("Info = %+v, want a present, valid token", info)
	}
	if info.User != "someone" || len(info.Scopes) != 1 {
		t.Errorf("Info scope record = %q %v", info.User, info.Scopes)
	}
	if info.LastUsedAt == nil {
		t.Error("last_used_at was not stamped by Get")
	}

	if err := svc.Delete(ctx, model.SecretHFToken); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, model.SecretHFToken); !errors.Is(err, ErrNotStored) {
		t.Fatalf("Get after Delete = %v, want ErrNotStored", err)
	}
	// Deleting what is not there is what the caller asked for.
	if err := svc.Delete(ctx, model.SecretHFToken); err != nil {
		t.Errorf("Delete of an absent secret = %v, want nil", err)
	}
}

// TestTheDatabaseNeverHoldsThePlaintext is D46's whole claim, asserted against
// the file rather than against the API: a `db-backups/` entry, a `VACUUM INTO`
// snapshot and a diagnostics bundle all carry these bytes, and the token must
// not be among them.
func TestTheDatabaseNeverHoldsThePlaintext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, st, dir := newService(t)
	if err := svc.Put(ctx, model.SecretHFToken, hfToken, Verdict{Valid: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("read the database file: %v", err)
	}
	if strings.Contains(string(raw), hfToken) {
		t.Fatal("THE TOKEN IS IN THE DATABASE FILE IN THE CLEAR")
	}
	// The hint is there, and it is not the token.
	if !strings.Contains(string(raw), Hint(hfToken)) {
		t.Error("the display hint was not stored")
	}
}

func TestHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "a Hugging Face token", in: "hf_ABCDEFGHIJKLMNOPqrs", want: "hf_A…qrs"},
		{name: "a GitHub token", in: "ghp_ABCDEFGHIJKLMNOPqrs", want: "ghp_…qrs"},
		{name: "too short to mask usefully", in: "hf_abc", want: "…"},
		{name: "empty", in: "", want: "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Hint(tc.in)
			if got != tc.want {
				t.Errorf("Hint(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(tc.in) > 8 && strings.Contains(got, tc.in) {
				t.Errorf("Hint(%q) returned the whole value", tc.in)
			}
		})
	}
}

func TestKeyFile(t *testing.T) {
	t.Parallel()

	t.Run("it is created 0600 and reused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		k1, err := LoadOrCreateKey(dir)
		if err != nil {
			t.Fatalf("LoadOrCreateKey: %v", err)
		}
		info, err := os.Stat(filepath.Join(dir, KeyFileName))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("the key file is mode %#o, want 0600", perm)
		}

		k2, err := LoadOrCreateKey(dir)
		if err != nil {
			t.Fatalf("second LoadOrCreateKey: %v", err)
		}
		nonce, box, err := k1.Seal("hf_token", []byte("value"))
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		out, err := k2.Open("hf_token", nonce, box)
		if err != nil {
			t.Fatalf("the second load produced a different key: %v", err)
		}
		if string(out) != "value" {
			t.Errorf("Open = %q", out)
		}
	})

	t.Run("a widened key file is refused rather than repaired", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, err := LoadOrCreateKey(dir); err != nil {
			t.Fatalf("LoadOrCreateKey: %v", err)
		}
		if err := os.Chmod(filepath.Join(dir, KeyFileName), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKey(dir); !errors.Is(err, ErrKeyMode) {
			t.Fatalf("err = %v, want ErrKeyMode", err)
		}
	})
}

// TestSealIsBoundToTheSecretName: the name is the AEAD's additional data, so a
// row whose `name` was edited to swap one credential for the other fails to open
// rather than opening as the wrong token.
func TestSealIsBoundToTheSecretName(t *testing.T) {
	t.Parallel()

	k, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	nonce, box, err := k.Seal(string(model.SecretHFToken), []byte(hfToken))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := k.Open(string(model.SecretGitHubToken), nonce, box); err == nil {
		t.Fatal("a Hugging Face token opened as a GitHub token")
	}
	if _, err := k.Open(string(model.SecretHFToken), nonce, box); err != nil {
		t.Fatalf("the correctly named open failed: %v", err)
	}
}

// TestOpenNeverEchoesTheCiphertext: an error message is the one part of a
// response that reliably ends up in a journal.
func TestOpenNeverEchoesTheCiphertext(t *testing.T) {
	t.Parallel()

	k, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	other, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	nonce, box, err := k.Seal("hf_token", []byte(hfToken))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, err = other.Open("hf_token", nonce, box)
	if err == nil {
		t.Fatal("a box opened under the wrong key")
	}
	if strings.Contains(err.Error(), string(box[:4])) {
		t.Errorf("the error echoed the ciphertext: %v", err)
	}
}

func TestTokenFuncTreatsAnAbsentTokenAsAnonymous(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _, _ := newService(t)

	fn := svc.TokenFunc(model.SecretGitHubToken)
	got, err := fn(ctx)
	if err != nil {
		t.Fatalf("TokenFunc on a host with no token = %v, want no error", err)
	}
	if got != "" {
		t.Errorf("TokenFunc = %q, want the empty string", got)
	}
}
