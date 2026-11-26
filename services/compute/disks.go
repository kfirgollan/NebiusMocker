package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// registerDisks wires DiskService and DiskSnapshotService. Disk sources are
// resolved through the image service of the same module.
func registerDisks(reg grpc.ServiceRegistrar, deps mocker.Deps, images domain.ImageLookup) (*domain.DiskService, error) {
	disks, snapshots := dal.NewDiskRepo(deps.Store), dal.NewSnapshotRepo(deps.Store)
	dd := domain.DiskDeps{
		Disks: disks, Snapshots: snapshots, Images: images, OpIndex: dal.NewDiskOpIndex(deps.Store),
		Projects: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Quota: peer.QuotaGateFor(deps.Peers),
	}
	diskSvc, err := domain.NewDiskService(dd)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	snapSvc, err := domain.NewSnapshotService(dd)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindDisk, func(ctx context.Context, id string) (string, error) {
		d, err := disks.Get(ctx, id)
		return d.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindDiskSnapshot, func(ctx context.Context, id string) (string, error) {
		s, err := snapshots.Get(ctx, id)
		return s.GetMetadata().GetParentId(), err
	})
	api.RegisterDiskAuthzRules(deps.Rules)
	computepb.RegisterDiskServiceServer(reg, api.NewDiskHandler(diskSvc))
	computepb.RegisterDiskSnapshotServiceServer(reg, api.NewSnapshotHandler(snapSvc))
	return diskSvc, nil
}
