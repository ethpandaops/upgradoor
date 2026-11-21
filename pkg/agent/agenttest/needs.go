package agenttest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// leaseTTL is how long a lease of Lease is valid.
const leaseTTL = time.Minute

// Lease builds a lease granted at now, valid for leaseTTL, granting needs.
func Lease(now time.Time, id string, needs ...protocol.Need) *protocol.Lease {
	return &protocol.Lease{
		ID: id, GrantedAt: now, ExpiresAt: now.Add(leaseTTL),
		TTLSeconds: int64(leaseTTL / time.Second), Needs: needs,
	}
}

// ImageNeedFor is the image need of a container from one digest to another.
func ImageNeedFor(container, image, from, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.ImageNeedID(container, to),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: container, Image: image, FromDigest: from, ToDigest: to,
		},
	}
}

// RebootNeed is the observed reboot need with the reason flag.
func RebootNeed() protocol.Need {
	return protocol.Need{
		ID: protocol.RebootNeedID, Kind: protocol.NeedReboot,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonFlag}},
	}
}

// PackagesNeed is the packages need of the docker-ce upgrade of DockerUpgrade.
func PackagesNeed() protocol.Need {
	pkgs := []protocol.PackageChange{{Name: docker, From: dockerV1, To: "28.1"}}

	return protocol.Need{
		ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}
}

// NeedIDs returns the IDs of the needs a sync request reported.
func NeedIDs(req *protocol.SyncRequest) []string {
	ids := make([]string, 0, len(req.Needs))
	for _, n := range req.Needs {
		ids = append(ids, n.ID)
	}

	return ids
}

// ContainerOf returns the reported container of that name or fails the test.
func ContainerOf(t *testing.T, req *protocol.SyncRequest, name string) protocol.Container {
	t.Helper()

	for i := range req.Containers {
		if req.Containers[i].Name == name {
			return req.Containers[i]
		}
	}

	require.FailNow(t, "container not reported", name)

	return protocol.Container{}
}

// StepOf returns the step of a need in an operation or fails the test.
func StepOf(t *testing.T, op *protocol.Operation, needID string) protocol.Step {
	t.Helper()

	for _, s := range op.Steps {
		if s.NeedID == needID {
			return s
		}
	}

	require.FailNow(t, "step not reported", needID)

	return protocol.Step{}
}

// AckOf returns the acknowledgement of a directive in a sync request, or nil.
func AckOf(req *protocol.SyncRequest, id string) *protocol.DirectiveAck {
	for i := range req.DirectiveAcks {
		if req.DirectiveAcks[i].ID == id {
			return &req.DirectiveAcks[i]
		}
	}

	return nil
}
