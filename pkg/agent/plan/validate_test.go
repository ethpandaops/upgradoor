package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// leaseOf is a lease that grants needs.
func leaseOf(needs ...protocol.Need) *protocol.Lease {
	return &protocol.Lease{ID: "lease-1", Needs: needs}
}

// leaseView is the view a lease is validated on: no registry checks.
func leaseView(p *Policy, facts *Facts, files *Overrides) *View {
	v := Classify(p, facts, files, nil, testNow)

	return &v
}

func imageNeedFor(container, image, to string) protocol.Need {
	return protocol.Need{
		ID: protocol.ImageNeedID(container, to), Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{Container: container, Image: image, ToDigest: to},
	}
}

// A lease grants a slot; it cannot widen what the agent does.
func TestLeaseCannotWidenWhatTheAgentDoes(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll, protocol.VerbPackages)
	facts := hostFacts()
	facts.Containers[2].Kind = protocol.KindNone // dora is not managed

	needs := []protocol.Need{
		// reboot is not an allowed verb on this host
		{ID: protocol.RebootNeedID, Kind: protocol.NeedReboot, Reboot: &protocol.RebootNeed{Reasons: []string{"flag"}}},
		// dora is not a managed container
		imageNeedFor("dora", "ethpandaops/dora:latest", "sha256:dora-2"),
		// openssl is not in the blocked set
		{
			ID: "packages/widened", Kind: protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: []protocol.PackageChange{
				{Name: "docker-ce", From: "1", To: "2"}, {Name: "openssl", From: "1", To: "2"},
			}},
		},
		// a kind outside the closed verb set
		{ID: "exec/1", Kind: "exec"},
		// a manual need the agent never created
		{
			ID: protocol.ManualNeedID("forged"), Kind: protocol.NeedImage, DirectiveID: "forged",
			Image: &protocol.ImageNeed{Container: "geth", Image: "ethpandaops/geth:master", ToDigest: "sha256:a"},
		},
		// geth already runs this digest
		imageNeedFor("geth", "ethpandaops/geth:master", "sha256:a"),
		// teku is on another tag than the need says
		imageNeedFor("teku", "consensys/teku:latest", "sha256:t2"),
	}

	got, err := Validate(p, leaseOf(needs...), leaseView(p, facts, known()), nil)
	require.NoError(t, err)
	assert.True(t, got.Empty(), "nothing is done")
	assert.Empty(t, got.Decline)

	want := []struct {
		status protocol.StepStatus
		reason string
	}{
		{protocol.StepRefused, "verb reboot is not allowed"},
		{protocol.StepRefused, "dora is not managed"},
		{protocol.StepRefused, `"openssl" is not in the configured blocked set`},
		{protocol.StepRefused, "unknown need kind"},
		{protocol.StepSuperseded, "manual need is no longer open"},
		{protocol.StepSuperseded, "already runs the wanted digest"},
		{protocol.StepSuperseded, "the need was for consensys/teku:latest"},
	}

	require.Len(t, got.Verdicts, len(want))

	for i, w := range want {
		v := got.Verdicts[needs[i].ID]
		assert.Equal(t, w.status, v.Status, needs[i].ID)
		assert.Contains(t, v.Error, w.reason, needs[i].ID)
		assert.Equal(t, needs[i].ID, v.NeedID)
	}
}

// validateFacts adds a validator client and a stopped container of a kind
// without a team to hostFacts.
func validateFacts() *Facts {
	facts := hostFacts()
	facts.Containers = append(facts.Containers,
		protocol.Container{
			Name: "lh-vc", Image: "sigp/lighthouse:latest", Digest: "sha256:v", State: protocol.StateRunning,
			Kind: protocol.KindVC, Team: "lighthouse",
		},
		protocol.Container{
			Name: "exporter", Image: "exporter:1", Digest: "sha256:x", State: protocol.StateExited,
			Kind: "exporter",
		},
	)

	return facts
}

func validatePolicy() *Policy {
	p := needsPolicy(allVerbs...)
	p.DefaultPolicy = DefaultRoll

	return p
}

// overrideFiles puts geth on an image override.
func overrideFiles(image string) *Overrides {
	files := known()
	files.Entries = map[string]Entry{"geth": {Image: image, By: "sam"}}

	return files
}

