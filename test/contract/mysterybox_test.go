package contract

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	mbsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
)

const mbSecretValue = "hunter2-very-secret"

var mbBinary = []byte{0xff, 0xfe, 0x00, 0x80, 0xc3, 0x28, 0x01}

type mbClients struct {
	inst     *testkit.Instance
	secrets  mbsvc.SecretService
	versions mbsvc.SecretVersionService
	payloads mbsvc.PayloadService
}

func mbStart(t *testing.T) mbClients {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mysterybox.New()})
	sdk := inst.SDK(t)
	return mbClients{
		inst:     inst,
		secrets:  mbsvc.NewSecretService(sdk),
		versions: mbsvc.NewSecretVersionService(sdk),
		payloads: mbsvc.NewPayloadService(sdk),
	}
}

func mbStr(k, v string) *mysteryboxpb.Payload {
	return &mysteryboxpb.Payload{Key: k, Payload: &mysteryboxpb.Payload_StringValue{StringValue: v}}
}

func mbBin(k string, v []byte) *mysteryboxpb.Payload {
	return &mysteryboxpb.Payload{Key: k, Payload: &mysteryboxpb.Payload_BinaryValue{BinaryValue: v}}
}

func mbMixedPayload() []*mysteryboxpb.Payload {
	return []*mysteryboxpb.Payload{mbStr("password", mbSecretValue), mbBin("blob", mbBinary)}
}

func mbCreateReq(parent, name string, payload []*mysteryboxpb.Payload) *mysteryboxpb.CreateSecretRequest {
	req := &mysteryboxpb.CreateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name},
		Spec:     &mysteryboxpb.SecretSpec{Description: "desc " + name},
	}
	if payload != nil {
		req.Spec.SecretVersion = &mysteryboxpb.SecretVersionSpec{Payload: payload}
	}
	return req
}

// mbOp is the part of an SDK operation the helpers use.
type mbOp interface {
	ResourceID() string
	Wait(context.Context, ...grpc.CallOption) (operations.Operation, error)
}

// mbDone waits for a mutating call to finish, failing the test on any error,
// and returns the operation's resource ID.
func mbDone(t *testing.T, op mbOp, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	return op.ResourceID()
}

func mbCreateSecret(t *testing.T, s mbsvc.SecretService, parent, name string, payload []*mysteryboxpb.Payload) string {
	t.Helper()
	op, err := s.Create(t.Context(), mbCreateReq(parent, name, payload))
	return mbDone(t, op, err)
}

func mbCreateVersion(t *testing.T, v mbsvc.SecretVersionService, secretID string, payload []*mysteryboxpb.Payload, primary bool) string {
	t.Helper()
	op, err := v.Create(t.Context(), &mysteryboxpb.CreateSecretVersionRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: secretID},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: payload, SetPrimary: primary},
	})
	return mbDone(t, op, err)
}

func mbPrimary(t *testing.T, s mbsvc.SecretService, id string) string {
	t.Helper()
	sec, err := s.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	return sec.GetSpec().GetPrimaryVersionId()
}

