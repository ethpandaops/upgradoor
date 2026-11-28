// Command load runs a fleet of agents on in-memory hosts (agenttest) against a
// local controller with answer signing on, rolls one image out over the
// fleet, and reports what the controller cost: its CPU, the sync latency
// it measured, the growth of its database and the bytes one viewer of the
// event stream received.
//
// The controller and the fake beacon node run as processes of their own (so
// their CPU is theirs); the agents run in this process.
//
//	go run ./test/load -hosts 1000 -duration 4m
//
// Everything it writes goes to -dir (default .tmp/load), which it empties
// first.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	network  = "loadnet"
	gethRepo = "/ethpandaops/geth:master"
	clRepo   = "/sigp/lighthouse:latest"
	gethOld  = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	gethNew  = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	clDigest = "sha256:" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

type options struct {
	hosts        int
	duration     time.Duration
	rolloutAfter time.Duration
	rampUp       time.Duration
	dir          string
}

func main() {
	var o options

	flag.IntVar(&o.hosts, "hosts", 1000, "number of fake agents")
	flag.DurationVar(&o.duration, "duration", 4*time.Minute, "how long to run, ramp-up included")
	flag.DurationVar(&o.rolloutAfter, "rollout-after", 90*time.Second, "when the new geth image appears")
	flag.DurationVar(&o.rampUp, "ramp-up", 20*time.Second, "time over which the agents start")
	flag.StringVar(&o.dir, "dir", ".tmp/load", "work directory (emptied first)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, &o)

	stop()

	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

// world is what a run set up.
type world struct {
	o          *options
	ctlURL     string
	ctl        *exec.Cmd
	beacon     *exec.Cmd
	seed       []byte
	controller string
	hosts      []*agenttest.Host
	log        *logrus.Logger
	// registry is what the controller looks the tags up at; the images are
	// named under its address.
	registry           *agenttest.Registry
	gethImage, clImage string
}

func run(ctx context.Context, o *options) error {
	if err := os.RemoveAll(o.dir); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(o.dir, "s"), 0o755); err != nil {
		return err
	}

	w := &world{o: o, log: logrus.New()}
	defer w.stop()

	if err := w.start(ctx); err != nil {
		return err
	}

	viewer := &streamViewer{url: w.ctlURL + "/api/v1/events"}
	go viewer.run(ctx)

	agents, err := w.startAgents(ctx)
	if err != nil {
		return err
	}

	defer func() {
		for _, a := range agents {
			_ = a.Stop()
		}
	}()

	return w.measure(ctx, viewer)
}

// start builds and starts the fake beacon node and the controller.
func (w *world) start(ctx context.Context) error {
	bin := filepath.Join(w.o.dir, "bin")
	for name, pkg := range map[string]string{
		"upgradoor-controller": "./cmd/upgradoor-controller", "fakebeacon": "./test/e2e/fakebeacon",
	} {
		build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, name), pkg) //nolint:gosec // fixed packages
		build.Stdout, build.Stderr = os.Stdout, os.Stderr

		if err := build.Run(); err != nil {
			return fmt.Errorf("build %s: %w", pkg, err)
		}
	}

	ports, err := freePorts(3)
	if err != nil {
		return err
	}

	beaconAddr, ctlAddr, regAddr := "127.0.0.1:"+ports[0], "127.0.0.1:"+ports[1], "127.0.0.1:"+ports[2]
	w.ctlURL = "http://" + ctlAddr

	w.registry = agenttest.NewRegistry()
	w.gethImage, w.clImage = regAddr+gethRepo, regAddr+clRepo
	w.registry.Set(w.gethImage, gethOld)
	w.registry.Set(w.clImage, clDigest)

	//nolint:gosec // G114: a local test registry that lives as long as the process
	go func() { _ = http.ListenAndServe(regAddr, w.registry) }()

	if w.beacon, err = startProcess(filepath.Join(w.o.dir, "beacon.log"),
		filepath.Join(bin, "fakebeacon"), "-listen", beaconAddr); err != nil {
		return err
	}

	w.seed = []byte(randomHex(32))
	controllerSeed := randomHex(32)
	seedFile := filepath.Join(w.o.dir, "controller.seed")

	if err = os.WriteFile(seedFile, []byte(controllerSeed), 0o600); err != nil { //nolint:gocritic // err is in scope
		return err
	}

	signer, err := identity.DeriveSignerKey(w.seed)
	if err != nil {
		return err
	}

	ctlKey, err := identity.DeriveControllerKey([]byte(controllerSeed))
	if err != nil {
		return err
	}

	w.controller = identity.EncodePublicKey(identity.PublicKey(ctlKey))

	args := make([]string, 0, 32)
	args = append(args, "run")

	for _, set := range []string{
		"server.listen=" + ctlAddr,
		"store.dataDir=" + filepath.Join(w.o.dir, "data"),
		"network.name=" + network,
		"network.beaconEndpoints=http://" + beaconAddr,
		"agents.signerPublicKey=" + identity.EncodePublicKey(identity.PublicKey(signer)),
		"agents.controllerSeedFile=" + seedFile,
		"agents.allowInsecureHttp=true",
		"auth.allowOpenApi=true",
		"images.registryPlainHttp=" + regAddr,
		"images.registryAllowPrivate=true",
		"deployWindow=0s",
		"settingDefaults.lookupInterval=30s",
		"settingDefaults.pauseAfterCanaryImage=false",
	} {
		args = append(args, "--set", set)
	}

	w.ctl, err = startProcess(filepath.Join(w.o.dir, "controller.log"),
		filepath.Join(bin, "upgradoor-controller"), args...)
	if err != nil {
		return err
	}

	return waitUntil(ctx, 30*time.Second, func() bool {
		resp, err := http.Get(w.ctlURL + "/healthz") //nolint:noctx // a local poll
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	})
}