// targetNeedOf is the need reported for geth's target at digest.
func targetNeedOf(t *testing.T, p *Policy, files *Overrides, ref, digest string) protocol.Need {
	t.Helper()

	checks := map[string]protocol.RegistryCheck{ref: {Digest: digest, Pulled: true}}
	v := Classify(p, validateFacts(), files, checks, testNow)

	for _, n := range Needs(p, &v, nil) {
		if n.Container() == "geth" {
			return n
		}
	}

	require.FailNow(t, "no need for geth")

	return protocol.Need{}
}

func TestValidate(t *testing.T) {
	type verdict struct {
		status protocol.StepStatus
		reason string
	}

	p := validatePolicy()
	override := targetNeedOf(t, p, overrideFiles("ethpandaops/geth:fix"), "docker.io/ethpandaops/geth:fix", "sha256:f")
	require.Equal(t, protocol.NeedOverride, override.Kind)

	specFiles := known()
	specFiles.Deployed = map[string]Deployed{"geth": {Image: "ethpandaops/geth:v2"}}
	spec := targetNeedOf(t, p, specFiles, "docker.io/ethpandaops/geth:v2", "sha256:s")
	require.Equal(t, protocol.NeedSpec, spec.Kind)

	manualRoll := protocol.Need{
		ID: protocol.ManualNeedID("d1"), Kind: protocol.NeedImage, DirectiveID: "d1",
		Image: &protocol.ImageNeed{Container: "geth", Image: "ethpandaops/geth:master", ToDigest: "sha256:a"},
	}
	localRoll := protocol.Need{
		ID: protocol.LocalNeedID("r1"), Kind: protocol.NeedImage, Local: true,
		Image: &protocol.ImageNeed{Container: "teku", Image: "consensys/teku:develop", ToDigest: "sha256:t"},
	}
	manualReboot := protocol.Need{
		ID: protocol.ManualNeedID("d2"), Kind: protocol.NeedReboot, DirectiveID: "d2",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}
	reboot := protocol.Need{
		ID: protocol.RebootNeedID, Kind: protocol.NeedReboot, Reboot: &protocol.RebootNeed{Reasons: []string{"flag"}},
	}
	packages := func(names ...string) protocol.Need {
		pkgs := make([]protocol.PackageChange, 0, len(names))
		for _, n := range names {
			pkgs = append(pkgs, protocol.PackageChange{Name: n})
		}

		return protocol.Need{
			ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		}
	}
	gethImage := imageNeedFor("geth", "ethpandaops/geth:master", "sha256:b")
	noReboot := func(f *Facts, _ *Overrides) {
		f.System.OS.RebootRequired = false
		f.System.OS.NewestKernel = f.System.OS.RunningKernel
	}

	for _, tc := range []struct {
		name       string
		verbs      []protocol.Verb
		mutate     func(f *Facts, files *Overrides)
		files      *Overrides
		needs      []protocol.Need
		manual     []protocol.Need
		verdicts   map[string]verdict
		recreates  []Recreate
		reboot     []string
		packages   []string
		wasRunning []string
	}{
		{
			name: "an image need", needs: []protocol.Need{gethImage},
			recreates:  []Recreate{{IDs: []string{gethImage.ID}, Container: "geth", Digest: "sha256:b"}},
			wasRunning: []string{"geth", "lh-vc", "teku"},
		},
		{
			name: "recreates in start order, unlabelled running only when rolled",
			needs: []protocol.Need{
				imageNeedFor("dora", "ethpandaops/dora:latest", "sha256:e"),
				imageNeedFor("lh-vc", "sigp/lighthouse:latest", "sha256:v2"),
				imageNeedFor("teku", "consensys/teku:develop", "sha256:t2"),
				gethImage,
			},
			recreates: []Recreate{
				{IDs: []string{gethImage.ID}, Container: "geth", Digest: "sha256:b"},
				{IDs: []string{protocol.ImageNeedID("teku", "sha256:t2")}, Container: "teku", Digest: "sha256:t2"},
				{IDs: []string{protocol.ImageNeedID("lh-vc", "sha256:v2")}, Container: "lh-vc", Digest: "sha256:v2"},
				{IDs: []string{protocol.ImageNeedID("dora", "sha256:e")}, Container: "dora", Digest: "sha256:e"},
			},
			wasRunning: []string{"dora", "geth", "lh-vc", "teku"},
		},
		{
			name: "a manual roll is served at the running digest", needs: []protocol.Need{manualRoll, localRoll},
			manual: []protocol.Need{manualRoll, localRoll},
			recreates: []Recreate{
				{IDs: []string{manualRoll.ID}, Container: "geth", Digest: "sha256:a"},
				{IDs: []string{localRoll.ID}, Container: "teku", Digest: "sha256:t"},
			},
		},
		{
			name: "a manual need the agent does not hold", needs: []protocol.Need{manualRoll, localRoll},
			manual: []protocol.Need{{ID: manualRoll.ID, Kind: protocol.NeedImage, DirectiveID: "other"}},
			verdicts: map[string]verdict{
				manualRoll.ID: {protocol.StepSuperseded, "the manual need is no longer open"},
				localRoll.ID:  {protocol.StepSuperseded, "the manual need is no longer open"},
			},
		},
		{
			name: "a second need of a container at the same digest joins, at another one is superseded",
			needs: []protocol.Need{
				gethImage, {ID: "image/again", Kind: protocol.NeedImage, Image: gethImage.Image},
				imageNeedFor("geth", "ethpandaops/geth:master", "sha256:c"),
			},
			recreates: []Recreate{{IDs: []string{gethImage.ID, "image/again"}, Container: "geth", Digest: "sha256:b"}},
			verdicts: map[string]verdict{
				protocol.ImageNeedID("geth", "sha256:c"): {protocol.StepSuperseded, "another need of this lease recreates container geth"},
			},
		},
		{
			name: "image needs that do not apply",
			needs: []protocol.Need{
				imageNeedFor("ghost", "x/y:z", "sha256:1"),
				imageNeedFor("dora", "ethpandaops/dora:latest", "sha256:e"),
				{ID: "image/bad", Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: "geth"}},
			},
			mutate: func(f *Facts, _ *Overrides) { f.Containers[2].Image = "ethpandaops/dora@sha256:" + sha64 },
			verdicts: map[string]verdict{
				protocol.ImageNeedID("ghost", "sha256:1"): {protocol.StepSuperseded, "container ghost does not exist"},
				protocol.ImageNeedID("dora", "sha256:e"):  {protocol.StepRefused, "container dora is pinned to a digest"},
				"image/bad":                               {protocol.StepRefused, "malformed image need"},
			},
		},
		{
			name: "reboots", needs: []protocol.Need{reboot, manualReboot}, manual: []protocol.Need{manualReboot},
			reboot: []string{reboot.ID, manualReboot.ID}, wasRunning: []string{"geth", "lh-vc", "teku"},
		},
		{
			name: "no reboot reason any more: only the manual reboot", needs: []protocol.Need{reboot, manualReboot},
			manual: []protocol.Need{manualReboot}, mutate: noReboot, reboot: []string{manualReboot.ID},
			verdicts: map[string]verdict{reboot.ID: {protocol.StepSuperseded, "the host no longer needs a reboot"}},
		},
		{
			name:     "a malformed reboot need",
			needs:    []protocol.Need{{ID: "reboot", Kind: protocol.NeedReboot}},
			verdicts: map[string]verdict{"reboot": {protocol.StepRefused, "malformed reboot need"}},
		},
		{
			name:  "packages",
			needs: []protocol.Need{packages("docker-ce", "containerd.io"), packages("docker-ce")},
			// Each upgraded once, sorted.
			packages: []string{"containerd.io", "docker-ce"},
		},
		{
			name:  "packages that do not apply",
			needs: []protocol.Need{packages("docker-ce"), {ID: "packages/x", Kind: protocol.NeedPackages}},
			mutate: func(f *Facts, _ *Overrides) {
				f.System.Blocked = nil
			},
			verdicts: map[string]verdict{
				packages("docker-ce").ID: {protocol.StepSuperseded, "the packages are already upgraded"},
				"packages/x":             {protocol.StepRefused, "malformed packages need"},
			},
		},
		{
			name: "an override need also serves the image need at its digest", files: overrideFiles("ethpandaops/geth:fix"),
			needs: []protocol.Need{
				imageNeedFor("geth", "ethpandaops/geth:master", "sha256:f"), override,
			},
			recreates: []Recreate{{
				IDs: []string{override.ID, protocol.ImageNeedID("geth", "sha256:f")}, Container: "geth",
				Digest: "sha256:f", Target: override.ID,
			}},
		},
		{
			name: "a spec need", files: specFiles, needs: []protocol.Need{spec},
			recreates: []Recreate{{IDs: []string{spec.ID}, Container: "geth", Digest: "sha256:s", Target: spec.ID}},
		},
		{
			name: "a spec need of the container under another ID", files: specFiles,
			needs:     []protocol.Need{spec, {ID: "spec/again", Kind: protocol.NeedSpec, Spec: spec.Spec}},
			recreates: []Recreate{{IDs: []string{spec.ID}, Container: "geth", Digest: "sha256:s", Target: spec.ID}},
			verdicts: map[string]verdict{
				"spec/again": {protocol.StepSuperseded, "agent.yaml or the overrides file asks for something else now"},
			},
		},
		{
			name: "the overrides file asks for something else now", files: overrideFiles("ethpandaops/geth:other"),
			needs: []protocol.Need{override},
			verdicts: map[string]verdict{
				override.ID: {protocol.StepSuperseded, "agent.yaml or the overrides file asks for something else now"},
			},
		},
		{
			name: "the container runs it already", needs: []protocol.Need{override},
			verdicts: map[string]verdict{
				override.ID: {protocol.StepSuperseded, "container geth already runs what agent.yaml and the overrides file say"},
			},
		},
		{
			name: "an entry that cannot be applied", files: overrideFiles("bad@@ref"), needs: []protocol.Need{override},
			verdicts: map[string]verdict{override.ID: {protocol.StepSuperseded, "image reference"}},
		},
		{
			name: "the container is gone", files: overrideFiles("ethpandaops/geth:fix"), needs: []protocol.Need{override},
			mutate:   func(f *Facts, _ *Overrides) { f.Containers = f.Containers[1:] },
			verdicts: map[string]verdict{override.ID: {protocol.StepSuperseded, "the container does not exist"}},
		},
		{
			name: "the container is not managed", files: overrideFiles("ethpandaops/geth:fix"), needs: []protocol.Need{override},
			mutate:   func(f *Facts, _ *Overrides) { f.Containers[0].Kind = protocol.KindNone },
			verdicts: map[string]verdict{override.ID: {protocol.StepRefused, "container geth is not managed"}},
		},
		{
			name: "a container that is never rolled", files: specFiles, needs: []protocol.Need{spec},
			mutate: func(f *Facts, _ *Overrides) { f.Containers[0].Linked = true },
			verdicts: map[string]verdict{
				spec.ID: {protocol.StepRefused, "container geth is created with links"},
			},
		},
		{
			name:  "malformed target needs",
			needs: []protocol.Need{{ID: "override/x", Kind: protocol.NeedOverride}, {ID: "spec/x", Kind: protocol.NeedSpec}},
			verdicts: map[string]verdict{
				"override/x": {protocol.StepRefused, "malformed override need"},
				"spec/x":     {protocol.StepRefused, "malformed spec need"},
			},
		},
		{
			name: "verbs that are not allowed", verbs: []protocol.Verb{protocol.VerbReboot},
			files: overrideFiles("ethpandaops/geth:fix"), needs: []protocol.Need{override, gethImage},
			verdicts: map[string]verdict{
				override.ID:  {protocol.StepRefused, "verb roll is not allowed on this host"},
				gethImage.ID: {protocol.StepRefused, "verb roll is not allowed on this host"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validatePolicy()
			if tc.verbs != nil {
				p.Verbs = tc.verbs
			}

			facts, files := validateFacts(), tc.files
			if files == nil {
				files = known()
			}

			if tc.mutate != nil {
				tc.mutate(facts, files)
			}

			got, err := Validate(p, leaseOf(tc.needs...), leaseView(p, facts, files), tc.manual)
			require.NoError(t, err)

			want := make(map[string]protocol.Step, len(tc.verdicts))
			for id, v := range tc.verdicts {
				require.Contains(t, got.Verdicts, id)
				assert.Contains(t, got.Verdicts[id].Error, v.reason, id)
				want[id] = protocol.Step{NeedID: id, Status: v.status, Error: got.Verdicts[id].Error}
			}

			assert.Equal(t, want, got.Verdicts)
			assert.Equal(t, tc.recreates, got.Recreates)
			assert.Equal(t, tc.reboot, got.Reboot)
			assert.Equal(t, tc.packages, got.Packages)

			if tc.wasRunning != nil {
				assert.Equal(t, tc.wasRunning, got.WasRunning)
			}
		})
	}
}

