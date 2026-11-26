package contract

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/operations"
	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func invSpec(dest string) *storagepb.InventorySpec {
	return &storagepb.InventorySpec{
		Enabled: true, DestinationBucketId: dest, DestinationPrefix: "reports/",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
		OptionalFields: []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS},
	}
}

func invCreate(ctx context.Context, svc storagesvc.InventoryService, parent, name string, spec *storagepb.InventorySpec) (*storagepb.Inventory, error) {
	op, err := svc.Create(ctx, &storagepb.CreateInventoryRequest{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec})
	if err != nil {
		return nil, err
	}
	if _, err := op.Wait(ctx); err != nil {
		return nil, err
	}
	return svc.Get(ctx, &storagepb.GetInventoryRequest{Id: op.ResourceID()})
}

func invMustCreate(t *testing.T, svc storagesvc.InventoryService, parent, name string, spec *storagepb.InventorySpec) *storagepb.Inventory {
	t.Helper()
	inv, err := invCreate(t.Context(), svc, parent, name, spec)
	if err != nil {
		t.Fatalf("Create inventory %q: %v", name, err)
	}
	return inv
}

func TestInventoryLifecycle(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	svc := storagesvc.NewInventoryService(inst.SDK(t))
	ctx := t.Context()
	src := createBucket(t, buckets, inst.ProjectID, "inv-source", nil).GetMetadata().GetId()
	dst := createBucket(t, buckets, inst.ProjectID, "inv-reports", nil).GetMetadata().GetId()

	spec := invSpec(dst)
	spec.SourcePrefix = "logs/"
	inv := invMustCreate(t, svc, src, "nightly", spec)
	id := inv.GetMetadata().GetId()
	md := inv.GetMetadata()
	if md.GetParentId() != src || md.GetName() != "nightly" || md.GetResourceVersion() != 1 || md.GetCreatedAt() == nil {
		t.Fatalf("metadata = %v", md)
	}
	if !strings.HasPrefix(id, "storagebucketinventory-") {
		t.Errorf("id = %q, want storagebucketinventory-...", id)
	}
	if got := inv.GetSpec(); got.GetSourcePrefix() != "logs/" || got.GetDestinationBucketId() != dst || len(got.GetOptionalFields()) != 1 ||
		got.GetOutputFormat() != storagepb.InventorySpec_CSV_GZIP || got.GetSchedule() != storagepb.InventorySpec_DAILY || !got.GetEnabled() {
		t.Errorf("spec = %v", got)
	}
	if inv.GetStatus().GetErrorCode() != storagepb.InventoryStatus_NO_ERROR {
		t.Errorf("status = %v", inv.GetStatus())
	}

	byName, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: src, Name: "nightly"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	list, err := svc.List(ctx, &storagepb.ListInventoriesRequest{ParentId: src})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}

	// Change the schedule and clear the (zero-valued) enabled flag.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("spec.enabled"))
	if err != nil {
		t.Fatal(err)
	}
	next := invSpec(dst)
	next.Enabled = false
	next.Schedule = storagepb.InventorySpec_WEEKLY
	runOp(t, func() (operations.Operation, error) {
		return svc.Update(maskCtx, &storagepb.UpdateInventoryRequest{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: src}, Spec: next})
	})
	got, err := svc.Get(ctx, &storagepb.GetInventoryRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetEnabled() || got.GetSpec().GetSchedule() != storagepb.InventorySpec_WEEKLY || got.GetMetadata().GetResourceVersion() != 2 ||
		got.GetSpec().GetSourcePrefix() != "logs/" {
		t.Errorf("after update = %v", got)
	}

	// Re-enabling needs no reset mask: true is not a zero value.
	runOp(t, func() (operations.Operation, error) {
		return svc.Update(ctx, &storagepb.UpdateInventoryRequest{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: invSpec(dst)})
	})
	if got, _ = svc.Get(ctx, &storagepb.GetInventoryRequest{Id: id}); !got.GetSpec().GetEnabled() {
		t.Error("inventory not re-enabled")
	}

	runOp(t, func() (operations.Operation, error) {
		return svc.Delete(ctx, &storagepb.DeleteInventoryRequest{Id: id})
	})
	if _, err := svc.Get(ctx, &storagepb.GetInventoryRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete: code %v, want NotFound", status.Code(err))
	}
	// The name is free again.
	invMustCreate(t, svc, src, "nightly", invSpec(src))
}

