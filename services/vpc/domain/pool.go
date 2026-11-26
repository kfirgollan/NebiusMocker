package domain

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Names of the pools seeded in every project.
const (
	DefaultPrivatePoolName = "default-network-pool"
	DefaultPublicPoolName  = "default-public-pool"
)

// PoolService implements nebius.vpc.v1.PoolService.
type PoolService struct {
	*kind[*vpcpb.Pool, *vpcpb.PoolSpec]
}

func newPoolService(c *core) *PoolService {
	s := &PoolService{}
	s.kind = &kind[*vpcpb.Pool, *vpcpb.PoolSpec]{
		c: c, prefix: KindPool, what: "pool", repo: c.r.Pools,
		newMsg: func() *vpcpb.Pool { return &vpcpb.Pool{} }, newSpec: func() *vpcpb.PoolSpec { return &vpcpb.PoolSpec{} },
		build: s.build, apply: s.apply, checkDelete: s.checkDelete, remove: s.remove, decorate: s.decorate,
	}
	return s
}

// ListBySourcePool returns one page of the pools carved from poolID.
func (s *PoolService) ListBySourcePool(ctx context.Context, poolID string, pageSize int64, pageToken string) (Page[*vpcpb.Pool], error) {
	return s.listWhere(ctx, func(p *vpcpb.Pool) bool { return p.GetSpec().GetSourcePoolId() == poolID }, pageSize, pageToken)
}

func (s *PoolService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.PoolSpec) (*vpcpb.Pool, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	if spec.GetVersion() != vpcpb.IpVersion_IPV4 {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.version", Message: "only IPV4 is supported"})
	}
	if spec.GetVisibility() == vpcpb.IpVisibility_IP_VISIBILITY_UNSPECIFIED {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.visibility", Message: "required"})
	}
	p := &vpcpb.Pool{Metadata: md, Spec: spec, Status: &vpcpb.PoolStatus{State: vpcpb.PoolStatus_READY}}
	if err := s.resolve(ctx, nil, p); err != nil {
		return nil, err
	}
	scope, err := s.scopeLocked(ctx, p)
	if err != nil {
		return nil, err
	}
	p.Status.ScopeId = scope
	return p, nil
}

// scopeLocked returns the scope of p: its source pool's scope, or for a root
// pool the project's scope of its visibility (created on first use); core.mu
// must be held.
func (s *PoolService) scopeLocked(ctx context.Context, p *vpcpb.Pool) (string, error) {
	if srcID := p.GetSpec().GetSourcePoolId(); srcID != "" {
		src, err := ref(ctx, s.c.r.Pools, "spec.source_pool_id", srcID)
		if err != nil {
			return "", err
		}
		if id := src.GetStatus().GetScopeId(); id != "" {
			return id, nil
		}
		return s.scopeLocked(ctx, src)
	}
	return s.c.scopes.ensureLocked(ctx, p.GetMetadata().GetParentId(), p.GetSpec().GetVisibility())
}

// resolve fills defaults and status.cidrs of p; stored is its previous version.
func (s *PoolService) resolve(ctx context.Context, stored, p *vpcpb.Pool) error {
	for _, pc := range p.GetSpec().GetCidrs() {
		if pc.GetState() == vpcpb.AddressBlockState_STATE_UNSPECIFIED {
			pc.State = vpcpb.AddressBlockState_AVAILABLE
		}
		if pc.GetMaxMaskLength() == 0 {
			pc.MaxMaskLength = ipv4Bits
		}
	}
	plan, err := s.plan(ctx, p)
	if err != nil {
		return err
	}
	if stored != nil {
		plan.prev = newPrev(poolSpecCIDRs(stored), stored.GetStatus().GetCidrs())
	}
	got, err := plan.resolve(func(i int) string { return fmt.Sprintf("spec.cidrs[%d].cidr", i) }, poolSpecCIDRs(p), ipv4Bits)
	if err != nil {
		return err
	}
	p.Status.Cidrs = cidrStrings(got)
	return nil
}

func poolSpecCIDRs(p *vpcpb.Pool) []string {
	out := make([]string, 0, len(p.GetSpec().GetCidrs()))
	for _, pc := range p.GetSpec().GetCidrs() {
		out = append(out, pc.GetCidr())
	}
	return out
}

// plan returns where p's blocks may go: inside its source pool, or anywhere
// (explicit CIDRs only) for a root pool.
func (s *PoolService) plan(ctx context.Context, p *vpcpb.Pool) (blockPlan, error) {
	srcID := p.GetSpec().GetSourcePoolId()
	if srcID == "" {
		var space []netip.Prefix
		for i, c := range poolSpecCIDRs(p) {
			if strings.HasPrefix(c, "/") {
				return blockPlan{}, errs.BadRequest(errs.Violation{
					Field: fmt.Sprintf("spec.cidrs[%d].cidr", i), Message: "a prefix length needs spec.source_pool_id to allocate from",
				})
			}
			r, err := parseBlock(fmt.Sprintf("spec.cidrs[%d].cidr", i), c, ipv4Bits)
			if err != nil {
				return blockPlan{}, err
			}
			space = append(space, r.prefix)
		}
		return blockPlan{what: "pool", space: space}, nil
	}
	src, err := ref(ctx, s.c.r.Pools, "spec.source_pool_id", srcID)
	if err != nil {
		return blockPlan{}, err
	}
	if src.GetSpec().GetVisibility() != p.GetSpec().GetVisibility() || src.GetSpec().GetVersion() != p.GetSpec().GetVersion() {
		return blockPlan{}, errs.BadRequest(errs.Violation{Field: "spec.source_pool_id", Message: "source pool must have the same version and visibility"})
	}
	used, err := s.c.occupied(ctx, srcID, p.GetMetadata().GetId())
	if err != nil {
		return blockPlan{}, err
	}
	return planFor("pool", poolBlocks(src), used), nil
}

