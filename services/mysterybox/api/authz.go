package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Service names of the MysteryBox gRPC services served by this module.
const (
	svcSec     = "nebius.mysterybox.v1.SecretService"
	svcVer     = "nebius.mysterybox.v1.SecretVersionService"
	svcPayload = "nebius.mysterybox.v1.PayloadService"
)

// PermPayloadRead is the permission to read decrypted payloads. The viewer and
// editor roles do not have it; admin and mysterybox.payload-viewer do. Its verb
// is not "get", so a denial is PermissionDenied, not a NotFound that would
// contradict what a viewer can already see.
const PermPayloadRead = authz.Permission("mysterybox.payload.read")

// RegisterAuthzRules declares which permission each MysteryBox RPC needs.
// Undelete needs the delete permission: whoever may delete may restore.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(svcSec, "mysterybox", "secrets")
	r.Add("/"+svcSec+"/Undelete", authz.OnID(authz.Perm("mysterybox", "secrets", authz.VerbDelete)))

	v := func(method string) string { return "/" + svcVer + "/" + method }
	r.Add(v("Create"), authz.OnMetadataParent(authz.Perm("mysterybox", "secretVersions", authz.VerbCreate)))
	r.Add(v("Get"), authz.OnID(authz.Perm("mysterybox", "secretVersions", authz.VerbGet)))
	r.Add(v("List"), authz.OnParent(authz.Perm("mysterybox", "secretVersions", authz.VerbList)))
	r.Add(v("Delete"), authz.OnID(authz.Perm("mysterybox", "secretVersions", authz.VerbDelete)))
	r.Add(v("Undelete"), authz.OnID(authz.Perm("mysterybox", "secretVersions", authz.VerbDelete)))

	onSecret := authz.Rule{Permission: PermPayloadRead, Target: secretIDTarget}
	r.Add("/"+svcPayload+"/Get", onSecret)
	r.Add("/"+svcPayload+"/GetByKey", onSecret)
}

type secretIDGetter interface{ GetSecretId() string }

// secretIDTarget checks payload reads on the secret.
func secretIDTarget(_ context.Context, req any) (string, error) {
	if g, ok := req.(secretIDGetter); ok && g.GetSecretId() != "" {
		return g.GetSecretId(), nil
	}
	return "", errs.BadRequest(errs.Violation{Field: "secret_id", Message: "required"})
}
