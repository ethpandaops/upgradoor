package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
}

func sampleRecord() *operationRecord {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	return &operationRecord{
		Lease: protocol.Lease{
			ID: "lease-1", GrantedAt: at, ExpiresAt: at.Add(time.Hour), TTLSeconds: 3600,
			Needs: []protocol.Need{
				{
					ID: "image/geth/sha256:b", Kind: protocol.NeedImage,
					Image: &protocol.ImageNeed{Container: "geth", Image: "geth:x", ToDigest: "sha256:b"},
				},
				{
					ID: "packages/1", Kind: protocol.NeedPackages,
					Packages: &protocol.PackagesNeed{
						Packages: []protocol.PackageChange{{Name: "docker-ce", From: "1", To: "2"}},
					},
				},
				{
					ID: protocol.RebootNeedID, Kind: protocol.NeedReboot,
					Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonKernel}},
				},
			},
		},
		ReceivedAt: at,
		Deadline:   at.Add(time.Hour),
		Operation: protocol.Operation{
			LeaseID: "lease-1", Phase: protocol.PhaseRebooting, StartedAt: at, UpdatedAt: at,
			Steps:     []protocol.Step{{NeedID: protocol.RebootNeedID, Status: protocol.StepRunning}},
			Disrupted: true,
		},
		Started:        true,
		BootID:         "boot-a",
		WasRunning:     []string{"geth", "teku"},
		Touched:        []string{"geth"},
		RebootIssuedAt: at.Add(time.Minute),
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	store, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Nil(t, store.Snapshot().Current, "a missing file is a clean start")

	want := persistedState{
		Version: stateVersion,
		Handled: []handledDirective{
			{ID: "d-1", Status: protocol.DirectiveRejected, Error: "no", At: time.Unix(1, 0).UTC()},
		},
		ManualNeeds: []protocol.Need{{
			ID: "manual/d-2", Kind: protocol.NeedReboot, DirectiveID: "d-2",
			Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
		}},
		Current: sampleRecord(),
	}

	require.NoError(t, store.Update(func(st *persistedState) { *st = *want.clone() }))

	reopened, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, want, reopened.Snapshot(), "everything survives a restart (or a reboot)")

	info, err := os.Stat(filepath.Join(dir, StateFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStateSnapshotIsACopy(t *testing.T) {
	store, err := openStateStore(quietLog(), t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Update(func(st *persistedState) { st.Current = sampleRecord() }))

	snap := store.Snapshot()
	snap.Current.Operation.Steps[0].Status = protocol.StepFailed
	snap.Current.Lease.Needs[0].Image.ToDigest = "changed"
	snap.Current.Lease.Needs[1].Packages.Packages[0].Name = "changed"
	snap.Current.Lease.Needs[2].Reboot.Reasons[0] = "changed"
	snap.Current.WasRunning[0] = "changed"

	assert.Equal(t, *sampleRecord(), *store.Snapshot().Current)
}

// A write that fails leaves the old file complete and in place, and no
// temp file behind.
func TestStateWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()

	store, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	require.NoError(t, store.Update(func(st *persistedState) { st.Current = sampleRecord() }))

	path := filepath.Join(dir, StateFileName)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// No file can be created in a read-only directory. (Root ignores
	// directory permissions, so the test cannot run as root.)
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}

	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = store.Update(func(st *persistedState) { st.Current = nil })
	require.Error(t, err)
	assert.NotNil(t, store.Snapshot().Current, "memory still agrees with the disk")

	err = store.Apply(func(st *persistedState) { st.Current.Operation.Phase = protocol.PhaseStarting })
	require.Error(t, err)
	assert.Equal(t, protocol.PhaseStarting, store.Snapshot().Current.Operation.Phase,
		"Apply keeps the progress in memory")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the old file is untouched")

	require.NoError(t, os.Chmod(dir, 0o700))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file is left behind")

	// The next successful write carries what Apply kept.
	require.NoError(t, store.Apply(func(*persistedState) {}))

	reopened, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, protocol.PhaseStarting, reopened.Snapshot().Current.Operation.Phase)
}

// Readers of the file never see a partial write, however writes and reads
// interleave.
func TestStateFileIsAlwaysComplete(t *testing.T) {
	dir := t.TempDir()

	store, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	require.NoError(t, store.Update(func(st *persistedState) { st.Current = sampleRecord() }))

	path := filepath.Join(dir, StateFileName)
	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}

			assert.NoError(t, store.Update(func(st *persistedState) {
				st.Current.Touched = append(st.Current.Touched[:0], fmt.Sprintf("round-%d", i))
			}))
		}
	})

	for range 300 {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		var st persistedState
		require.NoError(t, json.Unmarshal(raw, &st), "state file read in the middle of a write: %q", raw)
		require.NotNil(t, st.Current)
	}

	close(stop)
	wg.Wait()
}

func TestCorruptStateIsMovedAside(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "truncated", content: `{"version":1,"current":{"lease":{"id":"lease-1"`},
		{name: "empty", content: ``},
		{name: "not json", content: "\x00\x00\x00\x00"},
		{name: "wrong type", content: `{"version":1,"current":"yes"}`},
		{name: "future version", content: `{"version":99}`},
		{name: "no version", content: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, StateFileName)

			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			// A temp file of a write that was cut short is cleaned up.
			require.NoError(t, os.WriteFile(path+".tmp-123", []byte("{"), 0o600))

			log, hook := test.NewNullLogger()

			store, err := openStateStore(log, dir)
			require.NoError(t, err, "a damaged state file must not keep the agent down")
			assert.Equal(t, persistedState{Version: stateVersion}, store.Snapshot())

			kept, err := os.ReadFile(path + corruptSuffix)
			require.NoError(t, err)
			assert.Equal(t, tt.content, string(kept), "the damaged file is kept")
			assert.NoFileExists(t, path)
			assert.NoFileExists(t, path+".tmp-123")

			require.NotEmpty(t, hook.AllEntries())
			assert.Equal(t, logrus.ErrorLevel, hook.LastEntry().Level, "it is logged loudly")
			assert.Contains(t, hook.LastEntry().Message, "CORRUPT")

			// And the store works from there.
			require.NoError(t, store.Update(func(st *persistedState) { st.Current = sampleRecord() }))

			reopened, err := openStateStore(quietLog(), dir)
			require.NoError(t, err)
			assert.NotNil(t, reopened.Snapshot().Current)
		})
	}
}

