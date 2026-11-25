package s3

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/rustfs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	auditKeyNID = "accesskey-e00test0000000001"
	auditSA     = "serviceaccount-e00test"
)

// sinkFake records what the front reports.
type sinkFake struct {
	mu  sync.Mutex
	got []peer.ObjectAccess
}

func (s *sinkFake) RecordObjectAccess(_ context.Context, a peer.ObjectAccess) {
	s.mu.Lock()
	s.got = append(s.got, a)
	s.mu.Unlock()
}

func (s *sinkFake) all() []peer.ObjectAccess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]peer.ObjectAccess(nil), s.got...)
}

// auditKeysFake resolves the test key with its Nebius key ID.
type auditKeysFake struct{}

func (auditKeysFake) ResolveAccessKey(_ context.Context, id string) (peer.AccessKey, error) {
	if id != testKeyID {
		return peer.AccessKey{}, errs.Unauthenticated("unknown key")
	}
	return peer.AccessKey{AccessKeyID: id, Secret: testSecret, PrincipalID: auditSA, KeyID: auditKeyNID}, nil
}

// errorUpstream answers NoSuchKey for the key "missing", else 200.
type errorUpstream struct{ upstreamFake }

func (u *errorUpstream) Forward(w http.ResponseWriter, fr rustfs.ForwardRequest) {
	if fr.Key == "missing" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
		return
	}
	u.upstreamFake.Forward(w, fr)
}

func auditedBucket(name string, mode storagepb.BucketSpec_ObjectAuditLogging) *storagepb.Bucket {
	b := bucket("storagebucket-"+name, name, storagepb.BucketStatus_ACTIVE)
	b.Spec = &storagepb.BucketSpec{ObjectAuditLogging: mode}
	return b
}

// startAuditedFront serves buckets "all" (ALL), "mutate" (MUTATE_ONLY) and
// "none" (default), reporting to sink when it is non-nil.
func startAuditedFront(t *testing.T, sink peer.AuditSink) *httptest.Server {
	t.Helper()
	gate, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: bucketsFake{items: []*storagepb.Bucket{
			auditedBucket("all", storagepb.BucketSpec_ALL),
			auditedBucket("mutate", storagepb.BucketSpec_MUTATE_ONLY),
			auditedBucket("none", storagepb.BucketSpec_OBJECT_AUDIT_LOGGING_UNSPECIFIED),
		}},
		Keys: auditKeysFake{}, Checker: checkerFake{}, Hierarchy: hierarchyFake{}, Mode: authz.ModeEnforce,
	})
	if err != nil {
		t.Fatal(err)
	}
	front := NewServer(Config{Domain: "s3.test", Region: "eu-north1"}, gate, &opsFake{}, &errorUpstream{}, clock.Real{}, slog.New(slog.DiscardHandler))
	peers := peer.NewDirectory()
	if sink != nil {
		if err := peer.Provide(peers, peer.AuditSinkKey, sink); err != nil {
			t.Fatal(err)
		}
	}
	front.AuditTo(peers)
	srv := httptest.NewServer(front)
	t.Cleanup(srv.Close)
	return srv
}

