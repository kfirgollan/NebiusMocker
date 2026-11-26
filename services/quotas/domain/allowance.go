package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Service implements QuotaAllowanceService and the quota gate.
type Service struct {
	repo      Repository
	hierarchy Hierarchy
	regions   ProjectRegions
	sources   UsageSources
	catalog   *quota.Catalog
	ops       Operations
	clk       clock.Clock
	all       []Region // every emulated region; a tenant has allowances in each

	// gate serializes Admit: usage is computed and the area's write applied
	// under it, so concurrent claims cannot both fit under one limit.
	gate sync.Mutex
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Repo      Repository
	Hierarchy Hierarchy
	Regions   ProjectRegions
	Sources   UsageSources
	Catalog   *quota.Catalog
	Ops       Operations
	Clock     clock.Clock
	// AllRegions are the emulated regions, the default first.
	AllRegions []Region
}

// NewService wires a Service.
func NewService(d Deps) (*Service, error) {
	if d.Repo == nil || d.Hierarchy == nil || d.Regions == nil || d.Sources == nil || d.Catalog == nil ||
		d.Ops == nil || d.Clock == nil || len(d.AllRegions) == 0 {
		return nil, errors.New("quotas: service dependencies must not be nil")
	}
	return &Service{
		repo: d.Repo, hierarchy: d.Hierarchy, regions: d.Regions, sources: d.Sources, catalog: d.Catalog,
		ops: d.Ops, clk: d.Clock, all: slices.Clone(d.AllRegions),
	}, nil
}

// CreateCmd is the input of Create.
type CreateCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *quotaspb.QuotaAllowanceSpec
	Request  proto.Message
}

// UpdateCmd is the input of Update (full replace guarded by a reset mask).
type UpdateCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *quotaspb.QuotaAllowanceSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns an allowance with live status.
func (s *Service) Get(ctx context.Context, id string) (*quotaspb.QuotaAllowance, error) {
	a, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, a)
}

// GetByName returns the allowance of quota name in region for a container.
func (s *Service) GetByName(ctx context.Context, parentID, name, region string) (*quotaspb.QuotaAllowance, error) {
	if _, err := s.containerRegions(ctx, parentID); err != nil {
		return nil, err
	}
	a, err := s.ensureOne(ctx, parentID, name, region, "name")
	if err != nil {
		return nil, err
	}
	return s.view(ctx, a)
}

// List returns one page of a container's allowances: one per catalog quota
// and region (a project's region, or every region for a tenant), whether or
// not anyone has set it, plus any other allowance set explicitly.
func (s *Service) List(ctx context.Context, parentID string, pageSize int64, pageToken string) ([]*quotaspb.QuotaAllowance, string, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return nil, "", err
	}
	regions, err := s.containerRegions(ctx, parentID)
	if err != nil {
		return nil, "", err
	}
	var defaults []Allowance
	for _, r := range regions {
		for _, e := range s.catalog.Entries() {
			a, err := s.newAllowance(parentID, e.Name, r)
			if err != nil {
				return nil, "", err
			}
			defaults = append(defaults, a)
		}
	}
	if err := s.repo.Ensure(ctx, defaults); err != nil {
		return nil, "", err
	}
	page, err := s.repo.List(ctx, parentID, size, pageToken)
	if err != nil {
		return nil, "", err
	}
	snap, err := s.snapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	out := make([]*quotaspb.QuotaAllowance, 0, len(page.Items))
	for _, a := range page.Items {
		out = append(out, s.render(a, snap))
	}
	return out, page.NextPageToken, nil
}

// Create sets a container's allowance of a quota. As in Nebius, an existing
// allowance (every catalog quota has one) has its value replaced.
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (*operations.Operation, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if cmd.Spec == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "required"})
	}
	if _, err := s.containerRegions(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	a, err := s.ensureOne(ctx, md.GetParentId(), md.GetName(), cmd.Spec.GetRegion(), "metadata.name")
	if err != nil {
		return nil, err
	}
	id := a.Msg.GetMetadata().GetId()
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create quota allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			cur, err := s.repo.Get(ctx, id)
			if err != nil {
				return err
			}
			next := proto.CloneOf(cur.Msg)
			next.Metadata.Labels = maps.Clone(md.GetLabels())
			next.Spec = &quotaspb.QuotaAllowanceSpec{Limit: cmd.Spec.Limit, Region: cmd.Spec.GetRegion()}
			return s.save(ctx, cur, next)
		},
	})
}

// Update replaces an allowance's labels and limit (reset-mask semantics).
// spec.region is immutable.
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	if cmd.Spec == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update quota allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

