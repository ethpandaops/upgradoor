package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestBaseFromLabels(t *testing.T) {
	assert.Nil(t, baseFromLabels(nil))
	assert.Nil(t, baseFromLabels(map[string]string{labelOverrideCommand: "not a list"}), "no record")
	assert.Equal(t, &protocol.OverrideBase{Image: "a/b:c"},
		baseFromLabels(map[string]string{labelOverrideImage: "a/b:c"}))
	assert.Equal(t, &protocol.OverrideBase{Command: []string{"--x"}, HasCommand: true},
		baseFromLabels(map[string]string{labelOverrideCommand: `["--x"]`}))
}

func TestSetBaseLabels(t *testing.T) {
	for name, tt := range map[string]struct {
		base *protocol.OverrideBase
		want map[string]string
	}{
		"nil removes both": {
			want: map[string]string{"own": "yes"},
		},
		"image only: the command label goes": {
			base: &protocol.OverrideBase{Image: "a/b:c"},
			want: map[string]string{"own": "yes", labelOverrideImage: "a/b:c"},
		},
		"command only: the image label goes": {
			base: &protocol.OverrideBase{Command: []string{"--x", "y z"}, HasCommand: true},
			want: map[string]string{"own": "yes", labelOverrideCommand: `["--x","y z"]`},
		},
		"an empty command line is a record": {
			base: &protocol.OverrideBase{HasCommand: true},
			want: map[string]string{"own": "yes", labelOverrideCommand: `[]`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			labels := map[string]string{
				"own": "yes", labelOverrideImage: "old/image:1", labelOverrideCommand: `["--old"]`,
			}

			setBaseLabels(labels, tt.base)
			assert.Equal(t, tt.want, labels)

			want := tt.base
			if want != nil && want.HasCommand && want.Command == nil {
				// A nil command line is written, and read back, as [].
				want = &protocol.OverrideBase{Image: want.Image, Command: []string{}, HasCommand: true}
			}

			assert.Equal(t, want, baseFromLabels(labels), "reads back")
		})
	}
}
