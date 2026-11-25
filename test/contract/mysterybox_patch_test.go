package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
)

// TestMysteryBoxUpdateWithoutParent mirrors the CLI's patch mode
// ("nebius mysterybox secret update <id> --primary-version-id …"), which
// sends metadata.id without metadata.parent_id. The update must be accepted:
// the parent is immutable and taken from the stored secret.
func TestMysteryBoxUpdateWithoutParent(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	id := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "patched", mbMixedPayload())
	v2 := mbCreateVersion(t, c.versions, id, []*mysteryboxpb.Payload{mbStr("k", "v")}, false)

	op, err := c.secrets.Update(t.Context(), &mysteryboxpb.UpdateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id},
		Spec:     &mysteryboxpb.SecretSpec{PrimaryVersionId: proto.String(v2)},
	})
	mbDone(t, op, err)
	if got := mbPrimary(t, c.secrets, id); got != v2 {
		t.Fatalf("primary = %s, want %s", got, v2)
	}
	sec, err := c.secrets.Get(t.Context(), &mysteryboxpb.GetSecretRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if sec.GetMetadata().GetParentId() != c.inst.ProjectID {
		t.Fatalf("parent = %s, want %s", sec.GetMetadata().GetParentId(), c.inst.ProjectID)
	}

	// Create still requires the parent.
	_, err = c.secrets.Create(t.Context(), &mysteryboxpb.CreateSecretRequest{Metadata: &commonpb.ResourceMetadata{Name: "orphan"}})
	assertCode(t, err, codes.InvalidArgument)
}

// TestMysteryBoxInlineVersionImmutable: Update accepts the inline version
// resent unchanged (as Terraform does) and rejects a changed one.
func TestMysteryBoxInlineVersionImmutable(t *testing.T) {
	t.Parallel()
	c := mbStart(t)
	req := mbCreateReq(c.inst.ProjectID, "immutable", mbMixedPayload())
	id := mbCreateSecret(t, c.secrets, c.inst.ProjectID, "immutable", mbMixedPayload())

	same := &mysteryboxpb.UpdateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: c.inst.ProjectID, Name: "immutable"},
		Spec:     &mysteryboxpb.SecretSpec{Description: "changed", SecretVersion: req.GetSpec().GetSecretVersion()},
	}
	op, err := c.secrets.Update(t.Context(), same)
	mbDone(t, op, err)

	changed := proto.CloneOf(same)
	changed.Spec.SecretVersion.Payload = []*mysteryboxpb.Payload{mbStr("password", "rotated-in-place")}
	_, err = c.secrets.Update(t.Context(), changed)
	assertCode(t, err, codes.InvalidArgument)
	if err != nil && strings.Contains(err.Error(), "rotated-in-place") {
		t.Fatalf("error leaks the payload: %v", err)
	}
}