// A host on hold declines the lease; a lease that needs unknown state is an error.
func TestValidateDeclinesAndWaits(t *testing.T) {
	p := validatePolicy()
	gethImage := imageNeedFor("geth", "ethpandaops/geth:master", "sha256:b")

	files := known()
	files.Holds = map[string]Hold{HoldLocal: {Reason: "debugging"}}
	got, err := Validate(p, leaseOf(gethImage), leaseView(p, validateFacts(), files), nil)
	require.NoError(t, err)
	assert.Equal(t, "host is on local hold: debugging", got.Decline)
	assert.True(t, got.Empty())

	facts := validateFacts()
	facts.System = nil
	reboot := protocol.Need{ID: protocol.RebootNeedID, Kind: protocol.NeedReboot, Reboot: &protocol.RebootNeed{}}

	_, err = Validate(p, leaseOf(gethImage, reboot), leaseView(p, facts, known()), nil)
	require.Error(t, err)

	got, err = Validate(p, leaseOf(gethImage), leaseView(p, facts, known()), nil)
	require.NoError(t, err, "an image need does not need the system state")
	assert.Len(t, got.Recreates, 1)

	stale := validateFacts()
	stale.Fresh = false
	_, err = Validate(p, leaseOf(gethImage), leaseView(p, stale, known()), nil)
	require.Error(t, err, "never planned on the last good list")
}

