package dal_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/dal"
)

const project = "project-e00mock000000000default"

func secret(id, name string, state mysteryboxpb.SecretStatus_State) *mysteryboxpb.Secret {
	return &mysteryboxpb.Secret{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: name, ResourceVersion: 1},
		Spec:     &mysteryboxpb.SecretSpec{Description: "d-" + id},
		Status:   &mysteryboxpb.SecretStatus{State: state},
	}
}

func version(id, secretID string) *mysteryboxpb.SecretVersion {
	return &mysteryboxpb.SecretVersion{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: secretID},
		Spec:     &mysteryboxpb.SecretVersionSpec{Description: "d-" + id},
		Status:   &mysteryboxpb.SecretVersionStatus{State: mysteryboxpb.SecretVersionStatus_ACTIVE},
	}
}

func TestSecretRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewSecretRepo(store.NewMemory())
	s := secret("mbsec-1", "db", mysteryboxpb.SecretStatus_ACTIVE)
	if err := r.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "mbsec-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(s, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if err := r.Create(ctx, s); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(same id) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}

	s.Spec.Description = "changed"
	if err := r.Update(ctx, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ = r.Get(ctx, "mbsec-1"); got.GetSpec().GetDescription() != "changed" {
		t.Errorf("description after update = %q, want changed", got.GetSpec().GetDescription())
	}
	if err := r.Update(ctx, secret("mbsec-x", "x", mysteryboxpb.SecretStatus_ACTIVE)); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound", errs.KindOf(err))
	}

	if err := r.Delete(ctx, "mbsec-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "mbsec-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mbsec-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestSecretRepoNameUniqueAmongActive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewSecretRepo(store.NewMemory())
	first := secret("mbsec-1", "db", mysteryboxpb.SecretStatus_ACTIVE)
	if err := r.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	err := r.Create(ctx, secret("mbsec-2", "db", mysteryboxpb.SecretStatus_ACTIVE))
	if errs.KindOf(err) != errs.KindAlreadyExists {
		t.Fatalf("Create(duplicate active name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}

	// Scheduling the first for deletion releases the name.
	first.Status.State = mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION
	if err := r.Update(ctx, first); err != nil {
		t.Fatalf("Update(schedule deletion): %v", err)
	}
	if _, err := r.GetByName(ctx, project, "db"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("GetByName(released) kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}
	second := secret("mbsec-2", "db", mysteryboxpb.SecretStatus_ACTIVE)
	if err := r.Create(ctx, second); err != nil {
		t.Fatalf("Create(reusing released name): %v", err)
	}
	byName, err := r.GetByName(ctx, project, "db")
	if err != nil || byName.GetMetadata().GetId() != "mbsec-2" {
		t.Fatalf("GetByName = %v, %v; want mbsec-2", byName.GetMetadata().GetId(), err)
	}

	// Restoring the first now collides with the second.
	first.Status.State = mysteryboxpb.SecretStatus_ACTIVE
	if err := r.Update(ctx, first); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Update(restore taken name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	// With a new name it works.
	first.Metadata.Name = "db-restored"
	if err := r.Update(ctx, first); err != nil {
		t.Fatalf("Update(restore under new name): %v", err)
	}
	if got, err := r.GetByName(ctx, project, "db-restored"); err != nil || got.GetMetadata().GetId() != "mbsec-1" {
		t.Errorf("GetByName(db-restored) = %v, %v; want mbsec-1", got.GetMetadata().GetId(), err)
	}
	// The same name in another project is independent.
	other := secret("mbsec-3", "db", mysteryboxpb.SecretStatus_ACTIVE)
	other.Metadata.ParentId = "project-e00mock000000000other"
	if err := r.Create(ctx, other); err != nil {
		t.Errorf("Create(same name, other project): %v", err)
	}
}

func TestSecretRepoListPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewSecretRepo(store.NewMemory())
	const total = 5
	for i := range total {
		state := mysteryboxpb.SecretStatus_ACTIVE
		if i == 1 {
			state = mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION // listed in any state
		}
		if err := r.Create(ctx, secret(fmt.Sprintf("mbsec-%d", i), fmt.Sprintf("s%d", i), state)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	other := secret("mbsec-other", "x", mysteryboxpb.SecretStatus_ACTIVE)
	other.Metadata.ParentId = "project-e00mock000000000other"
	if err := r.Create(ctx, other); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	var got []string
	token := ""
	pages := 0
	for {
		page, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List(token=%q): %v", token, err)
		}
		pages++
		for _, s := range page.Items {
			got = append(got, s.GetMetadata().GetId())
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	want := []string{"mbsec-0", "mbsec-1", "mbsec-2", "mbsec-3", "mbsec-4"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("paged ids (-want +got):\n%s", diff)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}

	page, err := r.List(ctx, project, 5, "")
	if err != nil || len(page.Items) != total || page.NextPageToken != "" {
		t.Errorf("List(size 5) = %d items, token %q, %v; want %d items, no token", len(page.Items), page.NextPageToken, err, total)
	}
	if _, err := r.List(ctx, project, 2, "!!not-a-token!!"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestVersionRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewVersionRepo(store.NewMemory())
	for _, v := range []*mysteryboxpb.SecretVersion{
		version("mbsecver-1", "mbsec-a"), version("mbsecver-2", "mbsec-a"), version("mbsecver-3", "mbsec-b"), version("mbsecver-4", "mbsec-a"),
	} {
		if err := r.Create(ctx, v); err != nil {
			t.Fatalf("Create %s: %v", v.GetMetadata().GetId(), err)
		}
	}
	if err := r.Create(ctx, version("mbsecver-1", "mbsec-a")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(duplicate id) kind = %v, want AlreadyExists", errs.KindOf(err))
	}

	ids := func(vs []*mysteryboxpb.SecretVersion) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.GetMetadata().GetId())
		}
		return out
	}
	all, err := r.All(ctx, "mbsec-a")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if diff := cmp.Diff([]string{"mbsecver-1", "mbsecver-2", "mbsecver-4"}, ids(all)); diff != "" {
		t.Errorf("All (-want +got):\n%s", diff)
	}

	p1, err := r.List(ctx, "mbsec-a", 2, "")
	if err != nil || p1.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, token %q, %v; want 2 items and a token", ids(p1.Items), p1.NextPageToken, err)
	}
	p2, err := r.List(ctx, "mbsec-a", 2, p1.NextPageToken)
	if err != nil || p2.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, token %q, %v; want the rest and no token", ids(p2.Items), p2.NextPageToken, err)
	}
	if diff := cmp.Diff([]string{"mbsecver-1", "mbsecver-2", "mbsecver-4"}, append(ids(p1.Items), ids(p2.Items)...)); diff != "" {
		t.Errorf("paged versions (-want +got):\n%s", diff)
	}

	v, err := r.Get(ctx, "mbsecver-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	v.Status.State = mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION
	if err := r.Update(ctx, v); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := r.Get(ctx, "mbsecver-2"); !proto.Equal(got, v) {
		t.Errorf("Get after Update = %v, want %v", got, v)
	}
	if err := r.Update(ctx, version("mbsecver-x", "mbsec-a")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mbsecver-2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "mbsecver-2"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mbsecver-2"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestPayloadRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewPayloadRepo(store.NewMemory())
	if _, err := r.Get(ctx, "mbsecver-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("Get(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	sealed := []byte{0, 1, 2, 0xff, 0}
	if err := r.Put(ctx, "mbsecver-1", "mbsec-a", sealed); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := r.Get(ctx, "mbsecver-1")
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatalf("Get = %v, %v; want %v", got, err, sealed)
	}
	if err := r.Put(ctx, "mbsecver-1", "mbsec-a", []byte("replaced")); err != nil {
		t.Fatalf("Put(replace): %v", err)
	}
	if got, _ := r.Get(ctx, "mbsecver-1"); string(got) != "replaced" {
		t.Errorf("Get after replace = %q, want replaced", got)
	}
	if err := r.Delete(ctx, "mbsecver-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "mbsecver-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "mbsecver-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}