func TestMysteryBoxSecretCRUD(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	project := c.inst.ProjectID

	id := mbCreateSecret(t, c.secrets, project, "db", mbMixedPayload())
	got, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetMetadata().GetName() != "db" || got.GetMetadata().GetParentId() != project {
		t.Errorf("metadata = %v", got.GetMetadata())
	}
	if got.GetSpec().GetPrimaryVersionId() == "" {
		t.Error("primary_version_id empty after create with secret_version")
	}
	if got.GetStatus().GetState() != mysteryboxpb.SecretStatus_ACTIVE {
		t.Errorf("state = %v", got.GetStatus().GetState())
	}
	if k := got.GetStatus().GetEffectiveKmsKeyId(); !strings.HasPrefix(k, "kmssymkey-") {
		t.Errorf("effective_kms_key_id = %q, want kmssymkey- prefix", k)
	}

	byName, err := c.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "db"})
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if diff := cmp.Diff(got, byName, protocmp.Transform()); diff != "" {
		t.Errorf("GetByName differs from Get (-get +byName):\n%s", diff)
	}
	_, err = c.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "nope"})
	assertCode(t, err, codes.NotFound)

	// Duplicate name.
	_, err = c.secrets.Create(ctx, mbCreateReq(project, "db", mbMixedPayload()))
	assertCode(t, err, codes.AlreadyExists)

	// Paging.
	mbCreateSecret(t, c.secrets, project, "second", []*mysteryboxpb.Payload{mbStr("k", "v")})
	mbCreateSecret(t, c.secrets, project, "third", nil)
	seen := map[string]bool{}
	token := ""
	for range 5 {
		page, err := c.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: project, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 1 {
			t.Fatalf("page has %d items, want <= 1", len(page.GetItems()))
		}
		for _, s := range page.GetItems() {
			seen[s.GetMetadata().GetName()] = true
			if s.GetSpec().GetSecretVersion() != nil {
				t.Error("List leaked spec.secret_version")
			}
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Errorf("paged names = %v, want db, second, third", seen)
	}
	third, err := c.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "third"})
	if err != nil {
		t.Fatalf("GetByName third: %v", err)
	}
	if third.GetSpec().GetPrimaryVersionId() != "" {
		t.Errorf("secret without version has primary %q", third.GetSpec().GetPrimaryVersionId())
	}

	// Update description, labels and name.
	upd := proto.CloneOf(got)
	upd.Metadata.Name = "db-renamed"
	upd.Metadata.Labels = map[string]string{"env": "dev"}
	upd.Spec.Description = "new description"
	op, err := c.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{Metadata: upd.GetMetadata(), Spec: upd.GetSpec()})
	mbDone(t, op, err)
	after, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if after.GetMetadata().GetName() != "db-renamed" || after.GetMetadata().GetLabels()["env"] != "dev" ||
		after.GetSpec().GetDescription() != "new description" {
		t.Errorf("after update = %v", after)
	}
	if after.GetSpec().GetPrimaryVersionId() != got.GetSpec().GetPrimaryVersionId() {
		t.Error("update changed the primary version")
	}
	if after.GetMetadata().GetResourceVersion() <= got.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version %d not increased from %d", after.GetMetadata().GetResourceVersion(), got.GetMetadata().GetResourceVersion())
	}
	_, err = c.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "db"})
	assertCode(t, err, codes.NotFound)

	// The SDK sends X-ResetMask for omitted fields: empty labels clear them.
	upd = proto.CloneOf(after)
	upd.Metadata.Labels = nil
	op, err = c.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{Metadata: upd.GetMetadata(), Spec: upd.GetSpec()})
	mbDone(t, op, err)
	cleared, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatalf("Get after reset: %v", err)
	}
	if len(cleared.GetMetadata().GetLabels()) != 0 {
		t.Errorf("labels = %v, want cleared by reset mask", cleared.GetMetadata().GetLabels())
	}

	// Update of a missing secret.
	_, err = c.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{Id: "mboxsecret-missing", ParentId: project, Name: "x"}, Spec: &mysteryboxpb.SecretSpec{},
	})
	assertCode(t, err, codes.NotFound)
}

func TestMysteryBoxSoftDelete(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	project := c.inst.ProjectID
	id := mbCreateSecret(t, c.secrets, project, "doomed", mbMixedPayload())
	keep := mbCreateSecret(t, c.secrets, project, "keep", mbMixedPayload())

	op, err := c.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: id})
	mbDone(t, op, err)

	_, err = c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id})
	assertCode(t, err, codes.NotFound)
	_, err = c.secrets.GetByName(ctx, &mysteryboxpb.GetSecretByNameRequest{ParentId: project, Name: "doomed"})
	assertCode(t, err, codes.NotFound)
	_, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: id})
	assertCode(t, err, codes.NotFound)

	sec, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id, ShowScheduledForDeletion: true})
	if err != nil {
		t.Fatalf("Get show_scheduled_for_deletion: %v", err)
	}
	st := sec.GetStatus()
	if st.GetState() != mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION {
		t.Fatalf("state = %v, want SCHEDULED_FOR_DELETION", st.GetState())
	}
	gap := st.GetPurgeAt().AsTime().Sub(st.GetDeletedAt().AsTime())
	if gap < 29*24*time.Hour || gap > 31*24*time.Hour {
		t.Errorf("purge_at - deleted_at = %v, want ~30 days", gap)
	}

	list, err := c.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: project})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := len(list.GetItems()); n != 1 || list.GetItems()[0].GetMetadata().GetId() != keep {
		t.Errorf("List hides deleted: got %d items, want only %s", n, keep)
	}
	list, err = c.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: project, ShowScheduledForDeletion: true})
	if err != nil || len(list.GetItems()) != 2 {
		t.Errorf("List show_scheduled_for_deletion = %d items, %v; want 2", len(list.GetItems()), err)
	}

	// Deleting again: the secret is gone for active-only lookups.
	_, err = c.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: id})
	assertCode(t, err, codes.NotFound)

	// The name is released; undeleting under the old name now collides.
	other := mbCreateSecret(t, c.secrets, project, "doomed", mbMixedPayload())
	uop, err := c.secrets.Undelete(ctx, &mysteryboxpb.UndeleteSecretRequest{Id: id})
	if err == nil {
		_, err = uop.Wait(ctx)
	}
	assertCode(t, err, codes.AlreadyExists)

	op, err = c.secrets.Undelete(ctx, &mysteryboxpb.UndeleteSecretRequest{Id: id, Name: "doomed-restored"})
	mbDone(t, op, err)
	back, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatalf("Get after undelete: %v", err)
	}
	if back.GetMetadata().GetName() != "doomed-restored" || back.GetStatus().GetState() != mysteryboxpb.SecretStatus_ACTIVE ||
		back.GetStatus().GetDeletedAt() != nil || back.GetStatus().GetPurgeAt() != nil {
		t.Errorf("restored secret = %v", back)
	}
	if _, err := c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: id}); err != nil {
		t.Errorf("payload after undelete: %v", err)
	}

	// Undelete of an active secret is rejected.
	uop, err = c.secrets.Undelete(ctx, &mysteryboxpb.UndeleteSecretRequest{Id: other})
	if err == nil {
		_, err = uop.Wait(ctx)
	}
	assertCode(t, err, codes.FailedPrecondition)

	// Undelete under the original name when it is free.
	op, err = c.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: keep})
	mbDone(t, op, err)
	op, err = c.secrets.Undelete(ctx, &mysteryboxpb.UndeleteSecretRequest{Id: keep})
	mbDone(t, op, err)
	if k, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: keep}); err != nil || k.GetMetadata().GetName() != "keep" {
		t.Errorf("restored keep = %v, %v", k, err)
	}
}

