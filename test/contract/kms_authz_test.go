package contract

import (
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
)

const kmsOutsiderToken = "kms-outsider-token"

// kmsAccess is what a role may do with KMS; allowed calls must not fail with
// PermissionDenied (or NotFound for hidden resources).
type kmsAccess struct {
	name, token                  string
	read, create, crypto, rotate bool
}

func TestKMSRoles(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), kms.New()},
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{kmsOutsiderToken: "serviceaccount-e00mock00000000kmsout"}),
	)
	admin := newKMSClients(inst.SDKAs(t, config.DefaultDevToken))

	// Fixtures made by the admin: one key of each kind.
	symKey := kmsSymKind(admin).mustCreate(t, inst.ProjectID, "fixture-sym", nil)
	ecID := kmsCreateAsym(t, admin, inst.ProjectID, "fixture-ec", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256)
	ct := kmsEncrypt(t, admin, symKey.id, nil, []byte("seed"))
	digest := sha256.Sum256([]byte("digest"))

	roles := []kmsAccess{
		{name: "admin", token: config.DefaultDevToken, read: true, create: true, crypto: true, rotate: true},
		{name: "editor", token: config.DefaultEditorToken, read: true, create: true, crypto: true, rotate: true},
		{name: "viewer", token: config.DefaultViewerToken, read: true},
		{name: "outsider", token: kmsOutsiderToken},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			t.Parallel()
			c := newKMSClients(inst.SDKAs(t, role.token))
			ctx := t.Context()

			// Allowed calls must succeed; denied ones fail with the given code
			// (NotFound for reads, which hide what the caller cannot see).
			expect := func(call string, allowed bool, err error, denied codes.Code) {
				t.Helper()
				switch {
				case allowed && err != nil:
					t.Errorf("%s: %v, want success", call, err)
				case !allowed && kmsCode(err) != denied:
					t.Errorf("%s: code = %v (err %v), want %v", call, kmsCode(err), err, denied)
				}
			}

			// Reads.
			_, err := c.sym.Get(ctx, &kmspb.GetSymmetricKeyRequest{Id: symKey.id})
			expect("sym Get", role.read, err, codes.NotFound)
			_, err = c.sym.List(ctx, &kmspb.ListSymmetricKeysRequest{ParentId: inst.ProjectID})
			expect("sym List", role.read, err, codes.PermissionDenied)
			_, err = c.asym.Get(ctx, &kmspb.GetAsymmetricKeyRequest{Id: ecID})
			expect("asym Get", role.read, err, codes.NotFound)
			_, err = c.asym.List(ctx, &kmspb.ListAsymmetricKeysRequest{ParentId: inst.ProjectID})
			expect("asym List", role.read, err, codes.PermissionDenied)
			_, err = c.asymCrypto.GetPublicKey(ctx, &kmspb.AsymmetricGetPublicKeyRequest{KeyId: ecID})
			expect("GetPublicKey", role.read, err, codes.NotFound)

			// Crypto.
			_, err = c.symCrypto.Encrypt(ctx, &kmspb.SymmetricEncryptRequest{KeyId: symKey.id, Plaintext: []byte("p")})
			expect("Encrypt", role.crypto, err, codes.PermissionDenied)
			_, err = c.symCrypto.GenerateDataKey(ctx, &kmspb.GenerateDataKeyRequest{KeyId: symKey.id, DataKeySpec: kmspb.SymmetricAlgorithm_AES_256})
			expect("GenerateDataKey", role.crypto, err, codes.PermissionDenied)
			_, err = c.symCrypto.Decrypt(ctx, &kmspb.SymmetricDecryptRequest{KeyId: symKey.id, Ciphertext: ct})
			expect("Decrypt", role.crypto, err, codes.PermissionDenied)
			_, err = c.asymCrypto.SignHash(ctx, &kmspb.AsymmetricSignHashRequest{KeyId: ecID, Hash: digest[:]})
			expect("SignHash", role.crypto, err, codes.PermissionDenied)

			// Key management.
			op, err := c.sym.Rotate(ctx, &kmspb.RotateSymmetricKeyRequest{Id: symKey.id})
			expect("Rotate", role.rotate, kmsWait(ctx, op, err), codes.PermissionDenied)

			op, err = c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "by-" + role.name},
				Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
			})
			expect("sym Create", role.create, kmsWait(ctx, op, err), codes.PermissionDenied)
			if !role.create {
				return
			}
			created := op.ResourceID()

			op, err = c.sym.Delete(ctx, &kmspb.DeleteSymmetricKeyRequest{Id: created})
			expect("sym Delete", role.create, kmsWait(ctx, op, err), codes.PermissionDenied)
			op, err = c.sym.Undelete(ctx, &kmspb.UndeleteSymmetricKeyRequest{Id: created})
			expect("sym Undelete", role.create, kmsWait(ctx, op, err), codes.PermissionDenied)

			aop, err := c.asym.Create(ctx, &kmspb.CreateAsymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "ec-by-" + role.name},
				Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256},
			})
			expect("asym Create", role.create, kmsWait(ctx, aop, err), codes.PermissionDenied)
			aid := aop.ResourceID()
			aop, err = c.asym.Delete(ctx, &kmspb.DeleteAsymmetricKeyRequest{Id: aid})
			expect("asym Delete", role.create, kmsWait(ctx, aop, err), codes.PermissionDenied)
			aop, err = c.asym.Undelete(ctx, &kmspb.UndeleteAsymmetricKeyRequest{Id: aid})
			expect("asym Undelete", role.create, kmsWait(ctx, aop, err), codes.PermissionDenied)
		})
	}
}

func TestKMSViewerCannotMutateExisting(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), kms.New()}, testkit.WithAuthz(authz.ModeEnforce))
	admin := newKMSClients(inst.SDKAs(t, config.DefaultDevToken))
	viewer := newKMSClients(inst.SDKAs(t, config.DefaultViewerToken))
	ctx := t.Context()
	key := kmsSymKind(admin).mustCreate(t, inst.ProjectID, "guarded", nil)

	op, err := viewer.sym.Delete(ctx, &kmspb.DeleteSymmetricKeyRequest{Id: key.id})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = viewer.sym.UpdateDeletionDelay(ctx, &kmspb.UpdateSymmetricKeyDeletionDelayRequest{Id: key.id, DeletionDelay: durationpb.New(48 * time.Hour)})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	op, err = viewer.sym.Update(ctx, &kmspb.UpdateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: key.id, ParentId: inst.ProjectID, Name: "renamed"},
		Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.PermissionDenied)
	got, err := admin.sym.Get(ctx, &kmspb.GetSymmetricKeyRequest{Id: key.id})
	if err != nil || got.GetMetadata().GetName() != "guarded" {
		t.Fatalf("key after denied calls = %v, %v; want untouched", got.GetMetadata(), err)
	}
}
