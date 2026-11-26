package compute

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// The instance service is the peer.ComputeNodes capability: mk8s node groups
// get their nodes as managed compute instances through it. It is in-process,
// with no RPC authorization: the consumer authorizes its own request.
var _ peer.ComputeNodes = (*domain.InstanceService)(nil)

// provideNodes makes the instance service available to the mk8s module.
func provideNodes(dir *peer.Directory, svc *domain.InstanceService) error {
	if err := peer.Provide[peer.ComputeNodes](dir, peer.ComputeNodesKey, svc); err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	return nil
}
