package api

import (
	"context"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	projectService   = "nebius.iam.v1.ProjectService"
	projectV2Service = "nebius.iam.v2.ProjectService"
)

// ProjectHandler implements nebius.iam.v1.ProjectService (deprecated
// upstream, still called by the v1 data source and `nebius iam project`).
type ProjectHandler struct {
	iampb.UnimplementedProjectServiceServer
	svc *domain.ProjectService
}

var _ iampb.ProjectServiceServer = (*ProjectHandler)(nil) //nolint:staticcheck // SA1019: the v1 data source and `nebius iam project` still call the v1 ProjectService

// NewProjectHandler returns a v1 handler backed by svc.
func NewProjectHandler(svc *domain.ProjectService) *ProjectHandler {
	return &ProjectHandler{svc: svc}
}

// Get returns a project by ID.
func (h *ProjectHandler) Get(ctx context.Context, req *iampb.GetProjectRequest) (*iampb.Container, error) {
	p, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// GetByName returns a project by tenant and name.
func (h *ProjectHandler) GetByName(ctx context.Context, req *iampb.GetProjectByNameRequest) (*iampb.Container, error) {
	p, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// List returns one page of the tenant's projects the caller may read.
func (h *ProjectHandler) List(ctx context.Context, req *iampb.ListProjectsRequest) (*iampb.ListProjectsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListProjectsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a project creation operation.
func (h *ProjectHandler) Create(ctx context.Context, req *iampb.CreateProjectRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateProjectCmd{Metadata: req.GetMetadata(), Region: req.GetSpec().GetRegion(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a project update; X-ResetMask selects fields to clear.
func (h *ProjectHandler) Update(ctx context.Context, req *iampb.UpdateProjectRequest) (*commonpb.Operation, error) {
	return updateProject(ctx, h.svc, req.GetMetadata(), req.GetSpec(), req)
}

// ProjectV2Handler implements nebius.iam.v2.ProjectService.
type ProjectV2Handler struct {
	iamv2pb.UnimplementedProjectServiceServer
	svc *domain.ProjectService
}

var _ iamv2pb.ProjectServiceServer = (*ProjectV2Handler)(nil)

// NewProjectV2Handler returns a v2 handler backed by svc.
func NewProjectV2Handler(svc *domain.ProjectService) *ProjectV2Handler {
	return &ProjectV2Handler{svc: svc}
}

// Get returns a project by ID.
func (h *ProjectV2Handler) Get(ctx context.Context, req *iamv2pb.GetProjectRequest) (*iamv2pb.Project, error) {
	p, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return toV2Project(p), nil
}

// GetByName returns a project by tenant and name.
func (h *ProjectV2Handler) GetByName(ctx context.Context, req *iamv2pb.GetProjectByNameRequest) (*iamv2pb.Project, error) {
	p, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return toV2Project(p), nil
}

// List returns one page of the tenant's projects the caller may read.
func (h *ProjectV2Handler) List(ctx context.Context, req *iamv2pb.ListProjectsRequest) (*iamv2pb.ListProjectsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	items := make([]*iamv2pb.Project, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, toV2Project(p))
	}
	return &iamv2pb.ListProjectsResponse{Items: items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a project creation operation.
func (h *ProjectV2Handler) Create(ctx context.Context, req *iamv2pb.CreateProjectRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Create(ctx, domain.CreateProjectCmd{Metadata: req.GetMetadata(), Region: req.GetSpec().GetRegion(), RequireRegion: true, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// Update starts a project update; X-ResetMask selects fields to clear.
func (h *ProjectV2Handler) Update(ctx context.Context, req *iamv2pb.UpdateProjectRequest) (*commonpb.Operation, error) {
	var spec *iampb.ContainerSpec
	if s := req.GetSpec(); s != nil {
		spec = &iampb.ContainerSpec{Region: s.GetRegion()}
	}
	return updateProject(ctx, h.svc, req.GetMetadata(), spec, req)
}

// Delete starts the deletion of an empty project (dry_run only checks).
func (h *ProjectV2Handler) Delete(ctx context.Context, req *iamv2pb.DeleteProjectRequest) (*commonpb.Operation, error) {
	op, err := h.svc.Delete(ctx, domain.DeleteProjectCmd{ID: req.GetId(), DryRun: req.GetDryRun(), Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

func updateProject(ctx context.Context, svc *domain.ProjectService, md *commonpb.ResourceMetadata, spec *iampb.ContainerSpec, req proto.Message) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	op, err := svc.Update(ctx, domain.UpdateProjectCmd{Metadata: md, Spec: spec, ResetMask: resetMask, Request: req})
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opsapi.ToProto(ctx, op), nil
}

// toV2Project is the v2 view of a stored project.
func toV2Project(c *iampb.Container) *iamv2pb.Project {
	state := iamv2pb.ProjectStatus_STATE_UNSPECIFIED
	if c.GetStatus().GetContainerState() == iampb.State_ACTIVE {
		state = iamv2pb.ProjectStatus_ACTIVE
	}
	return &iamv2pb.Project{
		Metadata: c.GetMetadata(),
		Spec:     &iamv2pb.ProjectSpec{Region: c.GetSpec().GetRegion()},
		Status:   &iamv2pb.ProjectStatus{ProjectState: state},
	}
}

// NameLookup resolves a child's name within its parent to the child's ID.
type NameLookup func(ctx context.Context, parentID, name string) (string, error)

// RegisterProjectAuthzRules declares the permissions of the v1 and v2
// ProjectService RPCs. GetByName is checked on the project it names, and List
// is open to every authenticated caller: the domain filters the listing to
// the projects the caller may read (a principal granted on one project lists
// that project only).
func RegisterProjectAuthzRules(r *authz.Rules, projectIDOf NameLookup) {
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "projects", verb) }
	for _, svc := range []string{projectService, projectV2Service} {
		m := func(method string) string { return "/" + svc + "/" + method }
		r.Add(m("Get"), authz.OnID(perm(authz.VerbGet)))
		r.Add(m("GetByName"), byName(perm(authz.VerbGet), projectIDOf))
		r.Add(m("List"), filteredList(perm(authz.VerbList)))
		r.Add(m("Create"), authz.OnMetadataParent(perm(authz.VerbCreate)))
		r.Add(m("Update"), authz.OnMetadataID(perm(authz.VerbUpdate)))
	}
	r.Add("/"+projectV2Service+"/Delete", authz.OnID(perm(authz.VerbDelete)))
}

type nameRequest interface {
	GetParentId() string
	GetName() string
}

// byName targets the resource a GetByName request names.
func byName(perm authz.Permission, idOf NameLookup) authz.Rule {
	return authz.Rule{Permission: perm, Target: func(ctx context.Context, req any) (string, error) {
		nr, ok := req.(nameRequest)
		if !ok || nr.GetName() == "" {
			return "", errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
		}
		return idOf(ctx, nr.GetParentId(), nr.GetName())
	}}
}

// filteredList lets every authenticated caller list; the handler returns only
// the items the caller may read. The target is the parent (for audit).
func filteredList(perm authz.Permission) authz.Rule {
	return authz.Rule{
		Permission: perm,
		Target: func(_ context.Context, req any) (string, error) {
			if g, ok := req.(interface{ GetParentId() string }); ok {
				return g.GetParentId(), nil
			}
			return "", nil
		},
		AllowIf: func(context.Context, string, any) bool { return true },
	}
}
