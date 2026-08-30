package toolchain

import "testing"

func TestParseLibc(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		kind    LibcKind
		version string
		known   bool
	}{
		{name: "getconf", fixture: "getconf-glibc-2.43.txt", kind: LibcGlibc, version: "2.43", known: true},
		{name: "ldd on glibc", fixture: "ldd-glibc-2.43.txt", kind: LibcGlibc, version: "2.43", known: true},
		{name: "ldd on musl", fixture: "ldd-musl-1.2.5.txt", kind: LibcMusl, version: "1.2.5", known: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := ParseLibc(fixture(t, tc.fixture))
			if l.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", l.Kind, tc.kind)
			}
			if l.VersionString != tc.version {
				t.Errorf("version = %q, want %q", l.VersionString, tc.version)
			}
			if l.Known() != tc.known {
				t.Errorf("Known() = %v, want %v", l.Known(), tc.known)
			}
		})
	}
}

func TestParseLibcUnrecognized(t *testing.T) {
	// A host whose libc we cannot name must produce an unknown, not a zero
	// version that a glibc comparison would then read as "2.0" and reject every
	// prebuilt against.
	for _, in := range []string{"", "some other libc\n", "ldd (unknown)\n"} {
		l := ParseLibc(in)
		if l.Kind != LibcUnknown || l.Known() {
			t.Errorf("ParseLibc(%q) = %+v, want unknown", in, l)
		}
	}
}
