//go:build e2e_storage

package audit_test

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk"
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

const dpRegion = "eu-north1"

// dpIdentity is a service account with an access key.
type dpIdentity struct {
	saID      string
	keyResID  string // accesskey-…, the resource id of the iam.v2 AccessKey
	awsKeyID  string
	secret    string
	s3        *s3.Client
	awsConfig func(t *testing.T) *awscli.CLI
}

// dpEnv is one mocker with the audit, IAM and Object Storage modules, an
// S3 front on RustFS, and the audit window the tests list over.
type dpEnv struct {
	inst     *testkit.Instance
	sdk      *gosdk.SDK
	s3Addr   string
	from, to *timestamppb.Timestamp
	seen     map[string]bool
}

func startDataPlane(t *testing.T) *dpEnv {
	t.Helper()
	srv := rustfsfixture.Start(t)
	storageMod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storageMod, auditmod.New()},
		testkit.WithObjStore(srv.Client(t)), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(50*time.Millisecond))
	now := clock.Real{}.Now()
	return &dpEnv{
		inst: inst, sdk: inst.SDK(t), s3Addr: storageMod.S3Addr(), seen: map[string]bool{},
		from: timestamppb.New(now.Add(-time.Hour)), to: timestamppb.New(now.Add(time.Hour)),
	}
}

