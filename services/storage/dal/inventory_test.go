package dal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func invOf(id, bucket, name string) *storagepb.Inventory {
	return &storagepb.Inventory{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: bucket, Name: name, ResourceVersion: 1},
		Spec:     &storagepb.InventorySpec{Enabled: true, DestinationBucketId: "storagebucket-d", Schedule: storagepb.InventorySpec_DAILY},
		Status:   &storagepb.InventoryStatus{},
	}
}

var invT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestInventoryRepoCRUD(t *testing.T) {
	t.Parallel()
	r := dal.NewInventoryRepo(store.NewMemory())
	ctx := t.Context()
	inv := invOf("i1", "storagebucket-a", "daily")
	st := domain.InventoryState{NextRun: invT0, CreatedBy: "user-1"}
	if err := r.Create(ctx, inv, st); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(inv, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if byName, err := r.GetByName(ctx, "storagebucket-a", "daily"); err != nil || byName.GetMetadata().GetId() != "i1" {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	if _, err := r.GetByName(ctx, "storagebucket-b", "daily"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName other bucket: %v, want NotFound", err)
	}
	gotState, err := r.State(ctx, "i1")
	if err != nil || gotState != st {
		t.Errorf("State = %+v, %v, want %+v", gotState, err, st)
	}

	inv.Status.ErrorCode = storagepb.InventoryStatus_QUOTA_EXCEEDED
	if err := r.Update(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "i1"); got.GetStatus().GetErrorCode() != storagepb.InventoryStatus_QUOTA_EXCEEDED {
		t.Errorf("Update not stored: %v", got)
	}
	st.LastRun = invT0
	if err := r.SetState(ctx, "i1", st); err != nil {
		t.Fatal(err)
	}
	if gotState, _ := r.State(ctx, "i1"); !gotState.LastRun.Equal(invT0) {
		t.Errorf("SetState not stored: %+v", gotState)
	}

	if err := r.Delete(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if _, err := r.State(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("State after delete: %v, want NotFound", err)
	}
	if err := r.Delete(ctx, "i1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second Delete: %v", err)
	}
	if err := r.Update(ctx, inv); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Update of a deleted inventory: %v", err)
	}
	if err := r.SetState(ctx, "i1", st); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("SetState of a deleted inventory: %v", err)
	}
}

func TestInventoryRepoNamesPerBucket(t *testing.T) {
	t.Parallel()
	r := dal.NewInventoryRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, invOf("i1", "storagebucket-a", "same"), domain.InventoryState{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, invOf("i2", "storagebucket-b", "same"), domain.InventoryState{}); err != nil {
		t.Errorf("same name in another bucket: %v", err)
	}
	err := r.Create(ctx, invOf("i3", "storagebucket-a", "same"), domain.InventoryState{})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name: %v, want AlreadyExists", err)
	}
	if _, err := r.State(ctx, "i3"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("a failed Create left state behind: %v", err)
	}
}

func TestInventoryRepoPagingAndCount(t *testing.T) {
	t.Parallel()
	r := dal.NewInventoryRepo(store.NewMemory())
	ctx := t.Context()
	for i := range 5 {
		if err := r.Create(ctx, invOf(fmt.Sprintf("i%d", i), "storagebucket-a", fmt.Sprintf("n%d", i)), domain.InventoryState{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, invOf("other", "storagebucket-b", "n0"), domain.InventoryState{}); err != nil {
		t.Fatal(err)
	}
	var names []string
	token := ""
	for {
		page, err := r.List(ctx, "storagebucket-a", 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range page.Items {
			names = append(names, i.GetMetadata().GetName())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if fmt.Sprint(names) != "[n0 n1 n2 n3 n4]" {
		t.Errorf("paged names = %v", names)
	}
	if n, err := r.Count(ctx, "storagebucket-a"); err != nil || n != 5 {
		t.Errorf("Count = %d, %v, want 5", n, err)
	}
	if all, err := r.ListAll(ctx); err != nil || len(all) != 6 {
		t.Errorf("ListAll = %d, %v, want 6", len(all), err)
	}
	if _, err := r.List(ctx, "storagebucket-a", 2, "!!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("malformed token: %v, want BadRequest", err)
	}
}