func (w *world) stop() {
	for _, p := range []*exec.Cmd{w.ctl, w.beacon} {
		if p != nil && p.Process != nil {
			_ = p.Process.Signal(syscall.SIGTERM)
			_ = p.Wait()
		}
	}
}

// startAgents starts the agents spread over the ramp-up.
func (w *world) startAgents(ctx context.Context) ([]agent.Service, error) {
	logFile, err := os.Create(filepath.Join(w.o.dir, "agents.log"))
	if err != nil {
		return nil, err
	}

	w.log.SetOutput(logFile)
	w.log.SetLevel(logrus.WarnLevel)

	agents := make([]agent.Service, 0, w.o.hosts)
	w.hosts = make([]*agenttest.Host, 0, w.o.hosts)
	gap := w.o.rampUp / time.Duration(max(w.o.hosts, 1))

	for i := range w.o.hosts {
		svc, host, err := w.newAgent(i)
		if err != nil {
			return agents, err
		}

		if err := svc.Start(ctx); err != nil {
			return agents, fmt.Errorf("start agent %d: %w", i, err)
		}

		agents = append(agents, svc)
		w.hosts = append(w.hosts, host)

		select {
		case <-ctx.Done():
			return agents, ctx.Err()
		case <-time.After(gap):
		}
	}

	return agents, nil
}

func (w *world) newAgent(i int) (agent.Service, *agenttest.Host, error) {
	name := fmt.Sprintf("load-%04d", i)
	dir := filepath.Join(w.o.dir, "hosts", name)

	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		return nil, nil, err
	}

	key, err := identity.GenerateHostKey()
	if err != nil {
		return nil, nil, err
	}

	cert, err := identity.IssueCertificate(w.seed, identity.PublicKey(key), identity.CertificateSpec{
		Network: network, Hostname: name,
	})
	if err != nil {
		return nil, nil, err
	}

	creds, err := identity.NewCredentials(key, cert)
	if err != nil {
		return nil, nil, err
	}

	host, err := agenttest.NewHost(&agenttest.Scenario{
		BootID:        name,
		OS:            agenttest.OS{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: "amd64"},
		RunningKernel: "6.12.57-amd64", NewestKernel: "6.12.57-amd64",
		Containers: []agenttest.Container{
			{Name: "geth", Image: w.gethImage, Digest: gethOld, Kind: "el", Team: "geth"},
			{Name: "beacon", Image: w.clImage, Digest: clDigest, Kind: "cl", Team: "lighthouse"},
		},
	})
	if err != nil {
		return nil, nil, err
	}

	cfg := &agent.Config{
		Controller: link.Config{
			URL: w.ctlURL, AllowInsecure: true, PublicKey: w.controller,
		},
		StateDir: filepath.Join(dir, "state"),
		Local: local.Config{
			OverridesFile: filepath.Join(dir, "home", "overrides.yaml"),
			Socket:        local.Socket{Path: filepath.Join(w.o.dir, "s", strconv.Itoa(i))},
		},
		Policy: plan.Policy{
			Verbs: []protocol.Verb{protocol.VerbRoll},
		},
		Verify: lease.Config{Window: time.Second, Timeout: 30 * time.Second},
	}
	cfg.ApplyDefaults()

	svc, err := agent.New(w.log, cfg, creds, &agent.Deps{Host: host, Version: "load"})

	return svc, host, err
}