func TestMysteryBoxVersions(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	sid := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "versioned", []*mysteryboxpb.Payload{mbStr("k", "one")})
	v1 := mbPrimary(t, c.secrets, sid)

	// Without set_primary the primary stays.
	v2 := mbCreateVersion(t, c.versions, sid, []*mysteryboxpb.Payload{mbStr("k", "two")}, false)
	if got := mbPrimary(t, c.secrets, sid); got != v1 {
		t.Errorf("primary = %s after plain create, want %s", got, v1)
	}
	// With set_primary it moves.
	v3 := mbCreateVersion(t, c.versions, sid, []*mysteryboxpb.Payload{mbStr("k", "three")}, true)
	if got := mbPrimary(t, c.secrets, sid); got != v3 {
		t.Errorf("primary = %s after set_primary, want %s", got, v3)
	}

	got, err := c.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: v2})
	if err != nil {
		t.Fatalf("Get version: %v", err)
	}
	if got.GetMetadata().GetParentId() != sid || got.GetStatus().GetState() != mysteryboxpb.SecretVersionStatus_ACTIVE {
		t.Errorf("version = %v", got)
	}
	if len(got.GetSpec().GetPayload()) != 0 {
		t.Error("version Get leaked the payload")
	}

	list, err := c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid})
	if err != nil {
		t.Fatalf("List versions: %v", err)
	}
	if len(list.GetItems()) != 3 {
		t.Fatalf("List = %d versions, want 3", len(list.GetItems()))
	}
	for _, v := range list.GetItems() {
		if len(v.GetSpec().GetPayload()) != 0 {
			t.Error("version List leaked a payload")
		}
	}
	page, err := c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid, PageSize: 1})
	if err != nil || len(page.GetItems()) != 1 || page.GetNextPageToken() == "" {
		t.Errorf("page of 1 = %d items, token %q, %v", len(page.GetItems()), page.GetNextPageToken(), err)
	}

	// Update spec.primary_version_id.
	sec, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: sid})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	upd := proto.CloneOf(sec)
	upd.Spec.PrimaryVersionId = proto.String(v1)
	op, err := c.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{Metadata: upd.GetMetadata(), Spec: upd.GetSpec()})
	mbDone(t, op, err)
	if got := mbPrimary(t, c.secrets, sid); got != v1 {
		t.Errorf("primary = %s after update, want %s", got, v1)
	}
	// A bogus primary is rejected.
	upd.Spec.PrimaryVersionId = proto.String("mboxversion-missing")
	upd.Metadata.ResourceVersion = 0
	_, err = c.secrets.Update(ctx, &mysteryboxpb.UpdateSecretRequest{Metadata: upd.GetMetadata(), Spec: upd.GetSpec()})
	assertCode(t, err, codes.InvalidArgument)

	// Delete and undelete a version.
	dop, err := c.versions.Delete(ctx, &mysteryboxpb.DeleteSecretVersionRequest{Id: v2})
	mbDone(t, dop, err)
	_, err = c.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: v2})
	assertCode(t, err, codes.NotFound)
	del, err := c.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: v2, ShowScheduledForDeletion: true})
	if err != nil || del.GetStatus().GetState() != mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION {
		t.Errorf("deleted version = %v, %v", del, err)
	}
	list, err = c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid})
	if err != nil || len(list.GetItems()) != 2 {
		t.Errorf("List after delete = %d, %v; want 2", len(list.GetItems()), err)
	}
	list, err = c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid, ShowScheduledForDeletion: true})
	if err != nil || len(list.GetItems()) != 3 {
		t.Errorf("List show deleted = %d, %v; want 3", len(list.GetItems()), err)
	}
	_, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid, VersionId: v2})
	assertCode(t, err, codes.NotFound)

	uop, err := c.versions.Undelete(ctx, &mysteryboxpb.UndeleteSecretVersionRequest{Id: v2})
	mbDone(t, uop, err)
	back, err := c.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: v2})
	if err != nil || back.GetStatus().GetState() != mysteryboxpb.SecretVersionStatus_ACTIVE {
		t.Errorf("restored version = %v, %v", back, err)
	}

	// Versions of a missing secret.
	_, err = c.versions.Create(ctx, &mysteryboxpb.CreateSecretVersionRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "mboxsecret-missing"},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: []*mysteryboxpb.Payload{mbStr("k", "v")}},
	})
	assertCode(t, err, codes.NotFound)
}