func TestInventoryListPaging(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	svc := storagesvc.NewInventoryService(inst.SDK(t))
	ctx := t.Context()
	src := createBucket(t, buckets, inst.ProjectID, "inv-page-src", nil).GetMetadata().GetId()
	other := createBucket(t, buckets, inst.ProjectID, "inv-page-other", nil).GetMetadata().GetId()
	for i := range 5 {
		invMustCreate(t, svc, src, fmt.Sprintf("inv-%d", i), invSpec(src))
	}
	invMustCreate(t, svc, other, "inv-0", invSpec(other))

	var names []string
	token := ""
	for pages := 0; ; pages++ {
		resp, err := svc.List(ctx, &storagepb.ListInventoriesRequest{ParentId: src, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetItems()) > 2 {
			t.Fatalf("page of %d items, page_size 2", len(resp.GetItems()))
		}
		for _, i := range resp.GetItems() {
			names = append(names, i.GetMetadata().GetName())
		}
		if token = resp.GetNextPageToken(); token == "" {
			if pages != 2 {
				t.Errorf("pages = %d, want 3", pages+1)
			}
			break
		}
	}
	if fmt.Sprint(names) != "[inv-0 inv-1 inv-2 inv-3 inv-4]" {
		t.Errorf("names = %v", names)
	}
	if _, err := svc.List(ctx, &storagepb.ListInventoriesRequest{ParentId: src, PageSize: 5000}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("page_size 5000: code %v, want InvalidArgument", status.Code(err))
	}
	if _, err := svc.List(ctx, &storagepb.ListInventoriesRequest{ParentId: "storagebucket-e00mock0000missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown bucket: code %v, want NotFound", status.Code(err))
	}
}

func TestInventoryErrors(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	svc := storagesvc.NewInventoryService(inst.SDK(t))
	ctx := t.Context()
	src := createBucket(t, buckets, inst.ProjectID, "inv-err-src", nil).GetMetadata().GetId()
	dst := createBucket(t, buckets, inst.ProjectID, "inv-err-dst", nil).GetMetadata().GetId()
	inv := invMustCreate(t, svc, src, "base", invSpec(dst))

	with := func(f func(*storagepb.InventorySpec)) *storagepb.InventorySpec {
		s := invSpec(dst)
		f(s)
		return s
	}
	for name, tc := range map[string]struct {
		parent, name string
		spec         *storagepb.InventorySpec
		want         string // the field the error names
	}{
		"no destination":      {src, "e1", with(func(s *storagepb.InventorySpec) { s.DestinationBucketId = "" }), "spec.destination_bucket_id"},
		"unknown destination": {src, "e2", with(func(s *storagepb.InventorySpec) { s.DestinationBucketId = "storagebucket-e00mock0000missing" }), "spec.destination_bucket_id"},
		"no format":           {src, "e3", with(func(s *storagepb.InventorySpec) { s.OutputFormat = 0 }), "spec.output_format"},
		"no schedule":         {src, "e4", with(func(s *storagepb.InventorySpec) { s.Schedule = 0 }), "spec.schedule"},
		"long prefix":         {src, "e5", with(func(s *storagepb.InventorySpec) { s.DestinationPrefix = strings.Repeat("p", 257) }), "spec.destination_prefix"},
		"duplicate optional": {src, "e6", with(func(s *storagepb.InventorySpec) {
			s.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS, storagepb.InventorySpec_TAGS}
		}), "spec.optional_fields"},
		"bad name":  {src, "no/slash", invSpec(dst), "metadata.name"},
		"long name": {src, strings.Repeat("n", 65), invSpec(dst), "metadata.name"},
	} {
		_, err := svc.Create(ctx, &storagepb.CreateInventoryRequest{Metadata: &commonpb.ResourceMetadata{ParentId: tc.parent, Name: tc.name}, Spec: tc.spec})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want InvalidArgument naming %s", name, err, tc.want)
		}
	}

	if _, err := svc.Create(ctx, &storagepb.CreateInventoryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "storagebucket-e00mock0000missing", Name: "x"}, Spec: invSpec(dst),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown source: code %v, want NotFound", status.Code(err))
	}
	if _, err := invCreate(ctx, svc, src, "base", invSpec(dst)); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate name: code %v, want AlreadyExists (%v)", status.Code(err), err)
	}
	if _, err := svc.Get(ctx, &storagepb.GetInventoryRequest{Id: "storagebucketinventory-e00mock0000missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("Get unknown: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: src, Name: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetByName unknown: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.Delete(ctx, &storagepb.DeleteInventoryRequest{Id: "storagebucketinventory-e00mock0000missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("Delete unknown: code %v, want NotFound", status.Code(err))
	}

	// IMMUTABLE_NAME.
	_, err := svc.Update(ctx, &storagepb.UpdateInventoryRequest{
		Metadata: &commonpb.ResourceMetadata{Id: inv.GetMetadata().GetId(), Name: "renamed"}, Spec: invSpec(dst),
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("rename: %v, want InvalidArgument naming metadata.name", err)
	}
	// A stale resource_version.
	if _, err := svc.Update(ctx, &storagepb.UpdateInventoryRequest{
		Metadata: &commonpb.ResourceMetadata{Id: inv.GetMetadata().GetId(), ResourceVersion: 42}, Spec: invSpec(dst),
	}); status.Code(err) != codes.Aborted && status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale resource_version: code %v, want Aborted or FailedPrecondition", status.Code(err))
	}
}

func TestInventoryLimitPerBucket(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t)
	buckets := storagesvc.NewBucketService(inst.SDK(t))
	svc := storagesvc.NewInventoryService(inst.SDK(t))
	src := createBucket(t, buckets, inst.ProjectID, "inv-limit-src", nil).GetMetadata().GetId()
	other := createBucket(t, buckets, inst.ProjectID, "inv-limit-other", nil).GetMetadata().GetId()
	for i := range 5 {
		invMustCreate(t, svc, src, fmt.Sprintf("inv-%d", i), invSpec(src))
	}
	_, err := invCreate(t.Context(), svc, src, "inv-5", invSpec(src))
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "5") {
		t.Errorf("sixth inventory: %v, want ResourceExhausted naming the limit of 5", err)
	}
	invMustCreate(t, svc, other, "inv-0", invSpec(src)) // the limit is per source bucket
}

