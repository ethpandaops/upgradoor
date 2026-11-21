package views

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestTargetDigest(t *testing.T) {
	at := func(minute int) time.Time {
		return controllertest.Epoch.Add(time.Duration(minute) * time.Minute)
	}
	reg := func(digest, rev string, checked time.Time, errText string) *protocol.RegistryCheck {
		return &protocol.RegistryCheck{
			Digest: digest, Revision: rev, CheckedAt: checked, Error: errText,
		}
	}

	tests := []struct {
		name         string
		uses         []imageUse
		wantDigest   string
		wantRevision string
	}{
		{
			name: "newest successful registry check wins",
			uses: []imageUse{
				{host: "a", digest: "sha256:old", registry: reg("sha256:old", "r-old", at(1), "")},
				{host: "b", digest: "sha256:old", registry: reg("sha256:new", "r-new", at(5), "")},
				{host: "c", digest: "sha256:old", registry: reg("sha256:err", "", at(9), "boom")},
			},
			wantDigest: "sha256:new", wantRevision: "r-new",
		},
		{
			name: "a failed check still knows the last good digest",
			uses: []imageUse{
				{host: "a", digest: "sha256:old", registry: reg("sha256:new", "r-new", at(5), "boom")},
				{host: "b", digest: "sha256:old"},
				{host: "c", digest: "sha256:old"},
			},
			wantDigest: "sha256:new", wantRevision: "r-new",
		},
		{
			name: "revision from a host that already runs the target",
			uses: []imageUse{
				{host: "a", digest: "sha256:new", revision: "r-running",
					registry: reg("sha256:new", "", at(1), "")},
				{host: "b", digest: "sha256:old", revision: "r-old"},
			},
			wantDigest: "sha256:new", wantRevision: "r-running",
		},
		{
			name: "without registry data the most common digest",
			uses: []imageUse{
				{host: "a", digest: "sha256:x", revision: "r-x"},
				{host: "b", digest: "sha256:y", revision: "r-y"},
				{host: "c", digest: "sha256:y", revision: "r-y"},
			},
			wantDigest: "sha256:y", wantRevision: "r-y",
		},
		{
			name:       "nothing known",
			uses:       []imageUse{{host: "a"}},
			wantDigest: "", wantRevision: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digest, revision := targetDigest(tt.uses)
			assert.Equal(t, tt.wantDigest, digest)
			assert.Equal(t, tt.wantRevision, revision)
		})
	}
}
