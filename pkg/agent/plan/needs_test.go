package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

var allVerbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot}

func needsPolicy(verbs ...protocol.Verb) *Policy {
	return &Policy{
		Verbs:    verbs,
		Packages: Packages{Blocked: []string{"docker-ce", "containerd.io"}},
	}
}

// known is a host whose overrides file reads and says nothing.
func known() *Overrides { return &Overrides{Known: true} }

// hostFacts is a host on which geth has a pulled new image, teku is up to
// date, dora is unlabelled, docker-ce and an unblocked package can
// be upgraded and both reboot reasons apply.
func hostFacts() *Facts {
	return &Facts{
		Fresh: true,
		Containers: []protocol.Container{
			{
				Name: "geth", Image: "ethpandaops/geth:master", Digest: "sha256:a", State: protocol.StateRunning,
				Kind: protocol.KindEL, Team: "geth",
			},
			{
				Name: "teku", Image: "consensys/teku:develop", Digest: "sha256:t", State: protocol.StateRunning,
				Kind: protocol.KindCL, Team: "teku",
			},
			{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:d", State: protocol.StateRunning},
		},
		System: &System{
			OS: protocol.OSSummary{RunningKernel: "6.12.41", NewestKernel: "6.12.48", RebootRequired: true},
			Blocked: []protocol.PackageChange{
				{Name: "docker-ce", From: "1", To: "2"},
				// Reported by the system but not in the configured set.
				{Name: "openssl", From: "1", To: "2"},
				{Name: "containerd.io", From: "3", To: "4"},
			},
		},
	}
}

func hostChecks() map[string]protocol.RegistryCheck {
	return map[string]protocol.RegistryCheck{
		"docker.io/ethpandaops/geth:master": {Digest: "sha256:b", Revision: "r2", Pulled: true},
		"docker.io/consensys/teku:develop":  {Digest: "sha256:t", Pulled: true},
		"docker.io/ethpandaops/dora:latest": {Digest: "sha256:e", Pulled: true},
	}
}

// observed is the view of hostFacts with an empty overrides file.
func observed(p *Policy) *View {
	v := Classify(p, hostFacts(), known(), hostChecks(), testNow)

	return &v
}

// notPulled makes the view say that no checked image is local.
func notPulled(v *View) {
	for key, rc := range v.Checks {
		rc.Pulled = false
		v.Checks[key] = rc
	}

	for i := range v.Containers {
		if r := v.Containers[i].Registry; r != nil {
			r.Pulled = false
		}
	}
}

func ids(needs []protocol.Need) []string {
	out := make([]string, 0, len(needs))
	for _, n := range needs {
		out = append(out, n.ID)
	}

	return out
}

// pullFailed makes the view say that a lease could not pull any checked image.
func pullFailed(v *View) {
	for key, rc := range v.Checks {
		rc.Pulled, rc.PullFailed, rc.Error = false, true, "pull: registry is down"
		v.Checks[key] = rc
	}
}

// A need whose image a lease could not pull says why, for its digest only.
func TestNeedsSayWhyTheirPullFailed(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll)
	v := observed(p)

	notPulled(v)

	got := Needs(p, v, nil)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].PullError, "not local is not a failure")

	pullFailed(v)

	got = Needs(p, v, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "pull: registry is down", got[0].PullError)

	rc := v.Checks["docker.io/ethpandaops/geth:master"]
	rc.Digest = "sha256:other"
	v.Checks["docker.io/ethpandaops/geth:master"] = rc
	assert.Empty(t, Needs(p, v, nil)[0].PullError, "the failure was of another digest")
}

