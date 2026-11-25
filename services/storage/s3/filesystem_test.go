package s3

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	fsName  = "fsb"
	fsStore = "fs-store-computefilesystem-1"
)

// fsUpstream records what is relayed to the object store, headers included,
// and answers with a configurable XML body.
type fsUpstream struct {
	mu   sync.Mutex
	reqs []fsRelayed
	resp string
}

type fsRelayed struct {
	method, bucket, key string
	header              http.Header
	body                string
}

func (u *fsUpstream) Forward(w http.ResponseWriter, fr rustfs.ForwardRequest) {
	body, _ := io.ReadAll(fr.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, fsRelayed{fr.In.Method, fr.Bucket, fr.Key, fr.In.Header.Clone(), string(body)})
	resp := u.resp
	u.mu.Unlock()
	w.Header().Set("ETag", `"etag"`)
	switch {
	case fr.In.URL.Query().Has("delete"):
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)
	case resp != "":
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, resp)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (u *fsUpstream) last(t *testing.T) fsRelayed {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("nothing was forwarded")
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *fsUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

// fsBucket is an ACTIVE filesystem bucket of the computefilesystem-1 filesystem.
// Its policy lets the test key delete objects (the IAM checker denies deletes).
func fsBucket() *storagepb.Bucket {
	return &storagepb.Bucket{
		Metadata: &commonpb.ResourceMetadata{Id: "storagebucket-fs", ParentId: "project-e00test", Name: fsName, CreatedAt: timestamppb.Now()},
		Spec: &storagepb.BucketSpec{
			BucketType: &storagepb.BucketSpec_FilesystemBucket{FilesystemBucket: &storagepb.BucketSpec_FilesystemBucketType{FilesystemId: "computefilesystem-1"}},
			BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{
				{Paths: []string{"*"}, Roles: []string{"storage.object-editor"}, Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: policyGroup}},
			}},
		},
		Status: &storagepb.BucketStatus{State: storagepb.BucketStatus_ACTIVE, BucketType: storagepb.BucketStatus_FILESYSTEM},
	}
}

func startFS(t *testing.T, up *fsUpstream, ops *opsFake) *awss3.Client {
	t.Helper()
	srv := startFrontWith(t, up, ops, fsBucket(), bucket("storagebucket-1", "data", storagepb.BucketStatus_ACTIVE))
	return client(srv.URL, testKeyID, testSecret)
}

func httpStatus(err error) int {
	if re, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		return re.HTTPStatusCode()
	}
	return 0
}

func TestFilesystemBucketPutRelaysToStore(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{}
	c := startFS(t, up, &opsFake{})
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(fsName), Key: aws.String("dir/a b.txt"), Body: strings.NewReader("hello")}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got := up.last(t)
	if got.method != http.MethodPut || got.bucket != fsStore || got.key != "dir/a b.txt" || got.body != "hello" {
		t.Fatalf("relayed = %+v, want PUT %s/dir/a b.txt", got, fsStore)
	}
	if class := got.header.Get("X-Amz-Storage-Class"); class != "STANDARD" || class != domain.BackendClass(storagepb.StorageClass_FILESYSTEM) {
		t.Errorf("backend storage class = %q, want STANDARD", class)
	}

	// Regular buckets are unchanged.
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("data"), Key: aws.String("k"), Body: strings.NewReader("v")}); err != nil {
		t.Fatalf("PutObject(data): %v", err)
	}
	if got := up.last(t); got.bucket != "data" || got.key != "k" || got.header.Get("X-Amz-Storage-Class") != "STANDARD" {
		t.Fatalf("regular relayed = %+v, want bucket data", got)
	}
}

func TestFilesystemBucketCopySourceRewritten(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{}
	c := startFS(t, up, &opsFake{})
	_, err := c.CopyObject(t.Context(), &awss3.CopyObjectInput{
		Bucket: aws.String("data"), Key: aws.String("copy"), CopySource: aws.String(fsName + "/" + url.PathEscape("a b") + "/c.txt"),
	})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	got := up.last(t)
	if got.bucket != "data" || got.key != "copy" {
		t.Fatalf("relayed = %+v, want the copy into data", got)
	}
	if src := got.header.Get("X-Amz-Copy-Source"); src != "/"+fsStore+"/a%20b/c.txt" {
		t.Errorf("X-Amz-Copy-Source = %q, want /%s/a%%20b/c.txt", src, fsStore)
	}

	// Copy into a filesystem bucket from a regular one keeps the source.
	if _, err := c.CopyObject(t.Context(), &awss3.CopyObjectInput{Bucket: aws.String(fsName), Key: aws.String("in"), CopySource: aws.String("data/k")}); err != nil {
		t.Fatalf("CopyObject into fs: %v", err)
	}
	got = up.last(t)
	if src := strings.TrimPrefix(got.header.Get("X-Amz-Copy-Source"), "/"); got.bucket != fsStore || got.key != "in" || src != "data/k" {
		t.Fatalf("relayed = %+v (copy source %q), want fs store dest and the regular source", got, got.header.Get("X-Amz-Copy-Source"))
	}
}

