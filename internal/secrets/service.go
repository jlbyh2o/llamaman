// Package secrets seals and opens the values that must not sit in the database
// in plaintext — the Hugging Face token and the optional GitHub token (DESIGN
// sections 1, 2.2 and 3.6) — using an AES-GCM box keyed by a 0600 key file
// beside the database.
//
// # The shape of the API, and why it is two methods rather than one
//
// Every caller in this design wants exactly one of two things, and they are not
// the same thing:
//
//   - Get returns the PLAINTEXT, and its only callers are the two clients that
//     put it in an `Authorization` header (internal/hf and
//     internal/llamacpp/github). Both take it through a `func(ctx) (string,
//     error)` rather than a string captured at construction, so a token the user
//     revoked stops being used at the next request rather than at the next
//     restart.
//   - Info returns presence, hint, validity and scopes and NEVER the value. It
//     is what `GET /api/v1/hf/token` and `GET /api/v1/github/token` answer with,
//     and what the settings screen renders.
//
// Nothing in this package logs a token, and no error message here contains one:
// §2.2's rule and CLAUDE.md's are the same rule, and an error string is the one
// part of a response that reliably ends up in a journal.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Store is the persistence this service needs. *store.Store satisfies it
// (DESIGN section 1: the consumer owns the interface).
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	Secret(ctx context.Context, tx store.Tx, name model.SecretName) (store.Secret, error)
	UpsertSecret(ctx context.Context, tx store.Tx, sec store.Secret) error
	DeleteSecret(ctx context.Context, tx store.Tx, name model.SecretName) (bool, error)
	TouchSecret(ctx context.Context, tx store.Tx, name model.SecretName, at int64) (bool, error)
	SetSecretValidity(ctx context.Context, tx store.Tx, name model.SecretName,
		valid bool, scopeJSON *string, at int64) (bool, error)
}

// ErrNotStored means no secret of that name is stored. It is the ordinary state
// of a host that has never been given a token, so callers test for it rather
// than treating it as a failure.
var ErrNotStored = errors.New("secrets: no such secret is stored")

// Info is everything about a stored secret that may be shown: presence, the
// masked hint, what the last validation said, and when it was last used. The
// value itself has no field here, deliberately — a struct with a `Token` field
// is a struct someone eventually marshals.
type Info struct {
	Name    model.SecretName
	Present bool
	// Hint is the masked form, `hf_…AbC`. Empty when nothing is stored.
	Hint string
	// Valid is what the last validation call answered, or nil when this token
	// has never been validated. Three states, because "never asked" and
	// "refused" are different sentences on screen.
	Valid  *bool
	Scopes []string
	// User is carried in the scope record so `GET /hf/token` can say which
	// account a token belongs to without a network call.
	User string

	CreatedAt  int64
	UpdatedAt  int64
	LastUsedAt *int64
}

// Config wires a Service.
type Config struct {
	Store Store
	// Key is the AES-GCM box. A Service with an unusable key answers every Get
	// with an error and refuses every Put; it does not silently store
	// plaintext.
	Key Key
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Service is the sealed-secret store.
type Service struct {
	store Store
	key   Key
	now   func() time.Time
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("secrets: a service needs a store")
	}
	if !cfg.Key.Usable() {
		return nil, errors.New("secrets: a service needs a key")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: cfg.Store, key: cfg.Key, now: now}, nil
}

// Get returns the plaintext of one secret, or ErrNotStored.
//
// It stamps `last_used_at` on the way out, which is what lets the settings
// screen tell a token that is wired up from one that is merely stored. The stamp
// is best effort: a token must not fail to be used because a bookkeeping UPDATE
// lost a race with another writer.
func (s *Service) Get(ctx context.Context, name model.SecretName) (string, error) {
	var sec store.Secret
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		sec, err = s.store.Secret(ctx, tx, name)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotStored
	}
	if err != nil {
		return "", err
	}

	plain, err := s.key.Open(string(name), sec.Nonce, sec.Ciphertext)
	if err != nil {
		return "", err
	}

	at := s.now().UnixMilli()
	_ = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.TouchSecret(ctx, tx, name, at)
		return err
	})
	return string(plain), nil
}

