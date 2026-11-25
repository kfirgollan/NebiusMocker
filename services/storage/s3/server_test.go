package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	testKeyID  = "NMKTESTKEY0000000001"
	testSecret = "test-secret-test-secret-test-secret-0000"
)

// bucketsFake serves a fixed set of buckets (read-only use by the gate).
type bucketsFake struct{ items []*storagepb.Bucket }

func (f bucketsFake) byName(name string) (*storagepb.Bucket, error) {
	for _, b := range f.items {
		if b.GetMetadata().GetName() == name {
			return proto.CloneOf(b), nil
		}
	}
	return nil, errs.NotFound(name)
}

func (f bucketsFake) Get(_ context.Context, id string) (*storagepb.Bucket, error) {
	for _, b := range f.items {
		if b.GetMetadata().GetId() == id {
			return proto.CloneOf(b), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f bucketsFake) GetByName(_ context.Context, _, name string) (*storagepb.Bucket, error) {
	return f.byName(name)
}

func (f bucketsFake) GetByBucketName(_ context.Context, name string) (*storagepb.Bucket, error) {
	return f.byName(name)
}

func (f bucketsFake) List(context.Context, string, int, string) (domain.BucketPage, error) {
	return domain.BucketPage{Items: f.items}, nil
}

func (f bucketsFake) ListAll(context.Context) ([]*storagepb.Bucket, error) { return f.items, nil }

func (bucketsFake) Create(context.Context, *storagepb.Bucket) error { return errors.New("read-only") }

func (bucketsFake) Update(context.Context, *storagepb.Bucket) error { return errors.New("read-only") }

func (bucketsFake) Delete(context.Context, string) error { return errors.New("read-only") }

type keysFake struct{}

func (keysFake) ResolveAccessKey(_ context.Context, id string) (peer.AccessKey, error) {
	if id != testKeyID {
		return peer.AccessKey{}, errs.Unauthenticated("unknown key")
	}
	return peer.AccessKey{AccessKeyID: id, Secret: testSecret, PrincipalID: "serviceaccount-e00test"}, nil
}

// checkerFake allows reads and writes but not deletes.
type checkerFake struct{}

func (checkerFake) Check(_ context.Context, _ string, perm authz.Permission, target string) (authz.Decision, error) {
	return authz.Decision{Allowed: perm != domain.PermObjectDelete, Permission: perm, Target: target}, nil
}

// upstreamFake records forwarded requests and answers 200.
type upstreamFake struct {
	mu   sync.Mutex
	reqs []forwarded
}

type forwarded struct {
	method, bucket, key, payloadHash string
	body                             []byte
	length                           int64
}

func (u *upstreamFake) Forward(w http.ResponseWriter, fr rustfs.ForwardRequest) {
	body, _ := io.ReadAll(fr.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, forwarded{fr.In.Method, fr.Bucket, fr.Key, fr.PayloadHash, body, fr.ContentLength})
	u.mu.Unlock()
	if fr.In.URL.Query().Has("delete") {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)
		return
	}
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
}

// membersFake puts the test key's account in policyGroup.
type membersFake struct{}

const policyGroup = "group-e00policy00000000001"

func (membersFake) IsMember(_ context.Context, principal, group string) (bool, error) {
	return principal == "serviceaccount-e00test" && group == policyGroup, nil
}

func (u *upstreamFake) last(t *testing.T) forwarded {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("nothing was forwarded")
	}
	return u.reqs[len(u.reqs)-1]
}

func bucket(id, name string, state storagepb.BucketStatus_State) *storagepb.Bucket {
	return &storagepb.Bucket{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-e00test", Name: name, CreatedAt: timestamppb.Now()},
		Status:   &storagepb.BucketStatus{State: state},
	}
}

