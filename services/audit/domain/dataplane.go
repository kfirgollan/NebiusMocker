package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/types/known/structpb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// storageService is service.name of Object Storage events.
const storageService = "STORAGE"

// HTTP statuses of S3 responses (the domain does not import net/http).
const (
	statusBadRequest          = 400
	statusUnauthorized        = 401
	statusForbidden           = 403
	statusNotFound            = 404
	statusConflict            = 409
	statusPreconditionFailed  = 412
	statusRangeNotSatisfiable = 416
	statusTooManyRequests     = 429
	statusInternalServerError = 500
	statusNotImplemented      = 501
	statusServiceUnavailable  = 503
)

// maxDataPlaneAncestry bounds the bucket ancestry walk.
const maxDataPlaneAncestry = 16

// DataPlaneRecorder stores data-plane events (satisfied by *audit.Recorder,
// which stamps the ID, time and versions).
type DataPlaneRecorder interface {
	RecordDataPlane(ctx context.Context, ev *auditv2pb.AuditEvent)
}

// objectOps are the S3 operations Nebius records as Object Storage
// data-plane events (docs.nebius.com/object-storage/logs), with whether they
// change objects. ListObjectsV2 is the current form of ListObjects.
var objectOps = map[string]bool{
	"PutObject":               true,
	"CopyObject":              true,
	"CreateMultipartUpload":   true,
	"UploadPart":              true,
	"UploadPartCopy":          true,
	"AbortMultipartUpload":    true,
	"CompleteMultipartUpload": true,
	"DeleteObject":            true,
	"DeleteObjects":           true,
	"DeleteObjectVersion":     true,
	"GetObject":               false,
	"HeadObject":              false,
	"ListParts":               false,
	"ListObjectVersions":      false,
	"ListObjects":             false,
	"ListObjectsV2":           false,
	"ListMultipartUploads":    false,
}

// DataPlaneSink turns S3 requests reported by the Object Storage front into
// DATA_PLANE audit events. It implements peer.AuditSink.
type DataPlaneSink struct {
	rec       DataPlaneRecorder
	hierarchy Hierarchy
}

var _ peer.AuditSink = (*DataPlaneSink)(nil)

// NewDataPlaneSink wires a DataPlaneSink.
func NewDataPlaneSink(rec DataPlaneRecorder, hierarchy Hierarchy) (*DataPlaneSink, error) {
	if rec == nil || hierarchy == nil {
		return nil, errors.New("audit: data-plane sink dependencies must not be nil")
	}
	return &DataPlaneSink{rec: rec, hierarchy: hierarchy}, nil
}

// RecordObjectAccess implements peer.AuditSink.
func (s *DataPlaneSink) RecordObjectAccess(ctx context.Context, a peer.ObjectAccess) {
	if !Logged(a) {
		return
	}
	s.rec.RecordDataPlane(ctx, s.event(ctx, a))
}

// Logged applies the Nebius data-plane logging policy to a: a documented
// operation on a resolved bucket whose object_audit_logging covers it, and a
// response other than 400 Bad Request or 429 Too Many Requests.
func Logged(a peer.ObjectAccess) bool {
	mutating, documented := objectOps[a.Operation]
	switch {
	case !documented || a.BucketID == "":
		return false
	case a.HTTPStatus == statusBadRequest || a.HTTPStatus == statusTooManyRequests:
		return false
	case a.Logging == peer.ObjectAuditAll:
		return true
	case a.Logging == peer.ObjectAuditMutateOnly:
		return mutating
	default:
		return false
	}
}

func (s *DataPlaneSink) event(ctx context.Context, a peer.ObjectAccess) *auditv2pb.AuditEvent {
	action := upperSnake(a.Operation)
	resType, _, _ := strings.Cut(a.BucketID, "-")
	c := rpcCode(a)
	ev := &auditv2pb.AuditEvent{
		Source:         "s3/" + a.Operation,
		Type:           "ai.nebius.storage." + resType + "." + strings.ToLower(action),
		Service:        &auditv2pb.Service{Name: storageService},
		Action:         action,
		Authentication: dataPlaneAuthn(a),
		Resource: &auditv2pb.Resource{
			Metadata:  &auditv2pb.ResourceMetadata{Id: a.BucketID, Name: a.BucketName, Type: resType},
			Hierarchy: s.ancestry(ctx, a.BucketID),
		},
		Request: &auditv2pb.Request{
			ClientIp:   a.ClientIP,
			UserAgent:  a.UserAgent,
			RequestId:  a.RequestID,
			TraceId:    a.RequestID,
			Parameters: requestParams(a),
		},
		Response: &auditv2pb.Response{StatusCode: c, Payload: responsePayload(a)},
		Status:   auditv2pb.Status_DONE,
	}
	if c != code.Code_OK {
		ev.Status = auditv2pb.Status_ERROR
		ev.Response.ErrorMessage = errorMessage(a)
	}
	return ev
}