func TestFilesystemBucketListMapsNamesAndClass(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{resp: `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Name>` + fsStore + `</Name><Prefix></Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>` +
		`<Contents><Key>a.txt</Key><Size>1</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`}
	c := startFS(t, up, &opsFake{})
	out, err := c.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: aws.String(fsName)})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if got := up.last(t); got.bucket != fsStore || got.method != http.MethodGet {
		t.Fatalf("relayed = %+v, want GET on %s", got, fsStore)
	}
	if aws.ToString(out.Name) != fsName {
		t.Errorf("listing Name = %q, want %q", aws.ToString(out.Name), fsName)
	}
	if len(out.Contents) != 1 || out.Contents[0].StorageClass != types.ObjectStorageClass("FILESYSTEM") {
		t.Fatalf("Contents = %+v, want one object in the FILESYSTEM class", out.Contents)
	}
}

func TestFilesystemBucketUnsupportedCalls(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{}
	c := startFS(t, up, &opsFake{})
	ctx := t.Context()
	tests := map[string]error{
		"PutBucketVersioning": errOf(c.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
			Bucket: aws.String(fsName), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
		})),
		"PutObjectTagging": errOf(c.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{
			Bucket: aws.String(fsName), Key: aws.String("k"), Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("b")}}},
		})),
		"GetObjectTagging": errOf(c.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Bucket: aws.String(fsName), Key: aws.String("k")})),
	}
	for name, err := range tests {
		if code(err) != "NotImplemented" || httpStatus(err) != http.StatusNotImplemented {
			t.Errorf("%s: %q status %d (%v), want 501 NotImplemented", name, code(err), httpStatus(err), err)
		}
	}
	if n := up.count(); n != 0 {
		t.Errorf("%d unsupported calls reached the store", n)
	}
}

func TestFilesystemBucketStorageClass(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{}
	c := startFS(t, up, &opsFake{})
	put := func(class types.StorageClass) error {
		_, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(fsName), Key: aws.String("k"), Body: strings.NewReader("v"), StorageClass: class})
		return err
	}
	if err := put(types.StorageClassStandard); code(err) != "InvalidStorageClass" || httpStatus(err) != http.StatusBadRequest {
		t.Fatalf("STANDARD upload = %q status %d (%v), want 400 InvalidStorageClass", code(err), httpStatus(err), err)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("the refused upload reached the store (%d requests)", n)
	}
	if err := put(types.StorageClass("FILESYSTEM")); err != nil {
		t.Fatalf("FILESYSTEM upload: %v", err)
	}
	if err := put(""); err != nil {
		t.Fatalf("upload without a class: %v", err)
	}
	if got := up.last(t); got.bucket != fsStore || got.header.Get("X-Amz-Storage-Class") != "STANDARD" {
		t.Fatalf("relayed = %+v, want the store bucket with backend class STANDARD", got)
	}
}

func TestFilesystemBucketCapacityConsulted(t *testing.T) {
	t.Parallel()
	full := &errs.Error{Kind: errs.KindQuotaExceeded, Message: "no space left on filesystem"}
	up := &fsUpstream{}
	ops := &opsFake{capErr: full}
	c := startFS(t, up, ops)
	_, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(fsName), Key: aws.String("k"), Body: strings.NewReader("v")})
	if code(err) != "QuotaExceeded" || httpStatus(err) != http.StatusBadRequest {
		t.Fatalf("PutObject to a full filesystem = %q status %d (%v), want 400 QuotaExceeded", code(err), httpStatus(err), err)
	}
	if n := up.count(); n != 0 {
		t.Errorf("the refused write reached the store (%d requests)", n)
	}
	ops.mu.Lock()
	consulted := ops.capCalls
	ops.mu.Unlock()
	if consulted == 0 {
		t.Error("CheckCapacity was not consulted for a filesystem bucket without max_size_bytes")
	}

	// Regular buckets without max_size_bytes skip the capacity check.
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("data"), Key: aws.String("k"), Body: strings.NewReader("v")}); err != nil {
		t.Fatalf("PutObject(data): %v", err)
	}
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if ops.capCalls != consulted {
		t.Errorf("CheckCapacity called %d times for a regular bucket without a limit, want 0", ops.capCalls-consulted)
	}
}

func TestFilesystemBucketDeleteObjectsRelaysToStore(t *testing.T) {
	t.Parallel()
	up := &fsUpstream{}
	c := startFS(t, up, &opsFake{})
	_, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{Bucket: aws.String(fsName), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("a")}, {Key: aws.String("dir/b")}},
	}})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	got := up.last(t)
	if got.method != http.MethodPost || got.bucket != fsStore || !strings.Contains(got.body, "<Key>a</Key>") || !strings.Contains(got.body, "<Key>dir/b</Key>") {
		t.Fatalf("relayed = %+v, want POST ?delete on %s with both keys", got, fsStore)
	}
}
