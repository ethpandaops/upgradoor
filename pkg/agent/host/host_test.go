package host

import (
	"io"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host/docker"
)

func TestNewAndClose(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	cfg := func(daemon string) *Config {
		return &Config{Docker: docker.Config{Host: daemon}}
	}

	_, err := New(nil, cfg("unix:///var/run/docker.sock"), nil, nil)
	require.Error(t, err)

	_, err = New(log, cfg(""), nil, nil)
	require.Error(t, err)

	// New does not connect: a socket that does not exist is fine.
	r, err := New(log, cfg("unix:///nonexistent/upgradoor-test.sock"), nil, nil)
	require.NoError(t, err)

	_, err = r.Containers(t.Context())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "list containers"), err.Error())

	require.NoError(t, r.Close())
}
