//go:build e2e_storage

package storage

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// --- helpers -----------------------------------------------------------

// startWithClock is start with the mocker on clk and operations completing at once.
func startWithClock(t *testing.T, clk clock.Clock) *env {
	t.Helper()
	srv := rustfsfixture.Start(t)
	mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), mod},
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(0), testkit.WithClock(clk))
	return &env{inst: inst, sdk: inst.SDK(t), s3Addr: mod.S3Addr(), rustfs: srv}
}

func (e *env) createBucket(t *testing.T, name string, spec *storagepb.BucketSpec) string {
	t.Helper()
	return wait(t)(storagesvc.NewBucketService(e.sdk).Create(t.Context(), &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec:     spec,
	})).ResourceID()
}

func (e *env) createBucketErr(t *testing.T, name string, spec *storagepb.BucketSpec) error {
	t.Helper()
	_, err := storagesvc.NewBucketService(e.sdk).Create(t.Context(), &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec:     spec,
	})
	return err
}

func (e *env) getBucket(t *testing.T, id string) *storagepb.Bucket {
	t.Helper()
	b, err := storagesvc.NewBucketService(e.sdk).Get(t.Context(), &storagepb.GetBucketRequest{Id: id})
	if err != nil {
		t.Fatalf("Get bucket %s: %v", id, err)
	}
	return b
}

// updateSpec reads the bucket, lets mutate change a copy of its spec, and updates it through gosdk.
func (e *env) updateSpec(t *testing.T, id string, mutate func(*storagepb.BucketSpec)) {
	t.Helper()
	b := e.getBucket(t, id)
	spec := proto.CloneOf(b.GetSpec())
	mutate(spec)
	wait(t)(storagesvc.NewBucketService(e.sdk).Update(t.Context(), &storagepb.UpdateBucketRequest{
		Metadata: proto.CloneOf(b.GetMetadata()), Spec: spec,
	}))
}

func countersByClass(b *storagepb.Bucket) map[storagepb.StorageClass]*storagepb.BucketCounters {
	m := map[storagepb.StorageClass]*storagepb.BucketCounters{}
	for _, c := range b.GetStatus().GetCounters() {
		m[c.GetStorageClass()] = c
	}
	return m
}

func s3Msg(err error) string {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		return ae.ErrorMessage()
	}
	return ""
}

func putObj(t *testing.T, c *s3.Client, bucket, key string, data []byte) {
	t.Helper()
	if _, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
		t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
}

func tryPut(t *testing.T, c *s3.Client, bucket, key string, data []byte, class string) error {
	t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}
	if class != "" {
		in.StorageClass = types.StorageClass(class)
	}
	_, err := c.PutObject(t.Context(), in)
	return err
}

func headClass(t *testing.T, c *s3.Client, bucket, key string) string {
	t.Helper()
	h, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject %s/%s: %v", bucket, key, err)
	}
	if h.StorageClass == "" {
		return "STANDARD" // like AWS S3, HEAD omits x-amz-storage-class for STANDARD
	}
	return string(h.StorageClass)
}

// httpResult is a plain net/http response, as a browser or curl would see it.
type httpResult struct {
	status int
	header http.Header
	body   string
}

func doHTTP(t *testing.T, method, url string, hdr map[string]string, body io.Reader) httpResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }() // read fully below
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}
	return httpResult{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

// poll waits until cond holds, calling tick before each check, up to 30 s.
func poll(t *testing.T, what string, tick func(), cond func() bool) {
	t.Helper()
	deadline := clock.Real{}.Now().Add(30 * time.Second)
	for {
		if tick != nil {
			tick()
		}
		if cond() {
			return
		}
		if (clock.Real{}).Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("test ended waiting for %s", what)
		case <-clock.Real{}.After(100 * time.Millisecond):
		}
	}
}

// policyKey creates a service account that is a member of a new group but has
// no access permit anywhere, with an access key. Only a bucket policy can grant it access.
func (e *env) policyKey(t *testing.T) (keyID, secret, groupID string) {
	t.Helper()
	ctx := t.Context()
	sa := wait(t)(iamsvc.NewServiceAccountService(e.sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: unique("sa")},
		Spec:     &iampb.ServiceAccountSpec{},
	})).ResourceID()
	groupID = wait(t)(iamsvc.NewGroupService(e.sdk).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: unique("g")}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(t)(iamsvc.NewGroupMembershipService(e.sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: groupID}, Spec: &iampb.GroupMembershipSpec{MemberId: sa},
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
	return sec.GetAwsAccessKeyId(), sec.GetSecret(), groupID
}

// --- 1. counters -------------------------------------------------------