// sample is the controller's cost so far.
type sample struct {
	at      time.Time
	cpu     time.Duration
	dbBytes int64
	wal     int64
	stream  int64
	syncs   histogram
}

// measure runs the fleet for the duration, starts the rollout on time and
// reports.
func (w *world) measure(ctx context.Context, viewer *streamViewer) error {
	start := time.Now()
	end := start.Add(w.o.duration - w.o.rampUp)
	rollout := start.Add(w.o.rolloutAfter - w.o.rampUp)
	rolled := false

	var first, prev *sample

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}

		s, err := w.sample(viewer)
		if err != nil {
			return err
		}

		if first == nil {
			// The first sample is after the ramp-up: inventories and first
			// syncs are behind.
			first = s
		}

		interval := 0.0
		if prev != nil {
			interval = 100 * (s.cpu - prev.cpu).Seconds() / s.at.Sub(prev.at).Seconds()
		}

		prev = s

		updated := w.updated()
		fmt.Printf("%s  cpu %6.1fs (%5.1f%%)  db %6.1f MiB  stream %7.1f KiB  syncs %7d  updated %d/%d\n",
			time.Since(start).Truncate(time.Second), s.cpu.Seconds(), interval, float64(s.dbBytes)/(1<<20),
			float64(s.stream)/1024, s.syncs.count, updated, len(w.hosts))

		if !rolled && time.Now().After(rollout) {
			for _, h := range w.hosts {
				h.SetRegistry(w.gethImage, gethNew, "")
			}

			w.registry.Set(w.gethImage, gethNew)

			rolled = true
			rollout = time.Now()

			fmt.Println("new geth image published")
		}
	}

	last, err := w.sample(viewer)
	if err != nil {
		return err
	}

	// What a page load costs now that the fleet is there.
	fresh := &streamViewer{url: viewer.url}
	snapCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	fresh.firstSnapshot(snapCtx)
	cancel()

	report(first, last, w.updated(), len(w.hosts), time.Since(rollout), fresh.snapshot.Load())

	return nil
}

func (w *world) updated() int {
	n := 0

	for _, h := range w.hosts {
		if c := h.Container("geth"); c != nil && c.Digest == gethNew {
			n++
		}
	}

	return n
}

func (w *world) sample(viewer *streamViewer) (*sample, error) {
	s := &sample{at: time.Now(), stream: viewer.bytes.Load()}

	var err error

	if s.cpu, err = processCPU(w.ctl.Process.Pid); err != nil {
		return nil, err
	}

	// The database file itself; the WAL in front of it is checkpointed now
	// and then, and reported on its own.
	db := filepath.Join(w.o.dir, "data", "upgradoor.db")
	if s.dbBytes, err = fileSize(db); err != nil {
		return nil, err
	}

	if s.wal, err = fileSize(db + "-wal"); err != nil {
		return nil, err
	}

	if s.syncs, err = syncHistogram(w.ctlURL + "/metrics"); err != nil {
		return nil, err
	}

	return s, nil
}

func report(first, last *sample, updated, hosts int, sinceRollout time.Duration, snapshot int64) {
	if first == nil {
		first = last
	}

	wall := last.at.Sub(first.at)
	minutes := max(wall.Minutes(), 1.0/60)
	window := last.syncs.minus(&first.syncs)

	fmt.Println()
	fmt.Printf("measured over %s after the ramp-up, %d hosts\n", wall.Truncate(time.Second), hosts)
	fmt.Printf("controller CPU      %.1f%% of a core (%.1fs)\n",
		100*(last.cpu-first.cpu).Seconds()/wall.Seconds(), (last.cpu - first.cpu).Seconds())
	fmt.Printf("syncs               %d (%.1f/s), p50 %s, p99 %s (server side)\n",
		window.count, float64(window.count)/wall.Seconds(), window.quantile(0.5), window.quantile(0.99))
	fmt.Printf("database            %.1f MiB, +%.1f MiB (%.1f KiB/min); WAL now %.1f MiB\n",
		float64(last.dbBytes)/(1<<20), float64(last.dbBytes-first.dbBytes)/(1<<20),
		float64(last.dbBytes-first.dbBytes)/1024/minutes, float64(last.wal)/(1<<20))
	fmt.Printf("stream (one viewer) snapshot %.1f KiB, then %.1f KiB/min\n",
		float64(snapshot)/1024, float64(last.stream-first.stream)/1024/minutes)
	fmt.Printf("rollout             %d of %d hosts on the new image, %s after it was published\n",
		updated, hosts, sinceRollout.Truncate(time.Second))
}