func dataPlaneAuthn(a peer.ObjectAccess) *auditv2pb.Authentication {
	authn := &auditv2pb.Authentication{
		Authenticated:      a.Authenticated,
		AuthenticationType: auditv2pb.AuthenticationType_STATIC_KEY,
	}
	if a.PrincipalID != "" {
		authn.Subject = &auditv2pb.Subject{
			Name: a.PrincipalID,
			Id:   &auditv2pb.Subject_ServiceAccountId{ServiceAccountId: a.PrincipalID},
		}
	}
	if a.AccessKeyID != "" {
		authn.Credential = &auditv2pb.Authentication_StaticKeyCredential{
			StaticKeyCredential: &auditv2pb.StaticKey{Id: a.AccessKeyID},
		}
	}
	return authn
}

// ancestry returns the bucket's ancestors, tenant first. Names are filled in
// by the recorder from what it has seen.
func (s *DataPlaneSink) ancestry(ctx context.Context, id string) []*auditv2pb.ResourceMetadata {
	var up []*auditv2pb.ResourceMetadata
	for range maxDataPlaneAncestry {
		p, err := s.hierarchy.Parent(ctx, id)
		if err != nil || p == "" {
			break
		}
		kind, _, _ := strings.Cut(p, "-")
		up = append(up, &auditv2pb.ResourceMetadata{Id: p, Type: kind})
		id = p
	}
	slices.Reverse(up)
	return up
}

// requestParams are the documented request.parameters (bucket_name,
// object_key, version) plus the caller's extra parameters.
func requestParams(a peer.ObjectAccess) *structpb.Struct {
	fields := map[string]*structpb.Value{"bucket_name": structpb.NewStringValue(a.BucketName)}
	if a.ObjectKey != "" {
		fields["object_key"] = structpb.NewStringValue(a.ObjectKey)
	}
	if len(a.ObjectKeys) > 0 {
		fields["object_keys"] = stringList(a.ObjectKeys)
	}
	if a.VersionID != "" {
		fields["version"] = structpb.NewStringValue(a.VersionID)
	}
	for k, v := range a.Params {
		if _, taken := fields[k]; !taken {
			fields[k] = structpb.NewStringValue(v)
		}
	}
	return &structpb.Struct{Fields: fields}
}

func stringList(ss []string) *structpb.Value {
	vs := make([]*structpb.Value, 0, len(ss))
	for _, s := range ss {
		vs = append(vs, structpb.NewStringValue(s))
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vs})
}

func responsePayload(a peer.ObjectAccess) *structpb.Struct {
	fields := map[string]*structpb.Value{"http_status": structpb.NewNumberValue(float64(a.HTTPStatus))}
	if a.ErrorCode != "" {
		fields["error_code"] = structpb.NewStringValue(a.ErrorCode)
	}
	if len(a.ObjectKeys) > 0 {
		// DeleteObjects: which keys went and which failed, with their codes.
		deleted := make([]string, 0, len(a.ObjectKeys))
		for _, k := range a.ObjectKeys {
			if _, failed := a.KeyErrors[k]; !failed {
				deleted = append(deleted, k)
			}
		}
		fields["deleted"] = stringList(deleted)
		if len(a.KeyErrors) > 0 {
			errs := make(map[string]*structpb.Value, len(a.KeyErrors))
			for k, c := range a.KeyErrors {
				errs[k] = structpb.NewStringValue(c)
			}
			fields["errors"] = structpb.NewStructValue(&structpb.Struct{Fields: errs})
		}
	}
	for k, v := range a.Response {
		if _, taken := fields[k]; !taken {
			fields[k] = structpb.NewStringValue(v)
		}
	}
	return &structpb.Struct{Fields: fields}
}

func errorMessage(a peer.ObjectAccess) string {
	switch {
	case a.ErrorCode != "" && a.ErrorMessage != "":
		return a.ErrorCode + ": " + a.ErrorMessage
	case a.ErrorCode != "":
		return a.ErrorCode
	case a.ErrorMessage != "":
		return a.ErrorMessage
	default:
		return fmt.Sprintf("HTTP %d", a.HTTPStatus)
	}
}

// rpcCode maps an S3 response to response.status_code (google.rpc.Code),
// the way the gRPC-HTTP mapping does, with S3's authentication errors
// (403 with a credential error code) as UNAUTHENTICATED.
func rpcCode(a peer.ObjectAccess) code.Code {
	switch a.ErrorCode {
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken", "MissingSecurityHeader":
		return code.Code_UNAUTHENTICATED
	}
	switch s := a.HTTPStatus; {
	case s < statusBadRequest:
		return code.Code_OK
	case s == statusUnauthorized:
		return code.Code_UNAUTHENTICATED
	case s == statusForbidden:
		return code.Code_PERMISSION_DENIED
	case s == statusNotFound:
		return code.Code_NOT_FOUND
	case s == statusConflict:
		return code.Code_ABORTED
	case s == statusPreconditionFailed:
		return code.Code_FAILED_PRECONDITION
	case s == statusRangeNotSatisfiable:
		return code.Code_OUT_OF_RANGE
	case s == statusNotImplemented:
		return code.Code_UNIMPLEMENTED
	case s == statusServiceUnavailable:
		return code.Code_UNAVAILABLE
	case s >= statusInternalServerError:
		return code.Code_INTERNAL
	default:
		return code.Code_INVALID_ARGUMENT
	}
}

// upperSnake turns "ListObjectsV2" into "LIST_OBJECTS_V2", like the
// control-plane action of a gRPC method.
func upperSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}