func TestBucketCounters(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	name := unique("counters")
	id := e.createBucket(t, name, &storagepb.BucketSpec{})
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)
	bucket := aws.String(name)

	putObj(t, c, name, "a", bytes.Repeat([]byte("a"), 100))
	putObj(t, c, name, "b", bytes.Repeat([]byte("b"), 200))
	putObj(t, c, name, "a", bytes.Repeat([]byte("a"), 150)) // overwrite

	const part1, part2 = 5 << 20, 1000
	mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: aws.String("mp")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	var parts []types.CompletedPart
	for i, n := range []int{part1, part2} {
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: aws.String("mp"), UploadId: mp.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(make([]byte, n)),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: aws.String("mp"), UploadId: mp.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	inflight, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: aws.String("inflight")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload (inflight): %v", err)
	}
	if _, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: aws.String("inflight"), UploadId: inflight.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(make([]byte, 4096)),
	}); err != nil {
		t.Fatalf("UploadPart (inflight): %v", err)
	}
	if err := tryPut(t, c, name, "fast", bytes.Repeat([]byte("f"), 300), "ENHANCED_THROUGHPUT"); err != nil {
		t.Fatalf("PutObject ENHANCED_THROUGHPUT: %v", err)
	}

	std := storagepb.StorageClass_STANDARD
	enh := storagepb.StorageClass_ENHANCED_THROUGHPUT
	got := countersByClass(e.getBucket(t, id))
	if len(got) != 2 {
		t.Fatalf("counters entries = %v, want STANDARD and ENHANCED_THROUGHPUT only", got)
	}
	want := &storagepb.CurrentBucketCounters{
		SimpleObjectsQuantity: 2, SimpleObjectsSize: 350, MultipartObjectsQuantity: 1, MultipartObjectsSize: part1 + part2,
		MultipartUploadsQuantity: 1, InflightPartsQuantity: 1, InflightPartsSize: 4096,
	}
	if !proto.Equal(got[std].GetCounters(), want) {
		t.Errorf("STANDARD counters = %v, want %v", got[std].GetCounters(), want)
	}
	if nc := got[std].GetNonCurrentCounters(); nc.GetSimpleObjectsQuantity() != 0 || nc.GetMultipartObjectsQuantity() != 0 {
		t.Errorf("STANDARD non_current_counters = %v, want zeros (unversioned bucket)", nc)
	}
	if !proto.Equal(got[enh].GetCounters(), &storagepb.CurrentBucketCounters{SimpleObjectsQuantity: 1, SimpleObjectsSize: 300}) {
		t.Errorf("ENHANCED_THROUGHPUT counters = %v, want 1 simple object of 300 bytes", got[enh].GetCounters())
	}
	if cls := headClass(t, c, name, "fast"); cls != "ENHANCED_THROUGHPUT" {
		t.Errorf("HeadObject storage class = %q, want ENHANCED_THROUGHPUT", cls)
	}
	lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	for _, o := range lo.Contents {
		wantClass := "STANDARD"
		if aws.ToString(o.Key) == "fast" {
			wantClass = "ENHANCED_THROUGHPUT"
		}
		if string(o.StorageClass) != wantClass {
			t.Errorf("ListObjectsV2 %s storage class = %q, want %q", aws.ToString(o.Key), o.StorageClass, wantClass)
		}
	}

	// GetByName and List carry the counters too.
	buckets := storagesvc.NewBucketService(e.sdk)
	byName, err := buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.inst.ProjectID, Name: name})
	if err != nil || !proto.Equal(countersByClass(byName)[std].GetCounters(), want) {
		t.Errorf("GetByName counters = %v (err %v), want %v", countersByClass(byName)[std].GetCounters(), err, want)
	}
	list, err := buckets.List(ctx, &storagepb.ListBucketsRequest{ParentId: e.inst.ProjectID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range list.GetItems() {
		if b.GetMetadata().GetId() == id && !proto.Equal(countersByClass(b)[std].GetCounters(), want) {
			t.Errorf("List counters = %v, want %v", countersByClass(b)[std].GetCounters(), want)
		}
	}

	// Versioning on: an overwrite and a delete leave noncurrent versions.
	e.updateSpec(t, id, func(s *storagepb.BucketSpec) { s.VersioningPolicy = storagepb.VersioningPolicy_ENABLED })
	putObj(t, c, name, "a", bytes.Repeat([]byte("A"), 50))
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String("b")}); err != nil {
		t.Fatalf("DeleteObject b: %v", err)
	}
	got = countersByClass(e.getBucket(t, id))
	if cur := got[std].GetCounters(); cur.GetSimpleObjectsQuantity() != 1 || cur.GetSimpleObjectsSize() != 50 {
		t.Errorf("current simple objects = %d/%d bytes, want 1/50 (a only; b is deleted)", cur.GetSimpleObjectsQuantity(), cur.GetSimpleObjectsSize())
	}
	if nc := got[std].GetNonCurrentCounters(); nc.GetSimpleObjectsQuantity() != 2 || nc.GetSimpleObjectsSize() != 350 {
		t.Errorf("non-current simple objects = %d/%d bytes, want 2/350 (old a and b)", nc.GetSimpleObjectsQuantity(), nc.GetSimpleObjectsSize())
	}

	// Abort the in-flight upload.
	if _, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: bucket, Key: aws.String("inflight"), UploadId: inflight.UploadId}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	if cur := countersByClass(e.getBucket(t, id))[std].GetCounters(); cur.GetMultipartUploadsQuantity() != 0 || cur.GetInflightPartsQuantity() != 0 || cur.GetInflightPartsSize() != 0 {
		t.Errorf("after abort: uploads=%d parts=%d size=%d, want zeros", cur.GetMultipartUploadsQuantity(), cur.GetInflightPartsQuantity(), cur.GetInflightPartsSize())
	}

	// Delete every version and marker.
	vs, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	del := func(key, ver *string) {
		t.Helper()
		if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key, VersionId: ver}); err != nil {
			t.Fatalf("DeleteObject %s@%s: %v", aws.ToString(key), aws.ToString(ver), err)
		}
	}
	for _, v := range vs.Versions {
		del(v.Key, v.VersionId)
	}
	for _, m := range vs.DeleteMarkers {
		del(m.Key, m.VersionId)
	}
	zero := &storagepb.NonCurrentBucketCounters{}
	for class, cs := range countersByClass(e.getBucket(t, id)) {
		if !proto.Equal(cs.GetCounters(), &storagepb.CurrentBucketCounters{}) || !proto.Equal(cs.GetNonCurrentCounters(), zero) {
			t.Errorf("after deleting all versions %s counters = %v, want zeros", class, cs)
		}
	}
}