// No need of a container labelled none is served.
func TestValidateExcluded(t *testing.T) {
	p := validatePolicy()
	facts := validateFacts()
	facts.Containers[2].Kind = protocol.KindNone
	image := imageNeedFor("dora", "ethpandaops/dora:latest", "sha256:e")
	target := protocol.Need{
		ID: "override/dora/x", Kind: protocol.NeedOverride, Local: true,
		Override: &protocol.OverrideNeed{Container: "dora", Image: "ethpandaops/dora:fix", ToDigest: "sha256:f"},
	}

	got, err := Validate(p, leaseOf(image, target), leaseView(p, facts, known()), nil)
	require.NoError(t, err)
	assert.True(t, got.Empty())
	assert.Equal(t, map[string]protocol.Step{
		image.ID:  {NeedID: image.ID, Status: protocol.StepRefused, Error: "container dora is not managed"},
		target.ID: {NeedID: target.ID, Status: protocol.StepRefused, Error: "container dora is not managed"},
	}, got.Verdicts)
}

// The kinds order the recreates, the start and the drain; a container stops
// within its own stop timeout.
func TestKindOrder(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll)

	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "vc", Image: "sigp/lighthouse:latest", Kind: protocol.KindVC, State: protocol.StateRunning},
		{Name: "snooper", Image: "snooper:latest", Kind: "snooper-el", State: protocol.StateRunning},
		{Name: "beacon", Image: "sigp/lighthouse:latest", Kind: protocol.KindCL, State: protocol.StateRunning},
		{
			Name: "execution", Image: "ethpandaops/geth:master", Kind: protocol.KindEL,
			State: protocol.StateRunning, StopTimeout: 10 * time.Second,
		},
	}}
	view := leaseView(p, facts, known())

	got, err := Validate(p, leaseOf(
		imageNeedFor("vc", "sigp/lighthouse:latest", "sha256:2"),
		imageNeedFor("snooper", "snooper:latest", "sha256:2"),
		imageNeedFor("beacon", "sigp/lighthouse:latest", "sha256:2"),
		imageNeedFor("execution", "ethpandaops/geth:master", "sha256:2"),
	), view, nil)
	require.NoError(t, err)

	order := make([]string, 0, len(got.Recreates))
	for _, r := range got.Recreates {
		order = append(order, r.Container)
	}

	assert.Equal(t, []string{"execution", "beacon", "vc", "snooper"}, order, "recreated first")
	assert.Equal(t, []string{"execution", "beacon", "vc", "snooper"}, StartOrder(got.WasRunning, view.Containers))
	assert.Equal(t, []string{"vc", "beacon", "execution", "snooper"}, DrainOrder(got.WasRunning, view.Containers))
	assert.Equal(t, 10*time.Second, StopTimeout(view.Container("execution")), "its own stop timeout")
}