// streamViewer reads the event stream like a page does and counts bytes.
type streamViewer struct {
	url      string
	bytes    atomic.Int64
	snapshot atomic.Int64
}

// firstSnapshot reads the stream until its snapshot has arrived.
func (v *streamViewer) firstSnapshot(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		for ctx.Err() == nil && v.snapshot.Load() == 0 {
			time.Sleep(50 * time.Millisecond)
		}

		cancel()
	}()

	v.run(ctx)
}

func (v *streamViewer) run(ctx context.Context) {
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, http.NoBody)
		if err != nil {
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(time.Second)

			continue
		}

		rd := bufio.NewReader(resp.Body)
		inSnapshot := false

		for {
			line, err := rd.ReadString('\n')
			v.bytes.Add(int64(len(line)))

			switch {
			case strings.HasPrefix(line, "event: snap"):
				inSnapshot = true
			case strings.HasPrefix(line, "event:"):
				inSnapshot = false
			case inSnapshot && strings.HasPrefix(line, "data:"):
				v.snapshot.Store(int64(len(line)))
			}

			if err != nil {
				break
			}
		}

		_ = resp.Body.Close()
	}
}

// histogram is the controller's sync duration histogram.
type histogram struct {
	bounds []float64
	counts []uint64 // cumulative, per bound
	count  uint64
}

func (h *histogram) minus(o *histogram) histogram {
	out := histogram{bounds: h.bounds, counts: make([]uint64, len(h.counts)), count: h.count - o.count}
	for i := range h.counts {
		out.counts[i] = h.counts[i]
		if i < len(o.counts) {
			out.counts[i] -= o.counts[i]
		}
	}

	return out
}

// quantile is the upper bound of the bucket the quantile falls in.
func (h *histogram) quantile(q float64) string {
	want := uint64(math.Ceil(q * float64(h.count)))
	for i, c := range h.counts {
		if c >= want && h.count > 0 {
			return fmt.Sprintf("≤%gms", h.bounds[i]*1000)
		}
	}

	return "n/a"
}

func syncHistogram(url string) (histogram, error) {
	var h histogram

	resp, err := http.Get(url) //nolint:noctx,gosec // the local controller
	if err != nil {
		return h, err
	}

	defer func() { _ = resp.Body.Close() }()

	type bucket struct {
		le    float64
		count uint64
	}

	buckets := make([]bucket, 0, 16)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "upgradoor_agent_request_duration_seconds_") ||
			!strings.Contains(line, `endpoint="sync"`) {
			continue
		}

		fields := strings.Fields(line)
		value, _ := strconv.ParseUint(fields[len(fields)-1], 10, 64)

		switch {
		case strings.Contains(line, "_count"):
			h.count = value
		case strings.Contains(line, `le="+Inf"`), !strings.Contains(line, "_bucket"):
		default:
			le := line[strings.Index(line, `le="`)+4:]
			bound, _ := strconv.ParseFloat(le[:strings.IndexByte(le, '"')], 64)
			buckets = append(buckets, bucket{le: bound, count: value})
		}
	}

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].le < buckets[j].le })

	for _, b := range buckets {
		h.bounds = append(h.bounds, b.le)
		h.counts = append(h.counts, b.count)
	}

	return h, sc.Err()
}

// processCPU is the user and system time of a process so far.
func processCPU(pid int) (time.Duration, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}

	// The fields after the command name, which is in parentheses.
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+2:]))
	if len(fields) < 13 {
		return 0, errors.New("short /proc stat")
	}

	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)

	// USER_HZ is 100 on Linux.
	return time.Duration(utime+stime) * 10 * time.Millisecond, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, err
	default:
		return info.Size(), nil
	}
}

func startProcess(logPath, bin string, args ...string) (*exec.Cmd, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(bin, args...) //nolint:noctx // the binaries this command built
	cmd.Stdout, cmd.Stderr = logFile, logFile

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	return cmd, nil
}

func freePorts(n int) ([]string, error) {
	out := make([]string, 0, n)
	listeners := make([]net.Listener, 0, n)

	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}

		listeners = append(listeners, l)

		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			return nil, errors.New("not a TCP address")
		}

		out = append(out, strconv.Itoa(addr.Port))
	}

	return out, nil
}

func waitUntil(ctx context.Context, limit time.Duration, ok func() bool) error {
	deadline := time.Now().Add(limit)

	for !ok() {
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = io.ReadFull(rand.Reader, b)

	return hex.EncodeToString(b)
}
