package agenttest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// stateCreated is the Docker state a recreate with start false leaves.
	stateCreated = "created"
	// stopExitCode is what "docker stop" leaves behind (SIGTERM).
	stopExitCode = 143
	// parkLimit keeps a forgotten BlockIn release from holding the agent's
	// Stop until the package's timeout.
	parkLimit = 3 * AwaitTimeout
	// parkPoll is how often a parked call looks whether its test failed.
	parkPoll = 20 * time.Millisecond
)

// Host is a deterministic in-memory host implementing host.Host with the
// same contracts, and the registry it pulls from. It records every call (Calls)
// and can fail or park any of them (Fail, OnCall). Safe for concurrent use.
// An injected Recreate failure leaves the container exited, modelling an
// interrupted recreate, not the real host's rollback.
type Host struct {
	mu sync.Mutex

	bootBase string
	boots    int
	bootedAt time.Time

	os             OS
	runningKernel  string
	newestKernel   string
	rebootRequired bool
	inertReboot    bool

	containers []*protocol.Container
	// created numbers the container IDs: a recreate gets a new one.
	created int
	// registry and local map a normalised tag reference to the image the
	// registry and the local image store hold for it.
	registry map[string]Tag
	local    map[string]Tag

	blocked    []string
	installed  []protocol.Package
	upgradable []protocol.Upgradable

	validators *host.Validators

	failures map[string]error
	resolves int
	// remote, when set, is asked for a tag's digest by every pull.
	remote func(ctx context.Context, ref string) (string, error)
	calls  []string
	notes  []string
	onCall func(call string)
}

var _ host.Host = (*Host)(nil)

// NewHost builds a host from a scenario; validators is agent.yaml's
// validators section, read with host.ReadValidators (nil: none).
func NewHost(sc *Scenario, validators *host.Validators) (*Host, error) {
	if sc == nil {
		return nil, errors.New("agenttest: no scenario")
	}

	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("agenttest: %w", err)
	}

	h := &Host{
		bootBase:       cmp.Or(sc.BootID, "boot"),
		bootedAt:       time.Now(),
		os:             sc.OS,
		runningKernel:  sc.RunningKernel,
		newestKernel:   cmp.Or(sc.NewestKernel, sc.RunningKernel),
		rebootRequired: sc.RebootRequired,
		containers:     make([]*protocol.Container, 0, len(sc.Containers)),
		registry:       make(map[string]Tag, len(sc.Registry)+len(sc.Containers)),
		local:          make(map[string]Tag, len(sc.Containers)),
		blocked:        slices.Clone(sc.Packages.Blocked),
		installed:      slices.Clone(sc.Packages.Installed),
		validators:     validators,
		failures:       make(map[string]error, 4),
	}

	for i := range sc.Containers {
		c := &sc.Containers[i]
		h.created++

		h.containers = append(h.containers, &protocol.Container{
			Name:      c.Name,
			ID:        fmt.Sprintf("%s-%d", c.Name, h.created),
			Image:     c.Image,
			ImageID:   imageID(c.Digest),
			Digest:    c.Digest,
			Revision:  c.Revision,
			State:     cmp.Or(c.State, protocol.StateRunning),
			StartedAt: h.bootedAt,
			Command:   slices.Clone(c.Command),
			Group:     c.Group,
			Kind:      protocol.ContainerKind(c.Kind),
			Linked:    c.Links,
		})

		key := imageref.Normalize(c.Image)
		if _, ok := h.local[key]; !ok && c.Digest != "" {
			h.local[key] = Tag{Digest: c.Digest, Revision: c.Revision}
			h.registry[key] = h.local[key]
		}
	}

	for ref, tag := range sc.Registry {
		h.registry[imageref.Normalize(ref)] = tag
	}

	h.setUpgradableLocked(sc.Packages.Upgradable)

	return h, nil
}

func imageID(digest string) string {
	if digest == "" {
		return ""
	}

	return "image-of-" + digest
}

// begin records call if not empty, runs the hook without the lock, and
// returns the context's error or the failure injected for op.
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

// clone copies a container so that no caller shares its slices.
func clone(c *protocol.Container) protocol.Container {
	out := *c
	out.Command = slices.Clone(c.Command)
	out.Base = cloneBase(c.Base)

	return out
}

// cloneBase copies an override base as the labels hand it back: nil when empty.
func cloneBase(b *protocol.OverrideBase) *protocol.OverrideBase {
	if b == nil || (b.Image == "" && !b.HasCommand) {
		return nil
	}

	out := *b
	out.Command = nil

	if b.HasCommand {
		out.Command = slices.Clone(b.Command)
	}

	return &out
}