// --- 2. storage classes ------------------------------------------------

func TestStorageClassSettings(t *testing.T) {
	e := start(t)
	name := unique("classes")
	id := e.createBucket(t, name, &storagepb.BucketSpec{DefaultStorageClass: storagepb.StorageClass_ENHANCED_THROUGHPUT})
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)

	if err := tryPut(t, c, name, "default", []byte("x"), ""); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got := headClass(t, c, name, "default"); got != "ENHANCED_THROUGHPUT" {
		t.Errorf("no header: class %q, want the bucket default ENHANCED_THROUGHPUT", got)
	}
	if err := tryPut(t, c, name, "std", []byte("x"), "STANDARD"); err != nil {
		t.Fatalf("PutObject STANDARD: %v", err)
	}
	if got := headClass(t, c, name, "std"); got != "STANDARD" {
		t.Errorf("header STANDARD: class %q, want STANDARD", got)
	}

	e.updateSpec(t, id, func(s *storagepb.BucketSpec) { s.ForceStorageClass = true })
	if err := tryPut(t, c, name, "forced", []byte("x"), "STANDARD"); err != nil {
		t.Fatalf("PutObject (forced): %v", err)
	}
	if got := headClass(t, c, name, "forced"); got != "ENHANCED_THROUGHPUT" {
		t.Errorf("force_storage_class: class %q, want ENHANCED_THROUGHPUT (header ignored)", got)
	}

	// With force off again an explicit unsupported class is rejected.
	e.updateSpec(t, id, func(s *storagepb.BucketSpec) { s.ForceStorageClass = false })
	for _, cls := range []string{"INTELLIGENT", "GLACIER"} {
		if got := s3Code(tryPut(t, c, name, "bad-"+strings.ToLower(cls), []byte("x"), cls)); got != "InvalidStorageClass" {
			t.Errorf("PutObject %s: S3 error %q, want InvalidStorageClass", cls, got)
		}
	}

	// Settings the mocker cannot emulate are rejected by name.
	for _, tt := range []struct {
		name string
		spec *storagepb.BucketSpec
		want string
	}{
		{"default INTELLIGENT", &storagepb.BucketSpec{DefaultStorageClass: storagepb.StorageClass_INTELLIGENT}, "spec.default_storage_class"},
		{"transition", &storagepb.BucketSpec{LifecycleConfiguration: &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{{
			Id: "t", Status: storagepb.LifecycleRule_ENABLED,
			Transition: &storagepb.LifecycleTransition{TransitedWith: &storagepb.LifecycleTransition_Days{Days: 30}, StorageClass: storagepb.StorageClass_ENHANCED_THROUGHPUT},
		}}}}, "transition"},
	} {
		err := e.createBucketErr(t, unique("rej"), tt.spec)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Create with %s: %v, want InvalidArgument mentioning %q", tt.name, err, tt.want)
		}
	}
}

