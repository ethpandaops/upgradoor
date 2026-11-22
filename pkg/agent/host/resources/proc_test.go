package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestParseStat(t *testing.T) {
	times, cores, err := parseStat([]byte("cpu  1 2 3 4 5 6 7 8 9 10\ncpu0 1 2 3 4 5 6 7 8 9 10\nintr 5\n"))
	require.NoError(t, err)
	assert.Equal(t, cpuTimes{busy: 1 + 2 + 3 + 6 + 7 + 8, total: 36}, times, "guest time is in user time")
	assert.Equal(t, 1, cores)

	for _, raw := range []string{"", "cpu 1 2 3\n", "cpu a b c d e f g h\n"} {
		_, _, err := parseStat([]byte(raw))
		assert.Error(t, err, raw)
	}
}

func TestParseLoadavg(t *testing.T) {
	load, err := parseLoadavg([]byte("0.50 1.00 2.25 1/100 42\n"))
	require.NoError(t, err)
	assert.Equal(t, [3]float64{0.5, 1, 2.25}, load)

	_, err = parseLoadavg([]byte("0.5"))
	assert.Error(t, err)
}

func TestParseMeminfo(t *testing.T) {
	got := parseMeminfo([]byte("MemTotal: 100 kB\nMemAvailable: 40 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\nbogus\n"))
	assert.Equal(t, protocol.MemoryStats{Total: 102400, Available: 40960}, got)
}
