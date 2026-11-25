package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	mbsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

const akmbSecretIDPrefix = "mbsec-"

// akmbSecretLen is the length of a generated AWS-style secret access key.
const akmbSecretLen = 40

// akmbPayload reads the payload of the MysteryBox secret secretID.
func akmbPayload(t *testing.T, p mbsvc.PayloadService, secretID string) *mysteryboxpb.SecretPayload {
	t.Helper()
	got, err := p.Get(t.Context(), &mysteryboxpb.GetPayloadRequest{SecretId: secretID})
	if err != nil {
		t.Fatalf("Get payload %s: %v", secretID, err)
	}
	return got
}

// akmbSecretValue checks the payload is exactly the access key secret and
// returns its value.
func akmbSecretValue(t *testing.T, got *mysteryboxpb.SecretPayload) string {
	t.Helper()
	if len(got.GetData()) != 1 {
		t.Fatalf("payload has %d entries, want exactly 1: %v", len(got.GetData()), got)
	}
	e := got.GetData()[0]
	if e.GetKey() != "secret" || domain.MysteryBoxSecretKey != "secret" {
		t.Fatalf("payload key = %q (domain %q), want %q", e.GetKey(), domain.MysteryBoxSecretKey, "secret")
	}
	if len(e.GetStringValue()) != akmbSecretLen {
		t.Fatalf("payload value has %d chars, want %d", len(e.GetStringValue()), akmbSecretLen)
	}
	return e.GetStringValue()
}

func akmbAccount(saID string) *iampb.Account {
	return &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}}
}

func TestAccessKeyDeliveryModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mode       iamv2pb.SecretDeliveryMode
		wantInline bool
		wantMB     bool
	}{
		{name: "unspecified behaves as inline", mode: iamv2pb.SecretDeliveryMode_UNSPECIFIED, wantInline: true},
		{name: "inline", mode: iamv2pb.SecretDeliveryMode_INLINE, wantInline: true},
		{name: "explicit", mode: iamv2pb.SecretDeliveryMode_EXPLICIT},
		{name: "mystery box", mode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX, wantMB: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()})
			sdk := inst.SDK(t)
			ctx := t.Context()
			sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), inst.ProjectID, "app")
			keys := iamv2svc.NewAccessKeyService(sdk)

			created := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), tt.mode)
			id := created.GetMetadata().GetId()
			awsID := created.GetStatus().GetAwsAccessKeyId()

			byAws, err := keys.GetByAwsId(ctx, &iamv2pb.GetAccessKeyByAwsIdRequest{AwsAccessKeyId: awsID})
			if err != nil {
				t.Fatalf("GetByAwsId: %v", err)
			}
			list, err := keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: inst.ProjectID})
			if err != nil || len(list.GetItems()) != 1 {
				t.Fatalf("List = %v, %v; want one key", list, err)
			}
			// createAccessKey returns the result of Get.
			for name, k := range map[string]*iamv2pb.AccessKey{"Get": created, "GetByAwsId": byAws, "List": list.GetItems()[0]} {
				st := k.GetStatus()
				if tt.wantInline != (st.GetSecret() != "") {
					t.Errorf("%s: status.secret set = %v, want %v", name, st.GetSecret() != "", tt.wantInline)
				}
				ref := st.GetSecretReferenceId()
				if tt.wantMB && !strings.HasPrefix(ref, akmbSecretIDPrefix) {
					t.Errorf("%s: secret_reference_id = %q, want prefix %q", name, ref, akmbSecretIDPrefix)
				}
				if !tt.wantMB && ref != "" {
					t.Errorf("%s: secret_reference_id = %q, want empty", name, ref)
				}
			}
			if tt.wantInline {
				return
			}
			sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: id})
			if err != nil || len(sec.GetSecret()) != akmbSecretLen || sec.GetAwsAccessKeyId() != awsID {
				t.Fatalf("GetSecret = %v, %v; want a %d-char secret for %s", sec, err, akmbSecretLen, awsID)
			}
		})
	}
}

func TestAccessKeyMysteryBoxDelivery(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()})
	sdk := inst.SDK(t)
	ctx := t.Context()
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), inst.ProjectID, "app")
	keys := iamv2svc.NewAccessKeyService(sdk)
	secrets := mbsvc.NewSecretService(sdk)
	payloads := mbsvc.NewPayloadService(sdk)

	k := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_MYSTERY_BOX)
	ref := k.GetStatus().GetSecretReferenceId()

	byName, err := secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: inst.ProjectID, Name: k.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("MysteryBox GetByName(%s): %v", k.GetMetadata().GetId(), err)
	}
	if byName.GetMetadata().GetId() != ref || byName.GetStatus().GetState() != mysteryboxpb.SecretStatus_ACTIVE {
		t.Fatalf("MysteryBox secret = %v, want ACTIVE with id %s", byName, ref)
	}
	byID, err := secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: ref})
	if err != nil || byID.GetMetadata().GetParentId() != inst.ProjectID {
		t.Fatalf("MysteryBox Get(%s) = %v, %v; want parent %s", ref, byID, err, inst.ProjectID)
	}

	value := akmbSecretValue(t, akmbPayload(t, payloads, ref))
	sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: k.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if sec.GetSecret() != value {
		t.Fatalf("GetSecret differs from the MysteryBox payload value")
	}
}