// identity creates a service account and an access key through the Nebius Go
// SDK; withRole gives it the editor role on the project.
func (e *dpEnv) identity(t *testing.T, role string) *dpIdentity {
	t.Helper()
	ctx := t.Context()
	wait := waitFor(t)
	sa := wait(iamsvc.NewServiceAccountService(e.sdk).Create(ctx, &iampb.CreateServiceAccountRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: uniqueName("sa")}, Spec: &iampb.ServiceAccountSpec{},
	})).ResourceID()
	if role != "" {
		group := wait(iamsvc.NewGroupService(e.sdk).Create(ctx, &iampb.CreateGroupRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: uniqueName("g")}, Spec: &iampb.GroupSpec{},
		})).ResourceID()
		wait(iamsvc.NewGroupMembershipService(e.sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: group}, Spec: &iampb.GroupMembershipSpec{MemberId: sa},
		}))
		wait(iamsvc.NewAccessPermitService(e.sdk).Create(ctx, &iampb.CreateAccessPermitRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: group},
			Spec:     &iampb.AccessPermitSpec{ResourceId: e.inst.ProjectID, Role: role},
		}))
	}
	keys := iamv2svc.NewAccessKeyService(e.sdk)
	id := wait(keys.Create(ctx, &iamv2pb.CreateAccessKeyRequest{
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
	return &dpIdentity{
		saID: sa, keyResID: id, awsKeyID: sec.GetAwsAccessKeyId(), secret: sec.GetSecret(),
		s3: e.s3Client(sec.GetAwsAccessKeyId(), sec.GetSecret()),
		awsConfig: func(t *testing.T) *awscli.CLI {
			t.Helper()
			c := awscli.New(t)
			c.Configure(t, sec.GetAwsAccessKeyId(), sec.GetSecret(), dpRegion, "http://"+e.s3Addr)
			return c
		},
	}
}

// s3Client is an AWS SDK for Go v2 client pointed at the mock's S3 front.
// Retries are off so each call is exactly one request.
func (e *dpEnv) s3Client(keyID, secret string) *s3.Client {
	return s3.New(s3.Options{
		Region:       dpRegion,
		BaseEndpoint: aws.String("http://" + e.s3Addr),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
		Credentials:  credentials.NewStaticCredentialsProvider(keyID, secret, ""),
	})
}

// bucket creates a bucket through the Go SDK with the given audit mode.
func (e *dpEnv) bucket(t *testing.T, mode storagepb.BucketSpec_ObjectAuditLogging) (name, id string) {
	t.Helper()
	name = uniqueName("dp-bucket")
	id = waitFor(t)(storagesvc.NewBucketService(e.sdk).Create(t.Context(), &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec:     &storagepb.BucketSpec{ObjectAuditLogging: mode},
	})).ResourceID()
	return name, id
}

// list returns every event of the type matching filter, through the Go SDK.
func (e *dpEnv) list(t *testing.T, typ auditv2pb.EventType, filter string) []*auditv2pb.AuditEvent {
	t.Helper()
	var out []*auditv2pb.AuditEvent
	for ev, err := range auditsvc.NewAuditEventService(e.sdk).Filter(t.Context(), &auditv2pb.ListAuditEventRequest{
		ParentId: e.inst.TenantID, Start: e.from, End: e.to, Filter: filter, EventType: typ, Region: dpRegion,
	}) {
		if err != nil {
			t.Fatalf("list %v events %q: %v", typ, filter, err)
		}
		out = append(out, ev)
	}
	return out
}

func param(ev *auditv2pb.AuditEvent, key string) string {
	return ev.GetRequest().GetParameters().GetFields()[key].GetStringValue()
}

// label is "ACTION key" for a data-plane event.
func label(ev *auditv2pb.AuditEvent) string {
	return strings.TrimSpace(ev.GetAction() + " " + param(ev, "object_key"))
}

// fresh returns the events of bucket name not returned by an earlier call,
// waiting up to 10 s for at least want of them (recording follows the S3
// response, so a fast list can run ahead of it).
func (e *dpEnv) fresh(t *testing.T, bucketName string, want int) []*auditv2pb.AuditEvent {
	t.Helper()
	filter := "service.name='STORAGE' AND resource.metadata.name='" + bucketName + "'"
	deadline := clock.Real{}.Now().Add(10 * time.Second)
	for {
		var got []*auditv2pb.AuditEvent
		for _, ev := range e.list(t, auditv2pb.EventType_DATA_PLANE, filter) {
			if !e.seen[ev.GetId()] {
				got = append(got, ev)
			}
		}
		if len(got) >= want || (clock.Real{}).Now().After(deadline) {
			for _, ev := range got {
				e.seen[ev.GetId()] = true
			}
			return got
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("test ended waiting for audit events")
		case <-clock.Real{}.After(100 * time.Millisecond):
		}
	}
}

// expect asserts that the calls made since the last expect recorded exactly
// the events labelled want ("ACTION key", any order) and returns them.
func (e *dpEnv) expect(t *testing.T, bucketName string, want ...string) []*auditv2pb.AuditEvent {
	t.Helper()
	got := e.fresh(t, bucketName, len(want))
	labels := make([]string, 0, len(got))
	for _, ev := range got {
		labels = append(labels, label(ev))
	}
	slices.Sort(labels)
	w := slices.Sorted(slices.Values(want))
	if !slices.Equal(labels, w) {
		t.Fatalf("data-plane events on %s = %q, want %q", bucketName, labels, w)
	}
	return got
}

// checkOK asserts the common shape of a successful event of identity id on
// the bucket.
func checkOK(t *testing.T, ev *auditv2pb.AuditEvent, id *dpIdentity, bucketName, bucketID string) {
	t.Helper()
	lbl := label(ev)
	if ev.GetStatus() != auditv2pb.Status_DONE || ev.GetResponse().GetStatusCode() != code.Code_OK {
		t.Errorf("%s: status = %v, code = %v, want DONE and OK (%s)", lbl, ev.GetStatus(), ev.GetResponse().GetStatusCode(), ev.GetResponse().GetErrorMessage())
	}
	if ev.GetService().GetName() != "STORAGE" || !strings.HasPrefix(ev.GetSource(), "s3/") {
		t.Errorf("%s: service = %q, source = %q", lbl, ev.GetService().GetName(), ev.GetSource())
	}
	if want := "ai.nebius.storage.storagebucket." + strings.ToLower(ev.GetAction()); ev.GetType() != want {
		t.Errorf("%s: type = %q, want %q", lbl, ev.GetType(), want)
	}
	md := ev.GetResource().GetMetadata()
	if md.GetId() != bucketID || md.GetName() != bucketName || md.GetType() != "storagebucket" {
		t.Errorf("%s: resource = %v, want %s/%s storagebucket", lbl, md, bucketID, bucketName)
	}
	if h := ev.GetResource().GetHierarchy(); len(h) != 2 || !strings.HasPrefix(h[0].GetId(), "tenant-") || !strings.HasPrefix(h[1].GetId(), "project-") {
		t.Errorf("%s: hierarchy = %v, want tenant then project", lbl, h)
	}
	a := ev.GetAuthentication()
	if a.GetAuthenticationType() != auditv2pb.AuthenticationType_STATIC_KEY || !a.GetAuthenticated() ||
		a.GetSubject().GetServiceAccountId() != id.saID || a.GetStaticKeyCredential().GetId() != id.keyResID {
		t.Errorf("%s: authentication = %v, want STATIC_KEY, authenticated, sa %s, key %s", lbl, a, id.saID, id.keyResID)
	}
	if param(ev, "bucket_name") != bucketName {
		t.Errorf("%s: bucket_name = %q, want %q", lbl, param(ev, "bucket_name"), bucketName)
	}
}

func must[T any](t *testing.T, what string) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
}

func okErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// TestDataPlaneStorageAudit exercises the whole data-plane audit trail of
// Object Storage: what each object_audit_logging mode records for AWS SDK and
// aws CLI traffic, failed requests, retrieval with the SDK and the nebius CLI,
// export, and redaction.
func TestDataPlaneStorageAudit(t *testing.T) {
	e := startDataPlane(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: e.inst.Addr, Token: e.inst.Token, ParentID: e.inst.ProjectID, TenantID: e.inst.TenantID})

	alice := e.identity(t, "editor")
	mallory := e.identity(t, "")

	bucketA, idA := e.bucket(t, storagepb.BucketSpec_ALL)
	bucketC, _ := e.bucket(t, storagepb.BucketSpec_OBJECT_AUDIT_LOGGING_UNSPECIFIED)
	bucketB := uniqueName("dp-bucket")
	var created struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	cli.JSON(t, &created, "storage", "bucket", "create", "--name", bucketB, "--object-audit-logging", "mutate_only")
	idB := created.Metadata.ID
	if !strings.HasPrefix(idB, "storagebucket-") {
		t.Fatalf("bucket create via the CLI returned id %q", idB)
	}

	t.Run("SDKOnALL", func(t *testing.T) { dpSDKOnAll(t, e, alice, bucketA, idA) })
	t.Run("AWSCLIOnALL", func(t *testing.T) { dpAWSCLI(t, e, alice, bucketA, idA) })
	t.Run("Failures", func(t *testing.T) { dpFailures(t, e, alice, mallory, bucketA, idA) })
	t.Run("MutateOnlyAndNone", func(t *testing.T) { dpModes(t, e, alice, bucketB, idB, bucketC) })
	t.Run("BulkDeletePolicy", func(t *testing.T) { dpBulkDeletePolicy(t, e, alice, mallory) })
	t.Run("Retrieval", func(t *testing.T) { dpRetrieval(t, e, cli, alice, mallory, bucketA) })
	t.Run("Export", func(t *testing.T) { dpExport(t, e, alice, bucketA) })
}

