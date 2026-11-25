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
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
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
	gate, err := domain.NewS3Gate(bucketsFake{items: []*storagepb.Bucket{
		bucket("storagebucket-1", "data", storagepb.BucketStatus_ACTIVE),
		bucket("storagebucket-2", "deleted", storagepb.BucketStatus_SCHEDULED_FOR_DELETION),
	}}, keysFake{}, checkerFake{}, authz.ModeEnforce)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(Config{Domain: "s3.test", Region: "eu-north1"}, gate, up, clock.Real{}, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv
}

func client(url, keyID, secret string) *awss3.Client {
	return awss3.New(awss3.Options{
		Region: "eu-north1", BaseEndpoint: aws.String(url), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})
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
		{"bucket management over S3", errOf(good.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("made-by-s3")})), "NotImplemented"},
		{"versioning over S3", errOf(good.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
			Bucket: aws.String("data"), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
		})), "NotImplemented"},
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
		want           authz.Permission
		wantErr        bool
	}{
		{http.MethodGet, "/", "", domain.PermBucketList, false},
		{http.MethodGet, "/b?list-type=2", "", domain.PermObjectList, false},
		{http.MethodGet, "/b", "", domain.PermObjectList, false},
		{http.MethodGet, "/b?versioning", "", domain.PermBucketGet, false},
		{http.MethodHead, "/b", "", domain.PermBucketGet, false},
		{http.MethodPost, "/b?delete", "", domain.PermObjectDelete, false},
		{http.MethodPut, "/b", "", "", true},
		{http.MethodDelete, "/b", "", "", true},
		{http.MethodGet, "/b/k", "", domain.PermObjectGet, false},
		{http.MethodPut, "/b/k", "", domain.PermObjectWrite, false},
		{http.MethodPut, "/b/k", "/src/x", domain.PermObjectWrite, false},
		{http.MethodPost, "/b/k?uploads", "", domain.PermObjectWrite, false},
		{http.MethodDelete, "/b/k?uploadId=1", "", domain.PermObjectWrite, false},
		{http.MethodDelete, "/b/k", "", domain.PermObjectDelete, false},
	} {
		r := httptest.NewRequestWithContext(t.Context(), tt.method, "http://h"+tt.target, http.NoBody)
		if tt.header != "" {
			r.Header.Set("X-Amz-Copy-Source", tt.header)
		}
		op, err := classify(r, parseTarget(r, ""))
		if (err != nil) != tt.wantErr || op.perm != tt.want {
			t.Errorf("classify(%s %s) = %v, %v; want %v (err=%v)", tt.method, tt.target, op.perm, err, tt.want, tt.wantErr)
		}
		if tt.header != "" && op.copySource != "src" {
			t.Errorf("copy source = %q, want src", op.copySource)
		}
	}
}

func errOf[T any](_ T, err error) error { return err }
