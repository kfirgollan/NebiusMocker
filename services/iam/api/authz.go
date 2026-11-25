package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the IAM gRPC services served by this module.
const (
	groupService      = "nebius.iam.v1.GroupService"
	membershipService = "nebius.iam.v1.GroupMembershipService"
	permitService     = "nebius.iam.v1.AccessPermitService"
)

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