func dpSDKOnAll(t *testing.T, e *dpEnv, alice *dpIdentity, bucketA, idA string) {
	t.Helper()
	ctx := t.Context()
	c := alice.s3
	b := aws.String(bucketA)
	check := func(got []*auditv2pb.AuditEvent) {
		t.Helper()
		for _, ev := range got {
			checkOK(t, ev, alice, bucketA, idA)
		}
	}
	body := func(s string) io.Reader { return strings.NewReader(s) }

	okErr(t, "PutObject", errOnly(c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String("docs/a.txt"), Body: body("hello")})))
	got := e.expect(t, bucketA, "PUT_OBJECT docs/a.txt")
	check(got)
	if got[0].GetResponse().GetPayload().GetFields()["etag"].GetStringValue() == "" {
		t.Errorf("PutObject payload = %v, want an etag", got[0].GetResponse().GetPayload())
	}

	okErr(t, "HeadObject", errOnly(c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: b, Key: aws.String("docs/a.txt")})))
	check(e.expect(t, bucketA, "HEAD_OBJECT docs/a.txt"))

	out := must[*s3.GetObjectOutput](t, "GetObject")(c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String("docs/a.txt")}))
	_, _ = io.Copy(io.Discard, out.Body)
	_ = out.Body.Close()
	check(e.expect(t, bucketA, "GET_OBJECT docs/a.txt"))

	okErr(t, "PutObject b", errOnly(c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String("docs/b.txt"), Body: body("b")})))
	e.expect(t, bucketA, "PUT_OBJECT docs/b.txt")
	okErr(t, "ListObjectsV2", errOnly(c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: b, Prefix: aws.String("docs/")})))
	ev := e.expect(t, bucketA, "LIST_OBJECTS_V2")
	check(ev)
	if param(ev[0], "prefix") != "docs/" {
		t.Errorf("ListObjectsV2 prefix = %q, want docs/", param(ev[0], "prefix"))
	}

	okErr(t, "CopyObject", errOnly(c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: b, Key: aws.String("docs/copy.txt"), CopySource: aws.String(bucketA + "/docs/a.txt")})))
	ev = e.expect(t, bucketA, "COPY_OBJECT docs/copy.txt")
	check(ev)
	if !strings.Contains(param(ev[0], "copy_source"), "docs/a.txt") {
		t.Errorf("CopyObject copy_source = %q", param(ev[0], "copy_source"))
	}

	// Multipart upload: create, upload a part, list, complete.
	mp := must[*s3.CreateMultipartUploadOutput](t, "CreateMultipartUpload")(c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: b, Key: aws.String("big/mp.bin")}))
	part := must[*s3.UploadPartOutput](t, "UploadPart")(c.UploadPart(ctx, &s3.UploadPartInput{Bucket: b, Key: aws.String("big/mp.bin"), UploadId: mp.UploadId, PartNumber: aws.Int32(1), Body: body("part one")}))
	okErr(t, "ListParts", errOnly(c.ListParts(ctx, &s3.ListPartsInput{Bucket: b, Key: aws.String("big/mp.bin"), UploadId: mp.UploadId})))
	okErr(t, "ListMultipartUploads", errOnly(c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: b})))
	okErr(t, "CompleteMultipartUpload", errOnly(c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: b, Key: aws.String("big/mp.bin"), UploadId: mp.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
	})))
	ev = e.expect(t, bucketA, "CREATE_MULTIPART_UPLOAD big/mp.bin", "UPLOAD_PART big/mp.bin", "LIST_PARTS big/mp.bin",
		"LIST_MULTIPART_UPLOADS", "COMPLETE_MULTIPART_UPLOAD big/mp.bin")
	check(ev)
	for _, x := range ev {
		if x.GetAction() == "UPLOAD_PART" && (param(x, "upload_id") != aws.ToString(mp.UploadId) || param(x, "part_number") != "1") {
			t.Errorf("UploadPart parameters = %v, want upload_id %s and part_number 1", x.GetRequest().GetParameters(), aws.ToString(mp.UploadId))
		}
	}

	// A second upload that is aborted.
	ab := must[*s3.CreateMultipartUploadOutput](t, "CreateMultipartUpload 2")(c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: b, Key: aws.String("big/abort.bin")}))
	okErr(t, "UploadPart 2", errOnly(c.UploadPart(ctx, &s3.UploadPartInput{Bucket: b, Key: aws.String("big/abort.bin"), UploadId: ab.UploadId, PartNumber: aws.Int32(1), Body: body("x")})))
	okErr(t, "AbortMultipartUpload", errOnly(c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: b, Key: aws.String("big/abort.bin"), UploadId: ab.UploadId})))
	check(e.expect(t, bucketA, "CREATE_MULTIPART_UPLOAD big/abort.bin", "UPLOAD_PART big/abort.bin", "ABORT_MULTIPART_UPLOAD big/abort.bin"))

	// Tagging and bucket configuration are never recorded.
	okErr(t, "PutObjectTagging", errOnly(c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: b, Key: aws.String("docs/a.txt"),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}},
	})))
	okErr(t, "GetObjectTagging", errOnly(c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: b, Key: aws.String("docs/a.txt")})))
	okErr(t, "GetBucketVersioning", errOnly(c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b})))

	okErr(t, "DeleteObject", errOnly(c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: aws.String("docs/copy.txt")})))
	check(e.expect(t, bucketA, "DELETE_OBJECT docs/copy.txt")) // nothing for the tagging calls

	okErr(t, "DeleteObjects", errOnly(c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: b, Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("docs/a.txt")}, {Key: aws.String("docs/b.txt")}},
	}})))
	ev = e.expect(t, bucketA, "DELETE_OBJECTS")
	check(ev)
	if got := listField(ev[0].GetRequest().GetParameters(), "object_keys"); !slices.Equal(got, []string{"docs/a.txt", "docs/b.txt"}) {
		t.Errorf("DeleteObjects object_keys = %v, want docs/a.txt and docs/b.txt", got)
	}
	if got := listField(ev[0].GetResponse().GetPayload(), "deleted"); !slices.Equal(got, []string{"docs/a.txt", "docs/b.txt"}) {
		t.Errorf("DeleteObjects payload deleted = %v, want docs/a.txt and docs/b.txt", got)
	}

	// Versioning: enable it, write twice, list the versions, delete one.
	okErr(t, "PutBucketVersioning", errOnly(c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: b, VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})))
	v1 := must[*s3.PutObjectOutput](t, "PutObject v1")(c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String("ver/v.txt"), Body: body("1")}))
	okErr(t, "PutObject v2", errOnly(c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String("ver/v.txt"), Body: body("2")})))
	okErr(t, "ListObjectVersions", errOnly(c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: b, Prefix: aws.String("ver/")})))
	okErr(t, "DeleteObject version", errOnly(c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: aws.String("ver/v.txt"), VersionId: v1.VersionId})))
	ev = e.expect(t, bucketA, "PUT_OBJECT ver/v.txt", "PUT_OBJECT ver/v.txt", "LIST_OBJECT_VERSIONS", "DELETE_OBJECT_VERSION ver/v.txt")
	check(ev)
	for _, x := range ev {
		if x.GetAction() == "DELETE_OBJECT_VERSION" && param(x, "version") != aws.ToString(v1.VersionId) {
			t.Errorf("DeleteObjectVersion version = %q, want %q", param(x, "version"), aws.ToString(v1.VersionId))
		}
	}
}

