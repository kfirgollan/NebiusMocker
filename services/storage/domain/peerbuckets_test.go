package domain_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestPeerBuckets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var buckets peer.Buckets = f.svc

	if _, err := buckets.BucketByName(t.Context(), projectID, "mlflow-x"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("missing bucket: %v", err)
	}
	created, err := buckets.CreateBucket(t.Context(), projectID, "mlflow-x")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.ProjectID != projectID || created.Name != "mlflow-x" {
		t.Fatalf("created = %+v", created)
	}
	// An ordinary bucket: BucketService sees it, and the peer resolves it.
	if b, err := f.svc.GetByName(t.Context(), projectID, "mlflow-x"); err != nil || b.GetMetadata().GetId() != created.ID {
		t.Fatalf("BucketService.GetByName = %v, %v", b, err)
	}
	if got, err := buckets.BucketByName(t.Context(), projectID, "mlflow-x"); err != nil || got != created {
		t.Fatalf("BucketByName = %+v, %v", got, err)
	}
	for name, tt := range map[string]struct {
		bucket string
		want   errs.Kind
	}{
		"name taken":   {"mlflow-x", errs.KindAlreadyExists},
		"invalid name": {"Not_Valid", errs.KindBadRequest},
	} {
		if _, err := buckets.CreateBucket(t.Context(), projectID, tt.bucket); errs.KindOf(err) != tt.want {
			t.Errorf("%s: kind %v, want %v (%v)", name, errs.KindOf(err), tt.want, err)
		}
	}
}

func TestPeerBucketsSkipsDeletedBuckets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	created, err := f.svc.CreateBucket(t.Context(), projectID, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.BucketByName(t.Context(), projectID, "gone"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("a bucket scheduled for deletion must not resolve: %v", err)
	}
}
