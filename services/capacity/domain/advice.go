package domain

import (
	"context"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
)

// ListResourceAdvice returns one page of resource advice for the tenant: one
// item per (block group, VM preset of its platform). Reserved availability
// is computed from the block group's free capacity. The mocker simulates no
// physical on-demand or preemptible capacity, so those blocks report
// DATA_STATE_UNKNOWN / AVAILABILITY_LEVEL_UNKNOWN ("failed to retrieve
// data") rather than invented numbers.
func (s *Service) ListResourceAdvice(ctx context.Context, tenantID string, size int64, pageToken string) (Page[*capacitypb.ResourceAdvice], error) {
	if err := s.checkTenant(ctx, "parent_id", tenantID); err != nil {
		return Page[*capacitypb.ResourceAdvice]{}, err
	}
	type entry struct {
		g      *BlockGroup
		p      PlatformDef
		preset PresetDef
	}
	var all []entry
	for _, g := range s.inv.InTenant(tenantID) {
		p, ok := s.inv.presets[g.Platform]
		if !ok {
			continue
		}
		for _, pr := range p.Presets {
			all = append(all, entry{g, p, pr})
		}
	}
	page, err := listPage(all, size, maxAllowancePageSize, pageToken)
	if err != nil {
		return Page[*capacitypb.ResourceAdvice]{}, err
	}
	now := s.clk.Now()
	out := Page[*capacitypb.ResourceAdvice]{NextPageToken: page.NextPageToken, Items: make([]*capacitypb.ResourceAdvice, 0, len(page.Items))}
	for _, e := range page.Items {
		usage, _, err := s.groupUsage(ctx, e.g.ID)
		if err != nil {
			return Page[*capacitypb.ResourceAdvice]{}, err
		}
		out.Items = append(out.Items, adviceProto(tenantID, e.g, e.p, e.preset, e.g.limitAt(now), usage, now))
	}
	return out, nil
}

func adviceProto(tenantID string, g *BlockGroup, p PlatformDef, pr PresetDef, limit, usage uint64, now time.Time) *capacitypb.ResourceAdvice {
	per := uint64(max(pr.GPUCount, 1)) // validated > 0 by ParseCatalog
	free := uint64(0)
	if limit > usage {
		free = limit - usage
	}
	reserved := &capacitypb.ResourceAdviceStatus_Availability{
		DataState:         capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_FRESH,
		Available:         clampUint32(free / per),
		Limit:             clampUint32(limit / per),
		AvailabilityLevel: capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_HIGH,
		EffectiveAt:       timestamppb.New(now),
	}
	if free < per {
		reserved.AvailabilityLevel = capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_LIMIT_REACHED
	}
	unknown := func() *capacitypb.ResourceAdviceStatus_Availability {
		return &capacitypb.ResourceAdviceStatus_Availability{
			DataState:         capacitypb.ResourceAdviceStatus_Availability_DATA_STATE_UNKNOWN,
			AvailabilityLevel: capacitypb.ResourceAdviceStatus_Availability_AVAILABILITY_LEVEL_UNKNOWN,
			EffectiveAt:       timestamppb.New(now),
		}
	}
	return &capacitypb.ResourceAdvice{
		Metadata: &commonpb.ResourceMetadata{ParentId: tenantID},
		Spec: &capacitypb.ResourceAdviceSpec{
			Region: g.Region,
			Fabric: g.Fabric,
			ResourceDetails: &capacitypb.ResourceAdviceSpec_ComputeInstance{ComputeInstance: &capacitypb.ComputeInstanceDetails{
				Platform: g.Platform,
				Preset: &capacitypb.ComputeInstanceDetails_Preset{
					Name: pr.Name,
					Resources: &capacitypb.ComputeInstanceDetails_Preset_Resources{
						VcpuCount: pr.VCPUCount, MemoryGibibytes: pr.MemoryGibibytes, GpuCount: pr.GPUCount,
					},
				},
				GpuMemoryGigabytes: p.GPUMemoryGigabytes,
			}},
		},
		Status: &capacitypb.ResourceAdviceStatus{Reserved: reserved, OnDemand: unknown(), Preemptible: unknown()},
	}
}

func clampUint32(v uint64) uint32 { return uint32(min(v, math.MaxUint32)) }
