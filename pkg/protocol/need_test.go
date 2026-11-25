package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An agent that does not report the command delta sends a need without it,
// and a need without a changed command line encodes as before.
func TestSpecNeedCommandIsOptional(t *testing.T) {
	var n Need
	require.NoError(t, json.Unmarshal([]byte(`{"id":"spec/geth/k","kind":"spec",`+
		`"spec":{"container":"geth","key":"k","image":"geth:v2","toDigest":"sha256:a","flags":true}}`), &n))
	require.NotNil(t, n.Spec)
	assert.True(t, n.Spec.Flags)
	assert.Nil(t, n.Spec.Command)

	raw, err := json.Marshal(&SpecNeed{Container: "geth", Key: "k", Image: "geth:v2", ToDigest: "sha256:a"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"container":"geth","key":"k","image":"geth:v2","toDigest":"sha256:a"}`, string(raw))
}

func TestCommandDeltaClone(t *testing.T) {
	assert.Nil(t, (*CommandDelta)(nil).Clone())

	d := &CommandDelta{Added: []string{"--a"}, Removed: []string{"--b"}, Truncated: true}
	c := d.Clone()
	assert.Equal(t, d, c)

	c.Added[0] = "--x"
	assert.Equal(t, "--a", d.Added[0])
}
