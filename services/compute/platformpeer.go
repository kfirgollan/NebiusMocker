package compute

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// platformPeer serves peer.ComputePlatformsKey from the platform catalog.
type platformPeer struct{ svc *domain.PlatformService }

var _ peer.ComputePlatforms = platformPeer{}

func (p platformPeer) Platform(ctx context.Context, projectID, name string) (peer.ComputePlatform, error) {
	pl, err := p.svc.GetByName(ctx, projectID, name)
	if err != nil {
		return peer.ComputePlatform{}, err
	}
	out := peer.ComputePlatform{
		Name:                   pl.GetMetadata().GetName(),
		AllowedForPreemptibles: pl.GetStatus().GetAllowedForPreemptibles(),
	}
	for _, pr := range pl.GetSpec().GetPresets() {
		r := pr.GetResources()
		out.Presets = append(out.Presets, peer.ComputePreset{
			Name: pr.GetName(), GPUs: int(r.GetGpuCount()), VCPUs: int(r.GetVcpuCount()), MemoryGiB: int64(r.GetMemoryGibibytes()),
		})
	}
	return out, nil
}
