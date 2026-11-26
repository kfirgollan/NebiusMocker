package domain

import (
	"context"
	"fmt"
	"sort"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

// QuotaUsage implements peer.QuotaUsageSource: it counts every stored VPC
// resource, system-created ones included, per project. A resource counts from
// creation until its record is removed, whatever its state. It reads the
// repositories only and never takes core.mu, so it can run inside Admit while
// a create holds that lock.
func (s *Service) QuotaUsage(ctx context.Context) ([]peer.QuotaUsage, error) {
	c := s.c
	acc := map[quotaKey]uint64{}
	for _, count := range []func(context.Context, map[quotaKey]uint64) error{
		c.pools.countUsage, c.networks.countUsage, c.subnets.countUsage, c.allocations.countUsage,
		c.routeTables.countUsage, c.routes.countUsage, c.securityGroups.countUsage, c.securityRules.countUsage,
	} {
		if err := count(ctx, acc); err != nil {
			return nil, err
		}
	}
	out := make([]peer.QuotaUsage, 0, len(acc))
	for k, n := range acc {
		out = append(out, peer.QuotaUsage{ContainerID: k.project, Name: k.name, Amount: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContainerID != out[j].ContainerID {
			return out[i].ContainerID < out[j].ContainerID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type quotaKey struct{ project, name string }

// countUsage adds the consumption of every stored resource of the kind to acc.
func (k *kind[T, S]) countUsage(ctx context.Context, acc map[quotaKey]uint64) error {
	if k.quota == nil {
		return nil
	}
	all, err := k.repo.All(ctx, func(T) bool { return true })
	if err != nil {
		return fmt.Errorf("count %s usage: %w", k.what, err)
	}
	for _, r := range all {
		project, amounts, err := k.quota(ctx, r)
		if errs.IsKind(err, errs.KindNotFound) {
			continue // orphaned by a concurrent delete
		}
		if err != nil {
			return fmt.Errorf("count %s usage: %w", k.what, err)
		}
		for name, n := range amounts {
			if n > 0 {
				acc[quotaKey{project, name}] += uint64(n)
			}
		}
	}
	return nil
}

// ownQuota charges one unit of name to the project the resource lives in.
func ownQuota[T Resource](name string) func(context.Context, T) (string, map[string]int64, error) {
	return func(_ context.Context, r T) (string, map[string]int64, error) {
		return r.GetMetadata().GetParentId(), map[string]int64{name: 1}, nil
	}
}

// routeQuota charges a route to the project of its route table.
func routeQuota(tables Repository[*vpcpb.RouteTable]) func(context.Context, *vpcpb.Route) (string, map[string]int64, error) {
	return func(ctx context.Context, r *vpcpb.Route) (string, map[string]int64, error) {
		t, err := tables.Get(ctx, r.GetMetadata().GetParentId())
		if err != nil {
			return "", nil, err
		}
		return t.GetMetadata().GetParentId(), map[string]int64{quota.VPCRouteCount: 1}, nil
	}
}

// ruleQuota charges a security rule to the project of its security group.
func ruleQuota(groups Repository[*vpcpb.SecurityGroup]) func(context.Context, *vpcpb.SecurityRule) (string, map[string]int64, error) {
	return func(ctx context.Context, r *vpcpb.SecurityRule) (string, map[string]int64, error) {
		g, err := groups.Get(ctx, r.GetMetadata().GetParentId())
		if err != nil {
			return "", nil, err
		}
		return g.GetMetadata().GetParentId(), map[string]int64{quota.VPCSecurityRuleCount: 1}, nil
	}
}

// allocationAmounts is what an allocation consumes: one allocation, and one
// public IPv4 address when it is a public one.
func allocationAmounts(a *vpcpb.Allocation) map[string]int64 {
	m := map[string]int64{quota.VPCAllocationCount: 1}
	if a.GetSpec().GetIpv4Public() != nil {
		m[quota.VPCPublicIPv4Count] = 1
	}
	return m
}

func allocationQuota(_ context.Context, a *vpcpb.Allocation) (project string, amounts map[string]int64, err error) {
	return a.GetMetadata().GetParentId(), allocationAmounts(a), nil
}
