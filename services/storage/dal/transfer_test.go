package dal_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/storage/dal"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

var trT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func trV1(id, name string) *domain.TransferRecord {
	return &domain.TransferRecord{
		API: domain.TransferAPIV1,
		V1: &storagepb.Transfer{
			Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-a", Name: name, ResourceVersion: 1},
			Spec: &storagepb.TransferSpec{
				Source: &storagepb.TransferSource{Prefix: "a/", Provider: &storagepb.TransferSource_S3Compatible{
					S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
						Endpoint: "https://s3.example.com", Region: "us-east-1", BucketName: "src",
						Credentials: &storagepb.TransferSource_S3CompatibleProvider_AccessKey{
							AccessKey: &storagepb.TransferCredentialsAccessKey{AccessKeyId: "AK", SecretAccessKey: "SK"},
						},
					},
				}},
				OverwriteStrategy: storagepb.TransferSpec_NEVER,
			},
		},
		Status: domain.TransferStatus{
			State: domain.TransferActive,
			Error: &domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "no"},
			LastIteration: &domain.Iteration{
				Seq: 3, State: domain.IterationCompleted, Start: trT0, End: trT0.Add(time.Minute), Transferred: 2, TransferredBytes: 10,
			},
		},
		NextRun: trT0.Add(time.Hour), EmptyStreak: 2, Watermark: trT0,
	}
}

func trAlpha(id, parent, name string) *domain.TransferRecord {
	return &domain.TransferRecord{
		API: domain.TransferAPIV1Alpha1,
		Alpha: &storagev1alpha1pb.Transfer{
			Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name},
			Spec: &storagev1alpha1pb.TransferSpec{
				Source:      &storagev1alpha1pb.TransferSpec_SourceBucket{Endpoint: "https://s3.example.com", BucketName: "src", Region: "r"},
				Destination: &storagev1alpha1pb.TransferSpec_DestinationBucket{BucketName: "dst"},
			},
		},
		Status: domain.TransferStatus{State: domain.TransferStopped},
	}
}

func TestTransferRepoRoundTrip(t *testing.T) {
	t.Parallel()
	r := dal.NewTransferRepo(store.NewMemory())
	ctx := t.Context()
	for _, rec := range []*domain.TransferRecord{trV1("storagetransfer-1", "one"), trAlpha("storagetransfer-2", "project-a", "two")} {
		if err := r.Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
		got, err := r.Get(ctx, rec.Metadata().GetId())
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
			t.Errorf("Get round trip (-want +got):\n%s", diff)
		}
		byName, err := r.GetByName(ctx, "project-a", rec.Metadata().GetName())
		if err != nil || byName.Metadata().GetId() != rec.Metadata().GetId() {
			t.Errorf("GetByName = %v, %v", byName, err)
		}
	}
	if _, err := r.Get(ctx, "storagetransfer-x"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get(unknown) = %v, want NotFound", err)
	}
	all, err := r.ListAll(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAll = %d, %v", len(all), err)
	}
}

func TestTransferRepoNamesUniqueAcrossVersions(t *testing.T) {
	t.Parallel()
	r := dal.NewTransferRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, trV1("storagetransfer-1", "same")); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, trAlpha("storagetransfer-2", "project-a", "same")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("same name through v1alpha1: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, trAlpha("storagetransfer-3", "project-b", "same")); err != nil {
		t.Fatalf("same name in another project: %v", err)
	}
	other := trV1("storagetransfer-4", "other")
	if err := r.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	other.V1.Metadata.Name = "same"
	if err := r.Update(ctx, other); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("rename to a taken name: %v, want AlreadyExists", err)
	}
	other.V1.Metadata.Name = "renamed"
	other.Status.State = domain.TransferFailed
	if err := r.Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetByName(ctx, "project-a", "renamed"); err != nil || got.Status.State != domain.TransferFailed {
		t.Fatalf("after rename: %v, %v", got, err)
	}
	if err := r.Update(ctx, trV1("storagetransfer-9", "ghost")); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Update(unknown) = %v, want NotFound", err)
	}
}

func TestTransferRepoListFilterAndPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewTransferRepo(store.NewMemory())
	ctx := t.Context()
	for i, rec := range []*domain.TransferRecord{
		trV1("storagetransfer-1", "t1"), trAlpha("storagetransfer-2", "project-a", "t2"),
		trV1("storagetransfer-3", "t3"), trAlpha("storagetransfer-4", "project-a", "t4"),
		trAlpha("storagetransfer-5", "project-b", "t5"),
	} {
		if err := r.Create(ctx, rec); err != nil {
			t.Fatal(i, err)
		}
	}
	collect := func(only domain.TransferAPI, size int) []string {
		var ids []string
		token := ""
		for range 10 {
			page, err := r.List(ctx, "project-a", only, size, token)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > size {
				t.Fatalf("page of %d items, size %d", len(page.Items), size)
			}
			for _, it := range page.Items {
				ids = append(ids, it.Metadata().GetId())
			}
			if token = page.NextPageToken; token == "" {
				return ids
			}
		}
		t.Fatal("paging does not end")
		return nil
	}
	if diff := cmp.Diff([]string{"storagetransfer-1", "storagetransfer-2", "storagetransfer-3", "storagetransfer-4"}, collect(0, 3)); diff != "" {
		t.Errorf("List all (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"storagetransfer-2", "storagetransfer-4"}, collect(domain.TransferAPIV1Alpha1, 1)); diff != "" {
		t.Errorf("List v1alpha1 (-want +got):\n%s", diff)
	}
	if page, err := r.List(ctx, "project-a", domain.TransferAPIV1Alpha1, 2, ""); err != nil || page.NextPageToken != "" {
		t.Errorf("exact last page: token %q, %v (want no token)", page.NextPageToken, err)
	}
	if _, err := r.List(ctx, "project-a", 0, 1, "!bad"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("malformed token: %v, want BadRequest", err)
	}
}

func TestTransferRepoHistoryAndDelete(t *testing.T) {
	t.Parallel()
	r := dal.NewTransferRepo(store.NewMemory())
	ctx := t.Context()
	const id = "storagetransfer-1"
	if err := r.Create(ctx, trV1(id, "one")); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 5; seq++ {
		it := domain.Iteration{Seq: seq, State: domain.IterationInProgress, Start: trT0}
		if err := r.PutIteration(ctx, id, it); err != nil {
			t.Fatal(err)
		}
		it.State, it.End = domain.IterationCompleted, trT0.Add(time.Duration(seq)*time.Minute)
		if err := r.PutIteration(ctx, id, it); err != nil { // replaces the in-progress row
			t.Fatal(err)
		}
	}
	var seqs []int64
	token := ""
	for {
		page, err := r.History(ctx, id, 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if it.State != domain.IterationCompleted {
				t.Errorf("iteration %d state %v, want replaced by COMPLETED", it.Seq, it.State)
			}
			seqs = append(seqs, it.Seq)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if diff := cmp.Diff([]int64{5, 4, 3, 2, 1}, seqs); diff != "" {
		t.Errorf("History newest first (-want +got):\n%s", diff)
	}
	if err := r.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after Delete = %v, want NotFound", err)
	}
	if page, err := r.History(ctx, id, 10, ""); err != nil || len(page.Items) != 0 {
		t.Errorf("History after Delete = %v, %v; want empty", page.Items, err)
	}
	if err := r.PutIteration(ctx, id, domain.Iteration{Seq: 6}); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("PutIteration after Delete = %v, want NotFound", err)
	}
	if err := r.Delete(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second Delete = %v, want NotFound", err)
	}
	// The name is free again.
	if err := r.Create(ctx, trV1("storagetransfer-2", "one")); err != nil {
		t.Errorf("reuse name after Delete: %v", err)
	}
}

func TestManagedRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewManagedRepo(store.NewMemory())
	ctx := t.Context()
	check := func(dest, key, etag string, want bool) {
		t.Helper()
		got, err := r.Managed(ctx, dest, key, etag)
		if err != nil || got != want {
			t.Errorf("Managed(%s, %s, %s) = %v, %v; want %v", dest, key, etag, got, err, want)
		}
	}
	check("nebius:eu-north1/b", "k", "e1", false)
	if err := r.MarkManaged(ctx, "nebius:eu-north1/b", "k", "e1"); err != nil {
		t.Fatal(err)
	}
	check("nebius:eu-north1/b", "k", "e1", true)
	check("nebius:eu-north1/b", "k", "e2", false) // overwritten by someone else
	check("nebius:eu-north1/other", "k", "e1", false)
	if err := r.MarkManaged(ctx, "nebius:eu-north1/b", "k", "e2"); err != nil {
		t.Fatal(err)
	}
	check("nebius:eu-north1/b", "k", "e2", true)
	for range 2 { // idempotent
		if err := r.Forget(ctx, "nebius:eu-north1/b", "k"); err != nil {
			t.Fatal(err)
		}
	}
	check("nebius:eu-north1/b", "k", "e2", false)
}