// --- 3. max_size_bytes -------------------------------------------------

func TestMaxSizeBytes(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	name := unique("quota")
	id := e.createBucket(t, name, &storagepb.BucketSpec{MaxSizeBytes: 1000})
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)

	putObj(t, c, name, "k1", make([]byte, 600))
	if err := tryPut(t, c, name, "k2", make([]byte, 600), ""); s3Code(err) != "QuotaExceeded" {
		t.Fatalf("second 600-byte PutObject: %v, want QuotaExceeded", err)
	}
	putObj(t, c, name, "k1", make([]byte, 900)) // an overwrite replaces the old bytes
	_, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(name), Key: aws.String("k3"), CopySource: aws.String(name + "/k1")})
	if s3Code(err) != "QuotaExceeded" {
		t.Errorf("CopyObject of 900 bytes into a full bucket: %v, want QuotaExceeded", err)
	}

	e.updateSpec(t, id, func(s *storagepb.BucketSpec) { s.MaxSizeBytes = 0 })
	putObj(t, c, name, "k2", make([]byte, 600))
}

// --- 4. CORS -----------------------------------------------------------

func TestCORS(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	name := unique("cors")
	e.createBucket(t, name, &storagepb.BucketSpec{Cors: &storagepb.CORSConfiguration{Rules: []*storagepb.CORSRule{{
		AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"content-type", "x-amz-*"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: proto.Int32(600),
	}}}})
	plain := unique("nocors")
	plainID := e.createBucket(t, plain, &storagepb.BucketSpec{})
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)
	putObj(t, c, name, "k", []byte("cors body"))

	const origin = "https://app.example.com"
	preflight := func(bucket string, hdr map[string]string) httpResult {
		t.Helper()
		return doHTTP(t, http.MethodOptions, "http://"+e.s3Addr+"/"+bucket+"/k", hdr, nil)
	}
	ok := preflight(name, map[string]string{
		"Origin": origin, "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "content-type, x-amz-date",
	})
	if ok.status != http.StatusOK {
		t.Fatalf("preflight = %d %s, want 200", ok.status, ok.body)
	}
	for h, want := range map[string]string{
		"Access-Control-Allow-Origin": origin, "Access-Control-Allow-Headers": "content-type, x-amz-date",
		"Access-Control-Max-Age": "600", "Access-Control-Allow-Credentials": "true",
	} {
		if got := ok.header.Get(h); got != want {
			t.Errorf("preflight %s = %q, want %q", h, got, want)
		}
	}
	if m := ok.header.Get("Access-Control-Allow-Methods"); !strings.Contains(m, "PUT") {
		t.Errorf("preflight Allow-Methods = %q, want PUT in it", m)
	}
	for _, tt := range []struct {
		name   string
		bucket string
		hdr    map[string]string
		want   int
	}{
		{"other origin", name, map[string]string{"Origin": "https://evil.example.com", "Access-Control-Request-Method": "PUT"}, http.StatusForbidden},
		{"method not allowed", name, map[string]string{"Origin": origin, "Access-Control-Request-Method": "DELETE"}, http.StatusForbidden},
		{"header not allowed", name, map[string]string{"Origin": origin, "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "x-custom"}, http.StatusForbidden},
		{"bucket without CORS", plain, map[string]string{"Origin": origin, "Access-Control-Request-Method": "PUT"}, http.StatusForbidden},
		{"no Origin", name, map[string]string{"Access-Control-Request-Method": "PUT"}, http.StatusBadRequest},
	} {
		if got := preflight(tt.bucket, tt.hdr); got.status != tt.want {
			t.Errorf("preflight (%s) = %d %s, want %d", tt.name, got.status, got.body, tt.want)
		}
	}

	// A real cross-origin GET: a presigned URL fetched with an Origin header, as a browser does.
	ps, err := s3.NewPresignClient(c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	withOrigin := doHTTP(t, http.MethodGet, ps.URL, map[string]string{"Origin": origin}, nil)
	if withOrigin.status != http.StatusOK || withOrigin.body != "cors body" {
		t.Fatalf("GET with Origin = %d %q", withOrigin.status, withOrigin.body)
	}
	if got := withOrigin.header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("GET Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := withOrigin.header.Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Errorf("GET Access-Control-Expose-Headers = %q, want ETag", got)
	}
	if got := doHTTP(t, http.MethodGet, ps.URL, nil, nil).header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("GET without Origin: Access-Control-Allow-Origin = %q, want none", got)
	}

	// CORS over S3 edits the Nebius spec.
	_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(plain), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
		AllowedOrigins: []string{"https://s3.example.com"}, AllowedMethods: []string{"GET", "HEAD"}, AllowedHeaders: []string{"*"},
		ExposeHeaders: []string{"x-amz-request-id"}, MaxAgeSeconds: aws.Int32(30),
	}}}})
	if err != nil {
		t.Fatalf("PutBucketCors: %v", err)
	}
	rules := e.getBucket(t, plainID).GetSpec().GetCors().GetRules()
	if len(rules) != 1 || !slices.Equal(rules[0].GetAllowedOrigins(), []string{"https://s3.example.com"}) ||
		!slices.Equal(rules[0].GetAllowedMethods(), []string{"GET", "HEAD"}) || rules[0].GetMaxAgeSeconds() != 30 {
		t.Errorf("gosdk spec.cors after PutBucketCors = %v", rules)
	}
	got, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(plain)})
	if err != nil || len(got.CORSRules) != 1 || got.CORSRules[0].AllowedOrigins[0] != "https://s3.example.com" {
		t.Errorf("GetBucketCors = %v, %v", got, err)
	}
	if _, err := c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(plain)}); err != nil {
		t.Fatalf("DeleteBucketCors: %v", err)
	}
	if n := len(e.getBucket(t, plainID).GetSpec().GetCors().GetRules()); n != 0 {
		t.Errorf("gosdk spec.cors after DeleteBucketCors has %d rules, want 0", n)
	}
	if _, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(plain)}); s3Code(err) != "NoSuchCORSConfiguration" {
		t.Errorf("GetBucketCors after delete: %v, want NoSuchCORSConfiguration", err)
	}
}

