package overrides

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyFlags(t *testing.T) {
	base := []string{
		"--datadir=/data", "--http", "--http.port", "8545", "--verbosity=3",
		"--bootnodes=a", "--bootnodes=b", "--metrics", "--syncmode", "snap",
	}

	for _, tt := range []struct {
		name        string
		add, remove []string
		want        string
	}{
		{name: "nothing", want: strings.Join(base, " ")},
		{
			name: "exact argument", remove: []string{"--bootnodes=a"},
			want: "--datadir=/data --http --http.port 8545 --verbosity=3 --bootnodes=b --metrics --syncmode snap",
		},
		{
			name: "by name, every value", remove: []string{"--bootnodes"},
			want: "--datadir=/data --http --http.port 8545 --verbosity=3 --metrics --syncmode snap",
		},
		{
			name: "by name, two-argument form", remove: []string{"--http.port"},
			want: "--datadir=/data --http --verbosity=3 --bootnodes=a --bootnodes=b --metrics --syncmode snap",
		},
		{
			name: "by name, a switch keeps the flag after it", remove: []string{"--http"},
			want: "--datadir=/data --http.port 8545 --verbosity=3 --bootnodes=a --bootnodes=b --metrics --syncmode snap",
		},
		{
			name: "a pair", remove: []string{"--syncmode snap"},
			want: "--datadir=/data --http --http.port 8545 --verbosity=3 --bootnodes=a --bootnodes=b --metrics",
		},
		{
			name: "a pair with another value stays", remove: []string{"--syncmode full"},
			want: strings.Join(base, " "),
		},
		{
			name: "add appends", add: []string{"--pprof", "--ws.port 8546"},
			want: strings.Join(base, " ") + " --pprof --ws.port 8546",
		},
		{
			name: "add of what is there adds nothing", add: []string{"--metrics", "--syncmode snap"},
			want: strings.Join(base, " "),
		},
		{
			name: "change a value", remove: []string{"--verbosity"}, add: []string{"--verbosity=5"},
			want: "--datadir=/data --http --http.port 8545 --bootnodes=a --bootnodes=b --metrics --syncmode snap --verbosity=5",
		},
		{
			name: "something that is no flag is one argument", add: []string{"beacon node"},
			want: strings.Join(base, " ") + " beacon node",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := applyFlags(base, tt.add, tt.remove)
			assert.Equal(t, tt.want, strings.Join(got, " "))
			assert.Equal(t, got, applyFlags(got, tt.add, tt.remove), "applying twice changes nothing")
		})
	}

	assert.Len(t, base, 10, "the command it was given is left alone")
	assert.Equal(t, []string{"beacon", "--a"}, applyFlags([]string{"beacon"}, []string{"--a"}, nil))
	assert.Empty(t, applyFlags(nil, nil, []string{"--a"}))
}

func TestValidateFlags(t *testing.T) {
	require.NoError(t, validateFlags([]string{"--a=b", "--c d", "-v"}, []string{"--e"}))

	for name, tt := range map[string]struct {
		flag string
		want string
	}{
		"empty":        {"", "is empty"},
		"blank around": {" --a", "blanks around"},
		"newline":      {"--a\n--b", "control character"},
		"three parts":  {"--a b c", "write the name and one value"},
		"too long":     {"--a=" + strings.Repeat("x", maxFlagLen), "too long"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, validateFlags([]string{tt.flag}, nil), tt.want)
			require.ErrorContains(t, validateFlags(nil, []string{tt.flag}), tt.want)
		})
	}

	many := make([]string, maxFlags+1)
	for i := range many {
		many[i] = "--x"
	}

	require.ErrorContains(t, validateFlags(many, nil), "more than")
}