func TestRecheck(t *testing.T) {
	p := validatePolicy()
	override := targetNeedOf(t, p, overrideFiles("ethpandaops/geth:fix"), "docker.io/ethpandaops/geth:fix", "sha256:f")
	task := &Recreate{IDs: []string{override.ID}, Container: "geth", Digest: "sha256:f", Target: override.ID}

	rec, reason := Recheck(task, leaseView(p, validateFacts(), overrideFiles("ethpandaops/geth:fix")))
	assert.Empty(t, reason)
	assert.Equal(t, Recreation{
		Image: "ethpandaops/geth:fix", SetBase: true, Base: &protocol.OverrideBase{Image: "ethpandaops/geth:master"},
	}, rec, "from the files: the image of the entry, the base for the labels")

	_, reason = Recheck(task, leaseView(p, validateFacts(), overrideFiles("ethpandaops/geth:other")))
	assert.Equal(t, "agent.yaml or the overrides file asks for something else now", reason,
		"a person changed the entry since the lease was validated")

	// A restore: both labels go.
	restored := validateFacts()
	restored.Containers[0].Image = "ethpandaops/geth:fix"
	restored.Containers[0].Base = &protocol.OverrideBase{Image: "ethpandaops/geth:master"}
	restore := targetNeedOfFacts(t, p, restored, "docker.io/ethpandaops/geth:master")
	rec, reason = Recheck(&Recreate{Container: "geth", Digest: "sha256:m", Target: restore.ID},
		leaseView(p, restored, known()))
	assert.Empty(t, reason)
	assert.Equal(t, Recreation{Image: "ethpandaops/geth:master", SetBase: true}, rec)

	// A plain recreate keeps the container's image and command line.
	plain := &Recreate{Container: "teku", Digest: "sha256:t2"}
	rec, reason = Recheck(plain, leaseView(p, validateFacts(), known()))
	assert.Empty(t, reason)
	assert.Equal(t, Recreation{}, rec)

	facts := validateFacts()
	facts.Containers[1].Image = "consensys/teku@sha256:" + sha64
	_, reason = Recheck(plain, leaseView(p, facts, known()))
	assert.Equal(t, "container teku is pinned to a digest", reason)

	_, reason = Recheck(&Recreate{Container: "ghost"}, leaseView(p, validateFacts(), known()))
	assert.Equal(t, "container ghost does not exist", reason)
}