// --- 5. bucket policy --------------------------------------------------

func TestBucketPolicy(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	polKey, polSecret, group := e.policyKey(t)
	name := unique("policy")
	groupRule := &storagepb.BucketPolicy_Rule{
		Paths: []string{"shared/*"}, Roles: []string{"storage.object-editor"}, Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: group},
	}
	anonRule := &storagepb.BucketPolicy_Rule{
		Paths: []string{"public/*"}, Roles: []string{"storage.object-viewer"},
		Subject: &storagepb.BucketPolicy_Rule_Anonymous{Anonymous: &storagepb.BucketPolicy_Rule_AnonymousAccess{}},
	}
	id := e.createBucket(t, name, &storagepb.BucketSpec{BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{groupRule, anonRule}}})

	edKey, edSecret, _ := e.grantedKey(t, "editor")
	ed := e.s3Client(edKey, edSecret)
	putObj(t, ed, name, "public/a.txt", []byte("public data"))
	putObj(t, ed, name, "private/b.txt", []byte("private data"))

	pc := e.s3Client(polKey, polSecret)
	putObj(t, pc, name, "shared/x", []byte("shared"))
	if got := getObject(t, pc, name, "shared/x"); string(got) != "shared" {
		t.Errorf("policy-only GetObject = %q", got)
	}
	if got := s3Code(tryPut(t, pc, name, "private/y", []byte("no"), "")); got != "AccessDenied" {
		t.Errorf("policy-only PutObject outside its paths: %q, want AccessDenied", got)
	}
	if _, err := pc.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String("shared/x")}); err != nil {
		t.Errorf("policy-only DeleteObject shared/x: %v", err)
	}

	// Bulk DeleteObjects is authorized per key: shared/ keys go, the private key stays.
	putObj(t, pc, name, "shared/1", []byte("1"))
	putObj(t, pc, name, "shared/2", []byte("2"))
	out, err := pc.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(name), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
		{Key: aws.String("shared/1")}, {Key: aws.String("private/b.txt")}, {Key: aws.String("shared/2")},
	}}})
	if err != nil {
		t.Fatalf("policy-only DeleteObjects: %v", err)
	}
	var deleted []string
	for _, d := range out.Deleted {
		deleted = append(deleted, aws.ToString(d.Key))
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"shared/1", "shared/2"}) || len(out.Errors) != 1 ||
		aws.ToString(out.Errors[0].Key) != "private/b.txt" || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("DeleteObjects = deleted %v, errors %+v; want shared/1, shared/2 deleted and AccessDenied for private/b.txt", deleted, out.Errors)
	}
	if got := getObject(t, ed, name, "private/b.txt"); string(got) != "private data" {
		t.Errorf("private/b.txt after a denied bulk delete = %q, want it kept", got)
	}
	if keys := listKeys(t, ed, name); slices.Contains(keys, "shared/1") || slices.Contains(keys, "shared/2") {
		t.Errorf("keys after bulk delete = %v, want shared/1 and shared/2 gone", keys)
	}
	// Anonymous bulk deletes get nothing (anonymous rules are read-only).
	anon := s3.New(s3.Options{Region: "eu-north1", BaseEndpoint: aws.String("http://" + e.s3Addr), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}})
	if out, err := anon.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(name), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
		{Key: aws.String("public/a.txt")},
	}}}); err != nil || len(out.Errors) != 1 || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("anonymous DeleteObjects = %+v, %v; want an AccessDenied entry", out, err)
	}
	if got := getObject(t, ed, name, "public/a.txt"); string(got) != "public data" {
		t.Errorf("public/a.txt after an anonymous bulk delete = %q, want it kept", got)
	}

	url := func(key string) string { return "http://" + e.s3Addr + "/" + name + "/" + key }
	if r := doHTTP(t, http.MethodGet, url("public/a.txt"), nil, nil); r.status != http.StatusOK || r.body != "public data" {
		t.Errorf("anonymous GET public/a.txt = %d %q, want 200 \"public data\"", r.status, r.body)
	}
	if r := doHTTP(t, http.MethodGet, url("private/b.txt"), nil, nil); r.status != http.StatusForbidden || !strings.Contains(r.body, "AccessDenied") {
		t.Errorf("anonymous GET private/b.txt = %d %s, want 403 AccessDenied", r.status, r.body)
	}
	if r := doHTTP(t, http.MethodPut, url("public/new.txt"), nil, strings.NewReader("x")); r.status != http.StatusForbidden {
		t.Errorf("anonymous PUT = %d %s, want 403", r.status, r.body)
	}
	if !e.getBucket(t, id).GetStatus().GetAnonymousAccessEnabled() {
		t.Error("status.anonymous_access_enabled = false, want true")
	}

	e.updateSpec(t, id, func(s *storagepb.BucketSpec) {
		s.BucketPolicy = &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{groupRule}}
	})
	if e.getBucket(t, id).GetStatus().GetAnonymousAccessEnabled() {
		t.Error("status.anonymous_access_enabled = true after removing the anonymous rule")
	}
	if r := doHTTP(t, http.MethodGet, url("public/a.txt"), nil, nil); r.status != http.StatusForbidden {
		t.Errorf("anonymous GET after removing the rule = %d, want 403", r.status)
	}

	err = e.createBucketErr(t, unique("badpol"), &storagepb.BucketSpec{BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{{
		Paths: []string{"*"}, Roles: []string{"storage.editor"},
		Subject: &storagepb.BucketPolicy_Rule_Anonymous{Anonymous: &storagepb.BucketPolicy_Rule_AnonymousAccess{}},
	}}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("anonymous storage.editor policy: %v, want InvalidArgument", err)
	}
}

