//go:build e2e_storage

// Package storage tests Object Storage end to end against a real RustFS
// container: buckets through the official Nebius Go SDK, objects through the
// AWS SDK for Go v2 (the S3 client Nebius documents) via the mocker's S3 front,
// authenticated with access keys created through the Nebius Go SDK.
//
// Run with: make e2e-storage   (needs Docker)
package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// env is a mocker backed by RustFS, with the S3 front on a random port.
type env struct {
	inst   *testkit.Instance
	sdk    *gosdk.SDK
	s3Addr string
	rustfs *rustfsfixture.Server
}

func start(t *testing.T) *env {
	t.Helper()
	srv := rustfsfixture.Start(t)
	mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mod},
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(50*time.Millisecond))
	return &env{inst: inst, sdk: inst.SDK(t), s3Addr: mod.S3Addr(), rustfs: srv}
}

func unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// wait returns a function that waits for a started operation to succeed:
// wait(t)(svc.Create(ctx, req)).
func wait(t *testing.T) func(operations.Operation, error) operations.Operation {
	t.Helper()
	return func(op operations.Operation, err error) operations.Operation {
		t.Helper()
		if err != nil {
			t.Fatalf("start operation: %v", err)
		}
		done, err := op.Wait(t.Context())
		if err != nil {
			t.Fatalf("operation %s: %v", op.ID(), err)
		}
		return done
	}
}

// s3Client is the AWS SDK for Go v2 configured the way Nebius documents for
// S3 tools (endpoint + region + access key), pointed at the mocker.
func (e *env) s3Client(keyID, secret string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "eu-north1",
		BaseEndpoint: aws.String("http://" + e.s3Addr),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})
}

// grantedKey creates a service account with role on the default project and
// an access key for it, all through the Nebius Go SDK.
func (e *env) grantedKey(t *testing.T, role string) (keyID, secret, accessKeyID string) {
	t.Helper()
	ctx := t.Context()
	sa := wait(t)(iamsvc.NewServiceAccountService(e.sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: unique("sa")},
		Spec:     &iampb.ServiceAccountSpec{},
	})).ResourceID()
	group := wait(t)(iamsvc.NewGroupService(e.sdk).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: unique("g")}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(t)(iamsvc.NewGroupMembershipService(e.sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group}, Spec: &iampb.GroupMembershipSpec{MemberId: sa},
	}))
	wait(t)(iamsvc.NewAccessPermitService(e.sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group},
		Spec:     &iampb.AccessPermitSpec{ResourceId: e.inst.ProjectID, Role: role},
	}))
	keys := iamv2svc.NewAccessKeyService(e.sdk)
	id := wait(t)(keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID},
		Spec: &iamv2pb.AccessKeySpec{
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: sa}}},
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_EXPLICIT,
		},
	})).ResourceID()
	sec, err := keys.GetSecret(ctx, &iamv2pb.GetAccessKeySecretRequest{Id: id})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	return sec.GetAwsAccessKeyId(), sec.GetSecret(), id
}

func s3Code(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorCode()
	}
	return ""
}

func getObject(t *testing.T, c *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }() // read fully below
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return b
}

func listKeys(t *testing.T, c *s3.Client, bucket string) []string {
	t.Helper()
	out, err := c.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2 %s: %v", bucket, err)
	}
	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	return keys
}

