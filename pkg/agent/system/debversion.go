package system

import (
	"cmp"
	"strings"
)

// CompareVersions compares two Debian package versions the way
// "dpkg --compare-versions" does and returns -1, 0 or +1.
//
// A version is [epoch:]upstream[-revision]. The epoch is compared
// numerically; upstream and revision are compared piecewise: runs of
// non-digits by a modified character order in which '~' sorts before
// everything, even the end of the string, letters sort before all other
// characters, and runs of digits by numeric value.
//
// Malformed input never fails: an epoch that is not a number is compared as
// part of the upstream version.
func CompareVersions(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)

	if c := compareNumeric(ea, eb); c != 0 {
		return c
	}

	if c := compareVersionPart(ua, ub); c != 0 {
		return c
	}

	return compareVersionPart(ra, rb)
}

// splitVersion cuts a version into epoch, upstream version and revision. The
// epoch ends at the first colon, the revision starts after the last hyphen.
func splitVersion(v string) (epoch, upstream, revision string) {
	v = strings.TrimSpace(v)

	if i := strings.IndexByte(v, ':'); i >= 0 && isDigits(v[:i]) {
		epoch, v = v[:i], v[i+1:]
	}

	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}

	return epoch, v, ""
}

func isDigits(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}

	return true
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// compareNumeric compares two strings of digits of any length by value. The
// empty string is zero.
func compareNumeric(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")

	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}

	return cmp.Compare(a, b)
}

// versionCharOrder is dpkg's order(): the weight of one character inside a
// non-digit run. Digits and the end of the string weigh 0.
func versionCharOrder(s string, i int) int {
	if i >= len(s) {
		return 0
	}

	c := s[i]

	switch {
	case c == '~':
		return -1
	case isDigit(c):
		return 0
	case isLetter(c):
		return int(c)
	default:
		return int(c) + 256
	}
}

// compareVersionPart is dpkg's verrevcmp() for one upstream version or
// revision.
func compareVersionPart(a, b string) int {
	i, j := 0, 0

	for i < len(a) || j < len(b) {
		// Non-digit runs, character by character.
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			if c := cmp.Compare(versionCharOrder(a, i), versionCharOrder(b, j)); c != 0 {
				return c
			}

			i++
			j++
		}

		// Digit runs, by value.
		startA := i
		for i < len(a) && isDigit(a[i]) {
			i++
		}

		startB := j
		for j < len(b) && isDigit(b[j]) {
			j++
		}

		if c := compareNumeric(a[min(startA, len(a)):min(i, len(a))],
			b[min(startB, len(b)):min(j, len(b))]); c != 0 {
			return c
		}
	}

	return 0
}