func TestAuditReportsObjectAccess(t *testing.T) {
	t.Parallel()
	sink := &sinkFake{}
	srv := startAuditedFront(t, sink)
	c := client(srv.URL, testKeyID, testSecret)
	ctx := t.Context()

	if _, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("all"), Key: aws.String("dir/a.txt"), Body: strings.NewReader("hello")}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	_, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("all"), Key: aws.String("missing")})
	if err == nil {
		t.Fatal("GetObject(missing) succeeded, want NoSuchKey")
	}
	// checkerFake denies deletes: AccessDenied from the front itself.
	if _, err := c.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("all"), Key: aws.String("dir/a.txt")}); err == nil {
		t.Fatal("DeleteObject succeeded, want AccessDenied")
	}
	if _, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("all"), Prefix: aws.String("dir/")}); err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	bad := client(srv.URL, testKeyID, "wrong-secret-wrong-secret-wrong-secret00")
	if _, err := bad.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("all"), Key: aws.String("dir/a.txt")}); err == nil {
		t.Fatal("HeadObject with a wrong secret succeeded")
	}

	common := peer.ObjectAccess{
		BucketID: "storagebucket-all", BucketName: "all", Logging: peer.ObjectAuditAll,
		Authenticated: true, PrincipalID: auditSA, AccessKeyID: auditKeyNID, ClientIP: "127.0.0.1",
	}
	with := func(f func(*peer.ObjectAccess)) peer.ObjectAccess {
		a := common
		f(&a)
		return a
	}
	want := []peer.ObjectAccess{
		with(func(a *peer.ObjectAccess) {
			a.Operation, a.ObjectKey, a.HTTPStatus = "PutObject", "dir/a.txt", http.StatusOK
			a.Response = map[string]string{"etag": "etag"}
		}),
		with(func(a *peer.ObjectAccess) {
			a.Operation, a.ObjectKey, a.HTTPStatus = "GetObject", "missing", http.StatusNotFound
			a.ErrorCode, a.ErrorMessage = "NoSuchKey", "The specified key does not exist."
		}),
		with(func(a *peer.ObjectAccess) {
			a.Operation, a.ObjectKey, a.HTTPStatus, a.ErrorCode = "DeleteObject", "dir/a.txt", http.StatusForbidden, "AccessDenied"
		}),
		with(func(a *peer.ObjectAccess) {
			a.Operation, a.HTTPStatus = "ListObjectsV2", http.StatusOK
			a.Params = map[string]string{"prefix": "dir/"}
			a.Response = map[string]string{"etag": "etag"}
		}),
		with(func(a *peer.ObjectAccess) {
			a.Operation, a.ObjectKey, a.HTTPStatus, a.ErrorCode = "HeadObject", "dir/a.txt", http.StatusForbidden, "SignatureDoesNotMatch"
			a.Authenticated, a.PrincipalID = false, ""
		}),
	}
	got := sink.all()
	opts := cmp.Options{
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(peer.ObjectAccess{}, "UserAgent", "RequestID", "ErrorMessage"),
	}
	if diff := cmp.Diff(want, got, opts); diff != "" {
		t.Errorf("reported accesses (-want +got):\n%s", diff)
	}
	if len(got) == len(want) && got[1].ErrorMessage != want[1].ErrorMessage {
		t.Errorf("NoSuchKey message = %q, want %q", got[1].ErrorMessage, want[1].ErrorMessage)
	}
	for _, a := range got {
		if a.UserAgent == "" {
			t.Errorf("%s: user agent not reported", a.Operation)
		}
	}
}

func TestAuditSkipsUnloggedBuckets(t *testing.T) {
	t.Parallel()
	sink := &sinkFake{}
	srv := startAuditedFront(t, sink)
	c := client(srv.URL, testKeyID, testSecret)
	ctx := t.Context()
	if _, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("none"), Key: aws.String("k"), Body: strings.NewReader("x")}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, err := c.ListBuckets(ctx, &awss3.ListBucketsInput{}); err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if _, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("nosuchbucket"), Key: aws.String("k")}); err == nil {
		t.Fatal("GetObject on a missing bucket succeeded")
	}
	if got := sink.all(); len(got) != 0 {
		t.Errorf("reported %d accesses for an unlogged bucket, ListBuckets and a missing bucket, want none: %+v", len(got), got)
	}
}

func TestAuditReportsAnonymousRequest(t *testing.T) {
	t.Parallel()
	sink := &sinkFake{}
	srv := startAuditedFront(t, sink)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/mutate/k?uploadId=u1&partNumber=2", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	// Unsigned: anonymous, denied by the gate; reported without credentials.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("reported %d accesses, want 1: %+v", len(got), got)
	}
	a := got[0]
	if a.Authenticated || a.PrincipalID != "" || a.AccessKeyID != "" {
		t.Errorf("anonymous request reported as %+v, want no credentials", a)
	}
	if a.Params["upload_id"] != "u1" || a.Params["part_number"] != "2" {
		t.Errorf("params = %v, want upload_id=u1 part_number=2", a.Params)
	}
	if a.Logging != peer.ObjectAuditMutateOnly {
		t.Errorf("logging = %v, want MUTATE_ONLY", a.Logging)
	}
}

func TestAuditOperationNames(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		action domain.S3Action
		query  string
		want   string
	}{
		{domain.ActDeleteObject, "", "DeleteObject"},
		{domain.ActDeleteObject, "versionId=v1", "DeleteObjectVersion"},
		{domain.ActListObjects, "list-type=2", "ListObjectsV2"},
		{domain.ActListObjects, "prefix=a", "ListObjects"},
		{domain.ActCompleteMultipartUpload, "uploadId=u", "CompleteMultipartUpload"},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/b/k?"+c.query, http.NoBody)
		if got := auditOperation(operation{action: c.action}, r); got != c.want {
			t.Errorf("auditOperation(%s, %q) = %q, want %q", c.action, c.query, got, c.want)
		}
	}
}

func TestAuditWithoutSinkIsNoop(t *testing.T) {
	t.Parallel()
	srv := startAuditedFront(t, nil)
	c := client(srv.URL, testKeyID, testSecret)
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("all"), Key: aws.String("k"), Body: strings.NewReader("x")}); err != nil {
		t.Fatalf("PutObject without an audit sink: %v", err)
	}
}
