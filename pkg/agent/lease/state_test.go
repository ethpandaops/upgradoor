package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func sampleRecord() *Record {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	return &Record{
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
		Deadline: at.Add(time.Hour),
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

// snapshot returns a deep copy of the store's content.
func snapshot(s *store) content {
	s.mu.Lock()
	defer s.mu.Unlock()

	return *s.cur.clone()
}

func manualReboot(directive string) protocol.Need {
	return protocol.Need{
		ID: protocol.ManualNeedID(directive), Kind: protocol.NeedReboot, DirectiveID: directive,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Nil(t, st.current(), "a missing file is a clean start")

	want := content{
		Version: stateVersion,
		Answers: []protocol.DirectiveAck{{ID: "d-1", Status: protocol.DirectiveRejected, Error: "no"}},
		Manual:  []protocol.Need{manualReboot("d-2")},
		Current: sampleRecord(),
	}

	require.NoError(t, st.update(func(c *content) { *c = *want.clone() }))

	reopened, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, want, snapshot(reopened), "everything survives a restart")

	info, err := os.Stat(filepath.Join(dir, StateFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStateSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	st, err := openStore(quietLog(), t.TempDir())
	require.NoError(t, err)
	require.NoError(t, st.update(func(c *content) { c.Current = sampleRecord() }))

	snap := snapshot(st)
	snap.Current.Operation.Steps[0].Status = protocol.StepFailed
	snap.Current.Lease.Needs[0].Image.ToDigest = "changed"
	snap.Current.Lease.Needs[1].Packages.Packages[0].Name = "changed"
	snap.Current.Lease.Needs[2].Reboot.Reasons[0] = "changed"
	snap.Current.WasRunning[0] = "changed"

	_, cur := st.ManualAndCurrent()
	cur.Operation.Steps[0].Status = protocol.StepFailed
	cur.WasRunning[0] = "changed"

	assert.Equal(t, *sampleRecord(), *st.current())
}

// A failed write leaves the old file and no temp file; only apply keeps the change in memory.
func TestStateWriteIsAtomic(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}

	dir := t.TempDir()

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	require.NoError(t, st.update(func(c *content) { c.Current = sampleRecord() }))

	path := filepath.Join(dir, StateFile)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.Error(t, st.update(func(c *content) { c.Current = nil }))
	assert.NotNil(t, st.current(), "memory still agrees with the disk")

	require.Error(t, st.apply(func(c *content) { c.Current.Operation.Phase = protocol.PhaseStarting }))
	assert.Equal(t, protocol.PhaseStarting, st.current().Operation.Phase, "apply keeps the progress in memory")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the old file is untouched")

	require.NoError(t, os.Chmod(dir, 0o700))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file is left behind")

	// The next successful write carries what apply kept.
	require.NoError(t, st.apply(func(*content) {}))

	reopened, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, protocol.PhaseStarting, reopened.current().Operation.Phase)
}

// Readers of the file never see a partial write.
func TestStateFileIsAlwaysComplete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	require.NoError(t, st.update(func(c *content) { c.Current = sampleRecord() }))

	path := filepath.Join(dir, StateFile)
	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}

			assert.NoError(t, st.update(func(c *content) {
				c.Current.Touched = append(c.Current.Touched[:0], fmt.Sprintf("round-%d", i))
			}))
		}
	})

	for range 300 {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		var c content
		require.NoError(t, json.Unmarshal(raw, &c), "state file read in the middle of a write: %q", raw)
		require.NotNil(t, c.Current)
	}

	close(stop)
	wg.Wait()
}

func TestCorruptStateIsMovedAside(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "truncated", content: `{"version":2,"current":{"lease":{"id":"lease-1"`},
		{name: "empty", content: ``},
		{name: "not json", content: "\x00\x00\x00\x00"},
		{name: "wrong type", content: `{"version":2,"current":"yes"}`},
		{name: "older version", content: `{"version":1}`},
		{name: "future version", content: `{"version":99}`},
		{name: "no version", content: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, StateFile)

			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			// A temp file of a write that was cut short is cleaned up.
			require.NoError(t, os.WriteFile(path+".tmp-123", []byte("{"), 0o600))

			log, hook := test.NewNullLogger()

			st, err := openStore(log, dir)
			require.NoError(t, err, "a damaged state file must not keep the agent down")
			assert.Equal(t, content{Version: stateVersion}, snapshot(st))

			kept, err := os.ReadFile(path + corruptSuffix)
			require.NoError(t, err)
			assert.Equal(t, tt.content, string(kept), "the damaged file is kept")
			assert.NoFileExists(t, path)
			assert.NoFileExists(t, path+".tmp-123")

			require.NotEmpty(t, hook.AllEntries())
			assert.Equal(t, logrus.ErrorLevel, hook.LastEntry().Level, "it is logged loudly")
			assert.Contains(t, hook.LastEntry().Message, "CORRUPT")

			require.NoError(t, st.update(func(c *content) { c.Current = sampleRecord() }))

			reopened, err := openStore(quietLog(), dir)
			require.NoError(t, err)
			assert.NotNil(t, reopened.current())
		})
	}
}

// Unknown fields are ignored.
func TestStateDecodingIsLenient(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, StateFile),
		[]byte(`{"version":2,"later":true,"answers":[{"id":"d-1","status":"accepted","extra":1}]}`), 0o600))

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.True(t, st.Answered("d-1"))
}

