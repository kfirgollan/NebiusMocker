package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// KMS-specific permission verbs.
const (
	VerbRotate   = "rotate"
	VerbUndelete = "undelete"
	VerbEncrypt  = "encrypt"
	VerbDecrypt  = "decrypt"
	VerbSign     = "sign"
)

// Service names of the KMS gRPC services served by this module.
const (
	symmetricKeyService     = "nebius.kms.v1.SymmetricKeyService"
	asymmetricKeyService    = "nebius.kms.v1.AsymmetricKeyService"
	symmetricCryptoService  = "nebius.kms.v1.SymmetricCryptoService"
	asymmetricCryptoService = "nebius.kms.v1.AsymmetricCryptoService"
)

type keyIDGetter interface{ GetKeyId() string }

// onKeyID checks perm on the request's key_id field (crypto RPCs).
func onKeyID(perm authz.Permission) authz.Rule {
	return authz.Rule{Permission: perm, Target: func(_ context.Context, req any) (string, error) {
		if g, ok := req.(keyIDGetter); ok && g.GetKeyId() != "" {
			return g.GetKeyId(), nil
		}
		return "", errs.BadRequest(errs.Violation{Field: "key_id", Message: "required"})
	}}
}

// RegisterAuthzRules declares which permission each KMS RPC needs.
func RegisterAuthzRules(r *authz.Rules) {
	registerKeyRules(r, symmetricKeyService, "symmetricKeys")
	registerKeyRules(r, asymmetricKeyService, "asymmetricKeys")
	r.Add("/"+symmetricKeyService+"/Rotate", authz.OnID(authz.Perm("kms", "symmetricKeys", VerbRotate)))

	sc := func(method string) string { return "/" + symmetricCryptoService + "/" + method }
	r.Add(sc("Encrypt"), onKeyID(authz.Perm("kms", "symmetricKeys", VerbEncrypt)))
	r.Add(sc("GenerateDataKey"), onKeyID(authz.Perm("kms", "symmetricKeys", VerbEncrypt)))
	r.Add(sc("Decrypt"), onKeyID(authz.Perm("kms", "symmetricKeys", VerbDecrypt)))

	ac := func(method string) string { return "/" + asymmetricCryptoService + "/" + method }
	r.Add(ac("SignHash"), onKeyID(authz.Perm("kms", "asymmetricKeys", VerbSign)))
	r.Add(ac("Decrypt"), onKeyID(authz.Perm("kms", "asymmetricKeys", VerbDecrypt)))
	// The public key is not secret: read-level access is enough.
	r.Add(ac("GetPublicKey"), onKeyID(authz.Perm("kms", "asymmetricKeys", authz.VerbGet)))
}

func registerKeyRules(r *authz.Rules, service, resources string) {
	r.CRUD(service, "kms", resources)
	r.Add("/"+service+"/UpdateDeletionDelay", authz.OnID(authz.Perm("kms", resources, authz.VerbUpdate)))
	r.Add("/"+service+"/Undelete", authz.OnID(authz.Perm("kms", resources, VerbUndelete)))
}