func TestNeeds(t *testing.T) {
	pkgs := []protocol.PackageChange{
		{Name: "containerd.io", From: "3", To: "4"},
		{Name: "docker-ce", From: "1", To: "2"},
	}
	imageID := protocol.ImageNeedID("geth", "sha256:b")
	packagesID := protocol.PackagesNeedID(pkgs)

	manualRoll := protocol.Need{
		ID: "manual/d-roll", Kind: protocol.NeedImage, DirectiveID: "d-roll",
		Image: &protocol.ImageNeed{Container: "geth", Image: "ethpandaops/geth:master", ToDigest: "sha256:b"},
	}
	manualReboot := protocol.Need{
		ID: "manual/d-reboot", Kind: protocol.NeedReboot, DirectiveID: "d-reboot",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}

	tests := []struct {
		name   string
		verbs  []protocol.Verb
		mutate func(v *View)
		manual []protocol.Need
		want   []string
	}{
		{name: "everything observed", verbs: allVerbs, want: []string{imageID, packagesID, protocol.RebootNeedID}},
		{
			name: "a local hold hides no need", verbs: allVerbs,
			mutate: func(v *View) { v.Hold.Local = true },
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{"manual/d-reboot", "manual/d-roll", packagesID},
		},
		{
			name: "a controller hold neither", verbs: allVerbs,
			mutate: func(v *View) { v.Hold = staffHold },
			want:   []string{imageID, packagesID, protocol.RebootNeedID},
		},
		{name: "only the allowed verbs", verbs: []protocol.Verb{protocol.VerbPackages}, want: []string{packagesID}},
		{
			name:   "no verbs, no needs, not even manual ones",
			manual: []protocol.Need{manualRoll, manualReboot}, want: []string{},
		},
		{name: "image not pulled yet", verbs: []protocol.Verb{protocol.VerbRoll}, mutate: notPulled, want: []string{imageID}},
		{
			name: "registry never answered", verbs: []protocol.Verb{protocol.VerbRoll},
			mutate: func(v *View) { v.Containers[0].Registry = nil }, want: []string{},
		},
		{
			name: "kernel only", verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(v *View) { v.System.OS.RebootRequired = false },
			want:   []string{protocol.RebootNeedID},
		},
		{
			name: "no reboot reason", verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(v *View) {
				v.System.OS.RebootRequired = false
				v.System.OS.NewestKernel = v.System.OS.RunningKernel
			},
			want: []string{},
		},
		{name: "system state unknown", verbs: allVerbs, mutate: func(v *View) { v.System = nil }, want: []string{imageID}},
		{
			name: "a manual need replaces the observed one for the same thing", verbs: allVerbs,
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{"manual/d-reboot", "manual/d-roll", packagesID},
		},
		{
			name: "a malformed manual need is not reported", verbs: allVerbs,
			manual: []protocol.Need{{ID: "manual/x", Kind: protocol.NeedImage}},
			want:   []string{imageID, packagesID, protocol.RebootNeedID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := needsPolicy(tt.verbs...)
			v := observed(p)

			if tt.mutate != nil {
				tt.mutate(v)
			}

			got := Needs(p, v, tt.manual)
			require.NotNil(t, got, "no needs is an empty list, not null")
			assert.Equal(t, tt.want, ids(got))
		})
	}

	// The content of the observed needs.
	p := needsPolicy(allVerbs...)
	got := Needs(p, observed(p), nil)
	require.Len(t, got, 3)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Kind: protocol.KindEL, Team: "geth", Image: "ethpandaops/geth:master",
		FromDigest: "sha256:a", ToDigest: "sha256:b", ToRevision: "r2",
	}, got[0].Image)
	assert.Equal(t, pkgs, got[1].Packages.Packages, "sorted, and only the configured blocked set")
	assert.Equal(t, []string{protocol.RebootReasonFlag, protocol.RebootReasonKernel}, got[2].Reboot.Reasons)

	// A manual need is reported as a copy.
	manual := []protocol.Need{manualRoll}
	got = Needs(p, observed(p), manual)
	got[0].Image.ToDigest = "changed"
	assert.Equal(t, "sha256:b", manual[0].Image.ToDigest)
}