func TestAnswersAreBounded(t *testing.T) {
	t.Parallel()

	st, err := openStore(quietLog(), t.TempDir())
	require.NoError(t, err)

	ack := func(i int) protocol.DirectiveAck {
		return protocol.DirectiveAck{ID: fmt.Sprintf("d-%d", i), Status: protocol.DirectiveAccepted}
	}

	// A full store, then two more.
	require.NoError(t, st.update(func(c *content) {
		for i := range maxAnswers {
			c.Answers = append(c.Answers, ack(i))
		}
	}))
	require.NoError(t, st.Answer(ack(maxAnswers), nil))
	require.NoError(t, st.Answer(ack(maxAnswers+1), nil))

	// Answering one twice changes nothing.
	require.NoError(t, st.Answer(protocol.DirectiveAck{ID: "d-100", Status: protocol.DirectiveRejected}, nil))

	snap := snapshot(st)
	require.Len(t, snap.Answers, maxAnswers)
	assert.False(t, st.Answered("d-0"), "the oldest are forgotten")
	assert.False(t, st.Answered("d-1"))
	assert.True(t, st.Answered("d-2"))
	assert.Equal(t, []protocol.DirectiveAck{ack(100)}, st.Acks([]string{"d-100"}))
	assert.Equal(t, ack(maxAnswers+1), snap.Answers[len(snap.Answers)-1])
}

func TestManualNeedBookkeeping(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)

	// An answer and its need are one write; a repeated answer adds no need.
	need := manualReboot("d-1")
	require.NoError(t, st.Answer(protocol.DirectiveAck{ID: "d-1", Status: protocol.DirectiveAccepted}, &need))
	require.NoError(t, st.Answer(protocol.DirectiveAck{ID: "d-1", Status: protocol.DirectiveAccepted}, &need))
	require.NoError(t, st.Answer(protocol.DirectiveAck{ID: "d-2", Status: protocol.DirectiveRejected, Error: "no"}, nil))

	local := protocol.Need{
		ID: protocol.LocalNeedID("r-1"), Kind: protocol.NeedImage, Local: true,
		Image: &protocol.ImageNeed{Container: "geth", Image: "geth:x", ToDigest: "sha256:a"},
	}
	require.NoError(t, st.AddManual(&local))

	gone := manualReboot("d-3")
	require.NoError(t, st.AddManual(&gone))

	assert.Equal(t, []protocol.DirectiveAck{
		{ID: "d-2", Status: protocol.DirectiveRejected, Error: "no"},
		{ID: "d-1", Status: protocol.DirectiveAccepted},
	}, st.Acks([]string{"d-2", "unknown", "d-1"}), "in the order asked, unknown ones left out")
	assert.Nil(t, st.Acks([]string{"unknown"}))
	assert.Nil(t, st.Acks(nil))

	both, cur := st.ManualAndCurrent()
	assert.Equal(t, st.Manual(), both)
	assert.Nil(t, cur)

	// Reconciled: one need dropped, one changed, the rest kept.
	moved := local
	moved.Image = &protocol.ImageNeed{Container: "geth", Image: "geth:x", ToDigest: "sha256:b"}
	require.NoError(t, st.ReconcileManual([]protocol.Need{moved}, []string{gone.ID}))

	want := []protocol.Need{need, moved}
	assert.Equal(t, want, st.Manual())

	manual := st.Manual()
	manual[1].Image.ToDigest = "changed"
	assert.Equal(t, want, st.Manual(), "Manual returns a copy")

	reopened, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, want, reopened.Manual())
	assert.True(t, reopened.Answered("d-2"))
}

// Holds are kept in the state file across a restart; a cleared one is gone.
func TestHoldsSurviveReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	st, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Empty(t, st.Holds())

	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	staff := plan.Hold{By: "alice", Reason: "fork", At: until.Add(-time.Hour), Until: &until}
	require.NoError(t, st.SetHold(protocol.HolderStaff, &staff))
	require.NoError(t, st.SetHold("geth", &plan.Hold{By: "bob", Reason: "sync"}))
	require.NoError(t, st.SetHold(protocol.HolderHost, &plan.Hold{By: "ops", Reason: "debugging"}))
	require.NoError(t, st.SetHold(protocol.HolderHost, nil))
	require.NoError(t, st.SetHold("geth", nil))

	holds := st.Holds()
	delete(holds, protocol.HolderStaff)

	reopened, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	assert.Equal(t, map[string]plan.Hold{protocol.HolderStaff: staff}, reopened.Holds(), "one hold per holder")
}

// After the rename memory follows the file, whatever the directory fsync says.
func TestStateFollowsTheFileWhenTheDirectoryCannotBeSynced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log, logs := test.NewNullLogger()

	st, err := openStore(log, dir)
	require.NoError(t, err)

	st.syncDir = func(string) error { return errors.New("input/output error") }

	require.NoError(t, st.update(func(c *content) { c.Current = sampleRecord() }),
		"the change is applied, so no error tells the caller otherwise")
	require.NotNil(t, st.current(), "memory agrees with the file")

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Equal(t, logrus.ErrorLevel, entry.Level)
	assert.Contains(t, entry.Message, "could not be synced")

	require.NoError(t, st.apply(func(c *content) { c.Current.Operation.Phase = protocol.PhaseStarting }))

	reopened, err := openStore(quietLog(), dir)
	require.NoError(t, err)
	require.NotNil(t, reopened.current())
	assert.Equal(t, snapshot(st), snapshot(reopened))
}

func TestReplaceFile(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "f")

	require.NoError(t, replaceFile(file, []byte("one\n"), 0o640))
	require.NoError(t, replaceFile(file, []byte("two\n"), 0o600))
	require.NoError(t, syncDir(filepath.Dir(file)))

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(raw))

	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	left, err := filepath.Glob(file + tempPattern)
	require.NoError(t, err)
	assert.Empty(t, left, "no temp file is left behind")

	require.Error(t, replaceFile(filepath.Join(file, "below-a-file"), nil, 0o600))
}
