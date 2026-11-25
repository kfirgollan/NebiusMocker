package peer

import "context"

// AuditSinkKey is the capability provided by the audit module: recording
// data-plane audit events (event_type DATA_PLANE). Data planes that serve
// no gRPC, such as the Object Storage S3 front, report through it; gRPC
// calls are audited centrally by interceptors.Audit.
var AuditSinkKey = NewKey[AuditSink]("audit.sink")

// AuditSink records data-plane audit events. Look it up per request: when the
// audit module is disabled there is no sink and nothing is recorded.
type AuditSink interface {
	// RecordObjectAccess records one S3 request on an object or object
	// listing of a bucket. It applies the Nebius logging policy itself, so
	// callers report every request they serve:
	//   - only the documented S3 operations are recorded (see
	//     docs/services/audit.md): writes when the bucket's Logging is
	//     ObjectAuditMutateOnly or ObjectAuditAll, reads only with All;
	//   - responses with HTTP 400 or 429 are not recorded;
	//   - requests without a resolved bucket (BucketID) are not recorded.
	//
	// It never fails and never blocks for long (an append to an in-memory
	// log); it must not be given credentials or signatures, only the fields
	// below.
	RecordObjectAccess(ctx context.Context, a ObjectAccess)
}

// ObjectAuditLogging is a bucket's spec.object_audit_logging.
type ObjectAuditLogging int

// Bucket object audit logging modes (storage.v1 BucketSpec.ObjectAuditLogging).
const (
	ObjectAuditNone       ObjectAuditLogging = iota // default (NONE or unspecified): nothing
	ObjectAuditMutateOnly                           // MUTATE_ONLY: writes and deletes
	ObjectAuditAll                                  // ALL: writes, deletes, reads and listings
)

// ObjectAccess describes one served S3 request.
type ObjectAccess struct {
	// Operation is the S3 API name: "PutObject", "GetObject",
	// "ListObjectsV2", "DeleteObjects", "CompleteMultipartUpload", …
	Operation string

	// BucketID is the Nebius bucket ID ("storagebucket-…"), BucketName its
	// name. Logging is the bucket's object_audit_logging mode.
	BucketID   string
	BucketName string
	Logging    ObjectAuditLogging

	// ObjectKey is the object the request names ("" for listings);
	// ObjectKeys the keys of a DeleteObjects batch. VersionID is the
	// requested object version, if any.
	ObjectKey  string
	ObjectKeys []string
	// KeyErrors are the per-key failures of a DeleteObjects batch (key →
	// S3 error code, e.g. AccessDenied from the bucket policy); the other
	// ObjectKeys were deleted.
	KeyErrors map[string]string
	VersionID string
	// Params are further request parameters worth auditing, e.g. "prefix",
	// "upload_id", "part_number", "copy_source". Never credentials.
	Params map[string]string

	// Authenticated reports a valid signature. PrincipalID is the service
	// account the access key acts as, AccessKeyID the Nebius ID of the key
	// ("accesskey-…"); both are empty when the key is unknown.
	Authenticated bool
	PrincipalID   string
	AccessKeyID   string

	// ClientIP, UserAgent and RequestID describe the HTTP request.
	ClientIP  string
	UserAgent string
	RequestID string

	// HTTPStatus is the response status; ErrorCode and ErrorMessage are the
	// S3 error ("AccessDenied", "NoSuchKey", …) of a failed request.
	HTTPStatus   int
	ErrorCode    string
	ErrorMessage string
	// Response are response fields worth auditing, e.g. "etag",
	// "version_id", "size".
	Response map[string]string
}
