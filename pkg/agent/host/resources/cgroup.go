package resources

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const pathCgroupRoot = "/sys/fs/cgroup"

// cgroupReading is the previous reading of a container's cgroup.
type cgroupReading struct {
	pid int
	// dir is the cgroup's directory below pathCgroupRoot.
	dir   string
	usage uint64
	at    time.Time
}

// readContainers reads the cgroups of the running managed containers. The
// readings of containers that are gone are dropped.
func (s *sampler) readContainers(
	containers []protocol.Container, cores int,
) map[string]protocol.ContainerStats {
	out := make(map[string]protocol.ContainerStats, len(containers))
	readings := make(map[string]cgroupReading, len(containers))

	for i := range containers {
		c := &containers[i]
		if !c.Managed || c.PID <= 0 {
			continue
		}

		prev, known := s.cgroups[c.ID]
		if !known || prev.pid != c.PID {
			dir, err := s.cgroupDir(c.PID, c.ID)
			if err != nil {
				s.log.WithError(err).WithField("container", c.Name).Debug("Cannot find the cgroup")

				continue
			}

			prev = cgroupReading{pid: c.PID, dir: dir}
		}

		stats, usage, err := s.readCgroup(prev.dir)
		if err != nil {
			s.log.WithError(err).WithField("container", c.Name).Debug("Cannot read the cgroup")

			continue
		}

		now := s.now()

		if !prev.at.IsZero() && usage >= prev.usage && cores > 0 {
			if wall := now.Sub(prev.at); wall > 0 {
				cpu := round1(float64(usage-prev.usage) / float64(wall.Microseconds()) /
					float64(cores) * 100)
				stats.CPU = &cpu
			}
		}

		readings[c.ID] = cgroupReading{pid: c.PID, dir: prev.dir, usage: usage, at: now}
		out[c.Name] = stats
	}

	s.cgroups = readings

	return out
}

// cgroupDir is the cgroup of a container: the unified-hierarchy entry of
// /proc/<pid>/cgroup, whatever the cgroup driver names it
// (system.slice/docker-<id>.scope with systemd, docker/<id> with cgroupfs,
// or a custom parent). A process that moved to a child cgroup counts with
// the container's own: the path is cut after the element naming the ID.
func (s *sampler) cgroupDir(pid int, id string) (string, error) {
	raw, err := os.ReadFile(s.path("/proc/" + strconv.Itoa(pid) + "/cgroup"))
	if err != nil {
		return "", fmt.Errorf("read cgroup of pid %d: %w", pid, err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		path, ok := strings.CutPrefix(scanner.Text(), "0::")
		if !ok {
			continue
		}

		parts := strings.Split(strings.Trim(path, "/"), "/")
		for i, part := range parts {
			if id != "" && strings.Contains(part, id) {
				parts = parts[:i+1]

				break
			}
		}

		return strings.Join(parts, "/"), nil
	}

	return "", fmt.Errorf("pid %d has no cgroup v2 entry", pid)
}

// readCgroup reads the memory and the CPU time in microseconds of a
// cgroup.
func (s *sampler) readCgroup(dir string) (protocol.ContainerStats, uint64, error) {
	base := s.path(pathCgroupRoot + "/" + dir)

	var stats protocol.ContainerStats

	cpuStat, err := os.ReadFile(base + "/cpu.stat")
	if err != nil {
		return stats, 0, fmt.Errorf("read cpu.stat: %w", err)
	}

	usage, ok := statValue(cpuStat, "usage_usec")
	if !ok {
		return stats, 0, errors.New("cpu.stat has no usage_usec")
	}

	current, err := readUint(base + "/memory.current")
	if err != nil {
		return stats, 0, err
	}

	memStat, err := os.ReadFile(base + "/memory.stat")
	if err != nil {
		return stats, 0, fmt.Errorf("read memory.stat: %w", err)
	}

	inactive, _ := statValue(memStat, "inactive_file")
	stats.Memory = current - min(inactive, current)

	if raw, err := os.ReadFile(base + "/memory.max"); err == nil {
		if limit, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			stats.MemoryLimit = limit
		}
	}

	return stats, usage, nil
}

// statValue is the value of a key of a flat-keyed cgroup file.
func statValue(raw []byte, key string) (uint64, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		k, v, ok := strings.Cut(scanner.Text(), " ")
		if !ok || k != key {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)

		return n, err == nil
	}

	return 0, false
}

func readUint(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}

	return n, nil
}
