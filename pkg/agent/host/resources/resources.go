// Package resources reads the host's resource use: processor and memory
// from /proc, and each running managed container's processor and memory use
// from its cgroup (cgroups v2). It
// reads files only, which the agent's unprivileged user may, and keeps the
// previous counters to turn them into rates.
package resources

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Sampler reads the resource use.
type Sampler interface {
	// Sample reads the host and the running managed containers of
	// containers. Rates are over the time since the previous call.
	Sample(ctx context.Context, containers []protocol.Container) Sample
}

// Sample is one reading.
type Sample struct {
	Host protocol.Resources
	// Containers are the stats of the running managed containers, by name.
	Containers map[string]protocol.ContainerStats
}

// New returns the sampler of this host.
func New(log logrus.FieldLogger) Sampler {
	return newSampler(log, "/")
}

var _ Sampler = (*sampler)(nil)

// sampler reads the files below root; now is replaced in tests.
type sampler struct {
	log  logrus.FieldLogger
	root string
	now  func() time.Time

	mu sync.Mutex
	// cpu is the previous /proc/stat reading; zero before the first.
	cpu cpuTimes
	// cgroups are the previous readings of the containers, by container ID.
	cgroups map[string]cgroupReading
}

func newSampler(log logrus.FieldLogger, root string) *sampler {
	return &sampler{
		log:     log.WithField("package", "resources"),
		root:    root,
		now:     time.Now,
		cgroups: make(map[string]cgroupReading, 8),
	}
}

// path resolves an absolute host path below root.
func (s *sampler) path(abs string) string {
	return filepath.Join(s.root, abs)
}

// Sample implements Sampler. A part that cannot be read stays zero; the
// failure is logged at debug level, as it repeats with every sync.
func (s *sampler) Sample(_ context.Context, containers []protocol.Container) Sample {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out Sample

	if err := s.readCPU(&out.Host.CPU); err != nil {
		s.log.WithError(err).Debug("Cannot read the CPU use")
	}

	if err := s.readMemory(&out.Host.Memory); err != nil {
		s.log.WithError(err).Debug("Cannot read the memory use")
	}

	out.Containers = s.readContainers(containers, out.Host.CPU.Cores)

	return out
}

// round1 rounds to one decimal, which is all a percentage needs on the wire.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
