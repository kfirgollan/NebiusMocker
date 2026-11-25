package domain

import (
	"strings"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
)

// Profile answers ProfileService.Get ("who am I") for a principal. Principals
// are static tokens in the mocker, so the profile is derived from the ID
// alone: service accounts get a service-account profile in the default
// project, user accounts a user profile in the default tenant, and anonymous
// callers an anonymous profile. The nebius CLI calls this before printing an
// access token (nebius iam get-access-token, docker credential helper).
func Profile(principalID, tenantID, projectID string) *iampb.GetProfileResponse {
	switch {
	case principalID == "":
		return &iampb.GetProfileResponse{Profile: &iampb.GetProfileResponse_AnonymousProfile{AnonymousProfile: &iampb.AnonymousAccount{}}}
	case strings.HasPrefix(principalID, KindServiceAccount+"-"):
		return &iampb.GetProfileResponse{Profile: &iampb.GetProfileResponse_ServiceAccountProfile{
			ServiceAccountProfile: &iampb.ServiceAccountProfile{Info: &iampb.ServiceAccount{
				Metadata: &commonpb.ResourceMetadata{Id: principalID, ParentId: projectID},
				Spec:     &iampb.ServiceAccountSpec{},
				Status:   &iampb.ServiceAccountStatus{Active: true},
			}},
		}}
	default:
		return &iampb.GetProfileResponse{Profile: &iampb.GetProfileResponse_UserProfile{UserProfile: &iampb.UserProfile{
			Id:               principalID,
			UserAccountState: iampb.UserAccountStatus_ACTIVE,
			Tenants: []*iampb.UserTenantInfo{{
				TenantId:               tenantID,
				TenantUserAccountId:    principalID,
				TenantUserAccountState: iampb.TenantUserAccountStatus_ACTIVE,
			}},
		}}}
	}
}

// ServiceAccountProfile is the profile of a caller authenticated as sa (for
// example with a token exchanged from the service account's JWT).
func ServiceAccountProfile(sa *iampb.ServiceAccount) *iampb.GetProfileResponse {
	return &iampb.GetProfileResponse{Profile: &iampb.GetProfileResponse_ServiceAccountProfile{
		ServiceAccountProfile: &iampb.ServiceAccountProfile{Info: sa},
	}}
}
