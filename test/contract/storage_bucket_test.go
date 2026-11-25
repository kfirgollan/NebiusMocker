package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/operations"
	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

func startStorage(t *testing.T, opts ...testkit.Option) (*testkit.Instance, *memory.Store) {
	t.Helper()
	objs := memory.New(clock.Real{})
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("127.0.0.1:0"))},
		append([]testkit.Option{testkit.WithObjStore(objs)}, opts...)...)
	return inst, objs
}

// runOp starts an operation and waits for it to succeed.
func runOp(t *testing.T, start func() (operations.Operation, error)) {
	t.Helper()
	op, err := start()
	if err != nil {
		t.Fatalf("start operation: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("operation %s: %v", op.ID(), err)
	}
}

func createBucket(t *testing.T, svc storagesvc.BucketService, parentID, name string, spec *storagepb.BucketSpec) *storagepb.Bucket {
	t.Helper()
	op, err := svc.Create(t.Context(), &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID, Name: name, Labels: map[string]string{"env": "test"}},
		Spec:     spec,
	})
	if err != nil {
		t.Fatalf("Create bucket %q: %v", name, err)
	}
	if op.ResourceID() == "" {
		t.Fatalf("Create bucket %q: empty resource_id", name)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create bucket %q wait: %v", name, err)
	}
	b, err := svc.Get(t.Context(), &storagepb.GetBucketRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get bucket: %v", err)
	}
	return b
}

func TestBucketLifecycle(t *testing.T) {
	t.Parallel()
	inst, objs := startStorage(t, testkit.WithOperationDelay(30*time.Millisecond))
	svc := storagesvc.NewBucketService(inst.SDK(t))
	ctx := t.Context()

	b := createBucket(t, svc, inst.ProjectID, "contract-bucket", &storagepb.BucketSpec{MaxSizeBytes: 1 << 20})
	st := b.GetStatus()
	if st.GetState() != storagepb.BucketStatus_ACTIVE || st.GetRegion() != "eu-north1" ||
		st.GetDomainName() != "contract-bucket.storage.eu-north1.mocker.localhost" || st.GetBucketType() != storagepb.BucketStatus_REGULAR {
		t.Fatalf("status = %v", st)
	}
	if got := b.GetSpec().GetVersioningPolicy(); got != storagepb.VersioningPolicy_DISABLED {
		t.Errorf("default versioning_policy = %v, want DISABLED", got)
	}
	if _, ok := objs.Versioning("contract-bucket"); !ok {
		t.Fatal("backing bucket was not created")
	}

	byName, err := svc.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: "contract-bucket"})
	if err != nil || byName.GetMetadata().GetId() != b.GetMetadata().GetId() {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	list, err := svc.List(ctx, &storagepb.ListBucketsRequest{ParentId: inst.ProjectID})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}

	// Enable versioning, clear labels via reset mask.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Update(maskCtx, &storagepb.UpdateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{Id: b.GetMetadata().GetId(), ParentId: inst.ProjectID},
		Spec:     &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_ENABLED},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	b, err = svc.Get(ctx, &storagepb.GetBucketRequest{Id: b.GetMetadata().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if b.GetSpec().GetVersioningPolicy() != storagepb.VersioningPolicy_ENABLED || len(b.GetMetadata().GetLabels()) != 0 ||
		b.GetSpec().GetMaxSizeBytes() != 1<<20 || b.GetMetadata().GetResourceVersion() != 2 || b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
		t.Fatalf("after update = %v", b)
	}
	if v, _ := objs.Versioning("contract-bucket"); v != objstore.VersioningEnabled {
		t.Errorf("backend versioning = %v, want enabled", v)
	}

	// Soft delete → undelete → delete → purge.
	id := b.GetMetadata().GetId()
	runOp(t, func() (operations.Operation, error) { return svc.Delete(ctx, &storagepb.DeleteBucketRequest{Id: id}) })
	b, err = svc.Get(ctx, &storagepb.GetBucketRequest{Id: id})
	if err != nil || b.GetStatus().GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION ||
		b.GetStatus().GetPurgeAt().AsTime().Sub(b.GetStatus().GetDeletedAt().AsTime()) != 7*24*time.Hour {
		t.Fatalf("after delete = %v, %v; want SCHEDULED_FOR_DELETION purging in 7 days", b.GetStatus(), err)
	}
	if _, err := svc.Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "contract-bucket"},
	}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("name of a soft-deleted bucket: code %v, want AlreadyExists (reserved until purge)", status.Code(err))
	}
	runOp(t, func() (operations.Operation, error) {
		return svc.Undelete(ctx, &storagepb.UndeleteBucketRequest{Id: id})
	})
	b, _ = svc.Get(ctx, &storagepb.GetBucketRequest{Id: id})
	if b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE || b.GetStatus().GetDeletedAt() != nil {
		t.Fatalf("after undelete = %v", b.GetStatus())
	}
	runOp(t, func() (operations.Operation, error) { return svc.Delete(ctx, &storagepb.DeleteBucketRequest{Id: id}) })
	runOp(t, func() (operations.Operation, error) { return svc.Purge(ctx, &storagepb.PurgeBucketRequest{Id: id}) })
	if _, err := svc.Get(ctx, &storagepb.GetBucketRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after purge: code %v, want NotFound", status.Code(err))
	}
	if _, ok := objs.Versioning("contract-bucket"); ok {
		t.Error("backing bucket survived purge")
	}
	// The name is free again.
	createBucket(t, svc, inst.ProjectID, "contract-bucket", nil)
}