func startFront(t *testing.T, up Upstream) *httptest.Server {
	t.Helper()
	return startFrontWith(t, up, &opsFake{},
		bucket("storagebucket-1", "data", storagepb.BucketStatus_ACTIVE),
		bucket("storagebucket-2", "deleted", storagepb.BucketStatus_SCHEDULED_FOR_DELETION))
}

// hierarchyFake places every principal in project-e00test.
type hierarchyFake struct{}

func (hierarchyFake) Parent(context.Context, string) (string, error) { return "project-e00test", nil }

// opsFake records the bucket operations the front performs.
type opsFake struct {
	mu       sync.Mutex
	created  []string
	deleted  []string
	specs    map[string]*storagepb.BucketSpec
	used     int64
	sizes    map[string]int64
	updateFn func(*storagepb.BucketSpec) error
	// capErr, when set, is returned by CheckCapacity whatever the bucket's
	// own limit (a filesystem that is full); capCalls counts the calls.
	capErr   error
	capCalls int
}

func (o *opsFake) CreateFromS3(_ context.Context, projectID, name string) (*storagepb.Bucket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.created = append(o.created, projectID+"/"+name)
	return bucket("storagebucket-new", name, storagepb.BucketStatus_ACTIVE), nil
}

func (o *opsFake) DeleteFromS3(_ context.Context, id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, id)
	return nil
}

func (o *opsFake) UpdateSpecFromS3(_ context.Context, id string, mutate func(*storagepb.BucketSpec)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.specs == nil {
		o.specs = map[string]*storagepb.BucketSpec{}
	}
	spec := &storagepb.BucketSpec{}
	mutate(spec)
	if o.updateFn != nil {
		if err := o.updateFn(spec); err != nil {
			return err
		}
	}
	o.specs[id] = spec
	return nil
}

func (o *opsFake) CheckCapacity(_ context.Context, b *storagepb.Bucket, incoming int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.capCalls++
	if o.capErr != nil {
		return o.capErr
	}
	if limit := b.GetSpec().GetMaxSizeBytes(); limit > 0 && o.used+incoming > limit {
		return &errs.Error{Kind: errs.KindQuotaExceeded, Message: "full"}
	}
	return nil
}

func (o *opsFake) ObjectSize(_ context.Context, bucket, key, _ string) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n, ok := o.sizes[bucket+"/"+key]; ok {
		return n, nil
	}
	return 0, objstore.ErrNotFound
}

// Backend maps filesystem buckets to "fs-store-<filesystem_id>".
func (o *opsFake) Backend(_ context.Context, b *storagepb.Bucket) (string, error) {
	if fb := b.GetSpec().GetFilesystemBucket(); fb != nil {
		return "fs-store-" + fb.GetFilesystemId(), nil
	}
	return b.GetMetadata().GetName(), nil
}

