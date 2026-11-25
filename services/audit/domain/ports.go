// Package domain holds the audit service business logic: querying recorded
// events and exporting them to object storage. It is transport-free and
// depends only on the ports declared in this file.
package domain

import (
	"context"
	"io"
	"iter"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// KindExport is the NID type of AuditEventExport resources.
const KindExport = "auditeventexport"

// EventSource yields recorded audit entries (satisfied by *audit.Log).
type EventSource interface {
	// Since yields entries with Seq > after, oldest first.
	Since(after uint64) iter.Seq[*audit.Entry]
}

// Export is a stored export together with the region it was started in
// (AuditEventExport has no region field).
type Export struct {
	Region   string
	Resource *auditv2pb.AuditEventExport
}

// ExportPage is one page of an export listing.
type ExportPage struct {
	Items         []*auditv2pb.AuditEventExport
	NextPageToken string
}

// ExportRepository stores audit event exports.
type ExportRepository interface {
	Get(ctx context.Context, id string) (Export, error)
	// Create fails with errs.AlreadyExists if the name is taken in the parent.
	Create(ctx context.Context, e Export) error
	// Modify reads the export, applies fn and stores the result in one
	// transaction; the region does not change. An error from fn aborts the
	// write and is returned as is.
	Modify(ctx context.Context, id string, fn func(*auditv2pb.AuditEventExport) error) (*auditv2pb.AuditEventExport, error)
	// List returns the exports of parentID that were started in region.
	List(ctx context.Context, parentID, region string, pageSize int, pageToken string) (ExportPage, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine).
type Hierarchy interface {
	// Parent returns the parent of id ("" for a root) or errs.NotFound.
	Parent(ctx context.Context, id string) (string, error)
}

// Buckets resolves destination buckets of the storage service.
type Buckets interface {
	// Validate checks that the caller may export to bucketID: the storage
	// module is enabled (errs.BadState otherwise), the bucket exists in the
	// tenant of parentID (errs.BadRequest) and the caller may write to it
	// (errs.PermissionDenied).
	Validate(ctx context.Context, bucketID, parentID string) error
	// BackendName returns the object-store bucket name behind bucketID.
	BackendName(ctx context.Context, bucketID string) (string, error)
}

// ObjectWriter writes export objects (satisfied by objstore.Store). The
// storage module owns the backend bucket; the writer never creates it.
type ObjectWriter interface {
	// BucketEmpty fails with objstore.ErrBucketNotFound if the bucket is missing.
	BucketEmpty(ctx context.Context, bucket string) (bool, error)
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opts objstore.PutOptions) (objstore.ObjectInfo, error)
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}