// TestBucketsAndObjectsWithSDKs is the Go SDK done-criterion: full bucket
// CRUD (plus purge/undelete) with gosdk and full object CRUD with the AWS SDK,
// verifying the RustFS side after each step.
func TestBucketsAndObjectsWithSDKs(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	buckets := storagesvc.NewBucketService(e.sdk)
	name := unique("sdk-bucket")

	// Create → Get → GetByName → List.
	id := wait(t)(buckets.Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name, Labels: map[string]string{"team": "a"}},
		Spec:     &storagepb.BucketSpec{MaxSizeBytes: 10 << 20},
	})).ResourceID()
	b, err := buckets.Get(ctx, &storagepb.GetBucketRequest{Id: id})
	if err != nil || b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
		t.Fatalf("Get = %v, %v; want ACTIVE", b, err)
	}
	if byName, err := buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.inst.ProjectID, Name: name}); err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	if list, err := buckets.List(ctx, &storagepb.ListBucketsRequest{ParentId: e.inst.ProjectID}); err != nil || !containsBucket(list.GetItems(), id) {
		t.Fatalf("List = %v, %v; want it to contain %s", list, err, id)
	}
	if empty, err := e.rustfs.Client(t).BucketEmpty(ctx, name); err != nil || !empty {
		t.Fatalf("RustFS bucket %s: empty=%v err=%v; want an existing empty bucket", name, empty, err)
	}

	// Update: enable versioning and change labels; RustFS follows.
	wait(t)(buckets.Update(ctx, &storagepb.UpdateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: e.inst.ProjectID, Labels: map[string]string{"team": "b"}},
		Spec:     &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_ENABLED, MaxSizeBytes: 10 << 20},
	}))
	if v, err := e.rustfs.Client(t).Versioning(ctx, name); err != nil || v != objstore.VersioningEnabled {
		t.Fatalf("RustFS versioning = %v, %v; want enabled", v, err)
	}

	// Objects with the AWS SDK and a Nebius access key.
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)
	put := func(key string, data []byte) {
		t.Helper()
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(name), Key: aws.String(key), Body: bytes.NewReader(data), ContentType: aws.String("text/plain"),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	put("docs/hello.txt", []byte("hello nebius"))
	put("docs/with space+plus.txt", []byte("odd key"))
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	put("blobs/big.bin", big)
	if got := getObject(t, c, name, "docs/hello.txt"); string(got) != "hello nebius" {
		t.Fatalf("GetObject = %q", got)
	}
	if got := getObject(t, c, name, "docs/with space+plus.txt"); string(got) != "odd key" {
		t.Fatalf("GetObject (odd key) = %q", got)
	}
	if got := getObject(t, c, name, "blobs/big.bin"); !bytes.Equal(got, big) {
		t.Fatalf("GetObject (1 MiB) differs: %d bytes", len(got))
	}
	if keys := listKeys(t, c, name); strings.Join(keys, ",") != "blobs/big.bin,docs/hello.txt,docs/with space+plus.txt" {
		t.Fatalf("ListObjectsV2 = %v", keys)
	}
	put("docs/hello.txt", []byte("hello again")) // overwrite
	if got := getObject(t, c, name, "docs/hello.txt"); string(got) != "hello again" {
		t.Fatalf("GetObject after overwrite = %q", got)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(name), Key: aws.String("docs/hello.txt")})
	if err != nil || aws.ToInt64(head.ContentLength) != int64(len("hello again")) {
		t.Fatalf("HeadObject = %v, %v", head, err)
	}
	if loc, err := c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(name)}); err != nil || string(loc.LocationConstraint) != "eu-north1" {
		t.Fatalf("GetBucketLocation = %v, %v; want eu-north1", loc, err)
	}
	if lb, err := c.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil || len(lb.Buckets) == 0 {
		t.Fatalf("ListBuckets = %v, %v", lb, err)
	}

	// A non-empty bucket cannot be deleted.
	if _, err := buckets.Delete(ctx, &storagepb.DeleteBucketRequest{Id: id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Delete non-empty: code %v, want FailedPrecondition (%v)", status.Code(err), err)
	}
	for _, k := range []string{"docs/hello.txt", "docs/with space+plus.txt", "blobs/big.bin"} {
		if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String(k)}); err != nil {
			t.Fatalf("DeleteObject %s: %v", k, err)
		}
	}
	if keys := listKeys(t, c, name); len(keys) != 0 {
		t.Fatalf("ListObjectsV2 after delete = %v, want empty", keys)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("docs/hello.txt")}); s3Code(err) != "NoSuchKey" {
		t.Fatalf("GetObject deleted: %v, want NoSuchKey", err)
	}

	// Soft delete hides the bucket from S3; undelete brings it back; purge removes it from RustFS.
	wait(t)(buckets.Delete(ctx, &storagepb.DeleteBucketRequest{Id: id}))
	if _, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(name)}); s3Code(err) != "NoSuchBucket" {
		t.Fatalf("S3 on a soft-deleted bucket: %v, want NoSuchBucket", err)
	}
	wait(t)(buckets.Undelete(ctx, &storagepb.UndeleteBucketRequest{Id: id}))
	listKeys(t, c, name)
	wait(t)(buckets.Delete(ctx, &storagepb.DeleteBucketRequest{Id: id}))
	wait(t)(buckets.Purge(ctx, &storagepb.PurgeBucketRequest{Id: id}))
	if _, err := buckets.Get(ctx, &storagepb.GetBucketRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get after purge: %v, want NotFound", err)
	}
	if _, err := e.rustfs.Client(t).BucketEmpty(ctx, name); !errors.Is(err, objstore.ErrBucketNotFound) {
		t.Fatalf("RustFS bucket after purge: err %v, want ErrBucketNotFound (old versions purged too)", err)
	}
}

