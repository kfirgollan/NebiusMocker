package compute

import (
	"fmt"
	"os"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// newGpuClusterMembership builds the GPU cluster membership port that
// InstanceService uses to join and leave clusters (spec.gpu_cluster).
func newGpuClusterMembership(deps mocker.Deps) (*domain.GpuClusterMembers, error) {
	fabrics, err := loadFabrics(deps.Config.ComputeFabricsFile)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	members, err := domain.NewGpuClusterMembers(dal.NewGpuClusterRepo(deps.Store), fabrics)
	if err != nil {
		return nil, fmt.Errorf("compute: %w", err)
	}
	return members, nil
}

// loadFabrics returns the built-in fabric catalog, or the one in path when set.
func loadFabrics(path string) (domain.FabricCatalog, error) {
	if path == "" {
		return domain.DefaultFabricCatalog()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.FabricCatalog{}, fmt.Errorf("read fabric catalog: %w", err)
	}
	return domain.ParseFabricCatalog(data)
}
