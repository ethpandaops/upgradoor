package resources

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	pathStat    = "/proc/stat"
	pathLoadavg = "/proc/loadavg"
	pathMeminfo = "/proc/meminfo"
)

// cpuTimes are the aggregate CPU counters of /proc/stat in clock ticks.
type cpuTimes struct {
	busy, total uint64
}

// readCPU reads the cores, the load and the busy share since the previous
// reading.
func (s *sampler) readCPU(out *protocol.CPUStats) error {
	raw, err := os.ReadFile(s.path(pathStat))
	if err != nil {
		return fmt.Errorf("read stat: %w", err)
	}

	times, cores, err := parseStat(raw)
	if err != nil {
		return err
	}

	out.Cores = cores

	if prev := s.cpu; prev.total > 0 && times.total > prev.total && times.busy >= prev.busy {
		busy := round1(float64(times.busy-prev.busy) / float64(times.total-prev.total) * 100)
		out.Busy = &busy
	}

	s.cpu = times

	raw, err = os.ReadFile(s.path(pathLoadavg))
	if err != nil {
		return fmt.Errorf("read loadavg: %w", err)
	}

	load, err := parseLoadavg(raw)
	out.Load1, out.Load5, out.Load15 = load[0], load[1], load[2]

	return err
}

// parseStat reads the aggregate "cpu" line of /proc/stat and counts the
// per-CPU lines. Busy is everything but idle and iowait; guest time is
// already part of user time.
func parseStat(raw []byte) (cpuTimes, int, error) {
	var (
		times cpuTimes
		found bool
		cores int
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}

		if fields[0] != "cpu" {
			cores++

			continue
		}

		if len(fields) < 9 {
			return cpuTimes{}, 0, errors.New("parse stat: short cpu line")
		}

		for i, f := range fields[1:9] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, 0, fmt.Errorf("parse stat: %w", err)
			}

			times.total += v

			// idle and iowait
			if i != 3 && i != 4 {
				times.busy += v
			}
		}

		found = true
	}

	if !found {
		return cpuTimes{}, 0, errors.New("parse stat: no cpu line")
	}

	return times, cores, nil
}

// parseLoadavg reads the 1, 5 and 15 minute load averages of /proc/loadavg.
func parseLoadavg(raw []byte) ([3]float64, error) {
	var load [3]float64

	fields := strings.Fields(string(raw))
	if len(fields) < len(load) {
		return load, errors.New("parse loadavg: short")
	}

	for i := range load {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, fmt.Errorf("parse loadavg: %w", err)
		}

		load[i] = v
	}

	return load, nil
}

// readMemory reads /proc/meminfo.
func (s *sampler) readMemory(out *protocol.MemoryStats) error {
	raw, err := os.ReadFile(s.path(pathMeminfo))
	if err != nil {
		return fmt.Errorf("read meminfo: %w", err)
	}

	*out = parseMeminfo(raw)

	return nil
}

// parseMeminfo reads the memory and swap totals of /proc/meminfo, in bytes.
func parseMeminfo(raw []byte) protocol.MemoryStats {
	var (
		out      protocol.MemoryStats
		swapFree uint64
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}

		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}

		switch key {
		case "MemTotal":
			out.Total = v
		case "MemAvailable":
			out.Available = v
		case "SwapTotal":
			out.SwapTotal = v
		case "SwapFree":
			swapFree = v
		}
	}

	out.SwapUsed = out.SwapTotal - min(swapFree, out.SwapTotal)

	return out
}
