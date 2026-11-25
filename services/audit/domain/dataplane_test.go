package domain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/structpb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

// fakeDataPlaneRecorder keeps the events it is given.
type fakeDataPlaneRecorder struct {
	mu     sync.Mutex
	events []*auditv2pb.AuditEvent
}

func (f *fakeDataPlaneRecorder) RecordDataPlane(_ context.Context, ev *auditv2pb.AuditEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

var (
	mutatingOps = []string{
		"PutObject", "CopyObject", "CreateMultipartUpload", "UploadPart", "UploadPartCopy",
		"AbortMultipartUpload", "CompleteMultipartUpload", "DeleteObject", "DeleteObjects", "DeleteObjectVersion",
	}
	readingOps = []string{
		"GetObject", "HeadObject", "ListParts", "ListObjectVersions", "ListObjects", "ListObjectsV2", "ListMultipartUploads",
	}
)

func access(op string, logging peer.ObjectAuditLogging, status int) peer.ObjectAccess {
	return peer.ObjectAccess{Operation: op, BucketID: bucketID, Logging: logging, HTTPStatus: status}
}

func TestLogged(t *testing.T) {
	t.Parallel()
	type row struct {
		name string
		a    peer.ObjectAccess
		want bool
	}
	var tests []row
	for _, op := range mutatingOps {
		tests = append(tests,
			row{op + " none", access(op, peer.ObjectAuditNone, 200), false},
			row{op + " mutate-only", access(op, peer.ObjectAuditMutateOnly, 200), true},
			row{op + " all", access(op, peer.ObjectAuditAll, 200), true},
		)
	}
	for _, op := range readingOps {
		tests = append(tests,
			row{op + " none", access(op, peer.ObjectAuditNone, 200), false},
			row{op + " mutate-only", access(op, peer.ObjectAuditMutateOnly, 200), false},
			row{op + " all", access(op, peer.ObjectAuditAll, 200), true},
		)
	}
	noBucket := access("PutObject", peer.ObjectAuditAll, 200)
	noBucket.BucketID = ""
	tests = append(tests,
		row{"undocumented GetObjectAcl", access("GetObjectAcl", peer.ObjectAuditAll, 200), false},
		row{"undocumented ListBuckets", access("ListBuckets", peer.ObjectAuditAll, 200), false},
		row{"empty operation", access("", peer.ObjectAuditAll, 200), false},
		row{"no bucket", noBucket, false},
		row{"400 dropped", access("PutObject", peer.ObjectAuditAll, 400), false},
		row{"429 dropped", access("GetObject", peer.ObjectAuditAll, 429), false},
		row{"403 logged", access("PutObject", peer.ObjectAuditMutateOnly, 403), true},
		row{"404 logged", access("GetObject", peer.ObjectAuditAll, 404), true},
		row{"500 logged", access("DeleteObject", peer.ObjectAuditAll, 500), true},
		row{"unset status logged", access("PutObject", peer.ObjectAuditAll, 0), true},
	)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.Logged(tt.a); got != tt.want {
				t.Fatalf("Logged(%+v) = %v, want %v", tt.a, got, tt.want)
			}
		})
	}
}

func newSink(t *testing.T) (*domain.DataPlaneSink, *fakeDataPlaneRecorder) {
	t.Helper()
	rec := &fakeDataPlaneRecorder{}
	h := newFakeHierarchy()
	h[bucketID] = projectID
	s, err := domain.NewDataPlaneSink(rec, h)
	if err != nil {
		t.Fatal(err)
	}
	return s, rec
}

func TestNewDataPlaneSinkRejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewDataPlaneSink(nil, newFakeHierarchy()); err == nil {
		t.Error("nil recorder: want error")
	}
	if _, err := domain.NewDataPlaneSink(&fakeDataPlaneRecorder{}, nil); err == nil {
		t.Error("nil hierarchy: want error")
	}
}

func TestRecordObjectAccessSkipsUnlogged(t *testing.T) {
	t.Parallel()
	s, rec := newSink(t)
	s.RecordObjectAccess(t.Context(), access("GetObject", peer.ObjectAuditMutateOnly, 200))
	s.RecordObjectAccess(t.Context(), access("GetObjectAcl", peer.ObjectAuditAll, 200))
	s.RecordObjectAccess(t.Context(), access("PutObject", peer.ObjectAuditAll, 429))
	if len(rec.events) != 0 {
		t.Fatalf("recorded %v, want nothing", rec.events)
	}
}

func strList(vs ...string) *structpb.Value {
	l := &structpb.ListValue{}
	for _, v := range vs {
		l.Values = append(l.Values, structpb.NewStringValue(v))
	}
	return structpb.NewListValue(l)
}

func newStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecordObjectAccessEvent(t *testing.T) {
	t.Parallel()
	hierarchy := []*auditv2pb.ResourceMetadata{
		{Id: tenantID, Type: "tenant"},
		{Id: projectID, Type: "project"},
	}
	authn := &auditv2pb.Authentication{
		Authenticated:      true,
		AuthenticationType: auditv2pb.AuthenticationType_STATIC_KEY,
		Subject:            &auditv2pb.Subject{Name: "serviceaccount-1", Id: &auditv2pb.Subject_ServiceAccountId{ServiceAccountId: "serviceaccount-1"}},
		Credential:         &auditv2pb.Authentication_StaticKeyCredential{StaticKeyCredential: &auditv2pb.StaticKey{Id: "accesskey-1"}},
	}
	anonAuthn := &auditv2pb.Authentication{AuthenticationType: auditv2pb.AuthenticationType_STATIC_KEY}
	badSigAuthn := &auditv2pb.Authentication{
		AuthenticationType: auditv2pb.AuthenticationType_STATIC_KEY,
		Credential:         &auditv2pb.Authentication_StaticKeyCredential{StaticKeyCredential: &auditv2pb.StaticKey{Id: "accesskey-1"}},
	}
	base := func(op string) peer.ObjectAccess {
		return peer.ObjectAccess{
			Operation: op, BucketID: bucketID, BucketName: "photos", Logging: peer.ObjectAuditAll,
			Authenticated: true, PrincipalID: "serviceaccount-1", AccessKeyID: "accesskey-1",
			ClientIP: "10.0.0.1", UserAgent: "aws-cli", RequestID: "req-1", HTTPStatus: 200,
		}
	}
	reqOf := func(params map[string]any) *auditv2pb.Request {
		return &auditv2pb.Request{ClientIp: "10.0.0.1", UserAgent: "aws-cli", RequestId: "req-1", TraceId: "req-1", Parameters: newStruct(t, params)}
	}
	event := func(op, action string, authn *auditv2pb.Authentication, req *auditv2pb.Request, resp *auditv2pb.Response, st auditv2pb.Status) *auditv2pb.AuditEvent {
		return &auditv2pb.AuditEvent{
			Source: "s3/" + op, Type: "ai.nebius.storage.storagebucket." + strings.ToLower(action), Service: &auditv2pb.Service{Name: "STORAGE"},
			Action: action, Authentication: authn,
			Resource: &auditv2pb.Resource{
				Metadata:  &auditv2pb.ResourceMetadata{Id: bucketID, Name: "photos", Type: "storagebucket"},
				Hierarchy: hierarchy,
			},
			Request: req, Response: resp, Status: st,
		}
	}
	ok := func(extra map[string]any) *auditv2pb.Response {
		m := map[string]any{"http_status": 200.0}
		for k, v := range extra {
			m[k] = v
		}
		return &auditv2pb.Response{StatusCode: code.Code_OK, Payload: newStruct(t, m)}
	}
	failed := func(c code.Code, msg string, payload map[string]any) *auditv2pb.Response {
		return &auditv2pb.Response{StatusCode: c, ErrorMessage: msg, Payload: newStruct(t, payload)}
	}

	put := base("PutObject")
	put.ObjectKey = "a/b.txt"
	put.VersionID = "v1"
	put.Params = map[string]string{"content_type": "text/plain", "object_key": "ignored"}
	put.Response = map[string]string{"etag": "abc", "http_status": "ignored"}

	del := base("DeleteObjects")
	del.ObjectKeys = []string{"k1", "k2"}

	delDenied := del
	delDenied.KeyErrors = map[string]string{"k2": "AccessDenied"}

	list := base("ListObjectsV2")
	list.Params = map[string]string{"prefix": "logs/"}

	denied := base("GetObject")
	denied.Authenticated, denied.PrincipalID, denied.AccessKeyID = false, "", ""
	denied.ObjectKey = "x"
	denied.HTTPStatus, denied.ErrorCode, denied.ErrorMessage = 403, "AccessDenied", "Access Denied"

	badKey := denied
	badKey.ErrorCode, badKey.ErrorMessage = "InvalidAccessKeyId", ""

	badSig := base("PutObject")
	badSig.Authenticated, badSig.PrincipalID = false, ""
	badSig.HTTPStatus, badSig.ErrorCode = 403, "SignatureDoesNotMatch"

	missing := base("GetObject")
	missing.ObjectKey = "gone"
	missing.HTTPStatus, missing.ErrorCode, missing.ErrorMessage = 404, "NoSuchKey", "The specified key does not exist."

	boom := base("HeadObject")
	boom.ObjectKey = "k"
	boom.HTTPStatus = 500

	delKeys := &auditv2pb.Request{
		ClientIp: "10.0.0.1", UserAgent: "aws-cli", RequestId: "req-1", TraceId: "req-1",
		Parameters: &structpb.Struct{Fields: map[string]*structpb.Value{
			"bucket_name": structpb.NewStringValue("photos"), "object_keys": strList("k1", "k2"),
		}},
	}

	delDeniedKeys := &auditv2pb.Request{
		ClientIp: "10.0.0.1", UserAgent: "aws-cli", RequestId: "req-1", TraceId: "req-1",
		Parameters: &structpb.Struct{Fields: map[string]*structpb.Value{
			"bucket_name": structpb.NewStringValue("photos"), "object_keys": strList("k1", "k2"),
		}},
	}

	tests := []struct {
		name string
		a    peer.ObjectAccess
		want *auditv2pb.AuditEvent
	}{
		{
			"put with version and extras", put,
			event("PutObject", "PUT_OBJECT", authn,
				reqOf(map[string]any{"bucket_name": "photos", "object_key": "a/b.txt", "version": "v1", "content_type": "text/plain"}),
				ok(map[string]any{"etag": "abc"}), auditv2pb.Status_DONE),
		},
		{"delete objects lists keys", del, event("DeleteObjects", "DELETE_OBJECTS", authn, delKeys, ok(map[string]any{"deleted": []any{"k1", "k2"}}), auditv2pb.Status_DONE)},
		{"delete objects reports per-key errors", delDenied, event("DeleteObjects", "DELETE_OBJECTS", authn, delDeniedKeys, ok(map[string]any{"deleted": []any{"k1"}, "errors": map[string]any{"k2": "AccessDenied"}}), auditv2pb.Status_DONE)},
		{
			"list objects v2", list,
			event("ListObjectsV2", "LIST_OBJECTS_V2", authn,
				reqOf(map[string]any{"bucket_name": "photos", "prefix": "logs/"}), ok(nil), auditv2pb.Status_DONE),
		},
		{
			"unauthenticated access denied", denied,
			event("GetObject", "GET_OBJECT", anonAuthn,
				reqOf(map[string]any{"bucket_name": "photos", "object_key": "x"}),
				failed(code.Code_PERMISSION_DENIED, "AccessDenied: Access Denied", map[string]any{"http_status": 403.0, "error_code": "AccessDenied"}),
				auditv2pb.Status_ERROR),
		},
		{
			"invalid access key is unauthenticated", badKey,
			event("GetObject", "GET_OBJECT", anonAuthn,
				reqOf(map[string]any{"bucket_name": "photos", "object_key": "x"}),
				failed(code.Code_UNAUTHENTICATED, "InvalidAccessKeyId", map[string]any{"http_status": 403.0, "error_code": "InvalidAccessKeyId"}),
				auditv2pb.Status_ERROR),
		},
		{
			"bad signature is unauthenticated", badSig,
			event("PutObject", "PUT_OBJECT", badSigAuthn,
				reqOf(map[string]any{"bucket_name": "photos"}),
				failed(code.Code_UNAUTHENTICATED, "SignatureDoesNotMatch", map[string]any{"http_status": 403.0, "error_code": "SignatureDoesNotMatch"}),
				auditv2pb.Status_ERROR),
		},
		{
			"not found", missing,
			event("GetObject", "GET_OBJECT", authn,
				reqOf(map[string]any{"bucket_name": "photos", "object_key": "gone"}),
				failed(code.Code_NOT_FOUND, "NoSuchKey: The specified key does not exist.", map[string]any{"http_status": 404.0, "error_code": "NoSuchKey"}),
				auditv2pb.Status_ERROR),
		},
		{
			"server error without code", boom,
			event("HeadObject", "HEAD_OBJECT", authn,
				reqOf(map[string]any{"bucket_name": "photos", "object_key": "k"}),
				failed(code.Code_INTERNAL, "HTTP 500", map[string]any{"http_status": 500.0}),
				auditv2pb.Status_ERROR),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, rec := newSink(t)
			s.RecordObjectAccess(t.Context(), tt.a)
			if len(rec.events) != 1 {
				t.Fatalf("recorded %d events, want 1", len(rec.events))
			}
			got := rec.events[0]
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Fatalf("event mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRecordObjectAccessUnknownAncestry(t *testing.T) {
	t.Parallel()
	rec := &fakeDataPlaneRecorder{}
	s, err := domain.NewDataPlaneSink(rec, fakeHierarchy{})
	if err != nil {
		t.Fatal(err)
	}
	s.RecordObjectAccess(t.Context(), access("PutObject", peer.ObjectAuditAll, 200))
	if len(rec.events) != 1 || len(rec.events[0].GetResource().GetHierarchy()) != 0 {
		t.Fatalf("events = %v, want one event without hierarchy", rec.events)
	}
}