// listField is the sorted string list at key of a protobuf Struct.
func listField(st *structpb.Struct, key string) []string {
	var out []string
	for _, v := range st.GetFields()[key].GetListValue().GetValues() {
		out = append(out, v.GetStringValue())
	}
	slices.Sort(out)
	return out
}

// errOnly drops the value of an SDK call.
func errOnly[T any](_ T, err error) error { return err }

func dpAWSCLI(t *testing.T, e *dpEnv, alice *dpIdentity, bucketA, idA string) {
	t.Helper()
	cli := alice.awsConfig(t)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "up.txt"), filepath.Join(dir, "down.txt")
	if err := os.WriteFile(src, []byte("from the aws cli"), 0o600); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}
	uri := "s3://" + bucketA + "/cli/up.txt"
	run := func(args ...string) {
		t.Helper()
		if out, err := cli.Run(t.Context(), args...); err != nil {
			t.Fatalf("aws %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("s3", "cp", src, uri)
	run("s3", "cp", uri, dst)
	run("s3", "ls", "s3://"+bucketA+"/cli/")
	run("s3", "rm", uri)

	got := e.fresh(t, bucketA, 4)
	have := map[string]bool{}
	for _, ev := range got {
		have[label(ev)] = true
		if ev.GetStatus() != auditv2pb.Status_DONE {
			t.Errorf("%s: status = %v (%s)", label(ev), ev.GetStatus(), ev.GetResponse().GetErrorMessage())
		}
		if a := ev.GetAuthentication(); a.GetSubject().GetServiceAccountId() != alice.saID || a.GetStaticKeyCredential().GetId() != alice.keyResID {
			t.Errorf("%s: authentication = %v, want sa %s key %s", label(ev), a, alice.saID, alice.keyResID)
		}
		if ev.GetResource().GetMetadata().GetId() != idA {
			t.Errorf("%s: bucket id = %q, want %q", label(ev), ev.GetResource().GetMetadata().GetId(), idA)
		}
	}
	for _, want := range []string{"PUT_OBJECT cli/up.txt", "GET_OBJECT cli/up.txt", "LIST_OBJECTS_V2", "DELETE_OBJECT cli/up.txt"} {
		if !have[want] {
			t.Errorf("aws CLI traffic recorded %v, missing %q", slices.Sorted(maps.Keys(have)), want)
		}
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "from the aws cli" {
		t.Errorf("downloaded file = %q, %v", b, err)
	}
}

func dpFailures(t *testing.T, e *dpEnv, alice, mallory *dpIdentity, bucketA, idA string) {
	t.Helper()
	ctx := t.Context()
	b := aws.String(bucketA)

	// Missing key.
	_, err := alice.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String("nope/missing")})
	if err == nil {
		t.Fatalf("GetObject of a missing key succeeded")
	}
	ev := e.expect(t, bucketA, "GET_OBJECT nope/missing")[0]
	if ev.GetStatus() != auditv2pb.Status_ERROR || ev.GetResponse().GetStatusCode() != code.Code_NOT_FOUND ||
		!strings.Contains(ev.GetResponse().GetErrorMessage(), "NoSuchKey") {
		t.Errorf("missing key event: status %v, code %v, message %q; want ERROR, NOT_FOUND, NoSuchKey", ev.GetStatus(), ev.GetResponse().GetStatusCode(), ev.GetResponse().GetErrorMessage())
	}
	if ev.GetAuthentication().GetSubject().GetServiceAccountId() != alice.saID || !ev.GetAuthentication().GetAuthenticated() {
		t.Errorf("missing key event authentication = %v, want alice %s", ev.GetAuthentication(), alice.saID)
	}

	// A service account with no role on the project.
	if _, err := mallory.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String("secret/data")}); err == nil {
		t.Fatalf("GetObject without a role succeeded")
	}
	ev = e.expect(t, bucketA, "GET_OBJECT secret/data")[0]
	if ev.GetStatus() != auditv2pb.Status_ERROR || ev.GetResponse().GetStatusCode() != code.Code_PERMISSION_DENIED {
		t.Errorf("denied event: status %v, code %v (%s); want ERROR, PERMISSION_DENIED", ev.GetStatus(), ev.GetResponse().GetStatusCode(), ev.GetResponse().GetErrorMessage())
	}
	if a := ev.GetAuthentication(); a.GetSubject().GetServiceAccountId() != mallory.saID || a.GetStaticKeyCredential().GetId() != mallory.keyResID {
		t.Errorf("denied event authentication = %v, want subject %s key %s", a, mallory.saID, mallory.keyResID)
	}
	if ev.GetResource().GetMetadata().GetId() != idA {
		t.Errorf("denied event bucket id = %q, want %q", ev.GetResource().GetMetadata().GetId(), idA)
	}

	// Right key id, wrong secret.
	bad := e.s3Client(alice.awsKeyID, alice.secret+"-wrong")
	if _, err := bad.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String("docs/any")}); err == nil {
		t.Fatalf("GetObject with a wrong secret succeeded")
	}
	ev = e.expect(t, bucketA, "GET_OBJECT docs/any")[0]
	a := ev.GetAuthentication()
	if ev.GetStatus() != auditv2pb.Status_ERROR || ev.GetResponse().GetStatusCode() != code.Code_UNAUTHENTICATED ||
		a.GetAuthenticated() || a.GetStaticKeyCredential().GetId() != alice.keyResID || a.GetSubject() != nil {
		t.Errorf("bad signature event: status %v, code %v, authentication %v; want ERROR, UNAUTHENTICATED, unauthenticated with key %s and no subject",
			ev.GetStatus(), ev.GetResponse().GetStatusCode(), a, alice.keyResID)
	}
}

