package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const projectService = "nebius.iam.v1.ProjectService"

// ProjectHandler implements the read side of nebius.iam.v1.ProjectService
// for the seeded project (e.g. `nebius iam project get`, used by the Object
// Storage quick start to find the tenant). Creating, listing and updating
// projects stay UNIMPLEMENTED.
type ProjectHandler struct {
	iampb.UnimplementedProjectServiceServer
	projects domain.ContainerRepository
}

var _ iampb.ProjectServiceServer = (*ProjectHandler)(nil) //nolint:staticcheck // SA1019: "nebius iam project get" (CLI 0.12) still calls the v1 ProjectService

// NewProjectHandler returns a handler over the project repository.
func NewProjectHandler(projects domain.ContainerRepository) *ProjectHandler {
	return &ProjectHandler{projects: projects}
}

// Get returns a project by ID.
func (h *ProjectHandler) Get(ctx context.Context, req *iampb.GetProjectRequest) (*iampb.Container, error) {
	p, err := h.projects.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return p, nil
}

// RegisterProjectAuthzRules adds the rule of ProjectService.Get.
func RegisterProjectAuthzRules(r *authz.Rules) {
	r.Add("/"+projectService+"/Get", authz.OnID(authz.Perm("iam", "projects", authz.VerbGet)))
}
