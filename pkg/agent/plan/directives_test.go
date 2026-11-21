package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestRollRefusal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verbs     []protocol.Verb
		hold      protocol.Hold
		container string
		want      string
	}{
		{name: "a managed, rolled container", container: "geth"},
		{name: "verb first", verbs: []protocol.Verb{}, hold: protocol.Hold{Controller: true}, container: "",
			want: "verb roll is not allowed on this host"},
		{name: "then the hold", hold: protocol.Hold{Local: true, LocalReason: "x"}, want: "host is on local hold: x"},
		{name: "no container", want: "roll directive names no container"},
		{name: "gone", container: "ghost", want: "container ghost does not exist"},
		{name: "not rolled", container: "dora",
			want: "container dora is in the default group, which this host does not roll (defaultGroup.roll)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := needsPolicy(allVerbs...)
			if tc.verbs != nil {
				p.Verbs = tc.verbs
			}

			v := observed(p)
			v.Hold = tc.hold

			assert.Equal(t, tc.want, RollRefusal(p, v, tc.container))
		})
	}

	p := needsPolicy(allVerbs...)
	v := observed(p)
	v.Containers[0].Managed = false
	assert.Equal(t, "container geth is not managed", RollRefusal(p, v, "geth"))
}

func TestRollNeed(t *testing.T) {
	p := needsPolicy(allVerbs...)
	ref := "ethpandaops/geth:master"

	need, reject := RollNeed(p, observed(p), "geth", ref, "manual/d1", nil, false)
	assert.Nil(t, need)
	assert.Empty(t, reject, "it waits for the lookup")

	same := &protocol.RegistryCheck{Digest: "sha256:a", Revision: "r1"}
	need, reject = RollNeed(p, observed(p), "geth", ref, "manual/d1", same, false)
	assert.Empty(t, reject)
	require.NotNil(t, need)
	assert.Equal(t, protocol.Need{
		ID: "manual/d1", Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: "geth", Group: "execution", Client: "geth", Image: ref,
			FromDigest: "sha256:a", ToDigest: "sha256:a", ToRevision: "r1",
		},
	}, *need, "a roll at the running digest restarts the container")

	for _, tc := range []struct {
		name   string
		ref    string
		check  *protocol.RegistryCheck
		gone   bool
		mutate func(v *View)
		want   string
	}{
		{name: "refused first", ref: ref, gone: true, mutate: func(v *View) { v.Hold.Controller = true },
			want: "host is on controller hold"},
		{name: "the tracker dropped the tag", ref: ref, gone: true, want: "container geth moved to another image"},
		{name: "the container moved", ref: "ethpandaops/geth:stable", want: "container geth moved to another image"},
		{name: "no digest", ref: ref, check: &protocol.RegistryCheck{},
			want: "the registry did not say where ethpandaops/geth:master points: no digest"},
		{name: "a lookup error", ref: ref, check: &protocol.RegistryCheck{Error: "401 unauthorized"},
			want: "the registry did not say where ethpandaops/geth:master points: 401 unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := observed(p)
			if tc.mutate != nil {
				tc.mutate(v)
			}

			need, reject := RollNeed(p, v, "geth", tc.ref, "manual/d1", tc.check, tc.gone)
			assert.Nil(t, need)
			assert.Equal(t, tc.want, reject)
		})
	}
}

func TestManualReboot(t *testing.T) {
	p := needsPolicy(allVerbs...)

	need, reject := ManualReboot(p, observed(p), "d1")
	assert.Empty(t, reject)
	assert.Equal(t, protocol.Need{
		ID: protocol.ManualNeedID("d1"), Kind: protocol.NeedReboot, DirectiveID: "d1",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}, need)

	v := observed(p)
	v.Hold.Controller = true
	_, reject = ManualReboot(p, v, "d1")
	assert.Equal(t, "host is on controller hold", reject)

	p.Verbs = []protocol.Verb{protocol.VerbRoll}
	_, reject = ManualReboot(p, v, "d1")
	assert.Equal(t, "verb reboot is not allowed on this host", reject)
}