func TestHandledDirectivesAreBounded(t *testing.T) {
	var st persistedState

	for i := range maxHandledDirectives + 50 {
		st.rememberDirective(handledDirective{
			ID: fmt.Sprintf("d-%d", i), Status: protocol.DirectiveAccepted,
		})
	}

	// Remembering one twice changes nothing.
	st.rememberDirective(handledDirective{ID: "d-100", Status: protocol.DirectiveRejected})

	require.Len(t, st.Handled, maxHandledDirectives)
	assert.Nil(t, st.handled("d-0"), "the oldest are forgotten")
	assert.Nil(t, st.handled("d-49"))
	require.NotNil(t, st.handled("d-50"))
	require.NotNil(t, st.handled("d-100"))
	assert.Equal(t, protocol.DirectiveAccepted, st.handled("d-100").Status)
	assert.Equal(t, fmt.Sprintf("d-%d", maxHandledDirectives+49), st.Handled[len(st.Handled)-1].ID)
}

func TestLeaseDeadline(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		expires time.Time
		ttl     int64
		want    time.Time
	}{
		{name: "clocks agree", expires: now.Add(30 * time.Minute), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock ahead", expires: now.Add(2 * time.Hour), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock behind", expires: now.Add(-time.Hour), ttl: 1800, want: now.Add(-time.Hour)},
		{name: "restated late", expires: now.Add(10 * time.Minute), ttl: 600, want: now.Add(10 * time.Minute)},
		{name: "no ttl", expires: now.Add(time.Hour), ttl: 0, want: now},
		{name: "negative ttl", expires: now.Add(time.Hour), ttl: -5, want: now},
		{name: "no expiry", ttl: 60, want: now.Add(time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaseDeadline(&protocol.Lease{ExpiresAt: tt.expires, TTLSeconds: tt.ttl}, now)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHolds(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{HoldFile: filepath.Join(dir, "etc", "hold"), StateDir: filepath.Join(dir, "state")}
	h := newHolds(cfg)

	assert.Equal(t, protocol.Hold{}, h.Read())

	// The controller hold is a flag in the state directory.
	require.NoError(t, h.SetController("maintenance\nwindow"))
	assert.Equal(t, protocol.Hold{Controller: true}, h.Read())
	assert.FileExists(t, filepath.Join(cfg.StateDir, controllerHoldFile))

	require.NoError(t, h.SetController(""), "setting it twice is fine")
	assert.True(t, newHolds(cfg).Read().Controller, "it is on disk")

	// The local hold is a file a person creates.
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.HoldFile), 0o700))
	require.NoError(t, os.WriteFile(cfg.HoldFile, []byte("  debugging\tteku  \nmore\n"), 0o600))
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku", Controller: true}, h.Read())

	// Releasing the controller hold leaves the local one alone.
	require.NoError(t, h.ReleaseController())
	require.NoError(t, h.ReleaseController(), "releasing twice is fine")
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku"}, h.Read())
	assert.FileExists(t, cfg.HoldFile)

	// An empty hold file holds too, without a reason.
	require.NoError(t, os.WriteFile(cfg.HoldFile, nil, 0o600))
	assert.Equal(t, protocol.Hold{Local: true}, h.Read())

	// So does anything else at that path: when in doubt, hold.
	require.NoError(t, os.Remove(cfg.HoldFile))
	require.NoError(t, os.Mkdir(cfg.HoldFile, 0o700))
	assert.True(t, h.Read().Local)

	require.NoError(t, os.Remove(cfg.HoldFile))
	assert.False(t, h.Read().Active())

	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}

	require.NoError(t, os.WriteFile(cfg.HoldFile, long, 0o600))
	assert.Len(t, h.Read().LocalReason, maxHoldReason, "the reported reason is bounded")
}

// Once the rename has happened the file holds the new state, whatever the
// fsync of the directory says. Memory has to follow the file: otherwise the
// agent would, for example, not act on a lease that the next start finds on
// disk and executes.
func TestStateFollowsTheFileWhenTheDirectoryCannotBeSynced(t *testing.T) {
	dir := t.TempDir()
	log, logs := test.NewNullLogger()

	store, err := openStateStore(log, dir)
	require.NoError(t, err)

	store.syncDir = func(string) error { return errors.New("input/output error") }

	require.NoError(t, store.Update(func(st *persistedState) { st.Current = sampleRecord() }),
		"the change is applied, so no error tells the caller otherwise")
	require.NotNil(t, store.Snapshot().Current, "memory agrees with the file")

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Equal(t, logrus.ErrorLevel, entry.Level)
	assert.Contains(t, entry.Message, "could not be synced")

	require.NoError(t, store.Apply(func(st *persistedState) {
		st.Current.Operation.Phase = protocol.PhaseStarting
	}))

	reopened, err := openStateStore(quietLog(), dir)
	require.NoError(t, err)
	require.NotNil(t, reopened.Snapshot().Current)
	assert.Equal(t, store.Snapshot(), reopened.Snapshot())
}