// TokenFunc adapts Get to the `func(ctx) (string, error)` both HTTP clients take.
// A secret that is not stored becomes the empty string rather than an error,
// because "no token" is a supported mode for both of them: the Hub serves public
// repositories anonymously and api.github.com allows 60 requests an hour.
func (s *Service) TokenFunc(name model.SecretName) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		v, err := s.Get(ctx, name)
		if errors.Is(err, ErrNotStored) {
			return "", nil
		}
		return v, err
	}
}

// Info reports on one secret without opening it. A secret that is not stored is
// `{Present: false}` and not an error: "is there a token" is a question with two
// good answers.
func (s *Service) Info(ctx context.Context, name model.SecretName) (Info, error) {
	var sec store.Secret
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		sec, err = s.store.Secret(ctx, tx, name)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return Info{Name: name}, nil
	}
	if err != nil {
		return Info{}, err
	}
	return infoOf(sec), nil
}

// Put seals a value and stores it, with the hint and validation record the
// settings screen shows.
//
// The caller validates FIRST and passes what the upstream API said: §3.6 stores
// a Hugging Face token only after `/api/whoami-v2` accepts it and a GitHub token
// only after `GET /user` returns 200, so this method records a verdict rather
// than reaching the network itself. That keeps the network policy — which
// endpoint, which error code, which 422 — in the package that owns the client.
func (s *Service) Put(ctx context.Context, name model.SecretName, value string, v Verdict) error {
	if !name.Valid() {
		return fmt.Errorf("secrets: %q is not a secret this design stores", name)
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("secrets: an empty value is a deletion, not a secret")
	}

	nonce, ciphertext, err := s.key.Seal(string(name), []byte(value))
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	hint := Hint(value)
	valid := v.Valid
	scopes := v.scopeJSON()

	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		created := now
		if existing, err := s.store.Secret(ctx, tx, name); err == nil {
			// "This host has had a token since March" stays true across a
			// rotation; `updated_at` is what moves.
			created = existing.CreatedAt
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return s.store.UpsertSecret(ctx, tx, store.Secret{
			Name: name, Nonce: nonce, Ciphertext: ciphertext,
			Hint: &hint, Valid: &valid, ScopeJSON: scopes,
			CreatedAt: created, UpdatedAt: now,
		})
	})
}

// Delete removes one secret. Deleting one that is not there is not an error:
// the caller asked for it to be gone and it is.
func (s *Service) Delete(ctx context.Context, name model.SecretName) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.DeleteSecret(ctx, tx, name)
		return err
	})
}

// RecordValidity updates the stored verdict without touching the sealed bytes,
// for a revalidation of a token already on file. A token the upstream API has
// started refusing is still the token the user gave us, and replacing it is
// their decision, not ours.
func (s *Service) RecordValidity(ctx context.Context, name model.SecretName, v Verdict) error {
	scopes := v.scopeJSON()
	at := s.now().UnixMilli()
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.SetSecretValidity(ctx, tx, name, v.Valid, scopes, at)
		return err
	})
}

// Verdict is what a validation call learned about a token. It is the caller's to
// produce: internal/hf validates against `/api/whoami-v2` and
// internal/llamacpp/github against `GET /user`, and each knows what its own
// answer means.
type Verdict struct {
	Valid  bool
	User   string
	Scopes []string
}

// Hint is the masked form §2.2 stores for display: the first four characters, an
// ellipsis, and the last three. It is deliberately lossy — enough for a person
// to recognize which token they pasted, and not enough for anyone to use it.
//
// A value too short to mask usefully becomes a fixed placeholder rather than
// most of itself, because a "hint" that is 80% of a short token is not a hint.
func Hint(value string) string {
	v := strings.TrimSpace(value)
	const head, tail = 4, 3
	if len(v) < head+tail+4 {
		return "…"
	}
	return v[:head] + "…" + v[len(v)-tail:]
}
