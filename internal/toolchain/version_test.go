package toolchain

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "cmake banner", in: "cmake version 4.3.0", want: "4.3.0", ok: true},
		{name: "gcc banner with a build date", in: "gcc (GCC) 16.2.1 20260819 (Red Hat 16.2.1-2)", want: "16.2.1", ok: true},
		{name: "gcc ubuntu banner", in: "gcc (Ubuntu 11.4.0-1ubuntu1~22.04) 11.4.0", want: "11.4.0", ok: true},
		{name: "bare version", in: "1.13.2", want: "1.13.2", ok: true},
		{name: "git banner", in: "git version 2.55.0", want: "2.55.0", ok: true},
		{name: "make banner", in: "GNU Make 4.4.1", want: "4.4.1", ok: true},
		{name: "two part", in: "glibc 2.43", want: "2.43", ok: true},
		{name: "single digit", in: "release 12", want: "12", ok: true},
		{name: "digits inside an identifier are skipped", in: "Built for x86_64-redhat-linux-gnu 4.4.1", want: "4.4.1", ok: true},
		{name: "no version at all", in: "musl libc", want: "", ok: false},
		{name: "empty", in: "", want: "", ok: false},
		{name: "trailing dot is not a component", in: "version 3. something", want: "3", ok: true},
		{name: "a v prefix is a version prefix", in: "v0.3.0", want: "0.3.0", ok: true},
		{name: "nvcc V field", in: "V12.6.85", want: "12.6.85", ok: true},
		{name: "arch suffix is not a version", in: "Built for x86_64-redhat-linux-gnu", want: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseVersion(tc.in)
			if ok != tc.ok {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if got.String() != tc.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tc.in, got.String(), tc.want)
			}
			if ok && got.Raw == "" {
				t.Error("Raw is empty; the UI has nothing to show when a comparison looks wrong")
			}
		})
	}
}

func TestVersionCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{name: "equal", a: "3.14.0", b: "3.14.0", want: 0},
		{name: "missing components are zero", a: "3.14", b: "3.14.0", want: 0},
		{name: "patch below", a: "3.14", b: "3.14.1", want: -1},
		{name: "minor above", a: "3.31.5", b: "3.14", want: 1},
		{name: "numeric not lexical", a: "3.9", b: "3.10", want: -1},
		{name: "major dominates", a: "4.0", b: "3.99.99", want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := MustParseVersion(tc.a), MustParseVersion(tc.b)
			if got := a.Compare(b); got != tc.want {
				t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			if got := a.AtLeast(b); got != (tc.want >= 0) {
				t.Errorf("%s.AtLeast(%s) = %v, want %v", tc.a, tc.b, got, tc.want >= 0)
			}
		})
	}
}

func TestVersionKnownAndString(t *testing.T) {
	var zero Version
	if zero.Known() {
		t.Error("the zero Version claims to be known")
	}
	if zero.String() != "" {
		t.Errorf("zero Version renders as %q, want empty", zero.String())
	}
}