func TestInventoryAuthz(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t, testkit.WithAuthz(authz.ModeEnforce))
	adminBuckets := storagesvc.NewBucketService(inst.SDK(t))
	admin := storagesvc.NewInventoryService(inst.SDK(t))
	viewer := storagesvc.NewInventoryService(inst.SDKAs(t, config.DefaultViewerToken))
	editor := storagesvc.NewInventoryService(inst.SDKAs(t, config.DefaultEditorToken))
	ctx := t.Context()
	src := createBucket(t, adminBuckets, inst.ProjectID, "inv-authz-src", nil).GetMetadata().GetId()
	dst := createBucket(t, adminBuckets, inst.ProjectID, "inv-authz-dst", nil).GetMetadata().GetId()
	inv := invMustCreate(t, admin, src, "by-admin", invSpec(dst))
	id := inv.GetMetadata().GetId()

	// The viewer reads.
	if _, err := viewer.Get(ctx, &storagepb.GetInventoryRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: src, Name: "by-admin"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if l, err := viewer.List(ctx, &storagepb.ListInventoriesRequest{ParentId: src}); err != nil || len(l.GetItems()) != 1 {
		t.Errorf("viewer List = %v, %v", l, err)
	}
	// But cannot write.
	denied := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("viewer %s: code %v, want PermissionDenied (%v)", what, status.Code(err), err)
		}
	}
	_, err := viewer.Create(ctx, &storagepb.CreateInventoryRequest{Metadata: &commonpb.ResourceMetadata{ParentId: src, Name: "by-viewer"}, Spec: invSpec(dst)})
	denied("Create", err)
	_, err = viewer.Update(ctx, &storagepb.UpdateInventoryRequest{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: invSpec(dst)})
	denied("Update", err)
	_, err = viewer.Delete(ctx, &storagepb.DeleteInventoryRequest{Id: id})
	denied("Delete", err)

	// The editor manages inventories, writing reports into another bucket.
	ed := invMustCreate(t, editor, src, "by-editor", invSpec(dst))
	runOp(t, func() (operations.Operation, error) {
		return editor.Update(ctx, &storagepb.UpdateInventoryRequest{Metadata: &commonpb.ResourceMetadata{Id: ed.GetMetadata().GetId()}, Spec: invSpec(src)})
	})
	runOp(t, func() (operations.Operation, error) {
		return editor.Delete(ctx, &storagepb.DeleteInventoryRequest{Id: ed.GetMetadata().GetId()})
	})
}