// --- 6. lifecycle ------------------------------------------------------

func TestLifecycle(t *testing.T) {
	clk := clock.NewManual(clock.Real{}.Now())
	e := startWithClock(t, clk)
	ctx := t.Context()
	keyID, secret, _ := e.grantedKey(t, "editor")
	c := e.s3Client(keyID, secret)

	// Lifecycle over S3 (the documented client path), while the clock is still near real time.
	cfgName := unique("lccfg")
	cfgID := e.createBucket(t, cfgName, &storagepb.BucketSpec{})
	_, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(cfgName),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID: aws.String("x"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(5)},
		}}},
	})
	if err != nil {
		t.Fatalf("PutBucketLifecycleConfiguration: %v", err)
	}
	if rs := e.getBucket(t, cfgID).GetSpec().GetLifecycleConfiguration().GetRules(); len(rs) != 1 || rs[0].GetId() != "x" ||
		rs[0].GetStatus() != storagepb.LifecycleRule_ENABLED || rs[0].GetExpiration().GetDays() != 5 {
		t.Errorf("gosdk spec.lifecycle_configuration = %v, want rule x enabled, 5 days", rs)
	}
	if got, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(cfgName)}); err != nil ||
		len(got.Rules) != 1 || aws.ToString(got.Rules[0].ID) != "x" || aws.ToInt32(got.Rules[0].Expiration.Days) != 5 {
		t.Errorf("GetBucketLifecycleConfiguration = %v, %v", got, err)
	}
	_, err = c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(cfgName),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID: aws.String("t"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{},
			Transitions: []types.Transition{{Days: aws.Int32(30), StorageClass: types.TransitionStorageClass("ENHANCED_THROUGHPUT")}},
		}}},
	})
	if s3Code(err) != "InvalidArgument" || !strings.Contains(strings.ToLower(s3Msg(err)), "transition") {
		t.Errorf("lifecycle with a transition: %v, want InvalidArgument naming transition", err)
	}

	// The expiry scenario.
	name := unique("lifecycle")
	enabled, disabled := storagepb.LifecycleRule_ENABLED, storagepb.LifecycleRule_DISABLED
	id := e.createBucket(t, name, &storagepb.BucketSpec{
		VersioningPolicy: storagepb.VersioningPolicy_ENABLED,
		LifecycleConfiguration: &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{
			{
				Id: "expire-tmp", Status: enabled, Filter: &storagepb.LifecycleFilter{Prefix: "tmp/"},
				Expiration: &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 1}},
			},
			{Id: "old-versions", Status: enabled, NoncurrentVersionExpiration: &storagepb.LifecycleNoncurrentVersionExpiration{NoncurrentDays: 1}},
			{Id: "markers", Status: enabled, Expiration: &storagepb.LifecycleExpiration{ExpiredObjectDeleteMarker: true}},
			{Id: "mpu", Status: enabled, AbortIncompleteMultipartUpload: &storagepb.LifecycleAbortIncompleteMultipartUpload{DaysAfterInitiation: 1}},
			{
				Id: "off", Status: disabled, Filter: &storagepb.LifecycleFilter{Prefix: "keep/"},
				Expiration: &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 1}},
			},
		}},
	})
	putObj(t, c, name, "tmp/a", []byte("tmp"))
	putObj(t, c, name, "keep/b", []byte("keep"))
	putObj(t, c, name, "data/c", []byte("one"))
	putObj(t, c, name, "data/c", []byte("two"))
	mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(name), Key: aws.String("data/upload")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if _, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(name), Key: aws.String("data/upload"), UploadId: mp.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("part"),
	}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	std := storagepb.StorageClass_STANDARD
	cur := countersByClass(e.getBucket(t, id))[std]
	if cur.GetCounters().GetSimpleObjectsQuantity() != 3 || cur.GetNonCurrentCounters().GetSimpleObjectsQuantity() != 1 || cur.GetCounters().GetMultipartUploadsQuantity() != 1 {
		t.Fatalf("before expiry: counters = %v, want 3 current, 1 noncurrent, 1 upload", cur)
	}

	// Let three days pass. S3 requests are signed with the real time and the
	// mocker's clock can only move forward (clock.Manual has no Set), so from
	// here on the bucket is checked through gosdk counters and by reading the
	// object store (RustFS) directly, not through S3.
	clk.Advance(3 * 24 * time.Hour)
	poll(t, "tmp/a to expire, the old data/c version and the upload to go", nil, func() bool {
		cs := countersByClass(e.getBucket(t, id))[std]
		return cs.GetNonCurrentCounters().GetSimpleObjectsQuantity() <= 1 && cs.GetCounters().GetMultipartUploadsQuantity() == 0 &&
			cs.GetCounters().GetSimpleObjectsQuantity() == 2
	})
	// Further sweeps (one per mocker minute) remove tmp/a's now-due noncurrent version, then its delete marker.
	store := e.rustfs.Client(t)
	snapshot := func() map[string][]string {
		out := map[string][]string{}
		for v, err := range store.ListVersions(ctx, name, "") {
			if err != nil {
				t.Fatalf("ListVersions: %v", err)
			}
			tag := "version"
			if v.DeleteMarker {
				tag = "marker"
			}
			out[v.Key] = append(out[v.Key], tag)
		}
		return out
	}
	poll(t, "the noncurrent tmp/a version and the delete marker to be removed", func() { clk.Advance(time.Minute) }, func() bool {
		_, tmp := snapshot()["tmp/a"]
		return !tmp
	})
	got := snapshot()
	if len(got) != 2 || !slices.Equal(got["keep/b"], []string{"version"}) || !slices.Equal(got["data/c"], []string{"version"}) {
		t.Errorf("object versions after lifecycle = %v, want keep/b and data/c with one version each", got)
	}
	for u, err := range store.ListUploads(ctx, name) {
		t.Errorf("upload %v still exists (err %v), want it aborted", u, err)
	}
	cs := countersByClass(e.getBucket(t, id))[std]
	if cs.GetCounters().GetSimpleObjectsQuantity() != 2 || cs.GetNonCurrentCounters().GetSimpleObjectsQuantity() != 0 || cs.GetCounters().GetMultipartUploadsQuantity() != 0 {
		t.Errorf("final counters = %v, want 2 current, 0 noncurrent, 0 uploads", cs)
	}
}

