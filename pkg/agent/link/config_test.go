package link_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/link"
)

func TestConfigControllerKey(t *testing.T) {
	key := controllerPublicKey(t)

	tests := []struct {
		name     string
		key      string
		unsigned bool
		want     bool
		wantErr  string
	}{
		{name: "the key", key: key, want: true},
		{name: "the key with a newline", key: key + "\n", want: true},
		{name: "the opt-out", unsigned: true},
		{name: "nothing configured", wantErr: "controller.publicKey is required"},
		{name: "key and the opt-out", key: key, unsigned: true, wantErr: "exclude each other"},
		{name: "not a key", key: "bm90IGEga2V5", wantErr: "controller.publicKey:"},
		{
			name: "a private key in place of a public one",
			key:  "3ccsBTOwGo_w6jKJ-xCQ6L5VPI5GOtA1pIHEY8gpUdA", wantErr: "controller.publicKey:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := link.Config{
				URL: "https://upgradoor.example", PublicKey: tt.key,
				AllowUnsignedAnswers: tt.unsigned,
			}

			got, err := cfg.Key()

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, cfg.Validate(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Equal(t, tt.want, got != nil)
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
