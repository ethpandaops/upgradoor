package views

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArgVotesCommon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands [][]string
		want     []string
	}{
		{
			name: "an argument on a strict majority is common",
			commands: [][]string{
				{"--http", "--nat=extip:1.1.1.1", "--port", "30303"},
				{"--http", "--nat=extip:2.2.2.2", "--port", "30303"},
				{"--http", "--nat=extip:3.3.3.3", "--port", "9000"},
			},
			want: []string{"--http", "--port", "30303"},
		},
		{
			name: "a value every host has its own of is not common",
			commands: [][]string{
				{"--nat=extip:1.1.1.1"},
				{"--nat=extip:2.2.2.2"},
			},
			want: nil,
		},
		{
			name: "half is no majority",
			commands: [][]string{
				{"--http", "--debug"},
				{"--http"},
			},
			want: []string{"--http"},
		},
		{
			name:     "a repeated argument counts once per host",
			commands: [][]string{{"--port", "--port"}, {"--http"}, {"--http"}},
			want:     []string{"--http"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := argVotes{count: make(map[string]int, 8)}
			for _, c := range tt.commands {
				v.add(c)
			}

			assert.Equal(t, tt.want, v.common())
		})
	}
}
