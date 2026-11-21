package agenttest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadScenarioRejectsBadInput(t *testing.T) {
	_, err := LoadScenario(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)

	tests := map[string]string{
		"not yaml":          "containers: [",
		"unknown key":       "control: 127.0.0.1:7777",
		"nameless":          "containers: [{image: x}]",
		"imageless":         "containers: [{name: x}]",
		"duplicate":         "containers: [{name: x, image: a}, {name: x, image: b}]",
		"registry no digst": "registry: {x: {revision: r}}",
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scenario.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			sc, loadErr := LoadScenario(path)
			if loadErr == nil {
				_, loadErr = NewHost(sc, nil)
			}

			require.Error(t, loadErr)
		})
	}

	_, err = NewHost(nil, nil)
	require.Error(t, err)
}
