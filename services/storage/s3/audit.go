package s3

import (
	"context"
	"encoding/xml"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/sigv4"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// Data-plane audit events: every served request is reported to the audit
// module's peer.AuditSink, which applies the Nebius policy (the bucket's
// object_audit_logging, the documented operations, no 400/429). Nothing here
// changes or delays the response beyond an in-memory append, and only
// structured fields are passed: never headers, credentials or signatures.

// AuditTo makes the front report requests to the peer.AuditSink provided in
// d, looked up per request (none when the audit module is disabled). Call it
// before Start.
func (s *Server) AuditTo(d *peer.Directory) { s.peers = d }

// maxErrorSniff bounds how much of an error response body is kept to read
// its S3 error code.
const maxErrorSniff = 2048

// statusRecorder sits under responseWriter, so it sees exactly what reaches
// the client: the final status and, for errors, the start of the body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	errBuf []byte
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= http.StatusBadRequest && len(w.errBuf) < maxErrorSniff {
		w.errBuf = append(w.errBuf, p[:min(len(p), maxErrorSniff-len(w.errBuf))]...)
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// s3ErrorBody is the part of an S3 error document the audit event keeps.
type s3ErrorBody struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// recordAccess reports one finished request. herr is the error handle
// returned (written by fail); bulk is the per-key outcome of DeleteObjects.
func (s *Server) recordAccess(r *http.Request, t target, sr *statusRecorder, bulk *bulkDeleteOutcome) {
	sink, ok := peer.Lookup(s.peers, peer.AuditSinkKey)
	if !ok || t.Bucket == "" {
		return
	}
	ctx := context.WithoutCancel(r.Context()) // the client may be gone; the record is not
	op, err := classify(r, t)
	if err != nil {
		return // unroutable: answered with 400/501, never recorded
	}
	b, err := s.gate.Bucket(ctx, t.Bucket)
	if err != nil {
		return // no such bucket: nothing to attribute the event to
	}
	logging := auditLogging(b.GetSpec().GetObjectAuditLogging())
	if logging == peer.ObjectAuditNone {
		return
	}
	a := peer.ObjectAccess{
		Operation:  auditOperation(op, r),
		BucketID:   b.GetMetadata().GetId(),
		BucketName: t.Bucket,
		Logging:    logging,
		VersionID:  r.URL.Query().Get("versionId"),
		Params:     auditParams(op, r),
		ClientIP:   clientIP(r),
		UserAgent:  r.UserAgent(),
		RequestID:  sr.Header().Get("X-Amz-Request-Id"),
		HTTPStatus: sr.status,
		Response:   auditResponse(sr),
	}
	if op.action.ObjectScoped() {
		a.ObjectKey = t.Key
	}
	if bulk != nil {
		a.ObjectKeys = append(slices.Clone(bulk.Deleted), slices.Sorted(maps.Keys(bulk.Errors))...)
		a.KeyErrors = maps.Clone(bulk.Errors)
	}
	if sr.status >= http.StatusBadRequest {
		var e s3ErrorBody
		if xml.Unmarshal(sr.errBuf, &e) == nil {
			a.ErrorCode, a.ErrorMessage = e.Code, e.Message
		}
	}
	s.auditPrincipal(ctx, r, &a)
	sink.RecordObjectAccess(ctx, a)
}

// auditPrincipal fills the credential fields: the access key a signed
// request names and, once its signature was accepted, the key's account.
func (s *Server) auditPrincipal(ctx context.Context, r *http.Request, a *peer.ObjectAccess) {
	auth, err := sigv4.Parse(r)
	if err != nil {
		return // unsigned (anonymous) or malformed
	}
	key, err := s.gate.ResolveKey(ctx, auth.AccessKeyID)
	if err != nil {
		return // unknown, inactive or expired key
	}
	a.AccessKeyID = key.KeyID
	if !credentialError(a.ErrorCode) {
		a.Authenticated = true
		a.PrincipalID = key.PrincipalID
	}
}

// credentialError reports S3 error codes that mean the signature or the
// credential itself was rejected.
func credentialError(code string) bool {
	switch code {
	case "SignatureDoesNotMatch", "InvalidAccessKeyId", "AuthorizationHeaderMalformed",
		"RequestTimeTooSkewed", "ExpiredToken", "InvalidToken", "AccessDeniedInvalidRegion":
		return true
	default:
		return false
	}
}

func auditLogging(m storagepb.BucketSpec_ObjectAuditLogging) peer.ObjectAuditLogging {
	switch m {
	case storagepb.BucketSpec_ALL:
		return peer.ObjectAuditAll
	case storagepb.BucketSpec_MUTATE_ONLY:
		return peer.ObjectAuditMutateOnly
	case storagepb.BucketSpec_NONE, storagepb.BucketSpec_OBJECT_AUDIT_LOGGING_UNSPECIFIED:
		return peer.ObjectAuditNone
	default:
		return peer.ObjectAuditNone
	}
}

// auditOperation is the S3 API name of op as the audit log names it: a
// versioned DeleteObject is DeleteObjectVersion, and a list-type=2 listing
// is ListObjectsV2 (the front routes both listings as ListObjects).
func auditOperation(op operation, r *http.Request) string {
	q := r.URL.Query()
	switch {
	case op.action == domain.ActDeleteObject && q.Get("versionId") != "":
		return "DeleteObjectVersion"
	case op.action == domain.ActListObjects && q.Get("list-type") == "2":
		return "ListObjectsV2"
	default:
		return string(op.action)
	}
}

// auditParams are the request parameters worth auditing beyond bucket, key
// and version.
func auditParams(op operation, r *http.Request) map[string]string {
	p := map[string]string{}
	q := r.URL.Query()
	if op.action.Listing() && op.target != "" {
		p["prefix"] = op.target
	}
	for param, name := range map[string]string{"uploadId": "upload_id", "partNumber": "part_number"} {
		if v := q.Get(param); v != "" {
			p[name] = v
		}
	}
	if op.copy != nil {
		p["copy_source"] = op.copy.Bucket + "/" + op.copy.Key
	}
	return p
}

func auditResponse(sr *statusRecorder) map[string]string {
	p := map[string]string{}
	h := sr.Header()
	if v := h.Get("ETag"); v != "" {
		p["etag"] = strings.Trim(v, `"`)
	}
	if v := h.Get("X-Amz-Version-Id"); v != "" {
		p["version_id"] = v
	}
	return p
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
