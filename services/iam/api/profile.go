package api

import (
	"context"
	"strings"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const profileService = "nebius.iam.v1.ProfileService"

// ServiceAccountGetter reads service accounts (satisfied by *domain.ServiceAccountService).
type ServiceAccountGetter interface {
	Get(ctx context.Context, id string) (*iampb.ServiceAccount, error)
}

// ProfileHandler implements nebius.iam.v1.ProfileService.
type ProfileHandler struct {
	iampb.UnimplementedProfileServiceServer
	tenantID, projectID string
	sas                 ServiceAccountGetter
}

var _ iampb.ProfileServiceServer = (*ProfileHandler)(nil)

// NewProfileHandler returns a handler that places user principals in the
// given default tenant and project, and reports service-account callers as
// the service account stored in sas.
func NewProfileHandler(tenantID, projectID string, sas ServiceAccountGetter) *ProfileHandler {
	return &ProfileHandler{tenantID: tenantID, projectID: projectID, sas: sas}
}

// Get returns the caller's own profile.
func (h *ProfileHandler) Get(ctx context.Context, _ *iampb.GetProfileRequest) (*iampb.GetProfileResponse, error) {
	id := reqctx.PrincipalFrom(ctx).ID
	if strings.HasPrefix(id, domain.KindServiceAccount+"-") {
		sa, err := h.sas.Get(ctx, id)
		if err == nil {
			return domain.ServiceAccountProfile(sa), nil
		}
		if !errs.IsKind(err, errs.KindNotFound) {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		// A static dev token may name a service account that was never created.
	}
	return domain.Profile(id, h.tenantID, h.projectID), nil
}

// RegisterProfileAuthzRule lets every authenticated caller read its own profile.
func RegisterProfileAuthzRule(r *authz.Rules, tenantID string) {
	r.Add("/"+profileService+"/Get", authz.Rule{
		Permission: authz.Perm("iam", "profiles", authz.VerbGet),
		Target:     func(context.Context, any) (string, error) { return tenantID, nil },
		AllowIf:    func(context.Context, string, any) bool { return true },
	})
}
