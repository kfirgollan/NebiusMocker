package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

func createServiceAccount(t *testing.T, svc iamsvc.ServiceAccountService, parentID, name string) *iampb.ServiceAccount {
	t.Helper()
	op, err := svc.Create(t.Context(), &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID, Name: name},
		Spec:     &iampb.ServiceAccountSpec{Description: "made by contract test"},
	})
	if err != nil {
		t.Fatalf("Create service account %q: %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create service account %q wait: %v", name, err)
	}
	sa, err := svc.Get(t.Context(), &iampb.GetServiceAccountRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get service account: %v", err)
	}
	return sa
}

func createAccessKey(t *testing.T, svc iamv2svc.AccessKeyService, parentID, saID string, mode iamv2pb.SecretDeliveryMode) *iamv2pb.AccessKey {
	t.Helper()
	op, err := svc.Create(t.Context(), &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}},
			Description:        "s3 key",
			SecretDeliveryMode: mode,
		},
	})
	if err != nil {
		t.Fatalf("Create access key: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create access key wait: %v", err)
	}
	k, err := svc.Get(t.Context(), &iamv2pb.GetAccessKeyRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get access key: %v", err)
	}
	return k
}

func TestServiceAccountLifecycle(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithOperationDelay(30*time.Millisecond))
	sdk := inst.SDK(t)
	sas := iamsvc.NewServiceAccountService(sdk)
	ctx := t.Context()

	sa := createServiceAccount(t, sas, inst.ProjectID, "deployer")
	if !sa.GetStatus().GetActive() || sa.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("new service account = %v, want active with resource_version 1", sa)
	}
	byName, err := sas.GetByName(ctx, &iampb.GetServiceAccountByNameRequest{ParentId: inst.ProjectID, Name: "deployer"})
	if err != nil || byName.GetMetadata().GetId() != sa.GetMetadata().GetId() {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	if _, err := sas.Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "deployer"}, Spec: &iampb.ServiceAccountSpec{},
	}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate name: code %v, want AlreadyExists (%v)", status.Code(err), err)
	}
	if _, err := sas.Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000missing", Name: "x"}, Spec: &iampb.ServiceAccountSpec{},
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing parent: code %v, want NotFound (%v)", status.Code(err), err)
	}

	// Clear the description with a reset mask and rename.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("spec.description"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := sas.Update(maskCtx, &iampb.UpdateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{Id: sa.GetMetadata().GetId(), ParentId: inst.ProjectID, Name: "deployer-2", Labels: map[string]string{"a": "b"}},
		Spec:     &iampb.ServiceAccountSpec{},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	got, err := sas.Get(ctx, &iampb.GetServiceAccountRequest{Id: sa.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetName() != "deployer-2" || got.GetSpec().GetDescription() != "" || got.GetMetadata().GetLabels()["a"] != "b" ||
		got.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after update = %v", got)
	}

	list, err := sas.List(ctx, &iampb.ListServiceAccountRequest{ParentId: inst.ProjectID})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("List = %v, %v; want one item", list, err)
	}

	keys := iamv2svc.NewAccessKeyService(sdk)
	k := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_EXPLICIT)

	op, err = sas.Delete(ctx, &iampb.DeleteServiceAccountRequest{Id: sa.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	if _, err := sas.Get(ctx, &iampb.GetServiceAccountRequest{Id: sa.GetMetadata().GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after delete: code %v, want NotFound", status.Code(err))
	}
	if _, err := keys.Get(ctx, &iamv2pb.GetAccessKeyRequest{Id: k.GetMetadata().GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("access key of deleted account: code %v, want NotFound (cascade)", status.Code(err))
	}
}

func TestAccessKeyLifecycle(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithOperationDelay(30*time.Millisecond))
	sdk := inst.SDK(t)
	ctx := t.Context()
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), inst.ProjectID, "s3-user")
	keys := iamv2svc.NewAccessKeyService(sdk)

	inline := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_INLINE)
	st := inline.GetStatus()
	if st.GetState() != iamv2pb.AccessKeyStatus_ACTIVE || len(st.GetAwsAccessKeyId()) != 20 || st.GetSecret() == "" {
		t.Fatalf("INLINE key status = %v, want ACTIVE, a 20-char AWS ID and the secret", st)
	}
	explicit := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_EXPLICIT)
	if explicit.GetStatus().GetSecret() != "" {
		t.Fatalf("EXPLICIT key exposes its secret in Get")
	}
	sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: explicit.GetMetadata().GetId()})
	if err != nil || sec.GetSecret() == "" || sec.GetAwsAccessKeyId() != explicit.GetStatus().GetAwsAccessKeyId() {
		t.Fatalf("GetSecret = %v, %v", sec, err)
	}
	byAws, err := keys.GetByAwsId(ctx, &iamv2pb.GetAccessKeyByAwsIdRequest{AwsAccessKeyId: explicit.GetStatus().GetAwsAccessKeyId()})
	if err != nil || byAws.GetMetadata().GetId() != explicit.GetMetadata().GetId() {
		t.Fatalf("GetByAwsId = %v, %v", byAws, err)
	}
	list, err := keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: inst.ProjectID})
	if err != nil || len(list.GetItems()) != 2 {
		t.Fatalf("List = %v, %v; want 2 keys", list, err)
	}
	byAccount, err := keys.ListByAccount(ctx, &iamv2pb.ListAccessKeysByAccountRequest{Account: explicit.GetSpec().GetAccount()})
	if err != nil || len(byAccount.GetItems()) != 2 {
		t.Fatalf("ListByAccount = %v, %v; want 2 keys", byAccount, err)
	}

	// Immutable fields cannot change; the description can.
	if _, err := keys.Update(ctx, &iamv2pb.UpdateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: explicit.GetMetadata().GetId(), ParentId: inst.ProjectID},
		Spec:     &iamv2pb.AccessKeySpec{ExpiresAt: timestamppb.New(time.Unix(4_000_000_000, 0))},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("change expires_at: code %v, want InvalidArgument (%v)", status.Code(err), err)
	}
	op, err := keys.Update(ctx, &iamv2pb.UpdateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: explicit.GetMetadata().GetId(), ParentId: inst.ProjectID, Name: "named-key"},
		Spec:     &iamv2pb.AccessKeySpec{Description: "renamed"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	op, err = keys.Deactivate(ctx, &iamv2pb.DeactivateAccessKeyRequest{Id: explicit.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := keys.Get(ctx, &iamv2pb.GetAccessKeyRequest{Id: explicit.GetMetadata().GetId()})
	if err != nil || got.GetStatus().GetState() != iamv2pb.AccessKeyStatus_INACTIVE || got.GetSpec().GetDescription() != "renamed" {
		t.Fatalf("after update+deactivate = %v, %v", got, err)
	}

	op, err = keys.Activate(ctx, &iamv2pb.ActivateAccessKeyRequest{Id: explicit.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	named, err := keys.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: inst.ProjectID, Name: "named-key"})
	if err != nil || named.GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE || named.GetMetadata().GetId() != explicit.GetMetadata().GetId() {
		t.Fatalf("GetByName after Activate = %v, %v", named, err)
	}

	op, err = keys.Delete(ctx, &iamv2pb.DeleteAccessKeyRequest{Id: inline.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.GetByAwsId(ctx, &iamv2pb.GetAccessKeyByAwsIdRequest{AwsAccessKeyId: inline.GetStatus().GetAwsAccessKeyId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetByAwsId after delete: code %v, want NotFound", status.Code(err))
	}
	if _, err := keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID},
		Spec:     &iamv2pb.AccessKeySpec{Account: explicit.GetSpec().GetAccount(), SecretDeliveryMode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("MYSTERY_BOX: code %v, want InvalidArgument", status.Code(err))
	}
}

// TestGetSecretNeedsAdmin: viewers and editors can read keys but not secrets.
func TestGetSecretNeedsAdmin(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthz(authz.ModeEnforce))
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(inst.SDK(t)), inst.ProjectID, "app")
	k := createAccessKey(t, iamv2svc.NewAccessKeyService(inst.SDK(t)), inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_EXPLICIT)
	for _, token := range []string{config.DefaultViewerToken, config.DefaultEditorToken} {
		keys := iamv2svc.NewAccessKeyService(inst.SDKAs(t, token))
		if _, err := keys.Get(t.Context(), &iamv2pb.GetAccessKeyRequest{Id: k.GetMetadata().GetId()}); err != nil {
			t.Errorf("%s Get: %v", token, err)
		}
		if _, err := keys.GetSecret(t.Context(), &iamv2pb.GetAccessKeySecretRequest{Id: k.GetMetadata().GetId()}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s GetSecret: code %v, want PermissionDenied", token, status.Code(err))
		}
	}
}
