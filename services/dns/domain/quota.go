package domain

import (
	"context"
	"fmt"
	"sort"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// QuotaUsage implements peer.QuotaUsageSource: it counts every stored zone per
// project, from creation until its record is removed. It reads the store only
// and never takes s.mu, so it can run inside Admit while a create holds it.
func (s *Service) QuotaUsage(ctx context.Context) ([]peer.QuotaUsage, error) {
	zones, err := s.zones.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("count zone usage: %w", err)
	}
	perProject := map[string]uint64{}
	for _, z := range zones {
		perProject[z.GetMetadata().GetParentId()]++
	}
	out := make([]peer.QuotaUsage, 0, len(perProject))
	for project, n := range perProject {
		out = append(out, peer.QuotaUsage{ContainerID: project, Name: quota.DNSZoneCount, Amount: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContainerID < out[j].ContainerID })
	return out, nil
}
