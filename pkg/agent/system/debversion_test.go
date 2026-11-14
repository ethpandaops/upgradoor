package system

import (
	"math/rand/v2"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionCases is the comparison table. TestCompareVersionsAgainstDpkg checks
// the same expectations against dpkg --compare-versions where dpkg exists.
var versionCases = []struct {
	a, b string
	want int
	// malformed marks versions dpkg itself rejects.
	malformed bool
}{
	// Equality.
	{"1.0", "1.0", 0, false},
	{"1.0-1", "1.0-1", 0, false},
	{"0:1.0", "1.0", 0, false},
	{"1.0", "1.0-0", 0, false},
	{"1.00", "1.0", 0, false},
	{"1.0-01", "1.0-1", 0, false},
	{"", "", 0, true},
	{"0", "", 0, true},

	// Numeric, not lexical.
	{"1.9", "1.10", -1, false},
	{"1.10", "1.9", 1, false},
	{"6.12.94", "6.12.107", -1, false},
	{"2.41-12", "2.41-9", 1, false},
	{"10", "9", 1, false},
	{"1.0", "1.0.1", -1, false},
	{"99999999999999999999999", "99999999999999999999998", 1, false},

	// Epoch wins over everything.
	{"1:1.0", "2.0", 1, false},
	{"2.0", "1:1.0", -1, false},
	{"2:1.0", "1:9.9", 1, false},
	{"5:28.1.0-1~debian.13~trixie", "5:29.9.3-1~debian.13~trixie", -1, false},
	{"1:1.3.dfsg+really1.3.1-1+b1", "1:1.3.dfsg+really1.3.1-1+deb13u1", -1, false},

	// Revision.
	{"1.0-1", "1.0-2", -1, false},
	{"1.0-1", "1.0", 1, false},
	{"1.0-1+b1", "1.0-1", 1, false},
	{"1.0-1", "1.0-1+deb13u1", -1, false},
	{"3.5.7-1~deb13u2", "3.5.7-1~deb13u3", -1, false},
	{"3.5.7-1~deb13u3", "3.5.7-1", -1, false},
	// The revision starts at the last hyphen.
	{"1.0-1-2", "1.0-1-10", -1, false},
	{"1.0-2-1", "1.0-10-1", -1, false},

	// Tilde sorts before everything, even the end.
	{"1.0~rc1", "1.0", -1, false},
	{"1.0", "1.0~rc1", 1, false},
	{"1.0~rc1", "1.0~rc2", -1, false},
	{"1.0~~", "1.0~", -1, false},
	{"1.0~", "1.0", -1, false},
	{"1.0~beta", "1.0~", 1, false},
	{"1.0-1~bpo12+1", "1.0-1", -1, false},
	{"2.4.2-4~debian.13~trixie", "2.4.2-4", -1, false},

	// Letters sort before other characters, the end before letters.
	{"1.0a", "1.0+", -1, false},
	{"1.0+", "1.0a", 1, false},
	{"1.0", "1.0a", -1, false},
	{"1.0a", "1.0b", -1, false},
	{"1.0A", "1.0a", -1, false},
	{"1.0+dfsg", "1.0", 1, false},
	{"1.0.", "1.0+", 1, false},
	{"1.0+1-1", "1.0-1-1", -1, false},
	{"2026c-0+deb13u1", "2026f-0+deb13u1", -1, false},
	{"a1", "1", 1, true},

	// Kernel releases as used for the newest installed kernel.
	{"6.12.94+deb13-cloud-amd64", "6.12.107+deb13-cloud-amd64", -1, false},
	{"6.12.115+deb13-cloud-amd64", "6.12.115+deb13.1-cloud-amd64", -1, false},
	{"6.1.0-9-amd64", "6.1.0-13-amd64", -1, false},
	{"6.8.0-45-generic", "6.8.0-100-generic", -1, false},
	{"5.15.0-1020-aws", "5.15.0-999-aws", 1, false},
	{"6.12.9+bpo-amd64", "6.12.9-amd64", 1, false},

	// Malformed input is ordered, not rejected.
	{"abc:1.0", "1.0", 1, true},
	{" 1.0 ", "1.0", 0, true},
}

func TestCompareVersions(t *testing.T) {
	for _, tt := range versionCases {
		t.Run(tt.a+" vs "+tt.b, func(t *testing.T) {
			assert.Equal(t, tt.want, CompareVersions(tt.a, tt.b))
			assert.Equal(t, -tt.want, CompareVersions(tt.b, tt.a), "reversed")
		})
	}
}

// dpkgCompare asks the real dpkg for the order of two versions.
func dpkgCompare(t *testing.T, dpkg, a, b string) int {
	t.Helper()

	holds := func(op string) bool {
		err := exec.CommandContext(t.Context(), dpkg, "--compare-versions", a, op, b).Run()
		if err == nil {
			return true
		}

		var exitErr *exec.ExitError

		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 1, exitErr.ExitCode(), "dpkg --compare-versions %q %s %q", a, op, b)

		return false
	}

	switch {
	case holds("lt"):
		return -1
	case holds("gt"):
		return 1
	default:
		require.True(t, holds("eq"))

		return 0
	}
}

func TestCompareVersionsAgainstDpkg(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("dpkg not installed")
	}

	for _, tt := range versionCases {
		if tt.malformed {
			continue
		}

		assert.Equal(t, tt.want, dpkgCompare(t, dpkg, tt.a, tt.b), "%q vs %q", tt.a, tt.b)
	}

	// Random versions over the awkward part of the alphabet.
	const alphabet = "0019aZb~+.-"

	rng := rand.New(rand.NewPCG(1, 2))
	gen := func() string {
		b := []byte{'0' + byte(rng.IntN(3))}
		for range rng.IntN(7) {
			b = append(b, alphabet[rng.IntN(len(alphabet))])
		}

		// dpkg rejects an empty revision.
		if b[len(b)-1] == '-' {
			b = append(b, '1')
		}

		if rng.IntN(4) == 0 {
			return "1:" + string(b)
		}

		return string(b)
	}

	for range 400 {
		a, b := gen(), gen()
		assert.Equal(t, dpkgCompare(t, dpkg, a, b), CompareVersions(a, b), "%q vs %q", a, b)
	}
}

func TestSplitVersion(t *testing.T) {
	tests := []struct {
		in                        string
		epoch, upstream, revision string
	}{
		{"1.0", "", "1.0", ""},
		{"1:1.0-2", "1", "1.0", "2"},
		{"5:28.1.0-1~debian.13~trixie", "5", "28.1.0", "1~debian.13~trixie"},
		{"1.0-1-2", "", "1.0-1", "2"},
		{"1:2:3-4", "1", "2:3", "4"},
		{"x:1", "", "x:1", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			epoch, upstream, revision := splitVersion(tt.in)
			assert.Equal(t, tt.epoch, epoch)
			assert.Equal(t, tt.upstream, upstream)
			assert.Equal(t, tt.revision, revision)
		})
	}
}