func dpModes(t *testing.T, e *dpEnv, alice *dpIdentity, bucketB, idB, bucketC string) {
	t.Helper()
	ctx := t.Context()
	c := alice.s3

	// C has the default (NONE): nothing is recorded.
	for _, bk := range []string{bucketC} {
		okErr(t, "PutObject C", errOnly(c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bk), Key: aws.String("c.txt"), Body: strings.NewReader("c")})))
		okErr(t, "GetObject C", errOnly(c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bk), Key: aws.String("c.txt")})))
		okErr(t, "DeleteObject C", errOnly(c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bk), Key: aws.String("c.txt")})))
	}

	// B is MUTATE_ONLY (created with the nebius CLI): only the put is recorded.
	bb := aws.String(bucketB)
	okErr(t, "PutObject B", errOnly(c.PutObject(ctx, &s3.PutObjectInput{Bucket: bb, Key: aws.String("b.txt"), Body: strings.NewReader("b")})))
	okErr(t, "GetObject B", errOnly(c.GetObject(ctx, &s3.GetObjectInput{Bucket: bb, Key: aws.String("b.txt")})))
	okErr(t, "ListObjectsV2 B", errOnly(c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bb})))
	okErr(t, "HeadObject B", errOnly(c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bb, Key: aws.String("b.txt")})))
	// A mutating sentinel after the reads: when it is visible, the reads have been dealt with.
	okErr(t, "DeleteObject B", errOnly(c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bb, Key: aws.String("b.txt")})))
	got := e.expect(t, bucketB, "PUT_OBJECT b.txt", "DELETE_OBJECT b.txt")
	for _, ev := range got {
		checkOK(t, ev, alice, bucketB, idB)
	}

	if n := len(e.list(t, auditv2pb.EventType_DATA_PLANE, "resource.metadata.name='"+bucketC+"'")); n != 0 {
		t.Errorf("bucket %s (object_audit_logging NONE) has %d data-plane events, want 0", bucketC, n)
	}
}

