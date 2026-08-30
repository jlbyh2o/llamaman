package store

import (
	"crypto/rand"
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
// Ids are minted here rather than in each caller so the entropy counter below is
// shared: two ULIDs minted in the same millisecond from independent readers can
// sort in either order, and that would put two events out of order on the SSE
// stream.
//
// # Why the entropy is a counter and not ulid.Monotonic
//
// `ulid.Monotonic` only increments the previous entropy when the id being minted
// carries the SAME timestamp as the previous one; a request for any other
// millisecond draws fresh random entropy. That is enough for a loop, and not
// enough here, because this mint is shared by every subsystem — jobs, boot ids,
// auth, setup, instance exec, and every event `AppendEvent` writes — so mints at
// differing milliseconds interleave constantly. Under `ulid.Monotonic`, minting
// at t, then at t+5ms, then at t again drew fresh entropy for the third id,
// which sorted BELOW the first one about half the time.
//
// The failure that follows is silent: two events appended inside one write
// transaction share a `now`, a job enqueued from another goroutine lands between
// them, the second event gets the lower id, and a client that reconnects to
// `EventsAfter` with the first event's id (`WHERE id > ? ORDER BY id`) never
// receives the second — a dropped state transition, on the stream the whole UI
// is driven from.
//
// So the entropy is a single 80-bit counter that advances on EVERY mint,
// independent of the timestamp. Two ids with the same timestamp then sort in
// mint order because their entropy does, and ids with different timestamps sort
// by timestamp as ULIDs always have. The counter is seeded from crypto/rand with
// its top bit cleared, which leaves 2^79 mints of headroom before it could wrap —
// unreachable by a daemon that would have to mint for longer than the age of the
// universe to get there.
//
// The counter makes ids within one process predictable by one increment. That is
// deliberate and safe: a ULID here is an identifier, never a credential. Session
// cookies, API tokens and the setup token carry their own crypto/rand secret and
// are stored hashed (§2.2); the ULID beside them is the public half.
var idMint struct {
	sync.Mutex
	entropy [10]byte
	seeded  bool
}

// NewID mints a ULID for the given instant. Successive ids are strictly
// increasing whenever their timestamps are equal or advancing, so ids keep their
// mint order as SSE cursors however the callers interleave.
//
// It panics only if crypto/rand fails, which on Linux means the kernel's entropy
// source is gone — a condition under which nothing else in this daemon (session
// secrets, the setup token, API tokens) could be minted safely either — or if
// `at` is beyond the year 10889, which a ULID cannot represent.
func NewID(at time.Time) string {
	idMint.Lock()
	defer idMint.Unlock()

	if !idMint.seeded {
		seedEntropyLocked()
	} else {
		bumpEntropyLocked()
	}

	var id ulid.ULID
	if err := id.SetTime(ulid.Timestamp(at)); err != nil {
		panic("store: cannot mint a ULID for " + at.String() + ": " + err.Error())
	}
	copy(id[6:], idMint.entropy[:])
	return id.String()
}

// seedEntropyLocked draws a fresh counter. The top bit is cleared so the counter
// cannot wrap: an id that wrapped would sort below its predecessor, which is the
// one thing this whole file exists to prevent.
func seedEntropyLocked() {
	if _, err := rand.Read(idMint.entropy[:]); err != nil {
		panic("store: the system entropy source is unavailable: " + err.Error())
	}
	idMint.entropy[0] &= 0x7f
	idMint.seeded = true
}

// bumpEntropyLocked advances the counter by one, least significant byte first.
func bumpEntropyLocked() {
	for i := len(idMint.entropy) - 1; i >= 0; i-- {
		idMint.entropy[i]++
		if idMint.entropy[i] != 0 {
			return
		}
	}
	// Unreachable: seedEntropyLocked clears the top bit, so reaching a carry out
	// of byte 0 takes 2^79 mints. Reseeding is still the only sane answer, and it
	// costs at most one out-of-order pair rather than an all-zero id.
	seedEntropyLocked()
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
