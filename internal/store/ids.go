package store

import (
	"crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// IDs (DESIGN section 2).
//
// "IDs are TEXT ULIDs (sortable by creation, readable in URLs, and they double
// as SSE cursors)." All three properties are load-bearing: `events.id` IS the
// SSE Last-Event-ID cursor, so a client reconnecting with an id must be able to
// ask for everything after it with a plain `WHERE id > ?` — which only works
// because ULIDs sort lexicographically in creation order. `runtime_info.boot_id`
// is a ULID per daemon start and is the job lease owner, which is what makes
// "this lease belongs to a boot that is gone" a string comparison.
//
// Ids are minted here rather than in each caller so the monotonic entropy source
// below is shared: two ULIDs minted in the same millisecond from independent
// readers can sort in either order, and that would put two events out of order
// on the SSE stream.

var idMint = struct {
	sync.Mutex
	entropy io.Reader
}{entropy: ulid.Monotonic(rand.Reader, 0)}

// NewID mints a ULID for the given instant. Within one millisecond successive
// ids are strictly increasing, so ids minted in a loop keep their order as SSE
// cursors.
//
// It panics only if crypto/rand fails, which on Linux means the kernel's entropy
// source is gone — a condition under which nothing else in this daemon (session
// secrets, the setup token, API tokens) could be minted safely either.
func NewID(at time.Time) string {
	idMint.Lock()
	defer idMint.Unlock()
	return ulid.MustNew(ulid.Timestamp(at), idMint.entropy).String()
}

// ParseIDTime recovers the millisecond timestamp a ULID was minted at. It is how
// an SSE cursor or a job id can be range-checked against a retention window
// without a second column.
func ParseIDTime(id string) (time.Time, error) {
	u, err := ulid.ParseStrict(id)
	if err != nil {
		return time.Time{}, err
	}
	return ulid.Time(u.Time()), nil
}