func TestAccessKeyMysteryBoxAuthz(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()},
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{outsiderToken: outsiderID}),
	)
	ctx := t.Context()
	adminSDK := inst.SDKAs(t, config.DefaultDevToken)
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(adminSDK), inst.ProjectID, "app")

	// The editor holds iam.accessKeys.create and creates the key.
	editorKeys := iamv2svc.NewAccessKeyService(inst.SDKAs(t, config.DefaultEditorToken))
	k := createAccessKey(t, editorKeys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_MYSTERY_BOX)
	ref := k.GetStatus().GetSecretReferenceId()
	if !strings.HasPrefix(ref, akmbSecretIDPrefix) {
		t.Fatalf("secret_reference_id = %q, want prefix %q", ref, akmbSecretIDPrefix)
	}

	akmbSecretValue(t, akmbPayload(t, mbsvc.NewPayloadService(adminSDK), ref))

	// Editor and viewer are denied, although the editor created the key.
	for _, token := range []string{config.DefaultEditorToken, config.DefaultViewerToken} {
		_, err := mbsvc.NewPayloadService(inst.SDKAs(t, token)).Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: ref})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Errorf("%s payload Get: code %v, want PermissionDenied (%v)", token, got, err)
		}
	}

	// A principal holding mysterybox.payload-viewer on the project reads it.
	groups := iamsvc.NewGroupService(adminSDK)
	members := iamsvc.NewGroupMembershipService(adminSDK)
	permits := iamsvc.NewAccessPermitService(adminSDK)
	gop, err := groups.Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "key-secret-readers"},
		Spec:     &iampb.GroupSpec{},
	})
	gid := mbDone(t, gop, err)
	mop, err := members.Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.GroupMembershipSpec{MemberId: outsiderID},
	})
	mbDone(t, mop, err)
	pop, err := permits.Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: gid},
		Spec:     &iampb.AccessPermitSpec{ResourceId: inst.ProjectID, Role: authz.RoleMysteryBoxPayloadViewer},
	})
	mbDone(t, pop, err)
	akmbSecretValue(t, akmbPayload(t, mbsvc.NewPayloadService(inst.SDKAs(t, outsiderToken)), ref))
}

func TestAccessKeyDeleteKeepsMysteryBoxSecret(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()})
	sdk := inst.SDK(t)
	ctx := t.Context()
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), inst.ProjectID, "app")
	keys := iamv2svc.NewAccessKeyService(sdk)
	payloads := mbsvc.NewPayloadService(sdk)
	k := createAccessKey(t, keys, inst.ProjectID, sa.GetMetadata().GetId(), iamv2pb.SecretDeliveryMode_MYSTERY_BOX)
	ref := k.GetStatus().GetSecretReferenceId()
	before := akmbSecretValue(t, akmbPayload(t, payloads, ref))

	op, err := keys.Delete(ctx, &iamv2pb.DeleteAccessKeyRequest{Id: k.GetMetadata().GetId()})
	mbDone(t, op, err)
	_, err = keys.Get(ctx, &iamv2pb.GetAccessKeyRequest{Id: k.GetMetadata().GetId()})
	assertCode(t, err, codes.NotFound)

	sec, err := mbsvc.NewSecretService(sdk).Get(ctx, &mysteryboxpb.GetSecretRequest{Id: ref})
	if err != nil || sec.GetStatus().GetState() != mysteryboxpb.SecretStatus_ACTIVE {
		t.Fatalf("MysteryBox secret after key delete = %v, %v; want ACTIVE", sec, err)
	}
	if after := akmbSecretValue(t, akmbPayload(t, payloads, ref)); after != before {
		t.Fatalf("payload changed after the access key was deleted")
	}
}

func TestAccessKeyMysteryBoxNotEnabled(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()})
	sdk := inst.SDK(t)
	ctx := t.Context()
	sa := createServiceAccount(t, iamsvc.NewServiceAccountService(sdk), inst.ProjectID, "app")
	keys := iamv2svc.NewAccessKeyService(sdk)

	_, err := keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            akmbAccount(sa.GetMetadata().GetId()),
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_MYSTERY_BOX,
		},
	})
	assertCode(t, err, codes.FailedPrecondition)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "MysteryBox") || !strings.Contains(msg, "not enabled") {
		t.Fatalf("message = %q, want it to mention MysteryBox and \"not enabled\"", msg)
	}

	list, err := keys.List(ctx, &iamv2pb.ListAccessKeysRequest{ParentId: inst.ProjectID})
	if err != nil || len(list.GetItems()) != 0 {
		t.Fatalf("List after failed create = %v, %v; want no keys", list, err)
	}
	byAccount, err := keys.ListByAccount(ctx, &iamv2pb.ListAccessKeysByAccountRequest{Account: akmbAccount(sa.GetMetadata().GetId())})
	if err != nil || len(byAccount.GetItems()) != 0 {
		t.Fatalf("ListByAccount after failed create = %v, %v; want no keys", byAccount, err)
	}
}
