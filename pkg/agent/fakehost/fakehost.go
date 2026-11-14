// Package fakehost is an in-memory host for local development and tests:
// one Host implements the agent's container runtime, registry and system
// interfaces from a small scenario, with no Docker, no apt and no reboot.
//
// It is deterministic: nothing happens unless the agent or the test asks
// for it. A "reboot" changes the boot id and restarts the running
// containers; the process keeps running.
package fakehost

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Docker states used by the fake.
const (
	StateRunning = "running"
	StateExited  = "exited"
	StateCreated = "created"
)

// Host is a fake host. It is safe for concurrent use.
type Host struct {
	mu sync.Mutex

	now      func() time.Time
	bootBase string
	boots    int
	bootedAt time.Time

	os             OS
	runningKernel  string
	newestKernel   string
	rebootRequired bool
	// inertReboot makes Reboot succeed without rebooting.
	inertReboot bool

	containers []*protocol.Container
	created    int
	// registry and local map a normalised tag reference to the digest the
	// registry and the local image store hold for it.
	registry map[string]Tag
	local    map[string]string

	blocked    []string
	installed  []protocol.Package
	upgradable []protocol.Upgradable

	failures map[string]error
	calls    []string
	onCall   func(call string)
}

var (
	_ agent.ContainerRuntime = (*Host)(nil)
	_ agent.Registry         = (*Host)(nil)
	_ agent.System           = (*Host)(nil)
)

// New builds a host from a scenario.
func New(sc *Scenario) (*Host, error) {
	if sc == nil {
		return nil, errors.New("fakehost: no scenario")
	}

	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("fakehost: %w", err)
	}

	h := &Host{
		now:            time.Now,
		bootBase:       cmp.Or(sc.BootID, "boot"),
		os:             sc.OS,
		runningKernel:  sc.RunningKernel,
		newestKernel:   cmp.Or(sc.NewestKernel, sc.RunningKernel),
		rebootRequired: sc.RebootRequired,
		containers:     make([]*protocol.Container, 0, len(sc.Containers)),
		registry:       make(map[string]Tag, len(sc.Registry)+len(sc.Containers)),
		local:          make(map[string]string, len(sc.Containers)),
		blocked:        slices.Clone(sc.Packages.Blocked),
		installed:      slices.Clone(sc.Packages.Installed),
		upgradable:     slices.Clone(sc.Packages.Upgradable),
		failures:       make(map[string]error, 4),
	}
	h.bootedAt = h.now()

	for _, c := range sc.Containers {
		h.created++

		key := agent.NormalizeReference(c.Image)

		h.containers = append(h.containers, &protocol.Container{
			Name:      c.Name,
			ID:        fmt.Sprintf("%s-%d", c.Name, h.created),
			Image:     c.Image,
			ImageID:   imageID(c.Digest),
			Digest:    c.Digest,
			Revision:  c.Revision,
			State:     cmp.Or(c.State, StateRunning),
			StartedAt: h.bootedAt,
		})

		// The image a container runs is local and, unless the scenario says
		// otherwise, what the registry has.
		if _, ok := h.local[key]; !ok && c.Digest != "" {
			h.local[key] = c.Digest
			h.registry[key] = Tag{Digest: c.Digest, Revision: c.Revision}
		}
	}

	for ref, tag := range sc.Registry {
		h.registry[agent.NormalizeReference(ref)] = tag
	}

	for i := range h.upgradable {
		h.upgradable[i].Blocked = slices.Contains(h.blocked, h.upgradable[i].Name)
	}

	return h, nil
}

func imageID(digest string) string {
	if digest == "" {
		return ""
	}

	return "image-of-" + digest
}

