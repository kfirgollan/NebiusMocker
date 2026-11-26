package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// registerNVLInstanceGroups wires NVLInstanceGroupService. It returns the
// membership port that InstanceService joins and leaves groups through
// (spec.nvl_instance_group_id); platforms tells which group type a platform's
// instances can join.
func registerNVLInstanceGroups(reg grpc.ServiceRegistrar, deps mocker.Deps, platforms domain.NVLPlatforms) (*domain.NVLMembers, error) {
	groups := dal.NewNVLInstanceGroupRepo(deps.Store)
	members, err := domain.NewNVLMembers(groups, platforms)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	svc, err := domain.NewNVLInstanceGroupService(domain.NVLInstanceGroupDeps{
		Groups: groups, Instances: dal.NewInstanceRepo(deps.Store), Projects: deps.Authz,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindNVLInstanceGroup, func(ctx context.Context, id string) (string, error) {
		g, err := groups.Get(ctx, id)
		return g.GetMetadata().GetParentId(), err
	})
	api.RegisterNVLInstanceGroupAuthzRules(deps.Rules)
	computepb.RegisterNVLInstanceGroupServiceServer(reg, api.NewNVLInstanceGroupHandler(svc))
	return members, nil
}