// targetNeedOfFacts is geth's target need with an empty overrides file.
func targetNeedOfFacts(t *testing.T, p *Policy, facts *Facts, ref string) protocol.Need {
	t.Helper()

	v := Classify(p, facts, known(), map[string]protocol.RegistryCheck{ref: {Digest: "sha256:m"}}, testNow)
	for _, n := range Needs(p, &v, nil) {
		if n.Container() == "geth" {
			require.True(t, n.Override.Restore)

			return n
		}
	}

	require.FailNow(t, "no need for geth")

	return protocol.Need{}
}

func TestStopTimeout(t *testing.T) {
	assert.Equal(t, DefaultStopTimeout, StopTimeout(nil))
	assert.Equal(t, time.Minute, StopTimeout(&protocol.Container{StopTimeout: time.Minute}))
	assert.Equal(t, DefaultStopTimeout, StopTimeout(&protocol.Container{}))
}

func TestStartAndDrainOrder(t *testing.T) {
	containers := []protocol.Container{
		{Name: "vc", Managed: true, Kind: protocol.KindVC},
		{Name: "beacon", Managed: true, Kind: protocol.KindCL},
		{Name: "geth", Managed: true, Kind: protocol.KindEL},
		{Name: "exporter", Managed: true, Kind: "exporter"},
		{Name: "loose", Kind: protocol.KindEL}, // not managed: last
	}
	names := []string{"zz-gone", "loose", "exporter", "vc", "geth", "beacon"}

	assert.Equal(t, []string{"geth", "beacon", "vc", "exporter", "loose", "zz-gone"}, StartOrder(names, containers))
	assert.Equal(t, []string{"vc", "beacon", "geth", "exporter", "loose", "zz-gone"}, DrainOrder(names, containers))
	assert.Equal(t, "zz-gone", names[0], "the input is not sorted in place")
}