// TestS3FrontRejects covers authentication and authorization at the S3 front.
func TestS3FrontRejects(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	name := unique("guarded")
	wait(t)(storagesvc.NewBucketService(e.sdk).Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
	}))
	editorKey, editorSecret, editorKeyNID := e.grantedKey(t, "editor")
	viewerKey, viewerSecret, _ := e.grantedKey(t, "viewer")
	putErr := func(c *s3.Client, bucket string) error {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("k"), Body: strings.NewReader("v")})
		return err
	}
	if err := putErr(e.s3Client(editorKey, editorSecret), name); err != nil {
		t.Fatalf("editor PutObject: %v", err)
	}
	tests := []struct {
		name   string
		client *s3.Client
		bucket string
		want   string
	}{
		{"wrong secret", e.s3Client(editorKey, "not-the-secret"), name, "SignatureDoesNotMatch"},
		{"unknown key", e.s3Client("NMKDOESNOTEXIST00000", "x"), name, "InvalidAccessKeyId"},
		{"viewer cannot write", e.s3Client(viewerKey, viewerSecret), name, "AccessDenied"},
		{"unknown bucket", e.s3Client(editorKey, editorSecret), unique("nope"), "NoSuchBucket"},
	}
	for _, tt := range tests {
		if got := s3Code(putErr(tt.client, tt.bucket)); got != tt.want {
			t.Errorf("%s: S3 error %q, want %q", tt.name, got, tt.want)
		}
	}
	if got := getObject(t, e.s3Client(viewerKey, viewerSecret), name, "k"); string(got) != "v" {
		t.Errorf("viewer GetObject = %q", got)
	}
	if _, err := e.s3Client(editorKey, editorSecret).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(unique("s3made"))}); s3Code(err) != "NotImplemented" {
		t.Errorf("S3 CreateBucket: %v, want NotImplemented (buckets are Nebius resources)", err)
	}
	// A deactivated key stops working at once.
	wait(t)(iamv2svc.NewAccessKeyService(e.sdk).Deactivate(ctx, &iamv2pb.DeactivateAccessKeyRequest{Id: editorKeyNID}))
	if got := s3Code(putErr(e.s3Client(editorKey, editorSecret), name)); got != "InvalidAccessKeyId" {
		t.Errorf("deactivated key: S3 error %q, want InvalidAccessKeyId", got)
	}
}

func containsBucket(items []*storagepb.Bucket, id string) bool {
	for _, b := range items {
		if b.GetMetadata().GetId() == id {
			return true
		}
	}
	return false
}