// A container off its target gets a spec or override need, never an image need.
func TestNeedsOfTargets(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll)

	spec := func(own string) protocol.Need {
		facts := hostFacts()
		facts.Containers[0].Command = []string{"--http", own}
		files := known()
		files.Deployed = map[string]Deployed{"geth": {
			Image: "ethpandaops/geth:v2", Command: []string{"--http", own, "--new"}, HasCommand: true,
		}}
		checks := hostChecks()
		checks["docker.io/ethpandaops/geth:v2"] = protocol.RegistryCheck{Digest: "sha256:v2"}

		v := Classify(p, facts, files, checks, testNow)
		got := Needs(p, &v, nil)
		require.Len(t, got, 1)

		return got[0]
	}

	n := spec("--nat=extip:10.0.0.1")
	assert.Equal(t, protocol.NeedSpec, n.Kind)
	assert.Equal(t, n.ID, spec("--nat=extip:10.0.0.2").ID, "the same change on another host")
	assert.Equal(t, &protocol.SpecNeed{
		Container: "geth", Kind: protocol.KindEL, Team: "geth", Key: n.Spec.Key,
		Image: "ethpandaops/geth:v2", FromImage: "ethpandaops/geth:master", FromDigest: "sha256:a",
		ToDigest: "sha256:v2", Flags: true,
	}, n.Spec)

	// An override.
	files := known()
	files.Entries = map[string]Entry{"geth": {Image: "ethpandaops/geth:fix", By: "sam"}}
	checks := hostChecks()

	v := Classify(p, hostFacts(), files, checks, testNow)
	assert.Empty(t, Needs(p, &v, nil), "waits for the registry to say where the tag points")

	checks["docker.io/ethpandaops/geth:fix"] = protocol.RegistryCheck{Digest: "sha256:f", Pulled: true}
	v = Classify(p, hostFacts(), files, checks, testNow)
	got := Needs(p, &v, nil)
	require.Len(t, got, 1)
	assert.Equal(t, protocol.NeedOverride, got[0].Kind)
	assert.True(t, got[0].Local)
	assert.Equal(t, &protocol.OverrideNeed{
		Container: "geth", Image: "ethpandaops/geth:fix", FromImage: "ethpandaops/geth:master",
		FromDigest: "sha256:a", ToDigest: "sha256:f",
	}, got[0].Override)

	// A container the recreate refuses gets no need, its target or not.
	for _, notRolled := range []func(c *protocol.Container){
		func(c *protocol.Container) { c.Linked = true },
		func(c *protocol.Container) { c.Image = "ethpandaops/geth@sha256:a" },
	} {
		facts := hostFacts()
		notRolled(&facts.Containers[0])

		v = Classify(p, facts, files, checks, testNow)
		require.NotEmpty(t, v.Container("geth").NotRolled)
		assert.Empty(t, Needs(p, &v, nil))
	}
}