// --- 7. S3 bucket management -------------------------------------------

func TestS3BucketManagement(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	buckets := storagesvc.NewBucketService(e.sdk)
	edKey, edSecret, _ := e.grantedKey(t, "editor")
	ed := e.s3Client(edKey, edSecret)
	vKey, vSecret, _ := e.grantedKey(t, "viewer")

	create := func(c *s3.Client, name, loc string) error {
		in := &s3.CreateBucketInput{Bucket: aws.String(name)}
		if loc != "" {
			in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(loc)}
		}
		_, err := c.CreateBucket(ctx, in)
		return err
	}
	name := unique("s3made")
	if err := create(ed, name, "eu-north1"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	b, err := buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.inst.ProjectID, Name: name})
	if err != nil || b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
		t.Fatalf("GetByName after S3 CreateBucket = %v, %v; want ACTIVE", b, err)
	}
	if got := s3Code(create(ed, name, "eu-north1")); got != "BucketAlreadyExists" {
		t.Errorf("second CreateBucket: %q, want BucketAlreadyExists", got)
	}
	if got := s3Code(create(ed, unique("loc"), "eu-west1")); got != "InvalidLocationConstraint" {
		t.Errorf("CreateBucket eu-west1: %q, want InvalidLocationConstraint", got)
	}
	if got := s3Code(create(ed, "Bad_Name", "")); got != "InvalidBucketName" {
		t.Errorf("CreateBucket Bad_Name: %q, want InvalidBucketName", got)
	}
	if got := s3Code(create(e.s3Client(vKey, vSecret), unique("viewer"), "")); got != "AccessDenied" {
		t.Errorf("viewer CreateBucket: %q, want AccessDenied", got)
	}

	if _, err := ed.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(name), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	if v := e.getBucket(t, b.GetMetadata().GetId()).GetSpec().GetVersioningPolicy(); v != storagepb.VersioningPolicy_ENABLED {
		t.Errorf("gosdk versioning_policy = %v, want ENABLED", v)
	}
	if gv, err := ed.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(name)}); err != nil || gv.Status != types.BucketVersioningStatusEnabled {
		t.Errorf("GetBucketVersioning = %v, %v", gv, err)
	}

	// Deleting: a bucket with an object is refused; an empty one is soft-deleted.
	del := unique("s3del")
	if err := create(ed, del, ""); err != nil {
		t.Fatalf("CreateBucket %s: %v", del, err)
	}
	putObj(t, ed, del, "k", []byte("v"))
	if _, err := ed.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(del)}); s3Code(err) != "BucketNotEmpty" {
		t.Errorf("DeleteBucket non-empty: %v, want BucketNotEmpty", err)
	}
	if _, err := ed.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(del), Key: aws.String("k")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := ed.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(del)}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	gone, err := buckets.GetByName(ctx, &storagepb.GetBucketByNameRequest{ParentId: e.inst.ProjectID, Name: del})
	if err != nil || gone.GetStatus().GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION {
		t.Errorf("gosdk Get after S3 DeleteBucket = %v, %v; want SCHEDULED_FOR_DELETION", gone, err)
	}
	if _, err := ed.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(del)}); s3Code(err) != "NotFound" {
		t.Errorf("HeadBucket after delete: %v, want NotFound", err)
	}
}

