package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// ProjectService implements nebius.iam.v1.ProjectService and
// nebius.iam.v2.ProjectService (both views of the same stored project).
//
// Nebius rules (docs.nebius.com/iam/manage-projects): a project lives in one
// tenant and one region, fixed at creation ("you can only edit the name");
// names are unique in the tenant; only an empty project can be deleted, and
// deletion is immediate (the v2 API has no scheduled-deletion states).
type ProjectService struct {
	projects ProjectRepository
	tenants  ContainerRepository
	vis      Visibility
	kinds    KindRegistry
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
	regions  []string
}

// ProjectDeps are the dependencies of a ProjectService. All are required.
type ProjectDeps struct {
	Projects   ProjectRepository
	Tenants    ContainerRepository
	Visibility Visibility
	Kinds      KindRegistry
	Ops        Operations
	IDs        IDGenerator
	Clock      clock.Clock
	// Regions are the emulated region names; the first is the default.
	Regions []string
}

// NewProjectService wires a ProjectService.
func NewProjectService(d ProjectDeps) (*ProjectService, error) {
	if d.Projects == nil || d.Tenants == nil || d.Visibility == nil || d.Kinds == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: project service dependencies must not be nil")
	}
	if len(d.Regions) == 0 {
		return nil, errors.New("iam: project service needs at least one region")
	}
	return &ProjectService{
		projects: d.Projects, tenants: d.Tenants, vis: d.Visibility, kinds: d.Kinds,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, regions: slices.Clone(d.Regions),
	}, nil
}

// CreateProjectCmd is the input of Create.
type CreateProjectCmd struct {
	Metadata *commonpb.ResourceMetadata
	// Region is spec.region. Empty means the default region, unless
	// RequireRegion is set.
	Region string
	// RequireRegion rejects an empty region: v2 ProjectSpec.region has no
	// default (only v1 ContainerSpec.region is NON_EMPTY_DEFAULT), and
	// nebius_iam_v2_project cannot round-trip a server-chosen region.
	RequireRegion bool
	Request       proto.Message
}

// UpdateProjectCmd is the input of Update (full replace guarded by a reset mask).
type UpdateProjectCmd struct {
	Metadata *commonpb.ResourceMetadata
	// Spec carries spec.region, which must stay unchanged.
	Spec      *iampb.ContainerSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// DeleteProjectCmd is the input of Delete.
type DeleteProjectCmd struct {
	ID string
	// DryRun only checks whether the deletion would succeed.
	DryRun  bool
	Request proto.Message
}

// Get returns a project by ID.
func (s *ProjectService) Get(ctx context.Context, id string) (*iampb.Container, error) {
	return s.projects.Get(ctx, id)
}

// GetByName returns a project by name within its tenant.
func (s *ProjectService) GetByName(ctx context.Context, tenantID, name string) (*iampb.Container, error) {
	return s.projects.GetByName(ctx, tenantID, name)
}

// List returns one page of the tenant's projects the caller may read
// (iam.projects.get on the project or the tenant), in creation order.
func (s *ProjectService) List(ctx context.Context, tenantID string, pageSize int64, pageToken, filter string) (ContainerPage, error) {
	size, err := listArgs(pageSize, filter)
	if err != nil {
		return ContainerPage{}, err
	}
	if tenantID == "" {
		return ContainerPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if _, err := s.tenants.Get(ctx, tenantID); err != nil {
		return ContainerPage{}, fmt.Errorf("projects of %s: %w", tenantID, err)
	}
	fetch := func(ctx context.Context, n int, tok string) ([]*iampb.Container, string, error) {
		p, err := s.projects.List(ctx, tenantID, n, tok)
		return p.Items, p.NextPageToken, err
	}
	items, next, err := visiblePage(ctx, size, pageToken, fetch, func(p *iampb.Container) (bool, error) {
		return s.vis.CanSee(ctx, permProjectsGet, p.GetMetadata().GetId())
	})
	if err != nil {
		return ContainerPage{}, err
	}
	return ContainerPage{Items: items, NextPageToken: next}, nil
}

// Create stores a new, active project.
func (s *ProjectService) Create(ctx context.Context, cmd CreateProjectCmd) (*operations.Operation, error) {
	p, err := s.newProject(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  p.GetMetadata().GetId(),
		Description: "Create project",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.projects.Create(ctx, p) },
	})
}

// Update changes name and labels (X-ResetMask semantics). The region and the
// tenant are immutable.
func (s *ProjectService) Update(ctx context.Context, cmd UpdateProjectCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update project",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

// Delete removes an empty project. A project that still contains resources
// is refused with FAILED_PRECONDITION naming them. With dryRun nothing is
// deleted: the operation only reports whether the deletion would succeed.
func (s *ProjectService) Delete(ctx context.Context, cmd DeleteProjectCmd) (*operations.Operation, error) {
	if cmd.ID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	desc, run := "Delete project", s.projects.DeleteIfEmpty
	if cmd.DryRun {
		desc, run = "Delete project (dry run)", s.projects.CheckEmpty
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  cmd.ID,
		Description: desc,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return run(ctx, cmd.ID, s.isResource) },
	})
}

// isResource tells resource records (any kind a module registered with
// authz, e.g. vpcnetwork or bucket) from internal records under a project.
func (s *ProjectService) isResource(kind string) bool { return s.kinds.HasKind(kind) }

func (s *ProjectService) newProject(ctx context.Context, cmd CreateProjectCmd) (*iampb.Container, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	tenantID := md.GetParentId()
	if tenantID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if !strings.HasPrefix(tenantID, KindTenant+"-") {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a tenant ID"})
	}
	if _, err := s.tenants.Get(ctx, tenantID); err != nil {
		return nil, fmt.Errorf("project parent %s: %w", tenantID, err)
	}
	if md.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	region := cmd.Region
	available := strings.Join(s.regions, ", ")
	switch {
	case region == "" && cmd.RequireRegion:
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.region", Message: "spec.region is required; available: " + available,
		})
	case region == "":
		region = s.regions[0]
	case !slices.Contains(s.regions, region):
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.region", Message: fmt.Sprintf("spec.region: unknown region %q; available: %s", region, available),
		})
	}
	id, err := s.ids.New(KindProject)
	if err != nil {
		return nil, fmt.Errorf("new project id: %w", err)
	}
	return newContainer(id, tenantID, md.GetName(), region, maps.Clone(md.GetLabels()), s.clk.Now()), nil
}

func (s *ProjectService) applyUpdate(ctx context.Context, cmd UpdateProjectCmd) error {
	stored, err := s.projects.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetMetadata().GetName() == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	// spec.region is NON_EMPTY_DEFAULT: empty (or reset) keeps the region.
	region := stored.GetSpec().GetRegion()
	if r := updated.GetSpec().GetRegion(); r != "" && r != region {
		return errs.BadRequest(errs.Violation{
			Field:   "spec.region",
			Message: fmt.Sprintf("spec.region is immutable: project %s stays in %s (create a new project in %s instead)", stored.GetMetadata().GetId(), region, r),
		})
	}
	updated.Spec = &iampb.ContainerSpec{Region: region}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	return s.projects.Update(ctx, updated)
}
