package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	capacitypb "github.com/nebius/gosdk/proto/nebius/capacity/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// computeService is status.service of block groups with a compute_v1
// resource affinity (the only variant the API has).
const computeService = "compute"

// Unit is what block groups and allowances count: GPUs.
const Unit = "GPU"

// GetBlockGroup returns a block group by ID.
func (s *Service) GetBlockGroup(ctx context.Context, id string) (*capacitypb.CapacityBlockGroup, error) {
	g, err := s.group(id, "")
	if err != nil {
		return nil, err
	}
	return s.blockGroupProto(ctx, g)
}

// Affinity selects a block group: a tenant, a region, a platform and,
// optionally, a fabric.
type Affinity struct {
	TenantID string
	Region   string
	Platform string
	Fabric   string
}

// GetBlockGroupByAffinity returns the tenant's block group in the region for
// the platform (and the fabric, when set). Without a fabric, the platform
// must identify one group in the region.
func (s *Service) GetBlockGroupByAffinity(ctx context.Context, a Affinity) (*capacitypb.CapacityBlockGroup, error) {
	if err := s.checkTenant(ctx, "parent_id", a.TenantID); err != nil {
		return nil, err
	}
	if a.Platform == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "resource_affinity.compute_v1.platform", Message: "required"})
	}
	var matches []*BlockGroup
	for _, g := range s.inv.InTenant(a.TenantID) {
		if g.Region == a.Region && g.Platform == a.Platform && (a.Fabric == "" || g.Fabric == a.Fabric) {
			matches = append(matches, g)
		}
	}
	switch len(matches) {
	case 0:
		return nil, &errs.Error{Kind: errs.KindNotFound, Message: fmt.Sprintf(
			"no capacity block group for platform %q%s in region %q", a.Platform, fabricSuffix(a.Fabric), a.Region)}
	case 1:
		return s.blockGroupProto(ctx, matches[0])
	default:
		fabrics := make([]string, len(matches))
		for i, g := range matches {
			fabrics[i] = g.Fabric
		}
		return nil, errs.BadRequest(errs.Violation{
			Field:   "resource_affinity.compute_v1.fabric",
			Message: fmt.Sprintf("the tenant has %d capacity block groups for %s in %s; set the fabric (one of %s)", len(matches), a.Platform, a.Region, strings.Join(fabrics, ", ")),
		})
	}
}

func fabricSuffix(fabric string) string {
	if fabric == "" {
		return ""
	}
	return fmt.Sprintf(" on fabric %q", fabric)
}

