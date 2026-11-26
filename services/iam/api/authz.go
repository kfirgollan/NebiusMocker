package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Service names of the IAM gRPC services served by this module.
const (
	groupService      = "nebius.iam.v1.GroupService"
	membershipService = "nebius.iam.v1.GroupMembershipService"
	permitService     = "nebius.iam.v1.AccessPermitService"
	saService         = "nebius.iam.v1.ServiceAccountService"
	accessKeyService  = "nebius.iam.v2.AccessKeyService"

	authPublicKeyService = "nebius.iam.v1.AuthPublicKeyService"
)

// VerbGetSecret is the access-key verb that reveals secrets. Only roles that
// grant it explicitly (admin) allow GetSecret; viewer's "get" does not.
const VerbGetSecret = "getSecret"

// AccessKeyIDLookup resolves an AWS access key ID to the key's NID (for
// authorizing the *ByAwsId RPCs on the key itself).
type AccessKeyIDLookup func(ctx context.Context, awsID string) (string, error)

// RegisterCredentialRules declares the permissions of the service-account and
// access-key RPCs.
func RegisterCredentialRules(r *authz.Rules, keyIDOf AccessKeyIDLookup) {
	r.CRUD(saService, "iam", "serviceAccounts")

	k := func(method string) string { return "/" + accessKeyService + "/" + method }
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "accessKeys", verb) }
	r.CRUD(accessKeyService, "iam", "accessKeys")
	r.Add(k("GetSecret"), authz.OnID(perm(VerbGetSecret)))
	r.Add(k("Activate"), authz.OnID(perm(authz.VerbUpdate)))
	r.Add(k("Deactivate"), authz.OnID(perm(authz.VerbUpdate)))
	r.Add(k("ListByAccount"), authz.Rule{
		Permission: perm(authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			lr, _ := req.(*iamv2pb.ListAccessKeysByAccountRequest)
			a := lr.GetAccount()
			if id := a.GetServiceAccount().GetId(); id != "" {
				return id, nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "account.service_account.id", Message: "required"})
		},
	})
	byAwsID := func(verb string, awsIDOf func(req any) string) authz.Rule {
		return authz.Rule{
			Permission: perm(verb),
			Target: func(ctx context.Context, req any) (string, error) {
				return keyIDOf(ctx, awsIDOf(req))
			},
		}
	}
	r.Add(k("GetByAwsId"), byAwsID(authz.VerbGet, func(req any) string {
		gr, _ := req.(*iamv2pb.GetAccessKeyByAwsIdRequest)
		return gr.GetAwsAccessKeyId()
	}))
	r.Add(k("DeleteByAwsId"), byAwsID(authz.VerbDelete, func(req any) string {
		dr, _ := req.(*iamv2pb.DeleteAccessKeyByAwsIdRequest)
		return dr.GetAwsAccessKeyId()
	}))
	r.Add(k("ActivateByAwsId"), byAwsID(authz.VerbUpdate, func(req any) string {
		ar, _ := req.(*iamv2pb.ActivateAccessKeyByAwsIdRequest)
		return ar.GetAwsAccessKeyId()
	}))
	r.Add(k("DeactivateByAwsId"), byAwsID(authz.VerbUpdate, func(req any) string {
		dr, _ := req.(*iamv2pb.DeactivateAccessKeyByAwsIdRequest)
		return dr.GetAwsAccessKeyId()
	}))
}

// RegisterAuthPublicKeyRules declares the permissions of the auth public key RPCs.
func RegisterAuthPublicKeyRules(r *authz.Rules) {
	k := func(method string) string { return "/" + authPublicKeyService + "/" + method }
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "authPublicKeys", verb) }
	r.CRUD(authPublicKeyService, "iam", "authPublicKeys")
	r.Add(k("Activate"), authz.OnID(perm(authz.VerbUpdate)))
	r.Add(k("Deactivate"), authz.OnID(perm(authz.VerbUpdate)))
	r.Add(k("ListByAccount"), authz.Rule{
		Permission: perm(authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			lr, _ := req.(*iampb.ListAuthPublicKeyByAccountRequest)
			if id := lr.GetAccount().GetServiceAccount().GetId(); id != "" {
				return id, nil
			}
			return "", errs.BadRequest(errs.Violation{Field: "account.service_account.id", Message: "required"})
		},
	})
}

// RegisterAuthzRules declares which permission each IAM RPC needs.
// tenantID is the tenant ListMemberOf is checked on: subjects (user accounts,
// service accounts) are not modelled as resources yet.
func RegisterAuthzRules(r *authz.Rules, tenantID string) {
	r.CRUD(groupService, "iam", "groups")

	m := func(method string) string { return "/" + membershipService + "/" + method }
	r.Add(m("Create"), authz.OnMetadataParent(authz.Perm("iam", "groupMemberships", authz.VerbCreate)))
	r.Add(m("Get"), authz.OnID(authz.Perm("iam", "groupMemberships", authz.VerbGet)))
	r.Add(m("GetWithAttributes"), authz.OnID(authz.Perm("iam", "groupMemberships", authz.VerbGet)))
	r.Add(m("Delete"), authz.OnID(authz.Perm("iam", "groupMemberships", authz.VerbDelete)))
	r.Add(m("ListMembers"), authz.OnParent(authz.Perm("iam", "groupMemberships", authz.VerbList)))
	r.Add(m("ListMembersWithAttributes"), authz.OnParent(authz.Perm("iam", "groupMemberships", authz.VerbList)))
	r.Add(m("ListMemberOf"), authz.Rule{
		Permission: authz.Perm("iam", "groupMemberships", authz.VerbList),
		Target:     func(context.Context, any) (string, error) { return tenantID, nil },
		// Anyone may list their own groups.
		AllowIf: func(_ context.Context, principalID string, req any) bool {
			lr, ok := req.(*iampb.ListMemberOfRequest)
			return ok && lr.GetSubjectId() == principalID
		},
	})

	p := func(method string) string { return "/" + permitService + "/" + method }
	r.Add(p("Create"), authz.OnMetadataParent(authz.Perm("iam", "accessPermits", authz.VerbCreate)))
	r.Add(p("Get"), authz.OnID(authz.Perm("iam", "accessPermits", authz.VerbGet)))
	r.Add(p("Delete"), authz.OnID(authz.Perm("iam", "accessPermits", authz.VerbDelete)))
	r.Add(p("List"), authz.OnParent(authz.Perm("iam", "accessPermits", authz.VerbList)))
}