// Containers implements host.Host (op "observe").
func (h *Host) Containers(ctx context.Context) ([]protocol.Container, error) {
	if err := h.begin(ctx, "observe", ""); err != nil {
		return nil, fmt.Errorf("agenttest: observe: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]protocol.Container, 0, len(h.containers))
	for _, c := range h.containers {
		out = append(out, clone(c))
	}

	return out, nil
}

// LocalImage implements host.Host (op "local:<ref>").
func (h *Host) LocalImage(ctx context.Context, ref string) (digest, revision string, err error) {
	if err := h.begin(ctx, "local:"+ref, ""); err != nil {
		return "", "", fmt.Errorf("agenttest: local image of %s: %w", ref, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	local := h.local[imageref.Normalize(ref)]

	return local.Digest, local.Revision, nil
}

// Pull implements host.Host: the tag's local image becomes the registry's.
func (h *Host) Pull(ctx context.Context, ref string) (digest, revision string, err error) {
	if err := h.begin(ctx, "pull:"+ref, "pull "+ref); err != nil {
		return "", "", fmt.Errorf("agenttest: pull %s: %w", ref, err)
	}

	h.mu.Lock()
	remote := h.remote
	h.mu.Unlock()

	key := imageref.Normalize(ref)

	if remote != nil {
		digest, err := remote(ctx, ref)
		if err != nil {
			return "", "", fmt.Errorf("agenttest: pull %s: %w", ref, err)
		}

		h.SetRegistry(ref, digest, "")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	tag, ok := h.registry[key]
	if !ok {
		return "", "", fmt.Errorf("agenttest: pull %s: manifest unknown", ref)
	}

	h.local[key] = tag

	return tag.Digest, tag.Revision, nil
}

// Recreate implements host.Host; a digest mismatch is refused before the
// call is recorded. A change appends " image=<image>" to the recorded call,
// and " command=<args>" when it sets the command.
func (h *Host) Recreate(
	ctx context.Context, name, digest string, _ time.Duration, start bool, change *host.Change,
) error {
	var ch host.Change
	if change != nil {
		ch = *change
	}

	h.mu.Lock()

	c := h.find(name)
	if c == nil {
		h.mu.Unlock()

		return fmt.Errorf("agenttest: recreate %s: no such container", name)
	}

	image := cmp.Or(ch.Image, c.Image)
	local := h.local[imageref.Normalize(image)]

	h.mu.Unlock()

	// An image without a registry digest is never rolled onto.
	if digest == "" || local.Digest != digest {
		return fmt.Errorf("agenttest: recreate %s: local image is %q, wanted %q: %w",
			name, local.Digest, digest, host.ErrDigestMismatch)
	}

	call := fmt.Sprintf("recreate %s %s start=%t", name, digest, start)
	if ch.Image != "" || ch.SetCommand || ch.SetBase || ch.Group != "" {
		call += " image=" + image
		if ch.SetCommand {
			call += " command=" + strings.Join(ch.Command, " ")
		}
	}

	err := h.begin(ctx, "recreate:"+name, call)

	h.mu.Lock()
	defer h.mu.Unlock()

	if c = h.find(name); c == nil {
		return fmt.Errorf("agenttest: recreate %s: no such container", name)
	}

	if err != nil {
		c.State = protocol.StateExited

		return fmt.Errorf("agenttest: recreate %s: %w", name, err)
	}

	h.created++
	c.ID = fmt.Sprintf("%s-%d", name, h.created)
	c.Image = image

	if ch.SetCommand {
		c.Command = slices.Clone(ch.Command)
	}

	// The override labels, read back as host reads them.
	if ch.SetBase {
		c.Base = cloneBase(ch.Base)
	}

	if ch.Group != "" {
		c.Group = ch.Group
		c.Kind = ch.Kind
	}

	c.Digest = local.Digest
	c.ImageID = imageID(local.Digest)
	c.Revision = local.Revision
	c.RestartCount = 0
	c.ExitCode = 0
	c.State = stateCreated
	c.StartedAt = time.Time{}

	if start {
		c.State = protocol.StateRunning
		c.StartedAt = time.Now()
	}

	return nil
}

// Stop implements host.Host (op "stop:<name>").
func (h *Host) Stop(ctx context.Context, name string, _ time.Duration) error {
	if err := h.begin(ctx, "stop:"+name, "stop "+name); err != nil {
		return fmt.Errorf("agenttest: stop %s: %w", name, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("agenttest: stop %s: no such container", name)
	}

	if c.State == protocol.StateRunning {
		c.State = protocol.StateExited
		c.ExitCode = stopExitCode
	}

	return nil
}

// Start implements host.Host (op "start:<name>").
func (h *Host) Start(ctx context.Context, name string) error {
	if err := h.begin(ctx, "start:"+name, "start "+name); err != nil {
		return fmt.Errorf("agenttest: start %s: %w", name, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("agenttest: start %s: no such container", name)
	}

	if c.State != protocol.StateRunning {
		c.State = protocol.StateRunning
		c.ExitCode = 0
		c.StartedAt = time.Now()
	}

	return nil
}

// System implements host.Host (op "status").
func (h *Host) System(ctx context.Context) (host.System, error) {
	if err := h.begin(ctx, "status", ""); err != nil {
		return host.System{}, fmt.Errorf("agenttest: status: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	inv, err := h.inventoryLocked()
	if err != nil {
		return host.System{}, err
	}

	sys := host.System{
		BootID: h.bootIDLocked(),
		Uptime: time.Since(h.bootedAt),
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
		sys.Pending.Upgradable++

		if u.Security {
			sys.Pending.Security++
		}

		if u.Blocked {
			sys.Pending.Blocked++
			sys.Blocked = append(sys.Blocked, protocol.PackageChange{
				Name: u.Name, From: u.Current, To: u.Candidate,
			})
		}
	}

	return sys, nil
}

// Inventory implements host.Host (op "inventory").
func (h *Host) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	if err := h.begin(ctx, "inventory", ""); err != nil {
		return nil, fmt.Errorf("agenttest: inventory: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.inventoryLocked()
}

func (h *Host) inventoryLocked() (*protocol.Inventory, error) {
	inv := &protocol.Inventory{
		CollectedAt: time.Now().UTC(),
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
		return nil, fmt.Errorf("agenttest: %w", err)
	}

	inv.Hash = hash

	return inv, nil
}

// UpgradePackages implements host.Host (op "upgrade"); a name outside the
// blocked set is refused before the call is recorded. Upgrading docker-ce
// restarts the daemon and with it the running containers.
func (h *Host) UpgradePackages(ctx context.Context, names []string) error {
	h.mu.Lock()
	blocked := h.blocked
	h.mu.Unlock()

	for _, name := range names {
		if !slices.Contains(blocked, name) {
			return fmt.Errorf("agenttest: upgrade packages: %q is not in the blocked set", name)
		}
	}

	sorted := slices.Sorted(slices.Values(names))

	if err := h.begin(ctx, "upgrade", "upgrade "+strings.Join(sorted, ",")); err != nil {
		return fmt.Errorf("agenttest: upgrade packages: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

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

	if slices.Contains(names, docker) {
		now := time.Now()

		for _, c := range h.containers {
			if c.State == protocol.StateRunning {
				c.StartedAt = now
			}
		}
	}

	return nil
}

// Reboot implements host.Host by rebooting in place (unless SetInertReboot):
// new boot id, newest kernel, flag cleared. Running containers restart with
// their restart count kept, as Docker keeps it across a daemon restart;
// stopped ones stay stopped (unless-stopped).
func (h *Host) Reboot(ctx context.Context) error {
	if err := h.begin(ctx, "reboot", "reboot"); err != nil {
		return fmt.Errorf("agenttest: reboot: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.inertReboot {
		return nil
	}

	h.boots++
	h.bootedAt = time.Now()
	h.runningKernel = h.newestKernel
	h.rebootRequired = false

	for _, c := range h.containers {
		if c.State == protocol.StateRunning {
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

// Probe implements host.Host: no node health.
func (h *Host) Probe(context.Context) protocol.NodeHealth {
	return protocol.NodeHealth{}
}

// Validators implements host.Host with host.ReadValidators.
func (h *Host) Validators(context.Context) *protocol.Validators {
	if h.validators == nil {
		return nil
	}

	return host.ReadValidators(h.validators)
}

// Close implements host.Host.
func (h *Host) Close() error {
	return nil
}

// Resolve looks a tag up in the registry (op "resolve:<ref>"), as the
// controller does: it is agenttest.Controller's Lookup. Every lookup counts
// in Resolves except one failed by injection.
func (h *Host) Resolve(ctx context.Context, ref string) (string, error) {
	if err := h.begin(ctx, "resolve:"+ref, ""); err != nil {
		return "", fmt.Errorf("agenttest: resolve %s: %w", ref, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.resolves++

	tag, ok := h.registry[imageref.Normalize(ref)]
	if !ok {
		return "", fmt.Errorf("agenttest: resolve %s: manifest unknown", ref)
	}

	return tag.Digest, nil
}

// PullFrom makes every pull take the tag's digest from resolve, a registry
// outside the host, instead of the host's own.
func (h *Host) PullFrom(resolve func(ctx context.Context, ref string) (string, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.remote = resolve
}

// SetRegistry points a tag at a digest in the registry.
func (h *Host) SetRegistry(ref, digest, revision string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.registry[imageref.Normalize(ref)] = Tag{Digest: digest, Revision: revision}
}

// SetUpgradable sets the upgradable packages.
func (h *Host) SetUpgradable(list []protocol.Upgradable) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.setUpgradableLocked(list)
}

func (h *Host) setUpgradableLocked(list []protocol.Upgradable) {
	h.upgradable = slices.Clone(list)

	for i := range h.upgradable {
		h.upgradable[i].Blocked = slices.Contains(h.blocked, h.upgradable[i].Name)
	}
}

// SetRebootRequired sets the reboot-required flag.
func (h *Host) SetRebootRequired(required bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.rebootRequired = required
}

// SetContainerImage swaps what a container runs, as a person with docker would.
func (h *Host) SetContainerImage(name, image, digest string) error {
	return h.change(name, func(c *protocol.Container) {
		c.Image, c.Digest, c.ImageID = image, digest, imageID(digest)
	})
}

// Crash makes a running container restart once (its restart count grows).
func (h *Host) Crash(name string) error {
	return h.change(name, func(c *protocol.Container) {
		c.RestartCount++
		c.StartedAt = time.Now()
	})
}

// Exit stops a container by itself with an exit code.
func (h *Host) Exit(name string, code int) error {
	return h.change(name, func(c *protocol.Container) {
		c.State = protocol.StateExited
		c.ExitCode = code
	})
}

// change applies fn to a container under the lock; it records no call.
func (h *Host) change(name string, fn func(c *protocol.Container)) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	c := h.find(name)
	if c == nil {
		return fmt.Errorf("agenttest: no such container %q", name)
	}

	fn(c)

	return nil
}

// SetInertReboot makes Reboot succeed without rebooting.
func (h *Host) SetInertReboot(inert bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.inertReboot = inert
}

// Fail makes an operation fail with err until it is cleared with nil. The
// operations are: observe (Containers), status (System), inventory,
// upgrade, reboot, local:<ref>, pull:<ref>, resolve:<ref>,
// recreate:<name>, stop:<name>, start:<name>.
func (h *Host) Fail(op string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err == nil {
		delete(h.failures, op)

		return
	}

	h.failures[op] = err
}

// OnCall sets a hook run, without the lock, at the start of every call that
// changes the host, with its Calls text; it may block to park the call.
func (h *Host) OnCall(hook func(call string)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.onCall = hook
}

// Calls returns the calls that changed the host, in order, recorded when
// they begin: "pull <ref>", "recreate <name> <digest> start=<bool>",
// "stop <name>", "start <name>", "upgrade <a,b>", "reboot".
func (h *Host) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

// Resolves counts the registry lookups.
func (h *Host) Resolves() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.resolves
}

// BootID is the current boot id.
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

	out := clone(c)

	return &out
}

// BlockIn parks the first call whose Calls text starts with prefix until
// release; entered closes once it is parked. A failed test or parkLimit
// releases it, so the code under test can still be stopped. It uses OnCall.
func (h *Host) BlockIn(t *testing.T, prefix string) (entered <-chan struct{}, release func()) {
	t.Helper()

	var (
		parked atomic.Bool
		opened atomic.Bool
		in     = make(chan struct{})
		out    = make(chan struct{})
	)

	h.OnCall(func(call string) {
		if !strings.HasPrefix(call, prefix) || !parked.CompareAndSwap(false, true) {
			return
		}

		close(in)

		limit := time.After(parkLimit)
		tick := time.NewTicker(parkPoll)

		defer tick.Stop()

		for {
			select {
			case <-out:
				return
			case <-limit:
				return
			case <-tick.C:
				if t.Failed() {
					return
				}
			}
		}
	})

	release = func() {
		if opened.CompareAndSwap(false, true) {
			close(out)
		}
	}

	t.Cleanup(release)

	return in, release
}

// AwaitParked waits until the call BlockIn parks has begun.
func AwaitParked(t *testing.T, entered <-chan struct{}, call string) {
	t.Helper()

	select {
	case <-entered:
	case <-time.After(AwaitTimeout):
		require.FailNow(t, "the code under test never began the call: "+call)
	}
}

// Disruptive filters calls down to those that change containers or the system.
func Disruptive(calls []string) []string {
	out := make([]string, 0, len(calls))

	for _, c := range calls {
		if !strings.HasPrefix(c, "pull ") {
			out = append(out, c)
		}
	}

	return out
}

// CallsAfter returns the calls that follow the first one with the prefix.
func CallsAfter(calls []string, prefix string) []string {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return calls[i+1:]
		}
	}

	return nil
}

// NotePull records how a lease's pull went; PullNotes returns the records.
func (h *Host) NotePull(ref string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	note := "pulled " + ref
	if err != nil {
		note = "failed " + ref
	}

	h.notes = append(h.notes, note)
}

// PullNotes returns "pulled <ref>" or "failed <ref>" for every NotePull.
func (h *Host) PullNotes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.notes)
}
