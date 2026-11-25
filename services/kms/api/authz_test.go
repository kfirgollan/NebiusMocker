package api_test

import (
	"errors"
	"testing"

	"google.golang.org/grpc"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/kms/api"
)

func newRules() *authz.Rules {
	r := authz.NewRules()
	api.RegisterAuthzRules(r)
	return r
}

func TestEveryRPCHasARule(t *testing.T) {
	t.Parallel()
	rules := newRules()
	descs := []grpc.ServiceDesc{
		kmspb.SymmetricKeyService_ServiceDesc, kmspb.AsymmetricKeyService_ServiceDesc,
		kmspb.SymmetricCryptoService_ServiceDesc, kmspb.AsymmetricCryptoService_ServiceDesc,
	}
	for _, d := range descs {
		if len(d.Methods) == 0 {
			t.Fatalf("%s has no methods", d.ServiceName)
		}
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			if _, ok := rules.Lookup(full); !ok {
				t.Errorf("no authz rule for %s", full)
			}
		}
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	rules := newRules()
	tests := []struct{ method, perm string }{
		{"/nebius.kms.v1.SymmetricKeyService/Rotate", "kms.symmetricKeys.rotate"},
		{"/nebius.kms.v1.SymmetricKeyService/Undelete", "kms.symmetricKeys.undelete"},
		{"/nebius.kms.v1.SymmetricKeyService/UpdateDeletionDelay", "kms.symmetricKeys.update"},
		{"/nebius.kms.v1.AsymmetricKeyService/Create", "kms.asymmetricKeys.create"},
		{"/nebius.kms.v1.AsymmetricKeyService/Undelete", "kms.asymmetricKeys.undelete"},
		{"/nebius.kms.v1.SymmetricCryptoService/Encrypt", "kms.symmetricKeys.encrypt"},
		{"/nebius.kms.v1.SymmetricCryptoService/GenerateDataKey", "kms.symmetricKeys.encrypt"},
		{"/nebius.kms.v1.SymmetricCryptoService/Decrypt", "kms.symmetricKeys.decrypt"},
		{"/nebius.kms.v1.AsymmetricCryptoService/SignHash", "kms.asymmetricKeys.sign"},
		{"/nebius.kms.v1.AsymmetricCryptoService/Decrypt", "kms.asymmetricKeys.decrypt"},
		{"/nebius.kms.v1.AsymmetricCryptoService/GetPublicKey", "kms.asymmetricKeys.get"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			rule, ok := rules.Lookup(tt.method)
			if !ok {
				t.Fatal("no rule")
			}
			if got := string(rule.Permission); got != tt.perm {
				t.Errorf("permission = %q, want %q", got, tt.perm)
			}
		})
	}
}

func TestCryptoRulesTargetKeyID(t *testing.T) {
	t.Parallel()
	rule, ok := newRules().Lookup("/nebius.kms.v1.SymmetricCryptoService/Encrypt")
	if !ok {
		t.Fatal("no rule")
	}
	got, err := rule.Target(t.Context(), &kmspb.SymmetricEncryptRequest{KeyId: "k-1"})
	if err != nil || got != "k-1" {
		t.Fatalf("Target = %q, %v; want k-1, nil", got, err)
	}
	_, err = rule.Target(t.Context(), &kmspb.SymmetricEncryptRequest{})
	if _, ok := errors.AsType[*errs.Error](err); !ok {
		t.Fatalf("empty key_id error = %v, want *errs.Error", err)
	}
}
