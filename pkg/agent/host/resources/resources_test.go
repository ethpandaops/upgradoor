package resources

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// testSampler samples a copy of testdata/host, which the test may change.
func testSampler(t *testing.T) (s *sampler, root string, clock *time.Time) {
	t.Helper()

	root = t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS("testdata/host")))

	log := logrus.New()
	log.SetOutput(io.Discard)

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s = newSampler(log, root)
	s.now = func() time.Time { return now }

	return s, root, &now
}

func testContainers() []protocol.Container {
	return []protocol.Container{
		{Name: "geth", ID: "aaaa1111", Managed: true, PID: 4242},
		{Name: "lighthouse", ID: "bbbb2222", Managed: true, PID: 5151},
		{Name: "stopped", ID: "cccc3333", Managed: true},
		{Name: "shell", ID: "dddd4444", PID: 4242},
	}
}

func TestSample(t *testing.T) {
	s, root, clock := testSampler(t)
	ctx := context.Background()

	first := s.Sample(ctx, testContainers())

	assert.Equal(t, protocol.CPUStats{Cores: 4, Load1: 1.25, Load5: 0.98, Load15: 0.75}, first.Host.CPU,
		"no busy share before a second reading")
	assert.Equal(t, protocol.MemoryStats{
		Total: 16384000 * 1024, Available: 4096000 * 1024, SwapTotal: 2097152 * 1024, SwapUsed: 524288 * 1024,
	}, first.Host.Memory)

	assert.Equal(t, map[string]protocol.ContainerStats{
		"geth":       {Memory: 2 << 30},
		"lighthouse": {Memory: 384 << 20, MemoryLimit: 1 << 30},
	}, first.Containers)

	write := func(rel, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(content), 0o600))
	}

	write("proc/stat", "cpu  10600 500 3000 80400 1000 100 200 0 0 0\ncpu0 1 1 1 1 1 1 1 1 0 0\n")
	write("sys/fs/cgroup/system.slice/docker-aaaa1111.scope/cpu.stat", "usage_usec 10000000\n")
	*clock = clock.Add(10 * time.Second)

	second := s.Sample(ctx, testContainers()[:1])

	require.NotNil(t, second.Host.CPU.Busy)
	assert.InDelta(t, 60.0, *second.Host.CPU.Busy, 0.001)
	assert.Equal(t, 1, second.Host.CPU.Cores)

	require.NotNil(t, second.Containers["geth"].CPU)
	assert.InDelta(t, 50.0, *second.Containers["geth"].CPU, 0.001, "5s of CPU in 10s on one core")
	assert.Len(t, s.cgroups, 1, "the reading of a container that is gone is dropped")
}
