package link_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/link"
)

func TestConfigControllerKeys(t *testing.T) {
	key0 := controllerPublicKey(t, 0)
	key1 := controllerPublicKey(t, 1)

	tests := []struct {
		name     string
		keys     []string
		unsigned bool
		want     int
		wantErr  string
	}{
		{name: "one key", keys: []string{key0}, want: 1},
		{name: "two keys for a rotation", keys: []string{key0, key1 + "\n"}, want: 2},
		{name: "the opt-out", unsigned: true, want: 0},
		{name: "nothing configured", wantErr: "controller.publicKeys is required"},
		{
			name: "keys and the opt-out", keys: []string{key0}, unsigned: true,
			wantErr: "exclude each other",
		},
		{name: "not a key", keys: []string{key0, "bm90IGEga2V5"}, wantErr: "publicKeys[1]"},
		{
			name: "a private key in place of a public one",
			keys: []string{"3ccsBTOwGo_w6jKJ-xCQ6L5VPI5GOtA1pIHEY8gpUdA"}, wantErr: "publicKeys[0]",
		},
		{
			name: "too many", keys: []string{key0, key1, key0, key1, key0},
			wantErr: "more than 4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := link.Config{
				URL: "https://upgradoor.example", PublicKeys: tt.keys,
				AllowUnsignedAnswers: tt.unsigned,
			}

			keys, err := cfg.Keys()

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, cfg.Validate(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Len(t, keys, tt.want)
		})
	}
}

func TestConfigURL(t *testing.T) {
	tests := []struct {
		url      string
		insecure bool
		wantErr  string
	}{
		{url: "https://upgradoor.example"},
		{url: "http://upgradoor.example", insecure: true},
		{url: "http://upgradoor.example", wantErr: "must be https"},
		{url: "upgradoor.example", wantErr: "absolute URL"},
	}

	for _, tt := range tests {
		cfg := link.Config{URL: tt.url, AllowInsecure: tt.insecure, AllowUnsignedAnswers: true}

		if tt.wantErr == "" {
			assert.NoError(t, cfg.Validate(), tt.url)
		} else {
			assert.ErrorContains(t, cfg.Validate(), tt.wantErr, tt.url)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := link.Config{SyncInterval: -time.Second, MaxSyncInterval: time.Hour}
	cfg.ApplyDefaults()

	assert.Equal(t, link.Config{
		SyncInterval: 30 * time.Second, LeaseSyncInterval: 5 * time.Second,
		MinSyncInterval: time.Second, MaxSyncInterval: time.Hour,
	}, cfg)
}
