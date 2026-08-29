package store

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestDSNCarriesPragmas pins the section-2 pragma set onto every connection
// string, in order, and checks that a pool's extra pragmas are appended rather
// than replacing them.
func TestDSNCarriesPragmas(t *testing.T) {
	got := dsn("/var/lib/llamaman/llamaman.db", nil)
	want := "file:/var/lib/llamaman/llamaman.db" +
		"?_pragma=journal_mode%28WAL%29" +
		"&_pragma=foreign_keys%28ON%29" +
		"&_pragma=busy_timeout%285000%29" +
		"&_pragma=synchronous%28NORMAL%29" +
		"&_pragma=temp_store%28MEMORY%29" +
		"&_pragma=auto_vacuum%28INCREMENTAL%29"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dsn mismatch (-want +got):\n%s", diff)
	}

	ro := dsn("/db", []string{"query_only(1)"})
	if want := "&_pragma=query_only%281%29"; ro[len(ro)-len(want):] != want {
		t.Errorf("read pool DSN does not end with query_only: %s", ro)
	}
}
