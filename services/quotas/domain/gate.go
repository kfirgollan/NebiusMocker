package domain

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

var _ peer.QuotaGate = (*Service)(nil)

// usageKey identifies the consumption of one quota by one container in one region.
type usageKey struct{ container, region, name string }

// snapshot is the usage of every container, computed from the areas' stores.
type snapshot struct {
	usage map[usageKey]uint64
	areas map[string]bool // areas that reported (their quotas' usage is known)
	// tenants and regions cache the hierarchy lookups made while building it.
	tenants map[string]string
	regions map[string]string
}

// snapshot asks every area for its usage and charges each entry to its
// container and, for a project, to the project's tenant.
func (s *Service) snapshot(ctx context.Context) (*snapshot, error) {
	snap := &snapshot{usage: map[usageKey]uint64{}, areas: map[string]bool{}, tenants: map[string]string{}, regions: map[string]string{}}
	sources := s.sources()
	for _, area := range slices.Sorted(maps.Keys(sources)) {
		us, err := sources[area].QuotaUsage(ctx)
		if err != nil {
			return nil, fmt.Errorf("quota usage of %s: %w", area, err)
		}
		snap.areas[area] = true
		for _, u := range us {
			if err := s.charge(ctx, snap, u); err != nil {
				return nil, err
			}
		}
	}
	return snap, nil
}

func (s *Service) charge(ctx context.Context, snap *snapshot, u peer.QuotaUsage) error {
	region := u.Region
	if region == "" {
		r, ok, err := s.projectRegion(ctx, snap, u.ContainerID)
		if err != nil || !ok {
			return err // a resource of a project that is gone counts nowhere
		}
		region = r
	}
	snap.usage[usageKey{u.ContainerID, region, u.Name}] += u.Amount
	tenant, ok, err := s.tenantOf(ctx, snap, u.ContainerID)
	if err != nil || !ok {
		return err
	}
	snap.usage[usageKey{tenant, region, u.Name}] += u.Amount
	return nil
}

// tenantOf returns the tenant of a project; ok is false for a tenant or a
// project that no longer exists.
func (s *Service) tenantOf(ctx context.Context, snap *snapshot, container string) (tenant string, ok bool, err error) {
	if !strings.HasPrefix(container, KindProject+"-") {
		return "", false, nil
	}
	if t, ok := snap.tenants[container]; ok {
		return t, t != "", nil
	}
	t, err := s.hierarchy.Parent(ctx, container)
	if errs.IsKind(err, errs.KindNotFound) {
		t, err = "", nil
	}
	if err != nil {
		return "", false, fmt.Errorf("tenant of %s: %w", container, err)
	}
	snap.tenants[container] = t
	return t, t != "", nil
}

func (s *Service) projectRegion(ctx context.Context, snap *snapshot, project string) (region string, ok bool, err error) {
	if r, ok := snap.regions[project]; ok {
		return r, r != "", nil
	}
	if !strings.HasPrefix(project, KindProject+"-") {
		return "", false, errs.Internal("quota usage of " + project + " has no region")
	}
	r, err := s.regions.Region(ctx, project)
	if errs.IsKind(err, errs.KindNotFound) {
		r, err = "", nil
	}
	if err != nil {
		return "", false, fmt.Errorf("region of %s: %w", project, err)
	}
	snap.regions[project] = r
	return r, r != "", nil
}

// Admit implements peer.QuotaGate.
func (s *Service) Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error {
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := s.check(ctx, c); err != nil {
		return err
	}
	if apply == nil {
		return nil
	}
	return apply(ctx)
}

func (s *Service) check(ctx context.Context, c peer.QuotaClaim) error {
	positive := false
	for _, d := range c.Amounts {
		positive = positive || d > 0
	}
	if !positive {
		return nil
	}
	snap, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	region := c.Region
	if region == "" {
		r, ok, err := s.projectRegion(ctx, snap, c.ContainerID)
		if err != nil {
			return err
		}
		if !ok {
			return errs.NotFound(c.ContainerID)
		}
		region = r
	}
	containers := []string{c.ContainerID}
	if t, ok, err := s.tenantOf(ctx, snap, c.ContainerID); err != nil {
		return err
	} else if ok {
		containers = append(containers, t)
	}
	violations, err := s.violations(ctx, snap, c.Amounts, containers, region)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, len(violations))
	for i, v := range violations {
		msgs[i] = v.Message
	}
	return errs.QuotaExceeded(strings.Join(msgs, "; "), violations...)
}