// --- 8. SigV4 region ---------------------------------------------------

func TestSigV4Region(t *testing.T) {
	e := start(t)
	ctx := t.Context()
	name := unique("region")
	e.createBucket(t, name, &storagepb.BucketSpec{})
	keyID, secret, _ := e.grantedKey(t, "editor")
	good := e.s3Client(keyID, secret)
	bad := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String("http://" + e.s3Addr), UsePathStyle: true,
		Credentials: good.Options().Credentials,
	})

	err := tryPut(t, bad, name, "k", []byte("v"), "")
	if s3Code(err) != "AuthorizationHeaderMalformed" || !strings.Contains(s3Msg(err), "expecting 'eu-north1'") {
		t.Errorf("PutObject signed for us-east-1: %v, want AuthorizationHeaderMalformed expecting 'eu-north1'", err)
	}
	putObj(t, good, name, "k", []byte("v"))

	ps, err := s3.NewPresignClient(bad).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	r := doHTTP(t, http.MethodGet, ps.URL, nil, nil)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "<Code>AuthorizationQueryParametersError</Code>") {
		t.Errorf("presigned GET for us-east-1 = %d %s, want 400 AuthorizationQueryParametersError", r.status, r.body)
	}
	okURL, err := s3.NewPresignClient(good).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if r := doHTTP(t, http.MethodGet, okURL.URL, nil, nil); r.status != http.StatusOK || r.body != "v" {
		t.Errorf("presigned GET for eu-north1 = %d %q, want 200 \"v\"", r.status, r.body)
	}
}
