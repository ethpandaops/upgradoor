package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestPolicyPermits(t *testing.T) {
	image := &Entry{Image: "ethpandaops/geth:fix"}
	flags := &Entry{Add: []string{"--verbosity=4"}}
	both := []string{protocol.OverrideImage, protocol.OverrideFlags}

	for _, tc := range []struct {
		name  string
		group Group
		in    string
		entry *Entry
		want  string
	}{
		{"tooling refuses an image", Group{}, "execution", image, "allows no overrides"},
		{"tooling refuses flags", Group{}, "execution", flags, "allows no overrides"},
		{"tooling takes an empty entry", Group{}, "execution", &Entry{}, ""},
		{"image only", Group{Overrides: []string{"image"}}, "execution", image, ""},
		{"image only refuses flags", Group{Overrides: []string{"image"}}, "execution", flags, "flag overrides"},
		{"flags only refuses an image", Group{Overrides: []string{"flags"}}, "execution", image, "image overrides"},
		{"both", Group{Overrides: both}, "execution", &Entry{Image: image.Image, Add: flags.Add}, ""},
		{"the default group allows nothing", Group{Overrides: both}, protocol.DefaultGroup, image, "default group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := partsPermitted(&tc.group, tc.in, "geth", tc.entry)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

// permitsView: geth overridable, nginx tooling, dora default, debug unmanaged.
func permitsView() (*Policy, *View) {
	p := &Policy{
		Verbs:  []protocol.Verb{protocol.VerbRoll},
		Groups: map[string]Group{"execution": {Overrides: []string{protocol.OverrideImage, protocol.OverrideFlags}}},
	}
	v := &View{Containers: []protocol.Container{
		{Name: "geth", Managed: true, Group: "execution", Kind: protocol.KindEL},
		{Name: "nginx", Managed: true, Group: "proxy", Kind: protocol.KindOther},
		{Name: "dora", Managed: true, Group: protocol.DefaultGroup, Kind: protocol.KindOther},
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
		{"an invalid entry", "geth", &Entry{}, nil, FromLocal, false, "names an image, flags or both"},
		{"tooling", "nginx", image(nil), image(nil), FromLocal, false, "allows no overrides"},
		{"the default group", "dora", image(nil), image(nil), FromLocal, false, "default group"},
		{"a removal asks nothing", "nginx", nil, nil, FromController, false, ""},
		{"what is not asked is not bounded", "nginx", image(nil), nil, FromController, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, v := permitsView()
			if tc.broken {
				v.OverridesError = "bad"
			}

			err := Permits(p, v, tc.container, tc.next, tc.asked, tc.origin, testNow)
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
		Image: "ethpandaops/geth:fix", Add: []string{"--a"}, By: "sam", Reason: "old",
		Until: &until,
	}
	later := testNow.Add(2 * time.Hour)

	setImage := &protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: " ethpandaops/geth:new ", Until: &later}
	clearImage := &protocol.OverrideInstruction{Part: protocol.OverrideImage}
	setFlags := &protocol.OverrideInstruction{
		Part: protocol.OverrideFlags, AddFlags: []string{"--b"}, RemoveFlags: []string{"--c"}, Until: &later,
	}
	clearFlags := &protocol.OverrideInstruction{Part: protocol.OverrideFlags}

	for _, tc := range []struct {
		name      string
		verbs     []protocol.Verb
		container string
		inst      *protocol.OverrideInstruction
		prev      *Entry
		want      *Entry
		refusal   string
	}{
		{
			name: "set the image, keep the flags", container: "geth", inst: setImage, prev: prev,
			want: &Entry{Image: "ethpandaops/geth:new", Add: []string{"--a"}, By: "pk", Reason: "why", Until: &later},
		},
		{
			name: "set the flags, keep the image", container: "geth", inst: setFlags, prev: prev,
			want: &Entry{
				Image: "ethpandaops/geth:fix", Add: []string{"--b"}, Remove: []string{"--c"},
				By: "pk", Reason: "why", Until: &later,
			},
		},
		{
			name: "take the image away: the old end stays", container: "geth", inst: clearImage, prev: prev,
			want: &Entry{Add: []string{"--a"}, By: "pk", Reason: "why", Until: &until},
		},
		{name: "take the last part away", container: "geth", inst: clearFlags, prev: &Entry{Add: []string{"--a"}}},
		{
			name: "take away on a tooling group: no opt-in needed", container: "nginx", inst: clearImage,
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
		{name: "tooling", container: "nginx", inst: setImage, refusal: "allows no overrides", want: &Entry{
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

			got, err := Instruct(p, v, tc.container, tc.inst, tc.prev, "pk", "why\nsecond line", testNow)
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

	assert.Equal(t, []string{"--a"}, prev.Add, "the entry that is there is not changed")

	// The entry returned shares no list with the one that is there.
	p, v := permitsView()
	got, err := Instruct(p, v, "geth", setImage, prev, "pk", "why", testNow)
	require.NoError(t, err)
	got.Add[0] = "--changed"
	assert.Equal(t, []string{"--a"}, prev.Add)
}

const sha64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The local hold is the host's alone.
func TestHoldSource(t *testing.T) {
	assert.Equal(t, HoldLocal, HoldSource(FromLocal))
	assert.Equal(t, HoldController, HoldSource(FromController))
	assert.Equal(t, HoldController, HoldSource("anything else"))
}

func TestLocalEntry(t *testing.T) {
	until := testNow.Add(time.Hour)
	p, v := permitsView()

	got, err := LocalEntry(p, v, &LocalRequest{
		Container: "geth", Image: " ethpandaops/geth:fix ", Add: []string{"--a"}, User: "local:pk",
		Reason: "why\nsecond line",
	}, testNow)
	require.NoError(t, err, "one set on the host may have no end")
	assert.Equal(t, &Entry{Image: "ethpandaops/geth:fix", Add: []string{"--a"}, By: "pk", Reason: "why"}, got)

	got, err = LocalEntry(p, v, &LocalRequest{Container: "geth", Clear: true, User: "local:pk"}, testNow)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = LocalEntry(p, v, &LocalRequest{Container: "nginx", Image: "a/b:c", User: "local:pk", Until: &until}, testNow)
	require.ErrorIs(t, err, ErrRefused)
	assert.Equal(t, &Entry{Image: "a/b:c", By: "pk", Until: &until}, got, "the entry refused")

	_, err = LocalEntry(p, v, &LocalRequest{Container: "debug", Clear: true}, testNow)
	require.ErrorContains(t, err, "not managed")
}

func TestLocalHold(t *testing.T) {
	assert.Equal(t, Hold{By: "pk", Reason: "held"}, LocalHold("local:pk", " "))
	assert.Equal(t, Hold{By: "pk", Reason: "debugging"}, LocalHold("local:pk", "debugging\nmore"))
}

func TestControllerHold(t *testing.T) {
	assert.Equal(t, Hold{By: "controller", Reason: "split\nmore"}, ControllerHold("split\nmore", nil),
		"local cuts the text when it writes the hold")
	assert.Equal(t, Hold{By: "pk@example.org", Reason: "split", Teams: []string{"geth", "lighthouse"}, Feature: "team_hold"},
		ControllerHold("split", &protocol.Actor{
			User: "pk@example.org", Teams: []string{"lighthouse", "geth", "Bad Team"}, Feature: "team_hold",
		}))
}
