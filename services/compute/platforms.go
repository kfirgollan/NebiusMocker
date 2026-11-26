package compute

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/api"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// registerPlatforms wires PlatformService and returns the service so instances
// can resolve platform and preset names. The "computeplatform" kind is not
// registered with authz: no RPC addresses a platform by ID.
func registerPlatforms(reg grpc.ServiceRegistrar, deps mocker.Deps, regions []domain.Region) (*domain.PlatformService, error) {
	catalog, err := loadPlatformCatalog(deps.Config.ComputePlatformsFile)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	svc, err := domain.NewPlatformService(domain.PlatformDeps{
		Catalog: catalog, Regions: regions, Projects: deps.Authz, ProjectRegions: peerRegions{dir: deps.Peers},
	})
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	if err := peer.Provide[peer.ComputePlatforms](deps.Peers, peer.ComputePlatformsKey, platformPeer{svc: svc}); err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	api.RegisterPlatformAuthzRules(deps.Rules)
	computepb.RegisterPlatformServiceServer(reg, api.NewPlatformHandler(svc))
	return svc, nil
}

// loadPlatformCatalog returns the built-in catalog, or the one in path when set.
func loadPlatformCatalog(path string) (domain.PlatformCatalog, error) {
	if path == "" {
		return domain.DefaultPlatformCatalog()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.PlatformCatalog{}, fmt.Errorf("read platform catalog: %w", err)
	}
	return domain.ParsePlatformCatalog(data)
}

// peerRegions finds the iam project-region capability at call time.
type peerRegions struct{ dir *peer.Directory }

func (p peerRegions) Region(ctx context.Context, projectID string) (region string, ok bool, err error) {
	r, found := peer.Lookup(p.dir, peer.ProjectRegionsKey)
	if !found {
		return "", false, nil
	}
	region, err = r.Region(ctx, projectID)
	return region, err == nil, err
}
