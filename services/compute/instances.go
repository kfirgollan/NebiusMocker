package compute

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
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
func (m *Module) registerInstances(reg grpc.ServiceRegistrar, deps mocker.Deps, w instanceWiring) error {
	instances := dal.NewInstanceRepo(deps.Store)
	svc, err := domain.NewInstanceService(domain.InstanceDeps{
		Instances: instances, OpIndex: dal.NewInstanceOpIndex(deps.Store), Projects: deps.Authz,
		Platforms: w.platforms, Network: peerNetwork{dir: deps.Peers}, Disks: w.disks,
		GpuClusters: w.gpuClusters, NVLGroups: w.nvlGroups, Filesystems: w.filesystems, Nodes: dal.NewNodeRepo(deps.Store),
		Perms: callerPerms{engine: deps.Authz, mode: deps.Config.Authz, log: deps.Log},
		Ops:   deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		PublicIPReleaseAfter: deps.Config.ComputePublicIPReleaseAfter,
	})
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	m.publicIPs = publicIPSweeper{svc: svc, clk: deps.Clock, log: deps.Log}
	deps.Authz.RegisterKind(domain.KindInstance, func(ctx context.Context, id string) (string, error) {
		inst, err := instances.Get(ctx, id)
		return inst.GetMetadata().GetParentId(), err
	})
	api.RegisterInstanceAuthzRules(deps.Rules)
	computepb.RegisterInstanceServiceServer(reg, api.NewInstanceHandler(svc))
	return provideNodes(deps.Peers, svc)
}

// publicIPSweepInterval is how often (on the injected clock) the dynamic
// public addresses of instances stopped for longer than
// NEBIUS_MOCKER_COMPUTE_PUBLIC_IP_RELEASE_AFTER are released.
const publicIPSweepInterval = time.Minute

// publicIPSweeper runs InstanceService.SweepPublicIPs in one background
// goroutine between Module.Start and Module.Stop.
type publicIPSweeper struct {
	svc  *domain.InstanceService
	clk  clock.Clock
	log  *slog.Logger
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// Start implements mocker.Lifecycle: it starts the public IP sweeper.
func (m *Module) Start(ctx context.Context) error {
	p := &m.publicIPs
	if p.svc == nil {
		return nil // not registered
	}
	ctx, p.stop = context.WithCancel(ctx)
	p.wg.Go(func() { p.run(ctx) })
	return nil
}

// Stop implements mocker.Lifecycle: it stops the public IP sweeper.
func (m *Module) Stop(context.Context) error {
	p := &m.publicIPs
	if p.stop != nil {
		p.stop()
	}
	p.wg.Wait()
	return nil
}

func (p *publicIPSweeper) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.clk.After(publicIPSweepInterval):
		}
		if n, err := p.svc.SweepPublicIPs(ctx); err != nil {
			p.log.WarnContext(ctx, "release public IPs of stopped instances", "released", n, "error", err)
		} else if n > 0 {
			p.log.InfoContext(ctx, "released public IPs of stopped instances", "instances", n)
		}
	}
}

// instanceWiring are the compute services InstanceService builds on.
type instanceWiring struct {
	disks       *domain.DiskService
	platforms   *domain.PlatformService
	gpuClusters domain.GpuClusterMembership
	filesystems domain.FilesystemAttachments
	nvlGroups   domain.NVLMembership
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
