package local

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// records reads the records of an audit log file.
func records(t *testing.T, file string) []Record {
	t.Helper()

	f, err := os.Open(file)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	var out []Record

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec Record
		require.NoError(t, json.Unmarshal(sc.Bytes(), &rec), "every line is one record")

		out = append(out, rec)
	}

	return out
}

func TestAuditLogRotates(t *testing.T) {
	dir := t.TempDir()
	cfg := AuditConfig{File: filepath.Join(dir, "sub", "audit.log"), MaxSizeBytes: 400, MaxFiles: 2}
	a := NewAudit(quiet(), &cfg, func() time.Time { return fileTime })

	for i := range 40 {
		a.Record(&Record{
			Action: "override.set", Source: SourceController, User: "sam@example.org",
			Teams: []string{"geth"}, Feature: "team_image_override", Container: "geth",
			Before: "nothing", After: "image ethpandaops/geth:fix", Result: Accepted,
			Reason: string(rune('a' + i%26)),
		})
	}

	entries, err := os.ReadDir(filepath.Dir(cfg.File))
	require.NoError(t, err)

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())

		info, infoErr := e.Info()
		require.NoError(t, infoErr)
		assert.LessOrEqual(t, info.Size(), cfg.MaxSizeBytes, e.Name())
		assert.Equal(t, os.FileMode(auditFileMode), info.Mode().Perm())
	}

	assert.Equal(t, []string{"audit.log", "audit.log.1", "audit.log.2"}, names,
		"the file and the two rotated ones, nothing older")

	recs := records(t, cfg.File)
	last := recs[len(recs)-1]
	assert.Equal(t, "n", last.Reason, "the newest record is in the file itself")
	assert.Equal(t, fileTime, last.Time)
	assert.Equal(t, "sam@example.org", last.User)
}

// Text is cut to one line before it is written to the audit log.
func TestAuditRecordIsOneLine(t *testing.T) {
	cfg := AuditConfig{File: filepath.Join(t.TempDir(), "audit.log"), MaxSizeBytes: 1 << 20, MaxFiles: 1}
	a := NewAudit(quiet(), &cfg, func() time.Time { return fileTime })

	rec := &Record{
		Action: "hold.set", Source: SourceController, User: "sam\nx", Teams: []string{"t\ny"},
		Feature: "f\nz", Reason: "why\n{\"action\":\"forged\"}", Error: "e\nf", Result: Accepted,
	}
	a.Record(rec)
	assert.Equal(t, "sam\nx", rec.User, "the caller's record is not changed")

	assert.Equal(t, []Record{{
		Time: fileTime, Action: "hold.set", Source: SourceController, User: "sam", Teams: []string{"t"},
		Feature: "f", Reason: "why", Error: "e", Result: Accepted,
	}}, records(t, cfg.File))
}

func TestLeaseResult(t *testing.T) {
	cfg := AuditConfig{File: filepath.Join(t.TempDir(), "audit.log"), MaxSizeBytes: 1 << 20, MaxFiles: 1}
	a := NewAudit(quiet(), &cfg, func() time.Time { return fileTime })
	done := &protocol.Step{Status: protocol.StepDone}

	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedOverride, Override: &protocol.OverrideNeed{
		Container: "geth", Image: "a/geth:fix", FromImage: "a/geth:1", FromDigest: "sha256:1",
		ToDigest: "sha256:2", Flags: true,
	}}, done)
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedOverride, Override: &protocol.OverrideNeed{
		Container: "geth", Image: "a/geth:1", FromDigest: "sha256:2", ToDigest: "sha256:1", Restore: true,
	}}, &protocol.Step{Status: protocol.StepFailed, Error: "boom"})
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedSpec, Spec: &protocol.SpecNeed{
		Container: "beacon", Image: "a/b:2", FromImage: "a/b:1", FromDigest: "sha256:3", ToDigest: "sha256:4",
	}}, done)
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedImage, Local: true, Image: &protocol.ImageNeed{
		Container: "geth", Image: "a/geth:1", FromDigest: "sha256:1", ToDigest: "sha256:5",
	}}, done)
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedReboot, DirectiveID: "d1"}, done)
	// What the controller rolls out by itself is not recorded.
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: "geth"}}, done)
	a.LeaseResult("l1", &protocol.Need{Kind: protocol.NeedReboot}, done)

	base := Record{Time: fileTime, Source: SourceAgent, Lease: "l1", Result: string(protocol.StepDone)}
	want := []Record{base, base, base, base, base}
	want[0].Action, want[0].Container = "override.apply", "geth"
	want[0].Before, want[0].After = "a/geth:1@sha256:1", "a/geth:fix@sha256:2, command line changed"
	want[1].Action, want[1].Container, want[1].Result, want[1].Error = "override.restore", "geth", "failed", "boom"
	want[1].Before, want[1].After = "a/geth:1@sha256:2", "a/geth:1@sha256:1"
	want[2].Action, want[2].Container = "spec.apply", "beacon"
	want[2].Before, want[2].After = "a/b:1@sha256:3", "a/b:2@sha256:4"
	want[3].Action, want[3].Container = "roll.result", "geth"
	want[3].Before, want[3].After = "a/geth:1@sha256:1", "a/geth:1@sha256:5"
	want[4].Action, want[4].Directive = "reboot.result", "d1"

	assert.Equal(t, want, records(t, cfg.File))
}

func TestAuditNeverStopsTheAgent(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))

	cfg := AuditConfig{File: filepath.Join(blocked, "audit.log"), MaxSizeBytes: 1 << 20, MaxFiles: 1}
	NewAudit(quiet(), &cfg, time.Now).Record(&Record{Action: "hold.set"})
}
