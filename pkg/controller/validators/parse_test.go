package validators

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func hostRange(host string, start, end uint64) fleet.HostRange {
	return fleet.HostRange{Host: host, ValidatorRange: identity.ValidatorRange{Start: start, End: end}}
}

func TestParse(t *testing.T) {
	got, err := Parse([]byte(`{
		"history": [
			{"effective_from": 0, "ranges": {"0-99": "lighthouse-geth-1"}},
			{"effective_from": 7, "ranges": {"0-199": "lighthouse-geth-1", "200-399": "lighthouse-geth-2"}}
		],
		"ranges": {"200-399": "lighthouse-geth-2", "0-199": "lighthouse-geth-1", "500-500": "lighthouse-geth-1"}
	}`))
	require.NoError(t, err)
	assert.Equal(t, []fleet.HostRange{
		hostRange("lighthouse-geth-1", 0, 200),
		hostRange("lighthouse-geth-2", 200, 400),
		hostRange("lighthouse-geth-1", 500, 501),
	}, got)

	empty, err := Parse([]byte(`{"ranges": {}}`))
	require.NoError(t, err)
	assert.Empty(t, empty)

	for name, raw := range map[string]string{
		"not json":            `{"ranges": `,
		"no ranges":           `{"history": []}`,
		"no dash":             `{"ranges": {"100": "a"}}`,
		"reversed":            `{"ranges": {"10-5": "a"}}`,
		"signed":              `{"ranges": {"+1-5": "a"}}`,
		"beyond the bound":    `{"ranges": {"0-1099511627776": "a"}}`,
		"overlap":             `{"ranges": {"0-10": "a", "10-20": "b"}}`,
		"invalid host":        `{"ranges": {"0-10": "a/b"}}`,
		"empty host":          `{"ranges": {"0-10": ""}}`,
		"invalid history":     `{"ranges": {}, "history": [{"ranges": {"x-1": "a"}}]}`,
		"overlapping history": `{"ranges": {}, "history": [{"ranges": {"0-5": "a", "5-9": "b"}}]}`,
	} {
		_, err := Parse([]byte(raw))
		assert.Error(t, err, name)
	}
}
