package compute

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// registerInstances wires InstanceService. Managed disks follow the
// DiskService rules; NIC addresses come from the vpc module through
// peer.NetworkInterfaces, looked up at call time.
func registerInstances(reg grpc.ServiceRegistrar, deps mocker.Deps, w instanceWiring) error {
	instances := dal.NewInstanceRepo(deps.Store)
	svc, err := domain.NewInstanceService(domain.InstanceDeps{
		Instances: instances, OpIndex: dal.NewInstanceOpIndex(deps.Store), Projects: deps.Authz,
		Platforms: w.platforms, Network: peerNetwork{dir: deps.Peers}, Disks: w.disks,
		GpuClusters: w.gpuClusters, Filesystems: w.filesystems,
		Perms: callerPerms{engine: deps.Authz, mode: deps.Config.Authz, log: deps.Log},
		Ops:   deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindInstance, func(ctx context.Context, id string) (string, error) {
		inst, err := instances.Get(ctx, id)
		return inst.GetMetadata().GetParentId(), err
	})
	api.RegisterInstanceAuthzRules(deps.Rules)
	computepb.RegisterInstanceServiceServer(reg, api.NewInstanceHandler(svc))
	return nil
}

// instanceWiring are the compute services InstanceService builds on.
type instanceWiring struct {
	disks       *domain.DiskService
	platforms   *domain.PlatformService
	gpuClusters domain.GpuClusterMembership
	filesystems domain.FilesystemAttachments
}

// peerNetwork finds the VPC NIC capability at call time.
type peerNetwork struct{ dir *peer.Directory }

func (p peerNetwork) NetworkInterfaces() (peer.NetworkInterfaces, bool) {
	return peer.Lookup(p.dir, peer.NetworkInterfacesKey)
}

// callerPerms checks the caller's own permission on a resource with the
// authz engine, under the configured mode (as the RPC interceptor does for
// the RPC's own rule).
type callerPerms struct {
	engine *authz.Engine
	mode   authz.Mode
	log    *slog.Logger
}

func (c callerPerms) Require(ctx context.Context, perm, resourceID string) error {
	if c.mode == authz.ModeOff {
		return nil
	}
	p := reqctx.PrincipalFrom(ctx)
	var denied error
	if p.Anonymous() {
		denied = errs.Unauthenticated("authorization requires credentials")
	} else {
		d, err := c.engine.Check(ctx, p.ID, authz.Permission(perm), resourceID)
		if err != nil {
			return err
		}
		if d.Allowed {
			return nil
		}
		denied = &errs.Error{
			Kind: errs.KindPermissionDenied, ResourceID: resourceID,
			Message: "permission " + perm + " denied on " + resourceID,
		}
	}
	c.log.WarnContext(ctx, "authz denied", "permission", perm, "resource", resourceID,
		"principal", p.ID, "enforced", c.mode == authz.ModeEnforce)
	if c.mode == authz.ModeEnforce {
		return denied
	}
	return nil
}