func TestReconcileManualNeeds(t *testing.T) {
	p := needsPolicy(allVerbs...)

	roll := func(container, image, to string) protocol.Need {
		return protocol.Need{
			ID: "manual/" + container, Kind: protocol.NeedImage, DirectiveID: container,
			Image: &protocol.ImageNeed{Container: container, Image: image, FromDigest: "sha256:old", ToDigest: to},
		}
	}
	reboot := protocol.Need{
		ID: "manual/reboot", Kind: protocol.NeedReboot, DirectiveID: "reboot",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}

	t.Run("a manual roll follows its tag", func(t *testing.T) {
		manual := []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a"), reboot}

		got := Reconcile(p, observed(p), manual)
		assert.True(t, got.Changed)
		assert.Empty(t, got.Dropped)
		require.Len(t, got.Kept, 2)
		assert.Equal(t, "sha256:a", got.Kept[0].Image.FromDigest, "the running digest")
		assert.Equal(t, "sha256:b", got.Kept[0].Image.ToDigest, "the tag's current digest")
		assert.Equal(t, "r2", got.Kept[0].Image.ToRevision)
		assert.Equal(t, "sha256:a", manual[0].Image.ToDigest, "the input is not modified")
	})

	t.Run("nothing to do", func(t *testing.T) {
		manual := []protocol.Need{reboot}

		got := Reconcile(p, observed(p), manual)
		assert.False(t, got.Changed)
		assert.Empty(t, got.Dropped)
		assert.Equal(t, manual, got.Kept)
	})

	t.Run("a tag that is not pulled yet is followed: the need waits for the pull", func(t *testing.T) {
		v := observed(p)
		v.Containers[0].Registry.Pulled = false
		v.Containers[0].Digest = "sha256:old"

		got := Reconcile(p, v, []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")})
		assert.True(t, got.Changed)
		assert.Equal(t, "sha256:b", got.Kept[0].Image.ToDigest)
	})

	drops := []struct {
		name   string
		mutate func(p *Policy, v *View)
		need   protocol.Need
		reason string
	}{
		{
			name:   "container is gone",
			mutate: func(_ *Policy, v *View) { v.Containers = v.Containers[1:] },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "does not exist",
		},
		{
			name:   "container is no longer managed",
			mutate: func(_ *Policy, v *View) { v.Containers[0].Managed = false },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "not managed",
		},
		{
			name:   "container is not managed",
			mutate: func(*Policy, *View) {},
			need:   roll("dora", "ethpandaops/dora:latest", "sha256:e"),
			reason: "is not managed",
		},
		{
			name:   "container moved to another tag",
			mutate: func(_ *Policy, v *View) { v.Containers[0].Image = "ethpandaops/geth:stable" },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "another tag",
		},
		{
			name:   "verb no longer allowed",
			mutate: func(p *Policy, _ *View) { p.Verbs = []protocol.Verb{protocol.VerbRoll} },
			need:   reboot,
			reason: "no longer allowed",
		},
		{
			name:   "malformed",
			mutate: func(*Policy, *View) {},
			need:   protocol.Need{ID: "manual/x", Kind: protocol.NeedImage},
			reason: "malformed",
		},
	}

	for _, tt := range drops {
		t.Run(tt.name, func(t *testing.T) {
			own := needsPolicy(allVerbs...)
			v := observed(own)
			tt.mutate(own, v)

			got := Reconcile(own, v, []protocol.Need{tt.need})
			assert.True(t, got.Changed)
			assert.Empty(t, got.Kept)
			assert.Contains(t, got.Dropped[tt.need.ID], tt.reason)
		})
	}

	t.Run("a stale view drops nothing", func(t *testing.T) {
		v := observed(p)
		v.Containers = nil
		v.Fresh = false

		manual := []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")}

		got := Reconcile(p, v, manual)
		assert.False(t, got.Changed)
		assert.Empty(t, got.Dropped)
		assert.Equal(t, manual, got.Kept)
	})
}

func TestWithdrawn(t *testing.T) {
	manual := []protocol.Need{
		{ID: "manual/a", Kind: protocol.NeedReboot, DirectiveID: "a"},
		{ID: "local/b", Kind: protocol.NeedImage, Local: true},
	}

	assert.Equal(t, []string{"manual/a"}, Withdrawn(manual, "a"))
	assert.Empty(t, Withdrawn(manual, "c"))
	assert.Empty(t, Withdrawn(manual, ""), "a local need has no directive to withdraw it")
}

// A request is good for one attempt: done and failed steps served it.
func TestServed(t *testing.T) {
	assert.Nil(t, Served(nil))
	assert.Equal(t, []string{"a", "b"}, Served(&protocol.Operation{Steps: []protocol.Step{
		{NeedID: "a", Status: protocol.StepDone},
		{NeedID: "b", Status: protocol.StepFailed},
		{NeedID: "c", Status: protocol.StepSuperseded},
		{NeedID: "d", Status: protocol.StepRefused},
		{NeedID: "e", Status: protocol.StepCancelled},
	}}))
}

func TestCloneNeeds(t *testing.T) {
	t.Parallel()

	assert.Nil(t, CloneNeeds(nil))

	in := []protocol.Need{
		{ID: "a", Image: &protocol.ImageNeed{Container: "geth"}},
		{ID: "b", Reboot: &protocol.RebootNeed{Reasons: []string{"flag"}}},
		{ID: "c", Packages: &protocol.PackagesNeed{Packages: []protocol.PackageChange{{Name: "docker-ce"}}}},
	}
	out := CloneNeeds(in)
	require.Equal(t, in, out)

	out[0].Image.Container = "changed"
	out[1].Reboot.Reasons[0] = "changed"
	out[2].Packages.Packages[0].Name = "changed"

	assert.Equal(t, "geth", in[0].Image.Container)
	assert.Equal(t, "flag", in[1].Reboot.Reasons[0])
	assert.Equal(t, "docker-ce", in[2].Packages.Packages[0].Name)
}
