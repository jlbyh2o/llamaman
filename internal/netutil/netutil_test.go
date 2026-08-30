package netutil

import (
	"errors"
	"net"
	"testing"
)

// The walk is tested against REAL sockets. A fake would test the loop and not
// the property that matters — whether a port can actually be bound on this host
// — which is the whole reason DESIGN section 2.8 calls the database-side check
// advisory and F6 exists as a runtime fallback.

const testBind = "127.0.0.1"

// occupy binds a port and closes it when the test ends. It returns the port,
// which is kernel-chosen so parallel tests never collide.
func occupy(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, port, err := Ephemeral(testBind)
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, port
}

func TestFree(t *testing.T) {
	t.Parallel()

	ln, port := occupy(t)
	if Free(testBind, port) {
		t.Fatalf("port %d reported free while this test holds it", port)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !Free(testBind, port) {
		t.Fatalf("port %d reported busy after the listener was closed", port)
	}
}

// TestWalkCollisionBehavior is section 11.1 step 7's walk: the desired port
// first, the next candidates when it is taken, past every excluded one, and
// ErrExhausted when the window runs out — never a refusal to start, which is the
// caller's branch into an ephemeral bind.
func TestWalkCollisionBehavior(t *testing.T) {
	t.Parallel()

	t.Run("the desired port when it is free", func(t *testing.T) {
		t.Parallel()
		ln, want := occupy(t)
		ln.Close()

		got, port, err := Walk(WalkOptions{Bind: testBind, Desired: want, Window: 5})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		defer got.Close()
		if port != want {
			t.Fatalf("the walk landed on %d, want the desired %d", port, want)
		}
	})

	t.Run("moves past an occupied desired port", func(t *testing.T) {
		t.Parallel()
		_, taken := occupy(t)

		ln, port, err := Walk(WalkOptions{Bind: testBind, Desired: taken, Window: 20})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		defer ln.Close()
		if port == taken {
			t.Fatal("the walk bound a port another listener holds")
		}
		if port <= taken || port > taken+20 {
			t.Fatalf("the walk landed on %d, want a port in (%d, %d]", port, taken, taken+20)
		}
	})

	t.Run("skips excluded ports even when they are free", func(t *testing.T) {
		t.Parallel()
		ln, desired := occupy(t)
		ln.Close()

		// The exclusion set is section 11.1 step 7's, and it is not cosmetic: a
		// bare "next free port" walk could take a port an instance owns and only
		// discover the theft when that instance's listener failed to bind (F6).
		excluded := NewPortSet()
		excluded.AddRange(desired, desired+3)

		got, port, err := Walk(WalkOptions{
			Bind: testBind, Desired: desired, Window: 20, Excluded: excluded.Contains,
		})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		defer got.Close()
		if excluded.Contains(port) {
			t.Fatalf("the walk landed on excluded port %d", port)
		}
		if port != desired+4 {
			t.Fatalf("the walk landed on %d, want the first candidate past the exclusion (%d)", port, desired+4)
		}
	})

	t.Run("exhausted when every candidate is excluded", func(t *testing.T) {
		t.Parallel()
		ln, desired := occupy(t)
		ln.Close()

		excluded := NewPortSet()
		excluded.AddRange(desired, desired+5)

		if _, _, err := Walk(WalkOptions{
			Bind: testBind, Desired: desired, Window: 5, Excluded: excluded.Contains,
		}); !errors.Is(err, ErrExhausted) {
			t.Fatalf("Walk = %v, want ErrExhausted", err)
		}
	})

	t.Run("exhausted when every candidate is occupied", func(t *testing.T) {
		t.Parallel()
		_, desired := occupy(t)

		// A window of zero means "the desired port only", which is the smallest
		// walk there is; occupying that one port exhausts it.
		if _, _, err := Walk(WalkOptions{
			Bind: testBind, Desired: desired, Window: 0,
			Excluded: func(p int) bool { return p != desired },
		}); !errors.Is(err, ErrExhausted) {
			t.Fatalf("Walk = %v, want ErrExhausted", err)
		}
	})

	t.Run("a port out of range is an error, not a panic", func(t *testing.T) {
		t.Parallel()
		for _, desired := range []int{0, -1, 70000} {
			if _, _, err := Walk(WalkOptions{Bind: testBind, Desired: desired}); err == nil {
				t.Fatalf("Walk with desired=%d returned no error", desired)
			}
		}
	})
}

// TestEphemeral is the fallback of section 11.1 step 7: bind SOMETHING rather
// than refuse to start, and report which port it landed on so the caller can
// advertise the truth.
func TestEphemeral(t *testing.T) {
	t.Parallel()

	ln, port, err := Ephemeral(testBind)
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}
	defer ln.Close()

	if port <= 0 || port > 65535 {
		t.Fatalf("Ephemeral returned port %d", port)
	}
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Fatalf("Ephemeral reported port %d but bound %d", port, got)
	}
}

func TestPortSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		build func() PortSet
		in    []int
		out   []int
	}{
		{
			name:  "explicit members",
			build: func() PortSet { return NewPortSet(8080, 8081) },
			in:    []int{8080, 8081},
			out:   []int{8079, 8082},
		},
		{
			name: "an added range, as the internal instance pool is excluded",
			build: func() PortSet {
				s := NewPortSet(5526)
				s.AddRange(21000, 21003)
				return s
			},
			in:  []int{5526, 21000, 21001, 21002, 21003},
			out: []int{5525, 20999, 21004},
		},
		{
			name:  "the empty set",
			build: func() PortSet { return NewPortSet() },
			out:   []int{1, 5526, 65535},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.build()
			for _, p := range tc.in {
				if !s.Contains(p) {
					t.Errorf("port %d is not in the set", p)
				}
			}
			for _, p := range tc.out {
				if s.Contains(p) {
					t.Errorf("port %d is in the set", p)
				}
			}
		})
	}
}
