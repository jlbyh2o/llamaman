package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenSmoke proves the two-pool arrangement of DESIGN section 2 actually
// holds against a real file: the write pool writes, the read pool reads the
// same data, and the read pool refuses a write.
func TestOpenSmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llamaman.db")

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.RW.Exec("CREATE TABLE t (a TEXT)"); err != nil {
		t.Fatalf("write pool exec: %v", err)
	}

	var n int
	if err := s.RO.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("read pool query: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}

	if _, err := s.RO.Exec("INSERT INTO t VALUES ('x')"); err == nil {
		t.Error("read pool accepted a write; query_only is not in effect")
	}
}