func startFrontWith(t *testing.T, up Upstream, ops BucketOps, buckets ...*storagepb.Bucket) *httptest.Server {
	t.Helper()
	gate, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: bucketsFake{items: buckets}, Keys: keysFake{}, Checker: checkerFake{}, Hierarchy: hierarchyFake{}, Mode: authz.ModeEnforce,
		Members: func() domain.GroupMembers { return membersFake{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(Config{Domain: "s3.test", Region: "eu-north1"}, gate, ops, up, clock.Real{}, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv
}

func client(url, keyID, secret string) *awss3.Client {
	return awss3.New(awss3.Options{
		Region: "eu-north1", BaseEndpoint: aws.String(url), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})
}

func regionClient(url, region string) *awss3.Client {
	return awss3.New(awss3.Options{
		Region: region, BaseEndpoint: aws.String(url), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(testKeyID, testSecret, ""),
	})
}

func anonymousClient(url string) *awss3.Client {
	return awss3.New(awss3.Options{Region: "eu-north1", BaseEndpoint: aws.String(url), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}})
}

func code(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorCode()
	}
	return ""
}

func TestSigV4AcceptAndForward(t *testing.T) {
	t.Parallel()
	up := &upstreamFake{}
	srv := startFront(t, up)
	c := client(srv.URL, testKeyID, testSecret)
	ctx := t.Context()

	// Seekable body: signed payload hash, relayed as declared.
	if _, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("data"), Key: aws.String("dir/a b.txt"), Body: bytes.NewReader([]byte("hello"))}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got := up.last(t)
	if got.method != http.MethodPut || got.bucket != "data" || got.key != "dir/a b.txt" || string(got.body) != "hello" || got.length != 5 {
		t.Fatalf("forwarded = %+v", got)
	}
	// Non-seekable body with UNSIGNED-PAYLOAD (aws-chunked bodies are covered in pkg/sigv4).
	if _, err := c.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("data"), Key: aws.String("stream"), Body: io.MultiReader(strings.NewReader("stream"), strings.NewReader("ed")), ContentLength: aws.Int64(8),
	}, awss3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)); err != nil {
		t.Fatalf("PutObject (stream): %v", err)
	}
	if got := up.last(t); string(got.body) != "streamed" || got.length != 8 || got.payloadHash != "UNSIGNED-PAYLOAD" {
		t.Fatalf("forwarded stream = %+v (%q)", got, got.body)
	}
	if _, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("data"), Key: aws.String("stream")}); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if got := up.last(t); got.method != http.MethodHead {
		t.Fatalf("forwarded = %+v, want HEAD", got)
	}

	// Answered locally from Nebius metadata.
	loc, err := c.GetBucketLocation(ctx, &awss3.GetBucketLocationInput{Bucket: aws.String("data")})
	if err != nil || string(loc.LocationConstraint) != "eu-north1" {
		t.Fatalf("GetBucketLocation = %v, %v", loc, err)
	}
	lb, err := c.ListBuckets(ctx, &awss3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 1 || aws.ToString(lb.Buckets[0].Name) != "data" {
		t.Fatalf("ListBuckets = %v, %v; want only the ACTIVE bucket", lb, err)
	}

	// Presigned GET.
	ps := awss3.NewPresignClient(c)
	req, err := ps.PresignGetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("data"), Key: aws.String("dir/a b.txt")}, awss3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, http.NoBody)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || up.last(t).key != "dir/a b.txt" {
		t.Fatalf("presigned GET: status %d, forwarded %+v", resp.StatusCode, up.last(t))
	}
}

func TestSigV4AndAccessRejections(t *testing.T) {
	t.Parallel()
	up := &upstreamFake{}
	srv := startFront(t, up)
	ctx := t.Context()
	put := func(c *awss3.Client, b string) error {
		_, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("k"), Body: strings.NewReader("v")})
		return err
	}
	good := client(srv.URL, testKeyID, testSecret)
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"wrong secret", put(client(srv.URL, testKeyID, "wrong"), "data"), "SignatureDoesNotMatch"},
		{"unknown key", put(client(srv.URL, "NMKUNKNOWN0000000000", testSecret), "data"), "InvalidAccessKeyId"},
		{"unknown bucket", put(good, "nope"), "NoSuchBucket"},
		{"soft-deleted bucket", put(good, "deleted"), "NoSuchBucket"},
		{"permission denied", errOf(good.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("data"), Key: aws.String("k")})), "AccessDenied"},
		{"AWS bucket policy over S3 (Nebius policies use the Nebius API)", errOf(good.PutBucketPolicy(ctx, &awss3.PutBucketPolicyInput{
			Bucket: aws.String("data"), Policy: aws.String("{}"),
		})), "NotImplemented"},
		{"credential scope for another region", put(regionClient(srv.URL, "us-east-1"), "data"), "AuthorizationHeaderMalformed"},
		{"anonymous write without a policy", put(anonymousClient(srv.URL), "data"), "AccessDenied"},
	}
	for _, tt := range tests {
		if got := code(tt.err); got != tt.want {
			t.Errorf("%s: S3 error %q (%v), want %q", tt.name, got, tt.err, tt.want)
		}
	}
	up.mu.Lock()
	n := len(up.reqs)
	up.mu.Unlock()
	if n != 0 {
		t.Errorf("%d rejected requests reached the upstream", n)
	}

	// Anonymous requests are refused.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/data/k", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "<Code>AccessDenied</Code>") {
		t.Fatalf("anonymous: %d %s", resp.StatusCode, body)
	}
}