// dpRetrieval lists the data-plane events with the Go SDK and the nebius CLI.
func dpRetrieval(t *testing.T, e *dpEnv, cli *nebiuscli.CLI, alice, mallory *dpIdentity, bucketA string) {
	t.Helper()
	all := e.list(t, auditv2pb.EventType_DATA_PLANE, "service.name='STORAGE'")
	if len(all) < 20 {
		t.Fatalf("DATA_PLANE STORAGE events = %d, want the traffic of the earlier steps", len(all))
	}
	ids := map[string]bool{}
	for _, ev := range all {
		ids[ev.GetId()] = true
		if !strings.HasPrefix(ev.GetSource(), "s3/") || ev.GetService().GetName() != "STORAGE" {
			t.Errorf("DATA_PLANE list returned a non data-plane event: %s %s", ev.GetSource(), ev.GetType())
		}
	}
	for _, ev := range e.list(t, auditv2pb.EventType_CONTROL_PLANE, "") {
		if ids[ev.GetId()] || strings.HasPrefix(ev.GetSource(), "s3/") {
			t.Errorf("CONTROL_PLANE list contains the data-plane event %s (%s)", ev.GetId(), ev.GetSource())
		}
	}

	byName := "service.name='STORAGE' AND resource.metadata.name='" + bucketA + "'"
	for _, tc := range []struct {
		name, filter string
		check        func(*auditv2pb.AuditEvent) bool
		min          int
	}{
		{"bucket name", byName, func(ev *auditv2pb.AuditEvent) bool { return ev.GetResource().GetMetadata().GetName() == bucketA }, 20},
		{"action", byName + " AND action='PUT_OBJECT'", func(ev *auditv2pb.AuditEvent) bool { return ev.GetAction() == "PUT_OBJECT" }, 3},
		{"static key", "authentication.static_key_credential.id='" + alice.keyResID + "'", func(ev *auditv2pb.AuditEvent) bool {
			return ev.GetAuthentication().GetStaticKeyCredential().GetId() == alice.keyResID
		}, 20},
		{"service account", "authentication.subject.service_account_id='" + mallory.saID + "'", func(ev *auditv2pb.AuditEvent) bool {
			return ev.GetAuthentication().GetSubject().GetServiceAccountId() == mallory.saID
		}, 1},
		{"status", byName + " AND status='ERROR'", func(ev *auditv2pb.AuditEvent) bool { return ev.GetStatus() == auditv2pb.Status_ERROR }, 3},
	} {
		got := e.list(t, auditv2pb.EventType_DATA_PLANE, tc.filter)
		if len(got) < tc.min {
			t.Errorf("filter %s (%s) = %d events, want at least %d", tc.name, tc.filter, len(got), tc.min)
		}
		for _, ev := range got {
			if !tc.check(ev) {
				t.Errorf("filter %s returned a non-matching event %s %s", tc.name, ev.GetId(), label(ev))
			}
		}
	}

	// The nebius CLI returns the same events.
	res, err := cli.Run(t.Context(), "audit", "v2", "audit-event", "list", "--parent-id", e.inst.TenantID,
		"--start", e.from.AsTime().Format(time.RFC3339), "--end", e.to.AsTime().Format(time.RFC3339),
		"--event-type", "data_plane", "--region", dpRegion, "--filter", "service.name='STORAGE'", "--all", "--format", "json")
	if err != nil {
		t.Fatalf("nebius audit-event list: %v\n%s", err, res.Stderr)
	}
	var resp auditv2pb.ListAuditEventResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(res.Stdout), &resp); err != nil {
		t.Fatalf("decode CLI output: %v\n%s", err, res.Stdout)
	}
	cliIDs := map[string]bool{}
	for _, ev := range resp.GetItems() {
		cliIDs[ev.GetId()] = true
	}
	if !maps.Equal(cliIDs, ids) {
		t.Errorf("CLI listed %d data-plane events, the SDK %d; ids differ", len(cliIDs), len(ids))
	}

	// Redaction: no credential material in any event.
	secrets := []string{alice.secret, mallory.secret, alice.secret + "-wrong", e.inst.Token, "AWS4-HMAC-SHA256", "Signature=", "X-Amz-Credential"}
	for _, ev := range all {
		b, err := protojson.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %s: %v", ev.GetId(), err)
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("event %s %s contains %q", ev.GetId(), label(ev), s)
			}
		}
	}
}