// ListBlockGroups returns one page of the tenant's block groups.
func (s *Service) ListBlockGroups(ctx context.Context, tenantID string, size int64, pageToken string) (Page[*capacitypb.CapacityBlockGroup], error) {
	if err := s.checkTenant(ctx, "parent_id", tenantID); err != nil {
		return Page[*capacitypb.CapacityBlockGroup]{}, err
	}
	page, err := listPage(s.inv.InTenant(tenantID), size, maxBlockGroupPageSize, pageToken)
	if err != nil {
		return Page[*capacitypb.CapacityBlockGroup]{}, err
	}
	out := Page[*capacitypb.CapacityBlockGroup]{NextPageToken: page.NextPageToken, Items: make([]*capacitypb.CapacityBlockGroup, 0, len(page.Items))}
	for _, g := range page.Items {
		p, err := s.blockGroupProto(ctx, g)
		if err != nil {
			return Page[*capacitypb.CapacityBlockGroup]{}, err
		}
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// ListBlockGroupResources returns the IDs of the resources (compute
// instances) that hold capacity of the block group.
func (s *Service) ListBlockGroupResources(ctx context.Context, id string) ([]string, error) {
	if _, err := s.group(id, ""); err != nil {
		return nil, err
	}
	usage, err := s.usage.ByBlockGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(usage))
	for _, u := range usage {
		ids = append(ids, u.ConsumerID)
	}
	return ids, nil
}

// groupUsage sums the capacity held in the group, in total and by project.
func (s *Service) groupUsage(ctx context.Context, id string) (total uint64, byProject map[string]uint64, err error) {
	usage, err := s.usage.ByBlockGroup(ctx, id)
	if err != nil {
		return 0, nil, err
	}
	byProject = map[string]uint64{}
	for _, u := range usage {
		total += u.Quantity
		byProject[u.ProjectID] += u.Quantity
	}
	return total, byProject, nil
}

func (s *Service) blockGroupProto(ctx context.Context, g *BlockGroup) (*capacitypb.CapacityBlockGroup, error) {
	now := s.clk.Now()
	usage, _, err := s.groupUsage(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	limit := g.limitAt(now)
	st := &capacitypb.CapacityBlockGroupStatus{
		Region:           g.Region,
		ResourceAffinity: g.affinity(),
		Service:          computeService,
		State:            capacitypb.CapacityBlockGroupStatus_STATE_INACTIVE,
		CurrentLimit:     limit,
		Usage:            usage,
		UsagePercentage:  percentage(usage, limit),
		UsageState:       capacitypb.CapacityBlockGroupStatus_USAGE_STATE_NOT_USED,
	}
	if limit > 0 {
		st.State = capacitypb.CapacityBlockGroupStatus_STATE_ACTIVE
	}
	if usage > 0 {
		st.UsageState = capacitypb.CapacityBlockGroupStatus_USAGE_STATE_USED
	}
	if at, to, ok := g.nextChange(now); ok {
		st.NextChangeAt, st.NextChangeTo = timestamppb.New(at), &to
	}
	if c, ok := g.continuousAt(now); ok {
		st.CurrentContinuousInterval = &capacitypb.CurrentContinuousInterval{
			StartTime: timestamppb.New(c.start), EndTime: timestamppb.New(c.end),
			Quantity: c.quantity, State: continuousStateProto(c.state),
		}
	}
	return &capacitypb.CapacityBlockGroup{
		Metadata: catalogMetadata(g.ID, g.TenantID, g.CreatedAt),
		Spec:     &capacitypb.CapacityBlockGroupSpec{},
		Status:   st,
	}, nil
}

func (g *BlockGroup) affinity() *capacitypb.ResourceAffinity {
	return &capacitypb.ResourceAffinity{Versions: &capacitypb.ResourceAffinity_ComputeV1{
		ComputeV1: &capacitypb.ResourceAffinityComputeV1{Fabric: g.Fabric, Platform: g.Platform},
	}}
}

// catalogMetadata is the metadata of a seeded resource: it never changes.
func catalogMetadata(id, parentID string, created time.Time) *commonpb.ResourceMetadata {
	ts := timestamppb.New(created)
	return &commonpb.ResourceMetadata{Id: id, ParentId: parentID, ResourceVersion: 1, CreatedAt: ts, UpdatedAt: ts}
}

func continuousStateProto(s ContinuousState) capacitypb.CurrentContinuousInterval_State {
	switch s {
	case StateScheduled:
		return capacitypb.CurrentContinuousInterval_STATE_SCHEDULED
	case StateActive:
		return capacitypb.CurrentContinuousInterval_STATE_ACTIVE
	case StateExpired:
		return capacitypb.CurrentContinuousInterval_STATE_EXPIRED
	}
	return capacitypb.CurrentContinuousInterval_STATE_UNSPECIFIED
}

// GetInterval returns a capacity interval by ID.
func (s *Service) GetInterval(_ context.Context, id string) (*capacitypb.CapacityInterval, error) {
	iv, ok := s.inv.Interval(id)
	if !ok {
		return nil, errs.NotFound(id)
	}
	g, _ := s.inv.Group(iv.BlockGroupID)
	return s.intervalProto(g, *iv), nil
}

// ListIntervals returns one page of a block group's intervals, by start time.
func (s *Service) ListIntervals(_ context.Context, blockGroupID string, size int64, pageToken string) (Page[*capacitypb.CapacityInterval], error) {
	g, err := s.group(blockGroupID, "")
	if err != nil {
		return Page[*capacitypb.CapacityInterval]{}, err
	}
	page, err := listPage(g.Intervals, size, maxBlockGroupPageSize, pageToken)
	if err != nil {
		return Page[*capacitypb.CapacityInterval]{}, err
	}
	out := Page[*capacitypb.CapacityInterval]{NextPageToken: page.NextPageToken, Items: make([]*capacitypb.CapacityInterval, 0, len(page.Items))}
	for _, iv := range page.Items {
		out.Items = append(out.Items, s.intervalProto(g, iv))
	}
	return out, nil
}

func (s *Service) intervalProto(g *BlockGroup, iv Interval) *capacitypb.CapacityInterval {
	state := capacitypb.CapacityIntervalStatus_STATE_UNSPECIFIED
	switch iv.stateAt(s.clk.Now()) {
	case StateScheduled:
		state = capacitypb.CapacityIntervalStatus_STATE_SCHEDULED
	case StateActive:
		state = capacitypb.CapacityIntervalStatus_STATE_ACTIVE
	case StateExpired:
		state = capacitypb.CapacityIntervalStatus_STATE_EXPIRED
	}
	return &capacitypb.CapacityInterval{
		Metadata: catalogMetadata(iv.ID, g.ID, g.CreatedAt),
		Spec:     &capacitypb.CapacityIntervalSpec{},
		Status: &capacitypb.CapacityIntervalStatus{
			ContainerId: g.TenantID, Region: g.Region, ResourceAffinity: g.affinity(), Service: computeService,
			Quantity: iv.Quantity, StartTime: timestamppb.New(iv.Start), EndTime: timestamppb.New(iv.End), State: state,
		},
	}
}
