package toolchain

import (
	"strconv"
	"strings"
)

// Version is a dotted numeric version as a tool printed it, kept alongside the
// raw string it was parsed from.
//
// Every tool this package probes prints its version differently and none of
// them promise a shape, so the parser is deliberately permissive — it finds the
// first dotted-numeric run in the output and stops at the first component that
// is not a number. `16.2.1 20260819 (Red Hat 16.2.1-2)` is 16.2.1, `cmake
// version 4.3.0` is 4.3.0, and `1.13.2` is 1.13.2. Raw keeps the whole first
// line so the UI can show what the tool actually said when a comparison looks
// surprising.
type Version struct {
	Parts []int
	Raw   string
}

// ParseVersion finds the first dotted-numeric version in s. It reports false
// when there is none, which is a real answer — a tool that ran but printed
// something we do not understand is `found` with an empty `version`, never a
// tool that failed.
func ParseVersion(s string) (Version, bool) {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			continue
		}
		// A digit in the middle of a longer token — the `6` of `x86_64`, the
		// `4` of `2.43` already consumed — is not the start of a version.
		if i > 0 && isVersionBodyByte(s[i-1]) && !isVeePrefix(s, i) {
			continue
		}
		v, end, ok := scanVersion(s[i:])
		if ok {
			v.Raw = strings.TrimSpace(s)
			return v, true
		}
		i += end
	}
	return Version{}, false
}

// scanVersion reads one dotted-numeric run from the start of s.
func scanVersion(s string) (Version, int, bool) {
	var parts []int
	i := 0
	for {
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start {
			break
		}
		n, err := strconv.Atoi(s[start:i])
		if err != nil { // absurdly long run of digits
			return Version{}, i, false
		}
		parts = append(parts, n)
		if i < len(s) && s[i] == '.' && i+1 < len(s) && isDigit(s[i+1]) {
			i++
			continue
		}
		break
	}
	if len(parts) == 0 {
		return Version{}, i, false
	}
	return Version{Parts: parts}, i, true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isVersionBodyByte reports whether b may not precede the first digit of a
// version. A letter, a digit, an underscore or a dot all mean the digit belongs
// to a token that started earlier — `x86_64`, or the `43` of a `2.43` whose `2`
// was already scanned.
func isVersionBodyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	case isDigit(b), b == '_', b == '.':
		return true
	}
	return false
}

// isVeePrefix is the one exception to the rule above: a `v` immediately before
// the digit, itself at a token boundary, is the conventional version prefix and
// not an identifier — `v0.3.0` is a version and `V12.6.85` is the one nvcc
// prints.
func isVeePrefix(s string, i int) bool {
	if i == 0 || (s[i-1] != 'v' && s[i-1] != 'V') {
		return false
	}
	return i == 1 || !isVersionBodyByte(s[i-2])
}

// Compare orders two versions component by component; a missing component is
// zero, so 3.14 == 3.14.0 and 3.14 < 3.14.1.
func (v Version) Compare(o Version) int {
	n := max(len(v.Parts), len(o.Parts))
	for i := range n {
		a, b := 0, 0
		if i < len(v.Parts) {
			a = v.Parts[i]
		}
		if i < len(o.Parts) {
			b = o.Parts[i]
		}
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
	}
	return 0
}

// AtLeast reports whether v satisfies a minimum.
func (v Version) AtLeast(min Version) bool { return v.Compare(min) >= 0 }

// Known reports whether this version has any numeric content at all.
func (v Version) Known() bool { return len(v.Parts) > 0 }

// String renders the numeric parts, which is what the API and the UI show —
// never Raw, which is a whole line of vendor banner.
func (v Version) String() string {
	if len(v.Parts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range v.Parts {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(p))
	}
	return b.String()
}

// MustParseVersion is ParseVersion for compile-time constants — the minimum
// versions in this package's own table. It panics on a string it cannot parse,
// which can only ever be a typo in that table.
func MustParseVersion(s string) Version {
	v, ok := ParseVersion(s)
	if !ok {
		panic("toolchain: unparsable version constant " + strconv.Quote(s))
	}
	return v
}
