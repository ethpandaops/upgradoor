package agent

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestConfigReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	write := func(geth string, extra string) {
		content := "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n" +
			"containers:\n" +
			"  - name: geth\n    kind: el\n" + geth +
			"  - name: exporter\n    image: org/exporter:1\n" + extra
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		// The stamp is size and modification time: make the change visible.
		later := time.Now().Add(time.Duration(len(content)) * time.Second)
		require.NoError(t, os.Chtimes(path, later, later))
	}

	write("    image: ethpandaops/geth:v1\n", "")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	log, hook := test.NewNullLogger()
	log.SetOutput(io.Discard)

	geth := cfg.Managed("geth")
	assert.Equal(t, "ethpandaops/geth:v1", cfg.current(geth).Image)
	assert.Nil(t, cfg.current(geth).Command, "no command line: the container's own is kept")

	// Ansible renders a new image and a command line: taken in, no restart.
	write("    image: ethpandaops/geth:v2\n    command: [\"--http\", \"--syncmode=full\"]\n", "")
	cfg.Reload(log)

	assert.Equal(t, "ethpandaops/geth:v2", cfg.current(geth).Image)
	assert.Equal(t, []string{"--http", "--syncmode=full"}, cfg.current(geth).Command)
	assert.Equal(t, "ethpandaops/geth:v1", geth.Image, "the configuration the agent started with stays")
	assert.Empty(t, warnings(hook))

	// A change beyond image and command line is logged and waits for a
	// restart; the image and command line of the same edit are taken in.
	write("    image: ethpandaops/geth:v3\n", "    impact: slot\n")
	cfg.Reload(log)

	assert.Equal(t, "ethpandaops/geth:v3", cfg.current(geth).Image)
	assert.Equal(t, protocol.ImpactIndependent, cfg.Managed("exporter").Impact)
	assert.Len(t, warnings(hook), 1)

	// The same file again says nothing more.
	cfg.Reload(log)
	assert.Len(t, warnings(hook), 1)

	// A file that does not load changes nothing.
	write("    image: \"\"\n", "")
	cfg.Reload(log)
	assert.Equal(t, "ethpandaops/geth:v3", cfg.current(geth).Image)
}

func warnings(hook *test.Hook) []*logrus.Entry {
	out := make([]*logrus.Entry, 0, len(hook.AllEntries()))

	for _, e := range hook.AllEntries() {
		if e.Level <= logrus.WarnLevel {
			out = append(out, e)
		}
	}

	return out
}