func (s *PoolService) apply(ctx context.Context, stored, updated *vpcpb.Pool) error {
	if s.isPublicDefault(stored) {
		return errs.BadState(stored.GetMetadata().GetId(), "the default public pool cannot be changed")
	}
	o, n := stored.GetSpec(), updated.GetSpec()
	switch {
	case o.GetSourcePoolId() != n.GetSourcePoolId():
		return immutable("spec.source_pool_id")
	case o.GetVersion() != n.GetVersion():
		return immutable("spec.version")
	case o.GetVisibility() != n.GetVisibility():
		return immutable("spec.visibility")
	}
	if err := s.resolve(ctx, stored, updated); err != nil {
		return err
	}
	return s.checkShrink(ctx, stored, updated)
}

// checkShrink refuses removing a block that still has carved-out space.
func (s *PoolService) checkShrink(ctx context.Context, stored, updated *vpcpb.Pool) error {
	kept := mustCIDRs(updated.GetStatus().GetCidrs())
	used, err := s.c.occupied(ctx, stored.GetMetadata().GetId(), "")
	if err != nil {
		return err
	}
	for _, old := range mustCIDRs(stored.GetStatus().GetCidrs()) {
		if slices.Contains(kept, old) {
			continue
		}
		if u, hit := firstOverlap(old, used); hit {
			return errs.BadState(stored.GetMetadata().GetId(), fmt.Sprintf("cannot remove %s: %s is still in use", old, u))
		}
	}
	return nil
}

// remove deletes a pool, and its scope once no pool is left in it: scopes are
// system-managed and must not keep an otherwise empty project alive.
func (s *PoolService) remove(ctx context.Context, p *vpcpb.Pool) error {
	if err := s.repo.Delete(ctx, p.GetMetadata().GetId()); err != nil {
		return err
	}
	scope := p.GetStatus().GetScopeId()
	if scope == "" {
		return nil
	}
	left, err := s.c.r.Pools.All(ctx, func(o *vpcpb.Pool) bool { return o.GetStatus().GetScopeId() == scope })
	if err != nil || len(left) > 0 {
		return err
	}
	if err := s.c.r.Scopes.Delete(ctx, scope); err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	return nil
}

func (s *PoolService) isPublicDefault(p *vpcpb.Pool) bool {
	return p.GetMetadata().GetName() == DefaultPublicPoolName && p.GetSpec().GetSourcePoolId() == "" &&
		p.GetSpec().GetVisibility() == vpcpb.IpVisibility_PUBLIC
}

func (s *PoolService) checkDelete(ctx context.Context, p *vpcpb.Pool) error {
	id := p.GetMetadata().GetId()
	if s.isPublicDefault(p) {
		return errs.BadState(id, "the default public pool cannot be deleted")
	}
	users, err := s.users(ctx, id)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return inUse(id, "pool", users)
	}
	return nil
}

// users lists every resource that references or draws from the pool.
func (s *PoolService) users(ctx context.Context, id string) ([]string, error) {
	nets, subs, err := s.assignment(ctx, id)
	if err != nil {
		return nil, err
	}
	children, err := s.c.r.Pools.All(ctx, func(p *vpcpb.Pool) bool { return p.GetSpec().GetSourcePoolId() == id })
	if err != nil {
		return nil, err
	}
	allocs, err := s.c.r.Allocations.All(ctx, func(a *vpcpb.Allocation) bool { return a.GetStatus().GetDetails().GetPoolId() == id })
	if err != nil {
		return nil, err
	}
	users := slices.Concat(nets, subs, idsOf(children), idsOf(allocs))
	return users, nil
}

// assignment returns the networks that use the pool and the subnets that draw from it.
func (s *PoolService) assignment(ctx context.Context, id string) (networks, subnets []string, err error) {
	nets, err := s.c.r.Networks.All(ctx, func(n *vpcpb.Network) bool {
		return slices.Contains(networkPoolIDs(n, vpcpb.IpVisibility_PRIVATE), id) ||
			slices.Contains(networkPoolIDs(n, vpcpb.IpVisibility_PUBLIC), id)
	})
	if err != nil {
		return nil, nil, err
	}
	subs, err := s.c.r.Subnets.All(ctx, func(sn *vpcpb.Subnet) bool {
		_, ok := carved(sn)[id]
		return ok
	})
	if err != nil {
		return nil, nil, err
	}
	return idsOf(nets), idsOf(subs), nil
}

func (s *PoolService) decorate(ctx context.Context, p *vpcpb.Pool) error {
	nets, subs, err := s.assignment(ctx, p.GetMetadata().GetId())
	if err != nil {
		return err
	}
	if p.GetStatus() == nil {
		p.Status = &vpcpb.PoolStatus{State: vpcpb.PoolStatus_READY}
	}
	p.Status.Assignment = &vpcpb.PoolAssignment{Networks: nets, Subnets: subs}
	return nil
}

// seedPool creates a root pool unless one with the name exists; core.mu must be held.
func (s *PoolService) seedPool(ctx context.Context, projectID, name, cidr string, vis vpcpb.IpVisibility) error {
	if _, err := s.repo.GetByName(ctx, projectID, name); err == nil {
		return nil
	} else if !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	id, err := s.c.ids.New(KindPool)
	if err != nil {
		return err
	}
	spec := &vpcpb.PoolSpec{
		Version: vpcpb.IpVersion_IPV4, Visibility: vis,
		Cidrs: []*vpcpb.PoolCidr{{Cidr: cidr}},
	}
	_, err = s.createLocked(ctx, s.c.metadata(id, projectID, name, nil), spec)
	return err
}