func TestMysteryBoxPayloads(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	sid := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "p", mbMixedPayload())
	v1 := mbPrimary(t, c.secrets, sid)
	second := []*mysteryboxpb.Payload{mbStr("other", "second-value")}
	v2 := mbCreateVersion(t, c.versions, sid, second, false)

	// Default: the primary version.
	p, err := c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid})
	if err != nil {
		t.Fatalf("payload Get: %v", err)
	}
	want := &mysteryboxpb.SecretPayload{VersionId: v1, Data: mbMixedPayload()}
	if diff := cmp.Diff(want, p, protocmp.Transform()); diff != "" {
		t.Errorf("primary payload (-want +got):\n%s", diff)
	}
	if got := p.GetData()[1].GetBinaryValue(); !bytes.Equal(got, mbBinary) {
		t.Errorf("binary = %x, want %x", got, mbBinary)
	}

	// Explicit version.
	p, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: sid, VersionId: v2})
	if err != nil {
		t.Fatalf("payload Get v2: %v", err)
	}
	if diff := cmp.Diff(&mysteryboxpb.SecretPayload{VersionId: v2, Data: second}, p, protocmp.Transform()); diff != "" {
		t.Errorf("v2 payload (-want +got):\n%s", diff)
	}

	// GetByKey.
	e, err := c.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, Key: "blob"})
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if diff := cmp.Diff(&mysteryboxpb.SecretPayloadEntry{VersionId: v1, Data: mbBin("blob", mbBinary)}, e, protocmp.Transform()); diff != "" {
		t.Errorf("GetByKey blob (-want +got):\n%s", diff)
	}
	e, err = c.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, VersionId: v1, Key: "password"})
	if err != nil || e.GetData().GetStringValue() != mbSecretValue {
		t.Errorf("GetByKey password = %v, %v", e, err)
	}
	_, err = c.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, Key: "missing"})
	assertCode(t, err, codes.NotFound)
	_, err = c.payloads.GetByKey(ctx, &mysteryboxpb.GetPayloadByKeyRequest{SecretId: sid, VersionId: v2, Key: "password"})
	assertCode(t, err, codes.NotFound)

	// Missing secret and a version of another secret.
	_, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: "mboxsecret-missing"})
	assertCode(t, err, codes.NotFound)
	other := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "other", []*mysteryboxpb.Payload{mbStr("k", "v")})
	_, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: other, VersionId: v1})
	assertCode(t, err, codes.NotFound)

	// A secret without versions has no primary to read.
	empty := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "empty", nil)
	_, err = c.payloads.Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: empty})
	if err == nil {
		t.Error("payload Get on a secret without versions succeeded")
	}
}