// violations checks every positive amount against the allowance of each
// container in region.
func (s *Service) violations(
	ctx context.Context, snap *snapshot, amounts map[string]int64, containers []string, region string,
) ([]errs.QuotaViolation, error) {
	var out []errs.QuotaViolation
	for _, name := range slices.Sorted(maps.Keys(amounts)) {
		delta := amounts[name]
		if delta <= 0 {
			continue
		}
		e, ok := s.catalog.Lookup(name)
		if !ok {
			return nil, errs.Internal("claim of unknown quota " + name)
		}
		for _, container := range containers {
			limit, err := s.limitOf(ctx, container, region, e)
			if err != nil {
				return nil, err
			}
			used := snap.usage[usageKey{container, region, name}]
			if used+uint64(delta) <= limit {
				continue
			}
			out = append(out, errs.QuotaViolation{
				Quota: name, Limit: strconv.FormatUint(limit, 10), Requested: strconv.FormatInt(delta, 10), Region: region,
				Message: fmt.Sprintf("quota %s of %s in %s exceeded: limit %d %s, used %d, requested %d",
					name, container, region, limit, e.Unit, used, delta),
			})
		}
	}
	return out, nil
}

// limitOf is the container's allowance of e in region: the value set on its
// QuotaAllowance, else the catalog default.
func (s *Service) limitOf(ctx context.Context, container, region string, e quota.Entry) (uint64, error) {
	a, err := s.repo.ByKey(ctx, container, e.Name, region)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return e.Default, nil
	case err != nil:
		return 0, err
	case a.Msg.GetSpec().Limit != nil:
		return a.Msg.GetSpec().GetLimit(), nil
	default:
		return e.Default, nil
	}
}

// view renders one allowance with live status.
func (s *Service) view(ctx context.Context, a Allowance) (*quotaspb.QuotaAllowance, error) {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return s.render(a, snap), nil
}

// render fills the effective limit and the status of a.
func (s *Service) render(a Allowance, snap *snapshot) *quotaspb.QuotaAllowance {
	m := proto.CloneOf(a.Msg)
	md := m.GetMetadata()
	e, known := s.catalog.Lookup(md.GetName())
	if m.GetSpec() == nil {
		m.Spec = &quotaspb.QuotaAllowanceSpec{}
	}
	if m.GetSpec().Limit == nil {
		m.Spec.Limit = proto.Uint64(e.Default)
	}
	used := snap.usage[usageKey{md.GetParentId(), a.Region, md.GetName()}]
	st := &quotaspb.QuotaAllowanceStatus{
		State:              quotaspb.QuotaAllowanceStatus_STATE_ACTIVE,
		Usage:              used,
		Service:            e.Service,
		ServiceDescription: e.ServiceDescription,
		Description:        e.Description,
		Unit:               e.Unit,
		UsagePercentage:    percentage(used, m.GetSpec().GetLimit()),
	}
	switch {
	case !known || !snap.areas[e.Service]:
		st.UsageState = quotaspb.QuotaAllowanceStatus_USAGE_STATE_UNKNOWN
	case used > 0:
		st.UsageState = quotaspb.QuotaAllowanceStatus_USAGE_STATE_USED
	default:
		st.UsageState = quotaspb.QuotaAllowanceStatus_USAGE_STATE_NOT_USED
	}
	m.Status = st
	return m
}

// percentage is usage/limit with two decimals ("0.12"); it exceeds 1 when
// usage is over the limit, and a zero limit in use reads as "1.00".
func percentage(used, limit uint64) string {
	switch {
	case limit > 0:
		return strconv.FormatFloat(float64(used)/float64(limit), 'f', 2, 64)
	case used > 0:
		return "1.00"
	default:
		return "0.00"
	}
}