func TestBucketErrors(t *testing.T) {
	t.Parallel()
	inst, objs := startStorage(t)
	svc := storagesvc.NewBucketService(inst.SDK(t))
	ctx := t.Context()
	b := createBucket(t, svc, inst.ProjectID, "errors-bucket", &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_ENABLED})

	for name, req := range map[string]*storagepb.CreateBucketRequest{
		"bad name":       {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "Bad_Name"}},
		"short name":     {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "ab"}},
		"ip name":        {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "10.0.0.1"}},
		"reserved name":  {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "nebius-mocker-x"}},
		"tenant parent":  {Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "fine-name"}},
		"filesystem":     {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "fs-bucket"}, Spec: &storagepb.BucketSpec{BucketType: &storagepb.BucketSpec_FilesystemBucket{FilesystemBucket: &storagepb.BucketSpec_FilesystemBucketType{FilesystemId: "computefilesystem-e00abc"}}}},
		"negative quota": {Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "neg-bucket"}, Spec: &storagepb.BucketSpec{MaxSizeBytes: -1}},
	} {
		if _, err := svc.Create(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code %v, want InvalidArgument (%v)", name, status.Code(err), err)
		}
	}
	if _, err := svc.Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000missing", Name: "orphan-bucket"},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing project: code %v, want NotFound", status.Code(err))
	}
	if _, err := svc.Update(ctx, &storagepb.UpdateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{Id: b.GetMetadata().GetId(), ParentId: inst.ProjectID},
		Spec:     &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_DISABLED},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("enabled → disabled: code %v, want InvalidArgument", status.Code(err))
	}
	if _, err := svc.Update(ctx, &storagepb.UpdateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{Id: b.GetMetadata().GetId(), ParentId: inst.ProjectID, Name: "renamed-bucket"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("rename: code %v, want InvalidArgument (IMMUTABLE_NAME)", status.Code(err))
	}
	if _, err := svc.Purge(ctx, &storagepb.PurgeBucketRequest{Id: b.GetMetadata().GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("purge active: code %v, want FailedPrecondition", status.Code(err))
	}
	// A non-empty bucket cannot be deleted.
	if _, err := objs.Put(ctx, "errors-bucket", "k", strings.NewReader("x"), 1, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delete(ctx, &storagepb.DeleteBucketRequest{Id: b.GetMetadata().GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete non-empty: code %v, want FailedPrecondition", status.Code(err))
	}
	if err := objs.Delete(ctx, "errors-bucket", "k"); err != nil {
		t.Fatal(err)
	}
	// Zero TTL purges at once.
	op, err := svc.Delete(ctx, &storagepb.DeleteBucketRequest{Id: b.GetMetadata().GetId(), Purge: &storagepb.DeleteBucketRequest_Ttl{Ttl: durationpb.New(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, &storagepb.GetBucketRequest{Id: b.GetMetadata().GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after zero-ttl delete: code %v, want NotFound", status.Code(err))
	}
}

func TestBucketAuthz(t *testing.T) {
	t.Parallel()
	inst, _ := startStorage(t, testkit.WithAuthz(authz.ModeEnforce))
	admin := storagesvc.NewBucketService(inst.SDK(t))
	b := createBucket(t, admin, inst.ProjectID, "authz-bucket", nil)
	viewer := storagesvc.NewBucketService(inst.SDKAs(t, config.DefaultViewerToken))
	if _, err := viewer.Get(t.Context(), &storagepb.GetBucketRequest{Id: b.GetMetadata().GetId()}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.Delete(t.Context(), &storagepb.DeleteBucketRequest{Id: b.GetMetadata().GetId()}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("viewer Delete: code %v, want PermissionDenied", status.Code(err))
	}
	editor := storagesvc.NewBucketService(inst.SDKAs(t, config.DefaultEditorToken))
	createBucket(t, editor, inst.ProjectID, "editor-bucket", nil)
}
