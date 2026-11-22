package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Only a labelled container with a team may be overridden, image and command
// line alike.
func TestOverridable(t *testing.T) {
	image := &Entry{Image: "ethpandaops/geth:fix"}
	flags := &Entry{Command: []string{"--verbosity=4"}, HasCommand: true}

	for _, tc := range []struct {
		name  string
		c     protocol.Container
		entry *Entry
		want  string
	}{
		{"a team takes an image", protocol.Container{Kind: protocol.KindEL, Team: "geth"}, image, ""},
		{"a team takes flags", protocol.Container{Kind: protocol.KindEL, Team: "geth"}, flags, ""},
		{"no team", protocol.Container{Kind: "snooper-el"}, image, "no " + protocol.LabelTeam + " label"},
		{"no team takes an empty entry", protocol.Container{Kind: "snooper-el"}, &Entry{}, ""},
		{"unlabelled", protocol.Container{}, flags, "no " + protocol.LabelKind + " label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.Name = "geth"

			err := overridable(&tc.c, tc.entry)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

// permitsView: geth with a team, snooper without, dora unlabelled, debug
// unmanaged.
func permitsView() (*Policy, *View) {
	p := &Policy{Verbs: []protocol.Verb{protocol.VerbRoll}}
	v := &View{Containers: []protocol.Container{
		{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth", Command: []string{"geth", "--a"}},
		{Name: "snooper", Managed: true, Kind: "snooper-el"},
		{Name: "dora", Managed: true},
		{Name: "debug"},
	}}

	return p, v
}

func TestPermits(t *testing.T) {
	at := func(d time.Duration) *time.Time {
		u := testNow.Add(d)

		return &u
	}
	image := func(until *time.Time) *Entry { return &Entry{Image: "a/b:c", Until: until} }

	for _, tc := range []struct {
		name      string
		container string
		next      *Entry
		asked     *Entry
		origin    Origin
		broken    bool
		want      string
	}{
		{"from the controller, with an end", "geth", image(at(time.Hour)), image(at(time.Hour)), FromController, false, ""},
		{"from the controller, no end", "geth", image(nil), image(nil), FromController, false, "needs an end"},
		{"from the controller, past", "geth", image(at(-time.Hour)), image(at(-time.Hour)), FromController, false,
			"has passed"},
		{"from the controller, at now", "geth", image(at(0)), image(at(0)), FromController, false, "has passed"},
		{"from the controller, a week and a minute", "geth", image(at(MaxOverride + time.Minute)),
			image(at(MaxOverride + time.Minute)), FromController, false, ""},
		{"from the controller, more than a week", "geth", image(at(8 * 24 * time.Hour)),
			image(at(8 * 24 * time.Hour)), FromController, false, "more than 168h0m0s"},
		{"on the host, no end", "geth", image(nil), image(nil), FromLocal, false, ""},
		{"not managed", "debug", image(nil), image(nil), FromLocal, false, "container debug is not managed on this host"},
		{"gone", "ghost", nil, nil, FromLocal, false, "container ghost is not managed"},
		{"the last view's file error is local's to check", "geth", nil, nil, FromLocal, true, ""},
		{"an invalid entry", "geth", &Entry{}, nil, FromLocal, false, "names an image, a command or both"},
		{"no team", "snooper", image(nil), image(nil), FromLocal, false, "no " + protocol.LabelTeam + " label"},
		{"unlabelled", "dora", image(nil), image(nil), FromLocal, false, "no " + protocol.LabelKind + " label"},
		{"a removal asks nothing", "snooper", nil, nil, FromController, false, ""},
		{"what is not asked is not bounded", "snooper", image(nil), nil, FromController, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, v := permitsView()
			if tc.broken {
				v.OverridesError = "bad"
			}

			err := Permits(v, tc.container, tc.next, tc.asked, tc.origin, testNow)
			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrRefused)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestInstruct(t *testing.T) {
	until := testNow.Add(time.Hour)
	prev := &Entry{
		Image: "ethpandaops/geth:fix", Command: []string{"geth", "--old"}, HasCommand: true, By: "sam", Reason: "old",
		Until: &until,
	}
	deployed := &Deployed{Image: "ethpandaops/geth:master", Command: []string{"geth", "--a", "--d"}, HasCommand: true}
	later := testNow.Add(2 * time.Hour)

	setImage := &protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: " ethpandaops/geth:new ", Until: &later}
	clearImage := &protocol.OverrideInstruction{Part: protocol.OverrideImage}
	setFlags := &protocol.OverrideInstruction{
		Part: protocol.OverrideFlags, AddFlags: []string{"--b"}, RemoveFlags: []string{"--a"}, Until: &later,
	}
	clearFlags := &protocol.OverrideInstruction{Part: protocol.OverrideFlags}

	for _, tc := range []struct {
		name      string
		verbs     []protocol.Verb
		container string
		inst      *protocol.OverrideInstruction
		prev      *Entry
		deployed  *Deployed
		want      *Entry
		refusal   string
	}{
		{
			name: "set the image, keep the command", container: "geth", inst: setImage, prev: prev,
			want: &Entry{
				Image: "ethpandaops/geth:new", Command: []string{"geth", "--old"}, HasCommand: true,
				By: "pk", Reason: "why", Until: &later,
			},
		},
		{
			name: "flags apply to the deployed command, keep the image", container: "geth", inst: setFlags,
			prev: prev, deployed: deployed,
			want: &Entry{
				Image: "ethpandaops/geth:fix", Command: []string{"geth", "--d", "--b"}, HasCommand: true,
				By: "pk", Reason: "why", Until: &later,
			},
		},
		{
			name: "flags apply to what the container was created with without a deployed one", container: "geth",
			inst: setFlags,
			want: &Entry{Command: []string{"geth", "--b"}, HasCommand: true, By: "pk", Reason: "why", Until: &later},
		},
		{
			name: "take the image away: the old end stays", container: "geth", inst: clearImage, prev: prev,
			want: &Entry{Command: []string{"geth", "--old"}, HasCommand: true, By: "pk", Reason: "why", Until: &until},
		},
		{
			name: "take the last part away", container: "geth", inst: clearFlags,
			prev: &Entry{Command: []string{"--a"}, HasCommand: true},
		},
		{name: "malformed flags", container: "geth", inst: &protocol.OverrideInstruction{
			Part: protocol.OverrideFlags, RemoveFlags: []string{"--a b c"}, Until: &later,
		}, refusal: "one value"},
		{
			name: "take away without a team: allowed", container: "snooper", inst: clearImage,
			prev: &Entry{Image: "x/y:z"},
		},
		{name: "no instruction", container: "geth", refusal: "names no container or no override"},
		{name: "no container", inst: setImage, refusal: "names no container or no override"},
		{name: "not managed before the verb", verbs: []protocol.Verb{}, container: "debug", inst: setImage,
			refusal: "container debug is not managed on this host"},
		{name: "verb before the part", verbs: []protocol.Verb{}, container: "geth",
			inst: &protocol.OverrideInstruction{Part: "env"}, refusal: "verb roll is not allowed on this host"},
		{name: "an unknown part", container: "geth", inst: &protocol.OverrideInstruction{Part: "env"},
			refusal: `unknown part "env" of an override`},
		{name: "an invalid image", container: "geth", inst: &protocol.OverrideInstruction{
			Part: protocol.OverrideImage, Image: "ethpandaops/geth@sha256:" + sha64, Until: &later,
		}, refusal: "pinned to a digest", want: &Entry{
			Image: "ethpandaops/geth@sha256:" + sha64, By: "pk", Reason: "why", Until: &later,
		}},
		{name: "no team", container: "snooper", inst: setImage, refusal: "no " + protocol.LabelTeam + " label", want: &Entry{
			Image: "ethpandaops/geth:new", By: "pk", Reason: "why", Until: &later,
		}},
		{name: "no end", container: "geth", inst: &protocol.OverrideInstruction{
			Part: protocol.OverrideImage, Image: "a/b:c",
		}, refusal: "needs an end", want: &Entry{Image: "a/b:c", By: "pk", Reason: "why"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, v := permitsView()
			if tc.verbs != nil {
				p.Verbs = tc.verbs
			}

			got, err := Instruct(p, v, tc.container, tc.inst, tc.prev, tc.deployed, "pk", "why\nsecond line", testNow)
			if tc.refusal != "" {
				require.ErrorIs(t, err, ErrRefused)
				require.ErrorContains(t, err, tc.refusal)
				assert.Equal(t, tc.want, got, "the entry refused, nil when none was built")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	assert.Equal(t, []string{"geth", "--old"}, prev.Command, "the entry that is there is not changed")

	// The entry returned shares no list with the one that is there.
	p, v := permitsView()
	got, err := Instruct(p, v, "geth", setImage, prev, nil, "pk", "why", testNow)
	require.NoError(t, err)
	got.Command[0] = "--changed"
	assert.Equal(t, []string{"geth", "--old"}, prev.Command)
}

const sha64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestLocalEntry(t *testing.T) {
	until := testNow.Add(time.Hour)
	_, v := permitsView()

	got, err := LocalEntry(v, &LocalRequest{
		Container: "geth", Image: " ethpandaops/geth:fix ", Add: []string{"--b"}, User: "local:pk",
		Reason: "why\nsecond line",
	}, nil, testNow)
	require.NoError(t, err, "one set on the host may have no end")
	assert.Equal(t, &Entry{
		Image: "ethpandaops/geth:fix", Command: []string{"geth", "--a", "--b"}, HasCommand: true, By: "pk", Reason: "why",
	}, got, "flags apply to what the container was created with")

	deployed := &Deployed{Image: "ethpandaops/geth:master", Command: []string{"geth", "--a", "--d"}, HasCommand: true}
	got, err = LocalEntry(v, &LocalRequest{
		Container: "geth", Add: []string{"--b"}, Remove: []string{"--a"}, User: "local:pk",
	}, deployed, testNow)
	require.NoError(t, err)
	assert.Equal(t, &Entry{Command: []string{"geth", "--d", "--b"}, HasCommand: true, By: "pk"}, got,
		"flags apply to the deployed command")

	got, err = LocalEntry(v, &LocalRequest{Container: "geth", Image: "a/b:c", User: "local:pk"}, deployed, testNow)
	require.NoError(t, err)
	assert.Equal(t, &Entry{Image: "a/b:c", By: "pk"}, got, "an image alone keeps the default command")

	_, err = LocalEntry(v, &LocalRequest{Container: "geth", Remove: []string{"--a b c"}, User: "local:pk"}, nil, testNow)
	require.ErrorContains(t, err, "one value")

	got, err = LocalEntry(v, &LocalRequest{Container: "geth", Clear: true, User: "local:pk"}, nil, testNow)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = LocalEntry(v, &LocalRequest{Container: "snooper", Image: "a/b:c", User: "local:pk", Until: &until}, nil, testNow)
	require.ErrorIs(t, err, ErrRefused)
	assert.Equal(t, &Entry{Image: "a/b:c", By: "pk", Until: &until}, got, "the entry refused")

	_, err = LocalEntry(v, &LocalRequest{Container: "debug", Clear: true}, nil, testNow)
	require.ErrorContains(t, err, "not managed")
}

func TestLocalHold(t *testing.T) {
	assert.Equal(t, Hold{By: "pk", Reason: "held", At: testNow}, LocalHold("local:pk", " ", testNow))
	assert.Equal(t, Hold{By: "pk", Reason: "debugging", At: testNow}, LocalHold("local:pk", "debugging\nmore", testNow))
}

func TestDirectedHold(t *testing.T) {
	d := &protocol.Directive{Kind: protocol.DirectiveHold, Holder: "geth", Reason: "split\nmore"}
	assert.Equal(t, Hold{By: "controller", Reason: "split", At: testNow}, DirectedHold(d, testNow))

	d.By = &protocol.Actor{User: "pk@example.org", Teams: []string{"geth"}, Feature: "team_manage"}
	assert.Equal(t, Hold{By: "pk@example.org", Reason: "split", At: testNow}, DirectedHold(d, testNow))
}
