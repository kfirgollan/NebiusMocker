package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const profileService = "nebius.iam.v1.ProfileService"

// ProfileHandler implements nebius.iam.v1.ProfileService.
type ProfileHandler struct {
	iampb.UnimplementedProfileServiceServer
	tenantID, projectID string
}

var _ iampb.ProfileServiceServer = (*ProfileHandler)(nil)

// NewProfileHandler returns a handler that places principals in the given
// default tenant and project.
func NewProfileHandler(tenantID, projectID string) *ProfileHandler {
	return &ProfileHandler{tenantID: tenantID, projectID: projectID}
}

// Get returns the caller's own profile.
func (h *ProfileHandler) Get(ctx context.Context, _ *iampb.GetProfileRequest) (*iampb.GetProfileResponse, error) {
	return domain.Profile(reqctx.PrincipalFrom(ctx).ID, h.tenantID, h.projectID), nil
}

// RegisterProfileAuthzRule lets every authenticated caller read its own profile.
func RegisterProfileAuthzRule(r *authz.Rules, tenantID string) {
	r.Add("/"+profileService+"/Get", authz.Rule{
		Permission: authz.Perm("iam", "profiles", authz.VerbGet),
		Target:     func(context.Context, any) (string, error) { return tenantID, nil },
		AllowIf:    func(context.Context, string, any) bool { return true },
	})
}
