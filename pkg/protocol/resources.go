package protocol

// Resources is the host's resource use, read when the sync was sent. Like
// OSSummary it is served to logins with the full view of the host only.
type Resources struct {
	CPU    CPUStats    `json:"cpu"`
	Memory MemoryStats `json:"memory"`
}

// CPUStats is the host's processor use.
type CPUStats struct {
	Cores  int     `json:"cores"`
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	// Busy is the percentage of CPU time not idle since the agent's previous
	// sample; absent on its first.
	Busy *float64 `json:"busy,omitempty"`
}

// MemoryStats is the host's memory in bytes. Total is zero when it could
// not be read.
type MemoryStats struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	SwapTotal uint64 `json:"swapTotal"`
	SwapUsed  uint64 `json:"swapUsed"`
}

// UsedPercent is the share of memory not available; false when unknown.
func (m MemoryStats) UsedPercent() (float64, bool) {
	if m.Total == 0 {
		return 0, false
	}

	return percent(m.Total-min(m.Available, m.Total), m.Total), true
}

// ContainerStats is the resource use of one running managed container,
// from its cgroup.
type ContainerStats struct {
	// CPU is the percentage of the host's CPU capacity the container used
	// since the agent's previous sample; absent on its first.
	CPU *float64 `json:"cpu,omitempty"`
	// Memory is the memory in use without the inactive page cache, as
	// "docker stats" counts it.
	Memory uint64 `json:"memory"`
	// MemoryLimit is the container's memory limit; absent without one.
	MemoryLimit uint64 `json:"memoryLimit,omitempty"`
}

func percent(part, whole uint64) float64 {
	return float64(part) / float64(whole) * 100
}
