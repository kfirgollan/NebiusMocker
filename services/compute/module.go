// Package compute is the NebiusMocker module for Nebius Compute
// (compute.api.nebius.cloud).
//
// Implemented: ImageService (public catalog per region and custom images
// imported from Object Storage), with image payloads in the shared object
// store (pkg/objstore); DiskService and DiskSnapshotService; GpuClusterService
// (inventory only). See docs/services/compute.md.
package compute

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/backend/blobstore"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// Module is the Compute service module.
type Module struct {
	// publicIPs releases the dynamic public addresses of long-stopped
	// instances (see instances.go).
	publicIPs publicIPSweeper
}

var (
	_ mocker.Module    = (*Module)(nil)
	_ mocker.Lifecycle = (*Module)(nil)
)

// New returns the Compute module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "compute" }

// Register seeds the public image catalog and registers the Compute gRPC services.
func (m *Module) Register(ctx context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	regions := make([]domain.Region, 0, len(deps.Config.Regions))
	for _, r := range deps.Config.Regions {
		regions = append(regions, domain.Region{Name: r.Name, RoutingCode: r.RoutingCode})
	}
	catalog, err := loadCatalog(deps.Config.ComputeImagesFile)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	blobs, err := blobstore.New(ctx, deps.ObjStore)
	if err != nil {
		return err
	}
	images := dal.NewImageRepo(deps.Store)
	if err := domain.SeedPublicCatalog(ctx, domain.SeedDeps{Images: images, Blobs: blobs, Clock: deps.Clock}, catalog, regions); err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	locks, err := domain.NewDiskLocks(dal.NewDiskRepo(deps.Store))
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	svc, err := domain.NewImageService(domain.ImageDeps{
		Images: images, OpIndex: dal.NewOpIndex(deps.Store), Blobs: blobs, Projects: deps.Authz,
		Disks: dal.NewDiskReader(deps.Store), DiskLocks: locks,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Regions: regions,
	})
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindImage, func(ctx context.Context, id string) (string, error) {
		img, err := images.Get(ctx, id)
		return img.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules, svc)
	computepb.RegisterImageServiceServer(reg, api.NewImageHandler(svc))
	// InstanceService joins GPU clusters (spec.gpu_cluster) and attaches
	// shared filesystems (spec.filesystems) through these ports.
	gpuClusters, err := registerGpuClusters(reg, deps)
	if err != nil {
		return err
	}
	filesystems, err := newFilesystemAttachments(deps)
	if err != nil {
		return err
	}
	if err := registerFilesystems(reg, deps); err != nil {
		return err
	}
	platforms, err := registerPlatforms(reg, deps, regions)
	if err != nil {
		return err
	}
	disks, err := registerDisks(reg, deps, svc)
	if err != nil {
		return err
	}
	return m.registerInstances(reg, deps, instanceWiring{
		disks: disks, platforms: platforms, gpuClusters: gpuClusters, filesystems: filesystems,
	})
}

// loadCatalog returns the built-in catalog, or the one in path when set.
func loadCatalog(path string) (domain.Catalog, error) {
	if path == "" {
		return domain.DefaultCatalog()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Catalog{}, fmt.Errorf("read image catalog: %w", err)
	}
	return domain.ParseCatalog(data)
}
