package agent

import (
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// baseSpec is what Ansible deployed one managed container as: its image and,
// when agent.yaml names one, its command line. It is the part of agent.yaml
// the agent takes in without a restart (soft deploy).
type baseSpec struct {
	image   string
	command []string
}

// baseTable holds the base specs as last read from agent.yaml. The loop
// writes it; the operation and the local socket read it.
type baseTable struct {
	mu    sync.RWMutex
	specs map[string]baseSpec
	// stamp is the file as last looked at; warned is the stamp whose changes
	// beyond image and command were logged.
	stamp  fileStamp
	warned fileStamp
}

// fileStamp tells whether a file changed since it was last read.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func newBaseTable(containers []ManagedContainer, stamp fileStamp) *baseTable {
	t := &baseTable{specs: make(map[string]baseSpec, len(containers)), stamp: stamp}

	for i := range containers {
		m := &containers[i]
		t.specs[m.Name] = baseSpec{image: m.Image, command: slices.Clone(m.Command)}
	}

	return t
}

// current returns the managed container as agent.yaml describes it now: the
// configuration it was started with, and the image and command line of the
// last read.
func (c *Config) current(m *ManagedContainer) ManagedContainer {
	out := *m

	if c.bases == nil {
		return out
	}

	c.bases.mu.RLock()
	defer c.bases.mu.RUnlock()

	if b, ok := c.bases.specs[m.Name]; ok {
		out.Image, out.Command = b.image, b.command
	}

	return out
}

// Reload takes in a changed agent.yaml: the image and the command line of
// every container it was started with. Anything else that changed (a
// container added or removed, its kind, impact, owners, the verbs, ...) is
// logged once and needs an agent restart. A file that cannot be read or
// does not validate changes nothing.
func (c *Config) Reload(log logrus.FieldLogger) {
	if c.bases == nil || c.path == "" {
		return
	}

	info, err := os.Stat(c.path)
	if err != nil {
		return
	}

	stamp := fileStamp{size: info.Size(), modTime: info.ModTime()}
	if stamp == c.bases.stamp {
		return
	}

	log = log.WithField("file", c.path)

	next, err := LoadConfig(c.path)
	if err != nil {
		if stamp != c.bases.warned {
			c.bases.warned = stamp

			log.WithError(err).Error("agent.yaml changed and does not load; keeping what was read before")
		}

		return
	}

	changed := 0

	c.bases.mu.Lock()

	for i := range c.Containers {
		m := &c.Containers[i]

		n := next.Managed(m.Name)
		if n == nil {
			continue
		}

		was := c.bases.specs[m.Name]
		if was.image != n.Image || !slices.Equal(was.command, n.Command) ||
			(was.command == nil) != (n.Command == nil) {
			c.bases.specs[m.Name] = baseSpec{image: n.Image, command: slices.Clone(n.Command)}
			changed++

			log.WithFields(logrus.Fields{"container": m.Name, "image": n.Image}).
				Info("agent.yaml names another image or command line; the container is rolled under a lease")
		}
	}

	c.bases.stamp = stamp
	c.bases.mu.Unlock()

	if !sameBeyondSpecs(c, next) && stamp != c.bases.warned {
		c.bases.warned = stamp

		log.Warn("agent.yaml changed beyond the image and command line of its containers; " +
			"restart the agent to take that in")
	}

	if changed == 0 {
		log.Debug("agent.yaml changed; no image or command line differs")
	}
}

// sameBeyondSpecs reports whether two configurations differ in nothing but
// the image and command line of their containers.
func sameBeyondSpecs(a, b *Config) bool {
	strip := func(c *Config) Config {
		out := *c
		out.path, out.bases = "", nil
		out.Containers = slices.Clone(c.Containers)

		for i := range out.Containers {
			out.Containers[i].Image, out.Containers[i].Command = "", nil
		}

		return out
	}

	return reflect.DeepEqual(strip(a), strip(b))
}
