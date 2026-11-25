//go:build e2e_storage

package rustfs_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/objstoretest"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// TestConformance runs the shared objstore suite against a real RustFS container.
func TestConformance(t *testing.T) {
	srv := rustfsfixture.Start(t)
	objstoretest.Run(t, srv.Client(t), "conformance-bucket")
}

// TestInventory covers what only RustFS offers: version history, delete
// markers, multipart uploads and tags.
func TestInventory(t *testing.T) {
	srv := rustfsfixture.Start(t)
	ctx := t.Context()
	store := srv.Client(t)
	raw := s3.New(s3.Options{
		Region:       rustfs.DefaultRegion,
		BaseEndpoint: aws.String(srv.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
	})
	const bucket = "inventory-bucket"
	if err := store.EnsureBucket(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DeleteBucket(context.WithoutCancel(t.Context()), bucket) }) // best-effort
	if err := store.SetVersioning(ctx, bucket, objstore.VersioningEnabled); err != nil {
		t.Fatal(err)
	}

	// Two versions and a delete marker, newest first.
	for _, body := range []string{"abc", "defgh"} {
		if _, err := store.Put(ctx, bucket, "hist", strings.NewReader(body), int64(len(body)), objstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, bucket, "hist"); err != nil {
		t.Fatal(err)
	}
	hist := collectVersions(t, store, bucket, "hist")
	t.Logf("versions: %+v", hist)
	if len(hist) != 3 || !hist[0].DeleteMarker || !hist[0].IsLatest || hist[1].Size != 5 || hist[2].Size != 3 ||
		hist[1].IsLatest || hist[2].IsLatest || hist[1].VersionID == "" || hist[2].StorageClass != "STANDARD" {
		t.Fatalf("ListVersions(hist) = %+v, want [latest marker, 5 bytes, 3 bytes]", hist)
	}

	if err := store.DeleteVersion(ctx, bucket, "hist", hist[2].VersionID); err != nil {
		t.Fatal(err)
	}
	if got := collectVersions(t, store, bucket, "hist"); len(got) != 2 {
		t.Errorf("versions after DeleteVersion = %+v, want 2", got)
	}

	// A completed single-part multipart object is Multipart() and taggable.
	create, err := raw.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mp")})
	if err != nil {
		t.Fatal(err)
	}
	part, err := raw.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String("mp"), UploadId: create.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("multipart-body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("mp"), UploadId: create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
	}); err != nil {
		t.Fatal(err)
	}
	mps := collectVersions(t, store, bucket, "mp")
	t.Logf("multipart versions: %+v", mps)
	if len(mps) != 1 || !mps[0].Multipart() || mps[0].Size != int64(len("multipart-body")) {
		t.Fatalf("multipart versions = %+v, want one Multipart() version with its size", mps)
	}

	if _, err := raw.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String(bucket), Key: aws.String("mp"), VersionId: aws.String(mps[0].VersionID),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("env"), Value: aws.String("test")}}},
	}); err != nil {
		t.Fatal(err)
	}
	tags, err := store.ObjectTags(ctx, bucket, "mp", mps[0].VersionID)
	if err != nil || len(tags) != 1 || tags["env"] != "test" {
		t.Errorf("ObjectTags = %v, %v; want env=test", tags, err)
	}

	// An in-flight upload with one part is listed with its size, then aborted.
	pending, err := raw.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("pending")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String("pending"), UploadId: pending.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("sixsix"),
	}); err != nil {
		t.Fatal(err)
	}
	var ups []objstore.Upload
	for u, err := range store.ListUploads(ctx, bucket) {
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, u)
	}
	t.Logf("uploads: %+v", ups)
	if len(ups) != 1 || ups[0].Key != "pending" || ups[0].UploadID != aws.ToString(pending.UploadId) ||
		ups[0].Parts != 1 || ups[0].PartsSize != 6 || ups[0].StorageClass != "STANDARD" || ups[0].Initiated.IsZero() {
		t.Fatalf("ListUploads = %+v, want the pending upload with 1 part of 6 bytes", ups)
	}
	if err := store.AbortUpload(ctx, bucket, "pending", ups[0].UploadID); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortUpload(ctx, bucket, "pending", ups[0].UploadID); err != nil {
		t.Errorf("AbortUpload (again) = %v, want nil", err)
	}
	for u, err := range store.ListUploads(ctx, bucket) {
		t.Errorf("ListUploads after abort = %+v, %v; want nothing", u, err)
	}
}

func collectVersions(t *testing.T, store *rustfs.Client, bucket, prefix string) []objstore.Version {
	t.Helper()
	var out []objstore.Version
	for v, err := range store.ListVersions(t.Context(), bucket, prefix) {
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		out = append(out, v)
	}
	return out
}
