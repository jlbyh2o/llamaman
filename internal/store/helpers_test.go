package store

import (
	"context"
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh database in a temp directory and applies every
// embedded migration to it, which is the state every boot after the first one
// starts from.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	s, err := Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.Migrate(ctx, MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// mustWrite runs fn in one write transaction and fails the test if it errors.
func mustWrite(t *testing.T, s *Store, fn func(context.Context, Tx) error) {
	t.Helper()
	if err := s.Write(context.Background(), fn); err != nil {
		t.Fatalf("write transaction: %v", err)
	}
}

// ptr is the shorthand the nullable row fields need in tests.
func ptr[T any](v T) *T { return &v }