// begin records a call, runs the test hook and returns the injected
// failure for op, if any.
func (h *Host) begin(ctx context.Context, op, call string) error {
	h.mu.Lock()

	if call != "" {
		h.calls = append(h.calls, call)
	}

	hook := h.onCall
	err := h.failures[op]

	h.mu.Unlock()

	if hook != nil && call != "" {
		hook(call)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

func (h *Host) find(name string) *protocol.Container {
	for _, c := range h.containers {
		if c.Name == name {
			return c
		}
	}

	return nil
}

// Observe implements agent.ContainerRuntime.
func (h *Host) Observe(ctx context.Context) ([]protocol.Container, error) {
	if err := h.begin(ctx, "observe", ""); err != nil {
		return nil, fmt.Errorf("fakehost: observe: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]protocol.Container, 0, len(h.containers))
	for _, c := range h.containers {
		out = append(out, *c)
	}

	return out, nil
}

// LocalDigest implements agent.ContainerRuntime.
func (h *Host) LocalDigest(ctx context.Context, ref string) (string, error) {
	if err := h.begin(ctx, "local:"+ref, ""); err != nil {
		return "", fmt.Errorf("fakehost: local digest of %s: %w", ref, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.local[agent.NormalizeReference(ref)], nil
}

// Pull implements agent.ContainerRuntime: the local image of ref becomes
// what the registry has. Containers are not touched.
func (h *Host) Pull(ctx context.Context, ref string) (string, error) {
	if err := h.begin(ctx, "pull:"+ref, "pull "+ref); err != nil {
		return "", fmt.Errorf("fakehost: pull %s: %w", ref, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	key := agent.NormalizeReference(ref)

	tag, ok := h.registry[key]
	if !ok {
		return "", fmt.Errorf("fakehost: pull %s: manifest unknown", ref)
	}

	h.local[key] = tag.Digest

	return tag.Digest, nil
}

// Recreate implements agent.ContainerRuntime. An injected failure leaves
// the container stopped, as a recreate that broke half-way would.
func (h *Host) Recreate(
	ctx context.Context, name, digest string, _ time.Duration, start bool,
) error {
	h.mu.Lock()

	c := h.find(name)
	if c == nil {
		h.mu.Unlock()

		return fmt.Errorf("fakehost: recreate %s: no such container", name)
	}

	key := agent.NormalizeReference(c.Image)
	local := h.local[key]

	h.mu.Unlock()

	// The digest check comes before anything is stopped, and before the
	// call counts as a disruption.
	if local != digest {
		return fmt.Errorf("fakehost: recreate %s: local image is %q, wanted %q: %w",
			name, local, digest, agent.ErrDigestMismatch)
	}

	err := h.begin(ctx, "recreate:"+name, fmt.Sprintf("recreate %s %s start=%t", name, digest, start))

	h.mu.Lock()
	defer h.mu.Unlock()

	if c = h.find(name); c == nil {
		return fmt.Errorf("fakehost: recreate %s: no such container", name)
	}

	if err != nil {
		c.State = StateExited

		return fmt.Errorf("fakehost: recreate %s: %w", name, err)
	}

	h.created++
	c.ID = fmt.Sprintf("%s-%d", name, h.created)
	c.Digest = local
	c.ImageID = imageID(local)
	c.Revision = h.registry[key].Revision
	c.RestartCount = 0
	c.State = StateCreated
	c.StartedAt = time.Time{}

	if start {
		c.State = StateRunning
		c.StartedAt = h.now()
	}

	return nil
}

// Stop implements agent.ContainerRuntime.
func (h *Host) Stop(ctx context.Context, name string, _ time.Duration) error {
	if err := h.begin(ctx, "stop:"+name, "stop "+name); err != nil {
		return fmt.Errorf("fakehost: stop %s: %w", name, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("fakehost: stop %s: no such container", name)
	}

	if c.State == StateRunning {
		c.State = StateExited
		// What "docker stop" leaves behind.
		c.ExitCode = 143
	}

	return nil
}

// Start implements agent.ContainerRuntime.
func (h *Host) Start(ctx context.Context, name string) error {
	if err := h.begin(ctx, "start:"+name, "start "+name); err != nil {
		return fmt.Errorf("fakehost: start %s: %w", name, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("fakehost: start %s: no such container", name)
	}

	if c.State != StateRunning {
		c.State = StateRunning
		c.ExitCode = 0
		c.StartedAt = h.now()
	}

	return nil
}

// Resolve implements agent.Registry.
func (h *Host) Resolve(ctx context.Context, ref string) (digest, revision string, err error) {
	if err := h.begin(ctx, "resolve:"+ref, ""); err != nil {
		return "", "", fmt.Errorf("fakehost: resolve %s: %w", ref, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	tag, ok := h.registry[agent.NormalizeReference(ref)]
	if !ok {
		return "", "", fmt.Errorf("fakehost: resolve %s: manifest unknown", ref)
	}

	return tag.Digest, tag.Revision, nil
}

// Status implements agent.System.
func (h *Host) Status(ctx context.Context) (*agent.SystemStatus, error) {
	if err := h.begin(ctx, "status", ""); err != nil {
		return nil, fmt.Errorf("fakehost: status: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	inv, err := h.inventoryLocked()
	if err != nil {
		return nil, err
	}

	st := &agent.SystemStatus{
		BootID: h.bootIDLocked(),
		Uptime: h.now().Sub(h.bootedAt),
		OS: protocol.OSSummary{
			ID:             h.os.ID,
			VersionID:      h.os.VersionID,
			PrettyName:     h.os.PrettyName,
			Arch:           h.os.Arch,
			RunningKernel:  h.runningKernel,
			NewestKernel:   h.newestKernel,
			RebootRequired: h.rebootRequired,
		},
		InventoryHash: inv.Hash,
	}

	for _, u := range h.upgradable {
		st.Pending.Upgradable++

		if u.Security {
			st.Pending.Security++
		}

		if u.Blocked {
			st.Pending.Blocked++
			st.Blocked = append(st.Blocked, protocol.PackageChange{
				Name: u.Name, From: u.Current, To: u.Candidate,
			})
		}
	}

	return st, nil
}

// Inventory implements agent.System.
func (h *Host) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	if err := h.begin(ctx, "inventory", ""); err != nil {
		return nil, fmt.Errorf("fakehost: inventory: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.inventoryLocked()
}

func (h *Host) inventoryLocked() (*protocol.Inventory, error) {
	inv := &protocol.Inventory{
		CollectedAt: h.now().UTC(),
		OS: protocol.OSInfo{
			ID: h.os.ID, VersionID: h.os.VersionID, PrettyName: h.os.PrettyName, Arch: h.os.Arch,
		},
		Kernels: protocol.KernelInfo{
			Running:   h.runningKernel,
			Newest:    h.newestKernel,
			Installed: []string{h.runningKernel},
		},
		Packages:   slices.Clone(h.installed),
		Upgradable: slices.Clone(h.upgradable),
	}

	if h.newestKernel != h.runningKernel {
		inv.Kernels.Installed = append(inv.Kernels.Installed, h.newestKernel)
	}

	slices.SortFunc(inv.Packages, func(a, b protocol.Package) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Arch, b.Arch))
	})
	slices.SortFunc(inv.Upgradable, func(a, b protocol.Upgradable) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Arch, b.Arch))
	})

	hash, err := inv.ComputeHash()
	if err != nil {
		return nil, fmt.Errorf("fakehost: %w", err)
	}

	inv.Hash = hash

	return inv, nil
}

// UpgradePackages implements agent.System: every name must be in the
// scenario's blocked set; those with a candidate move to it.
func (h *Host) UpgradePackages(ctx context.Context, names []string) error {
	sorted := slices.Sorted(slices.Values(names))

	if err := h.begin(ctx, "upgrade", "upgrade "+strings.Join(sorted, ",")); err != nil {
		return fmt.Errorf("fakehost: upgrade packages: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, name := range names {
		if !slices.Contains(h.blocked, name) {
			return fmt.Errorf("fakehost: upgrade packages: %q is not in the blocked set", name)
		}
	}

	h.upgradable = slices.DeleteFunc(h.upgradable, func(u protocol.Upgradable) bool {
		if !slices.Contains(names, u.Name) {
			return false
		}

		for i := range h.installed {
			if h.installed[i].Name == u.Name && h.installed[i].Arch == u.Arch {
				h.installed[i].Version = u.Candidate
			}
		}

		return true
	})

	return nil
}

// Reboot implements agent.System. It simulates the reboot in place: new
// boot id, the newest kernel runs, the reboot flag is gone, and containers
// that were running have restarted. Stopped containers stay stopped, as
// they do under Docker's unless-stopped policy.
func (h *Host) Reboot(ctx context.Context) error {
	if err := h.begin(ctx, "reboot", "reboot"); err != nil {
		return fmt.Errorf("fakehost: reboot: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.inertReboot {
		return nil
	}

	h.boots++
	h.bootedAt = h.now()
	h.runningKernel = h.newestKernel
	h.rebootRequired = false

	for _, c := range h.containers {
		if c.State == StateRunning {
			c.StartedAt = h.bootedAt
		}
	}

	return nil
}

func (h *Host) bootIDLocked() string {
	if h.boots == 0 {
		return h.bootBase
	}

	return fmt.Sprintf("%s-%d", h.bootBase, h.boots)
}

// The methods below steer the fake from a test or the control endpoint.

// SetClock replaces the host's clock.
func (h *Host) SetClock(now func() time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.now = now
}

// SetRegistry moves a tag in the fake registry.
func (h *Host) SetRegistry(ref, digest, revision string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.registry[agent.NormalizeReference(ref)] = Tag{Digest: digest, Revision: revision}
}

// SetUpgradable replaces the upgradable list. Blocked is derived from the
// scenario's blocked set.
func (h *Host) SetUpgradable(list []protocol.Upgradable) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.upgradable = slices.Clone(list)

	for i := range h.upgradable {
		h.upgradable[i].Blocked = slices.Contains(h.blocked, h.upgradable[i].Name)
	}
}

// SetRebootRequired sets or clears the reboot-required flag.
func (h *Host) SetRebootRequired(required bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.rebootRequired = required
}

// SetNewestKernel installs a kernel; the host wants a reboot while it
// differs from the running one.
func (h *Host) SetNewestKernel(release string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.newestKernel = release
}

// SetContainerImage swaps the image reference of a container, as a person
// debugging with a custom image would.
func (h *Host) SetContainerImage(name, image, digest string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("fakehost: no such container %q", name)
	}

	c.Image, c.Digest, c.ImageID = image, digest, imageID(digest)

	return nil
}

// Crash makes a container restart once: its restart count goes up.
func (h *Host) Crash(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("fakehost: no such container %q", name)
	}

	c.RestartCount++
	c.StartedAt = h.now()

	return nil
}

// Exit makes a running container stop by itself with the given exit code.
func (h *Host) Exit(name string, code int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("fakehost: no such container %q", name)
	}

	c.State = StateExited
	c.ExitCode = code

	return nil
}

// SetInertReboot makes Reboot return success without rebooting, like a
// reboot that systemd accepted and never carried out.
func (h *Host) SetInertReboot(inert bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.inertReboot = inert
}

// Fail makes an operation fail with err until it is cleared with a nil
// error. Operations: observe, status, inventory, upgrade, reboot,
// local:<ref>, pull:<ref>, resolve:<ref>, recreate:<name>, stop:<name>,
// start:<name>.
func (h *Host) Fail(op string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err == nil {
		delete(h.failures, op)

		return
	}

	h.failures[op] = err
}

// OnCall sets a hook that runs at the start of every call that changes the
// host (pull, recreate, stop, start, upgrade, reboot), with the same text
// Calls records. It runs without the host's lock held.
func (h *Host) OnCall(hook func(call string)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.onCall = hook
}

// Calls returns the calls that changed the host, in order: "pull <ref>",
// "recreate <name> <digest> start=<bool>", "stop <name>", "start <name>",
// "upgrade <a,b>", "reboot".
func (h *Host) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

// BootID returns the current boot id.
func (h *Host) BootID() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.bootIDLocked()
}

// Container returns a copy of a container, or nil.
func (h *Host) Container(name string) *protocol.Container {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return nil
	}

	out := *c

	return &out
}