func (s *Service) applyUpdate(ctx context.Context, cmd UpdateCmd) error {
	cur, err := s.repo.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	md := cur.Msg.GetMetadata()
	if p := cmd.Metadata.GetParentId(); p != "" && p != md.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if n := cmd.Metadata.GetName(); n != "" && n != md.GetName() {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "field is immutable"})
	}
	if r := cmd.Spec.GetRegion(); r != "" && r != cur.Region {
		return errs.BadRequest(errs.Violation{Field: "spec.region", Message: "field is immutable"})
	}
	if v := cmd.Metadata.GetResourceVersion(); v != 0 && v != md.GetResourceVersion() {
		return errs.Conflict(md.GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, md.GetResourceVersion()))
	}
	patch := &quotaspb.QuotaAllowance{
		Metadata: &commonpb.ResourceMetadata{Labels: cmd.Metadata.GetLabels()},
		Spec:     &quotaspb.QuotaAllowanceSpec{Limit: cmd.Spec.Limit},
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	next := proto.CloneOf(cur.Msg)
	if err := fieldmaskpb.PatchWithResetMask(next, patch, resetMask); err != nil {
		return errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	next.Metadata = restoreMetadata(next.GetMetadata(), md)
	if next.GetSpec() == nil {
		next.Spec = &quotaspb.QuotaAllowanceSpec{}
	}
	next.Spec.Region = cur.Msg.GetSpec().GetRegion()
	return s.save(ctx, cur, next)
}

// Delete resets an allowance to the catalog default. As in Nebius, the
// allowance itself remains.
func (s *Service) Delete(ctx context.Context, id string, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete quota allowance",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			cur, err := s.repo.Get(ctx, id)
			if err != nil {
				return err
			}
			next := proto.CloneOf(cur.Msg)
			next.Spec = &quotaspb.QuotaAllowanceSpec{Region: cur.Region}
			return s.save(ctx, cur, next)
		},
	})
}

// save stores next when it differs from cur, bumping resource_version.
func (s *Service) save(ctx context.Context, cur Allowance, next *quotaspb.QuotaAllowance) error {
	next.Status = nil
	if proto.Equal(next, cur.Msg) {
		return nil
	}
	next.Metadata.ResourceVersion = cur.Msg.GetMetadata().GetResourceVersion() + 1
	next.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.repo.Put(ctx, Allowance{Msg: next, Region: cur.Region})
}

func restoreMetadata(patched, stored *commonpb.ResourceMetadata) *commonpb.ResourceMetadata {
	out := proto.CloneOf(stored)
	out.Labels = maps.Clone(patched.GetLabels())
	return out
}

// containerRegions validates a container (an existing tenant or project) and
// returns the regions it has allowances in.
func (s *Service) containerRegions(ctx context.Context, parentID string) ([]Region, error) {
	const field = "parent_id"
	kind, _, _ := strings.Cut(parentID, "-")
	switch {
	case parentID == "":
		return nil, errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	case kind != KindTenant && kind != KindProject:
		return nil, errs.BadRequest(errs.Violation{Field: field, Message: "must be a tenant or project ID"})
	}
	if _, err := s.hierarchy.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("quota container %s: %w", parentID, err)
	}
	if kind == KindTenant {
		return slices.Clone(s.all), nil
	}
	name, err := s.regions.Region(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("region of project %s: %w", parentID, err)
	}
	r, ok := s.region(name)
	if !ok {
		return nil, errs.Internal("project " + parentID + " is in unknown region " + name)
	}
	return []Region{r}, nil
}

// ensureOne returns the allowance of (parentID, name, region), creating it
// with the catalog default if needed. An empty region means the project's
// region (a tenant has no single region, so it must name one).
func (s *Service) ensureOne(ctx context.Context, parentID, name, region, nameField string) (Allowance, error) {
	if name == "" {
		return Allowance{}, errs.BadRequest(errs.Violation{Field: nameField, Message: "required: the quota name, e.g. " + quota.ComputeInstanceCount})
	}
	if _, ok := s.catalog.Lookup(name); !ok {
		return Allowance{}, errs.BadRequest(errs.Violation{Field: nameField, Message: fmt.Sprintf("unknown quota %q", name)})
	}
	regions, err := s.containerRegions(ctx, parentID)
	if err != nil {
		return Allowance{}, err
	}
	var r Region
	switch {
	case region == "" && len(regions) == 1:
		r = regions[0]
	case region == "":
		return Allowance{}, errs.BadRequest(errs.Violation{Field: "spec.region", Message: "required for a tenant allowance"})
	default:
		i := slices.IndexFunc(regions, func(x Region) bool { return x.Name == region })
		if i < 0 {
			return Allowance{}, errs.BadRequest(errs.Violation{Field: "spec.region", Message: fmt.Sprintf(
				"%s has no quotas in region %q", parentID, region)})
		}
		r = regions[i]
	}
	if a, err := s.repo.ByKey(ctx, parentID, name, r.Name); err == nil || !errs.IsKind(err, errs.KindNotFound) {
		return a, err
	}
	a, err := s.newAllowance(parentID, name, r)
	if err != nil {
		return Allowance{}, err
	}
	if err := s.repo.Ensure(ctx, []Allowance{a}); err != nil {
		return Allowance{}, err
	}
	return s.repo.ByKey(ctx, parentID, name, r.Name)
}

// newAllowance builds the default allowance of a quota. Its ID is derived
// from (container, quota, region), so materializing it twice is harmless.
func (s *Service) newAllowance(parentID, name string, r Region) (Allowance, error) {
	id, err := nid.Deterministic(KindAllowance, r.RoutingCode, parentID+"/"+name+"/"+r.Name)
	if err != nil {
		return Allowance{}, fmt.Errorf("quota allowance id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	return Allowance{Region: r.Name, Msg: &quotaspb.QuotaAllowance{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: name, ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: &quotaspb.QuotaAllowanceSpec{Region: r.Name},
	}}, nil
}

func (s *Service) region(name string) (Region, bool) {
	i := slices.IndexFunc(s.all, func(r Region) bool { return r.Name == name })
	if i < 0 {
		return Region{}, false
	}
	return s.all[i], true
}

func pageSizeOf(pageSize int64) (int, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if pageSize == 0 {
		return defaultPageSize, nil
	}
	return int(pageSize), nil
}