func TestNoDataPlane(t *testing.T) {
	t.Parallel()
	srv := startFront(t, NoDataPlane{})
	_, err := client(srv.URL, testKeyID, testSecret).GetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String("data"), Key: aws.String("k")})
	if code(err) != "NotImplemented" {
		t.Fatalf("GetObject without RustFS: %v, want NotImplemented", err)
	}
}

func TestParseTarget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ host, path, bucket, key string }{
		{"127.0.0.1:4580", "/data/dir/a.txt", "data", "dir/a.txt"},
		{"127.0.0.1:4580", "/data", "data", ""},
		{"127.0.0.1:4580", "/", "", ""},
		{"data.s3.test:4580", "/dir/a.txt", "data", "dir/a.txt"},
		{"data.s3.test", "/", "data", ""},
		{"s3.test", "/data/k", "data", "k"},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+tt.host+tt.path, http.NoBody)
		if got := parseTarget(r, "s3.test"); got.Bucket != tt.bucket || got.Key != tt.key {
			t.Errorf("parseTarget(%s%s) = %+v, want %s/%s", tt.host, tt.path, got, tt.bucket, tt.key)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		method, target string
		header         string
		want           domain.S3Action
		wantErr        bool
	}{
		{http.MethodGet, "/", "", domain.ActListBuckets, false},
		{http.MethodGet, "/b?list-type=2&prefix=p/", "", domain.ActListObjects, false},
		{http.MethodGet, "/b", "", domain.ActListObjects, false},
		{http.MethodGet, "/b?versioning", "", domain.ActGetBucketVersioning, false},
		{http.MethodGet, "/b?lifecycle", "", domain.ActGetBucketLifecycle, false},
		{http.MethodGet, "/b?cors", "", domain.ActGetBucketCors, false},
		{http.MethodGet, "/b?location", "", domain.ActGetBucketLocation, false},
		{http.MethodGet, "/b?versions", "", domain.ActListObjectVersions, false},
		{http.MethodGet, "/b?uploads", "", domain.ActListMultipartUploads, false},
		{http.MethodGet, "/b?acl", "", domain.ActGetBucketOther, false},
		{http.MethodHead, "/b", "", domain.ActHeadBucket, false},
		{http.MethodPost, "/b?delete", "", domain.ActDeleteObjects, false},
		{http.MethodPut, "/b", "", domain.ActCreateBucket, false},
		{http.MethodPut, "/b?versioning", "", domain.ActPutBucketVersioning, false},
		{http.MethodPut, "/b?lifecycle", "", domain.ActPutBucketLifecycle, false},
		{http.MethodPut, "/b?cors", "", domain.ActPutBucketCors, false},
		{http.MethodPut, "/b?policy", "", "", true},
		{http.MethodDelete, "/b", "", domain.ActDeleteBucket, false},
		{http.MethodDelete, "/b?cors", "", domain.ActDeleteBucketCors, false},
		{http.MethodDelete, "/b?lifecycle", "", domain.ActDeleteBucketLifecycle, false},
		{http.MethodDelete, "/b?website", "", "", true},
		{http.MethodGet, "/b/k", "", domain.ActGetObject, false},
		{http.MethodGet, "/b/k?tagging", "", domain.ActGetObjectTagging, false},
		{http.MethodGet, "/b/k?uploadId=1", "", domain.ActListParts, false},
		{http.MethodHead, "/b/k", "", domain.ActHeadObject, false},
		{http.MethodPut, "/b/k", "", domain.ActPutObject, false},
		{http.MethodPut, "/b/k", "/src/x", domain.ActCopyObject, false},
		{http.MethodPut, "/b/k?partNumber=1&uploadId=1", "", domain.ActUploadPart, false},
		{http.MethodPut, "/b/k?partNumber=1&uploadId=1", "/src/x", domain.ActUploadPartCopy, false},
		{http.MethodPut, "/b/k?tagging", "", domain.ActPutObjectTagging, false},
		{http.MethodPost, "/b/k?uploads", "", domain.ActCreateMultipartUpload, false},
		{http.MethodPost, "/b/k?uploadId=1", "", domain.ActCompleteMultipartUpload, false},
		{http.MethodPost, "/b/k?restore", "", "", true},
		{http.MethodDelete, "/b/k?uploadId=1", "", domain.ActAbortMultipartUpload, false},
		{http.MethodDelete, "/b/k?tagging", "", domain.ActDeleteObjectTagging, false},
		{http.MethodDelete, "/b/k", "", domain.ActDeleteObject, false},
	} {
		r := httptest.NewRequestWithContext(t.Context(), tt.method, "http://h"+tt.target, http.NoBody)
		if tt.header != "" {
			r.Header.Set("X-Amz-Copy-Source", tt.header)
		}
		op, err := classify(r, parseTarget(r, ""))
		if (err != nil) != tt.wantErr || op.action != tt.want {
			t.Errorf("classify(%s %s) = %v, %v; want %v (err=%v)", tt.method, tt.target, op.action, err, tt.want, tt.wantErr)
		}
		if tt.header != "" && (op.copy == nil || op.copy.Bucket != "src" || op.copy.Key != "x") {
			t.Errorf("copy source = %+v, want src/x", op.copy)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://h/b?list-type=2&prefix=p/", http.NoBody)
	if op, _ := classify(r, parseTarget(r, "")); op.target != "p/" {
		t.Errorf("listing target = %q, want the prefix p/", op.target)
	}
}

func errOf[T any](_ T, err error) error { return err }

// TestDeleteObjectsPerKeyPolicy checks that bulk deletes are authorized per
// key: the test key has no IAM delete permission (checkerFake), only a
// bucket policy rule on shared/*.
func TestDeleteObjectsPerKeyPolicy(t *testing.T) {
	t.Parallel()
	up := &upstreamFake{}
	b := bucket("storagebucket-p", "policed", storagepb.BucketStatus_ACTIVE)
	b.Spec = &storagepb.BucketSpec{BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{
		{Paths: []string{"shared/*"}, Roles: []string{"storage.object-editor"}, Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: policyGroup}},
	}}}
	srv := startFrontWith(t, up, &opsFake{}, b)
	c := client(srv.URL, testKeyID, testSecret)
	del := func(keys ...string) (*awss3.DeleteObjectsOutput, error) {
		var ids []types.ObjectIdentifier
		for _, k := range keys {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		return c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{Bucket: aws.String("policed"), Delete: &types.Delete{Objects: ids}})
	}

	out, err := del("shared/a", "private/b", "shared/c")
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != "private/b" || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Fatalf("DeleteObjects errors = %+v, want AccessDenied for private/b only", out.Errors)
	}
	got := up.last(t)
	if !strings.Contains(string(got.body), "<Key>shared/a</Key>") || !strings.Contains(string(got.body), "<Key>shared/c</Key>") ||
		strings.Contains(string(got.body), "private/b") {
		t.Fatalf("forwarded delete body = %s, want only the shared/ keys", got.body)
	}

	// Nothing is forwarded when every key is denied.
	up.mu.Lock()
	n := len(up.reqs)
	up.mu.Unlock()
	out, err = del("private/x", "other")
	if err != nil || len(out.Errors) != 2 {
		t.Fatalf("all-denied DeleteObjects = %+v, %v; want 2 AccessDenied errors", out, err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.reqs) != n {
		t.Fatalf("an all-denied bulk delete reached the store")
	}
}