// dpExport exports DATA_PLANE events to a bucket and downloads the file with
// the AWS SDK, as a user would.
func dpExport(t *testing.T, e *dpEnv, alice *dpIdentity, bucketA string) {
	t.Helper()
	ctx := t.Context()
	wait := waitFor(t)
	destName, destID := e.bucket(t, storagepb.BucketSpec_OBJECT_AUDIT_LOGGING_UNSPECIFIED)
	const filter = "service.name='STORAGE'"
	want := map[string]bool{}
	for _, ev := range e.list(t, auditv2pb.EventType_DATA_PLANE, filter) {
		want[ev.GetId()] = true
	}

	exports := auditsvc.NewAuditEventExportService(e.sdk)
	op := wait(exports.Start(ctx, &auditv2pb.StartRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID},
		Region:   dpRegion,
		Spec: &auditv2pb.AuditEventExportSpec{
			Params: &auditv2pb.AuditEventExportParams{From: e.from, To: e.to, Filter: filter, EventType: auditv2pb.EventType_DATA_PLANE},
			ExportDestination: &auditv2pb.AuditEventExportSpec_NebiusObjectStorage{
				NebiusObjectStorage: &auditv2pb.NebiusObjectStorageDestination{
					Bucket: &auditv2pb.NebiusObjectStorageDestination_BucketById{BucketById: &auditv2pb.BucketById{Id: destID}},
				},
			},
		},
	}))
	exp, err := exports.Get(ctx, &auditv2pb.GetRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("get export: %v", err)
	}
	if exp.GetStatus().GetState() != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("export state = %v, want DONE: %v", exp.GetStatus().GetState(), exp.GetStatus())
	}

	prefix := "auditlogs/" + exp.GetStatus().GetExportOperationId() + "/"
	list, err := alice.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(destName), Prefix: aws.String(prefix)})
	if err != nil || len(list.Contents) == 0 {
		t.Fatalf("ListObjectsV2 %s/%s = %v, %v; want the exported files", destName, prefix, list, err)
	}
	got := map[string]bool{}
	for _, o := range list.Contents {
		out, err := alice.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(destName), Key: o.Key})
		if err != nil {
			t.Fatalf("GetObject %s: %v", aws.ToString(o.Key), err)
		}
		b, err := io.ReadAll(out.Body)
		err = errors.Join(err, out.Body.Close())
		if err != nil {
			t.Fatalf("read %s: %v", aws.ToString(o.Key), err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			var ev auditv2pb.AuditEvent
			if err := protojson.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			if !strings.HasPrefix(ev.GetSource(), "s3/") || ev.GetService().GetName() != "STORAGE" {
				t.Errorf("export contains a non data-plane event: %s %s", ev.GetSource(), ev.GetType())
			}
			if ev.GetResource().GetMetadata().GetName() == bucketA {
				got[ev.GetId()] = true
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("export holds no events of bucket %s", bucketA)
	}
	for id := range got {
		if !want[id] {
			t.Errorf("exported event %s was not in the DATA_PLANE list", id)
		}
	}
}

// dpBulkDeletePolicy: a bucket policy lets mallory (no IAM role) delete only
// shared/*; a DeleteObjects for two keys is authorized per key and audited as
// one DONE event with the deleted keys and the per-key errors.
func dpBulkDeletePolicy(t *testing.T, e *dpEnv, alice, mallory *dpIdentity) {
	t.Helper()
	ctx := t.Context()
	wait := waitFor(t)
	group := wait(iamsvc.NewGroupService(e.sdk).Create(ctx, &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: uniqueName("g")}, Spec: &iampb.GroupSpec{},
	})).ResourceID()
	wait(iamsvc.NewGroupMembershipService(e.sdk).Create(ctx, &iampb.CreateGroupMembershipRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: group}, Spec: &iampb.GroupMembershipSpec{MemberId: mallory.saID},
	}))
	name := uniqueName("dp-policy")
	id := wait(storagesvc.NewBucketService(e.sdk).Create(ctx, &storagepb.CreateBucketRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name},
		Spec: &storagepb.BucketSpec{
			ObjectAuditLogging: storagepb.BucketSpec_ALL,
			BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{{
				Paths: []string{"shared/*"}, Roles: []string{"storage.object-editor"},
				Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: group},
			}}},
		},
	})).ResourceID()

	b := aws.String(name)
	for _, k := range []string{"shared/1", "private/b.txt"} {
		okErr(t, "PutObject "+k, errOnly(alice.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String(k), Body: strings.NewReader("x")})))
	}
	e.expect(t, name, "PUT_OBJECT shared/1", "PUT_OBJECT private/b.txt")

	out, err := mallory.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: b, Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("shared/1")}, {Key: aws.String("private/b.txt")}},
	}})
	if err != nil {
		t.Fatalf("DeleteObjects as mallory: %v", err)
	}
	if len(out.Deleted) != 1 || aws.ToString(out.Deleted[0].Key) != "shared/1" ||
		len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != "private/b.txt" || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Fatalf("DeleteObjects = deleted %v, errors %v; want shared/1 deleted and AccessDenied for private/b.txt", out.Deleted, out.Errors)
	}

	ev := e.expect(t, name, "DELETE_OBJECTS")[0]
	if ev.GetStatus() != auditv2pb.Status_DONE || ev.GetResponse().GetStatusCode() != code.Code_OK {
		t.Errorf("DELETE_OBJECTS status = %v, code %v (%s); want DONE and OK", ev.GetStatus(), ev.GetResponse().GetStatusCode(), ev.GetResponse().GetErrorMessage())
	}
	if a := ev.GetAuthentication(); a.GetSubject().GetServiceAccountId() != mallory.saID || a.GetStaticKeyCredential().GetId() != mallory.keyResID || !a.GetAuthenticated() {
		t.Errorf("DELETE_OBJECTS authentication = %v, want mallory %s with key %s", a, mallory.saID, mallory.keyResID)
	}
	if ev.GetResource().GetMetadata().GetId() != id {
		t.Errorf("DELETE_OBJECTS bucket id = %q, want %q", ev.GetResource().GetMetadata().GetId(), id)
	}
	if got := listField(ev.GetRequest().GetParameters(), "object_keys"); !slices.Equal(got, []string{"private/b.txt", "shared/1"}) {
		t.Errorf("object_keys = %v, want private/b.txt and shared/1", got)
	}
	pl := ev.GetResponse().GetPayload()
	if got := listField(pl, "deleted"); !slices.Equal(got, []string{"shared/1"}) {
		t.Errorf("payload deleted = %v, want [shared/1]", got)
	}
	if errs := pl.GetFields()["errors"].GetStructValue().AsMap(); len(errs) != 1 || errs["private/b.txt"] != "AccessDenied" {
		t.Errorf("payload errors = %v, want {private/b.txt: AccessDenied}", errs)
	}
}