func TestMysteryBoxPayloadNeverLeaks(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	op, err := c.secrets.Create(ctx, mbCreateReq(c.inst.ProjectID, "leaky", mbMixedPayload()))
	sid := mbDone(t, op, err)

	sec, err := c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: sid})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sec.GetSpec().GetSecretVersion() != nil {
		t.Error("Get leaked spec.secret_version")
	}
	list, err := c.secrets.List(ctx, &mysteryboxpb.ListSecretsRequest{ParentId: c.inst.ProjectID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list.GetItems() {
		if s.GetSpec().GetSecretVersion() != nil {
			t.Error("List leaked spec.secret_version")
		}
	}
	mbNoSecret(t, "secret Get", sec)
	mbNoSecret(t, "secret List", list)
	vs, err := c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: sid})
	if err != nil {
		t.Fatalf("List versions: %v", err)
	}
	mbNoSecret(t, "version list", vs)

	// The operation, as returned and as fetched again, carries a redacted request.
	again, err := c.secrets.GetOperation(ctx, &commonpb.GetOperationRequest{Id: op.ID()})
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	for name, reqMsg := range map[string]proto.Message{"create": op.Request(), "refetched": again.Request()} {
		req, ok := reqMsg.(*mysteryboxpb.CreateSecretRequest)
		if !ok {
			t.Fatalf("%s: request is %T, want CreateSecretRequest", name, reqMsg)
		}
		if len(req.GetSpec().GetSecretVersion().GetPayload()) != 0 {
			t.Errorf("%s: operation request carries payload entries", name)
		}
		if req.GetMetadata().GetName() != "leaky" {
			t.Errorf("%s: request name = %q, want it echoed", name, req.GetMetadata().GetName())
		}
		mbNoSecret(t, name+" operation request", req)
	}

	// The version create operation too.
	vop, err := c.versions.Create(ctx, &mysteryboxpb.CreateSecretVersionRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: sid},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: mbMixedPayload()},
	})
	mbDone(t, vop, err)
	mbNoSecret(t, "version operation request", vop.Request())
	if r, ok := vop.Request().(*mysteryboxpb.CreateSecretVersionRequest); !ok || len(r.GetSpec().GetPayload()) != 0 {
		t.Errorf("version operation request = %v, want payload-free CreateSecretVersionRequest", vop.Request())
	}
}

func mbNoSecret(t *testing.T, what string, m proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	if bytes.Contains(raw, []byte(mbSecretValue)) || bytes.Contains(raw, mbBinary) {
		t.Errorf("%s contains payload bytes", what)
	}
}

func TestMysteryBoxErrors(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	ctx := t.Context()
	project := c.inst.ProjectID

	bad := map[string][]*mysteryboxpb.Payload{
		"empty list":     {},
		"no value":       {{Key: "k"}},
		"empty string":   {mbStr("k", "")},
		"empty binary":   {mbBin("k", nil)},
		"missing key":    {mbStr("", "v")},
		"duplicate keys": {mbStr("k", "a"), mbBin("k", []byte("b"))},
	}
	sid := mbCreateSecret(t, c.secrets, project, "target", []*mysteryboxpb.Payload{mbStr("k", "v")})
	for name, payload := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := mbCreateReq(project, "bad-"+strings.ReplaceAll(name, " ", "-"), nil)
			req.Spec.SecretVersion = &mysteryboxpb.SecretVersionSpec{Payload: payload}
			_, err := c.secrets.Create(ctx, req)
			assertCode(t, err, codes.InvalidArgument)

			_, err = c.versions.Create(ctx, &mysteryboxpb.CreateSecretVersionRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: sid},
				Spec:     &mysteryboxpb.SecretVersionSpec{Payload: payload},
			})
			assertCode(t, err, codes.InvalidArgument)
		})
	}

	req := mbCreateReq(project, "with-primary", mbMixedPayload())
	req.Spec.PrimaryVersionId = proto.String("mboxversion-x")
	_, err := c.secrets.Create(ctx, req)
	assertCode(t, err, codes.InvalidArgument)

	_, err = c.secrets.Create(ctx, mbCreateReq("", "no-parent", mbMixedPayload()))
	assertCode(t, err, codes.InvalidArgument)

	_, err = c.secrets.Get(ctx, &mysteryboxpb.GetSecretRequest{Id: "mboxsecret-nonexistent"})
	assertCode(t, err, codes.NotFound)
	_, err = c.secrets.Delete(ctx, &mysteryboxpb.DeleteSecretRequest{Id: "mboxsecret-nonexistent"})
	assertCode(t, err, codes.NotFound)
	_, err = c.versions.Get(ctx, &mysteryboxpb.GetSecretVersionRequest{Id: "mboxversion-nonexistent"})
	assertCode(t, err, codes.NotFound)
	_, err = c.versions.List(ctx, &mysteryboxpb.ListSecretVersionsRequest{ParentId: "mboxsecret-nonexistent"})
	assertCode(t, err, codes.NotFound)
}
