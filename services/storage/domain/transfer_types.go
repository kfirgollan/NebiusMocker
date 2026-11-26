package domain

import (
	"context"
	"fmt"
	"io"
	"iter"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"
)

// This file holds the version-neutral model of data transfers shared by the
// transfer service (transfer.go), the iteration engine (transfer_engine.go)
// and the adapters of the Endpoint port (backend/s3endpoint, backend/azblob).
// Both API versions (storage.v1 and the deprecated storage.v1alpha1) are
// validated into a Plan; the engine only ever sees a Plan.

// Transfer defaults (docs.nebius.com/object-storage/transfer: limiters "If
// set to zero, default limit will be applied": 4000 requests/s and 50 GiB/s;
// inter_iteration_interval "Default is 15 minutes if not specified").
const (
	DefaultTransferRequestsPerSecond = 4000
	DefaultTransferBandwidth         = 50 << 30
	DefaultTransferInterval          = 15 * time.Minute
	// TransferMultipartThreshold and TransferPartSize: objects of 100 MB or
	// more are copied as multipart uploads of ~50 MB parts (transfer overview).
	TransferMultipartThreshold = 100 << 20
	TransferPartSize           = 50 << 20
	// TransferListPage is the ListObjects page size the docs describe.
	TransferListPage = 1000
)

// TransferAPI is the API version a transfer was created with.
type TransferAPI int

// API versions.
const (
	TransferAPIV1 TransferAPI = iota + 1
	TransferAPIV1Alpha1
)

// ProviderKind is the storage provider of one side of a transfer.
type ProviderKind int

// Providers.
const (
	ProviderNebius       ProviderKind = iota + 1 // a bucket of this mocker, reached through its S3 front
	ProviderS3Compatible                         // any S3-compatible endpoint
	ProviderAzureBlob                            // Azure Blob Storage (source only)
)

func (k ProviderKind) String() string {
	switch k {
	case ProviderNebius:
		return "nebius"
	case ProviderS3Compatible:
		return "s3_compatible"
	case ProviderAzureBlob:
		return "azure_blob_storage"
	default:
		return fmt.Sprintf("provider(%d)", int(k))
	}
}

// Credentials authenticate the transfer at one side. Exactly one form is set:
// Anonymous, an S3 access key (AccessKeyID+Secret) or an Azure storage
// account (AzureAccount+AzureKey).
type Credentials struct {
	Anonymous    bool
	AccessKeyID  string
	Secret       string
	AzureAccount string
	AzureKey     string
}

// Side is one end of a transfer.
type Side struct {
	Kind ProviderKind
	// Endpoint is the provider URL (scheme://host[:port][/path], no trailing
	// slash). Empty for ProviderNebius: the mocker's own S3 front is used.
	Endpoint string
	Region   string
	// Bucket is the bucket name, or the Azure container name.
	Bucket string
	Prefix string
	Creds  Credentials
}

// Identity names the storage location of a side (bucket on an endpoint),
// independent of credentials and prefix. Managed-object records are keyed by it.
func (s Side) Identity() string {
	if s.Kind == ProviderNebius {
		return "nebius:" + s.Region + "/" + s.Bucket
	}
	return s.Kind.String() + ":" + s.Endpoint + "/" + s.Bucket
}

// Overwrite is the overwrite strategy for objects that exist in the destination.
type Overwrite int

// Overwrite strategies.
const (
	OverwriteNever   Overwrite = iota + 1
	OverwriteIfNewer           // source Last-Modified is newer than the destination's
	OverwriteAlways            // v1alpha1 only
)

// StopKind is the stop condition checked after every iteration.
type StopKind int

// Stop conditions.
const (
	StopAfterOneIteration StopKind = iota + 1
	StopAfterNEmptyIterations
	StopInfinite
)

// StopCondition is the stop condition of a transfer.
type StopCondition struct {
	Kind StopKind
	// EmptyThreshold is the number of consecutive empty iterations for
	// StopAfterNEmptyIterations.
	EmptyThreshold uint32
}

// Plan is what a transfer does, independent of the API version.
type Plan struct {
	Source      Side
	Destination Side
	Overwrite   Overwrite
	// DeleteInDestination deletes destination objects that have no source
	// counterpart (v1 enable_deletes_in_destination).
	DeleteInDestination bool
	// TouchUnmanaged allows overwriting and deleting destination objects that
	// no transfer wrote. Always true for v1alpha1 transfers.
	TouchUnmanaged bool
	// RequestsPerSecond and BandwidthBytesPerSecond are the effective source
	// limiters (defaults applied: never zero).
	RequestsPerSecond       uint64
	BandwidthBytesPerSecond uint64
	Stop                    StopCondition
	// Interval is the effective wait between iterations (default applied).
	Interval time.Duration
}

// DestinationKey maps a source key to its destination key: source.prefix is
// removed and destination.prefix prepended.
func (p Plan) DestinationKey(srcKey string) string {
	return p.Destination.Prefix + srcKey[len(p.Source.Prefix):]
}

// IterationState is the state of one transfer iteration.
type IterationState int

// Iteration states.
const (
	IterationInProgress IterationState = iota + 1
	IterationCompleted
	IterationInterrupted // stopped by the user (v1alpha1: STOPPED)
	IterationFailed
)

// ErrorOrigin says which side an error came from.
type ErrorOrigin int

// Error origins.
const (
	OriginUnspecified ErrorOrigin = iota
	OriginSource
	OriginDestination
)

// TransferError describes why an iteration or a transfer failed.
type TransferError struct {
	Origin ErrorOrigin
	// Code and Message usually come from the endpoint's response (S3 error
	// code such as "AccessDenied" or "NoSuchBucket").
	Code    string
	Message string
}

// Iteration is one pass over the source (and, with deletes, the destination).
type Iteration struct {
	Seq   int64 // 1-based
	State IterationState
	Error *TransferError // set when State is IterationFailed
	Start time.Time
	End   time.Time // zero while in progress
	// Discovered counts listed source objects under the source prefix;
	// Transferred those copied (or whose metadata was replaced); Skipped
	// those left alone; Deleted destination objects removed.
	Discovered       int64
	Transferred      int64
	Skipped          int64
	Deleted          int64
	TransferredBytes int64
	// ThroughputBytes is the average throughput in bytes per second.
	ThroughputBytes int64
}

// Empty reports whether nothing was transferred or deleted (the "empty
// iteration" of the stop condition).
func (it Iteration) Empty() bool { return it.Transferred == 0 && it.Deleted == 0 }

// TransferState is the state of a transfer.
type TransferState int

// Transfer states.
const (
	TransferActive TransferState = iota + 1
	TransferStopping
	TransferStopped
	TransferFailing
	TransferFailed
	TransferDeleting
)

// TransferStatus is the observed state of a transfer.
type TransferStatus struct {
	State TransferState
	// Error is set when State is TransferFailed (the failed iteration's error).
	Error *TransferError
	// LastIteration is the running iteration, or the last finished one; nil
	// before the first iteration starts.
	LastIteration *Iteration
}

// TransferRecord is a stored transfer: the resource as created through its
// API version (spec with input-only secrets; status not stored there) plus
// the runner state. Exactly one of V1 and Alpha is set, per API.
type TransferRecord struct {
	API   TransferAPI
	V1    *storagepb.Transfer
	Alpha *storagev1alpha1pb.Transfer

	Status TransferStatus
	// NextRun is when the next iteration starts (ACTIVE transfers that are
	// waiting between iterations).
	NextRun time.Time
	// EmptyStreak counts consecutive empty iterations.
	EmptyStreak uint32
	// Watermark: source objects last modified before it are skipped (set
	// after a successful iteration; zero before the first one).
	Watermark time.Time
	// AlphaSourceKind is the provider of a v1alpha1 transfer's source
	// (azure_access_key credentials make it Azure). Set on the copies the
	// service returns, whose input-only credentials are removed, so views
	// need not guess it; zero for v1 transfers.
	AlphaSourceKind ProviderKind
}

// Metadata returns the resource metadata of either API version.
func (r *TransferRecord) Metadata() *commonpb.ResourceMetadata {
	if r.API == TransferAPIV1Alpha1 {
		return r.Alpha.GetMetadata()
	}
	return r.V1.GetMetadata()
}

// TransferPage is one page of a transfer listing.
type TransferPage struct {
	Items         []*TransferRecord
	NextPageToken string
}

// IterationPage is one page of iteration history, newest first.
type IterationPage struct {
	Items         []Iteration
	NextPageToken string
}

// TransferRepository stores transfers and their iteration history. Names are
// unique per project across both API versions.
type TransferRepository interface {
	Get(ctx context.Context, id string) (*TransferRecord, error)
	GetByName(ctx context.Context, parentID, name string) (*TransferRecord, error)
	// List returns a project's transfers in creation order; a non-zero only
	// keeps the transfers created through that API version.
	List(ctx context.Context, parentID string, only TransferAPI, pageSize int, pageToken string) (TransferPage, error)
	// ListAll returns every transfer (for the runner).
	ListAll(ctx context.Context) ([]*TransferRecord, error)
	// Create fails with errs.AlreadyExists when the name is taken in the project.
	Create(ctx context.Context, r *TransferRecord) error
	// Update replaces the record; it fails with errs.NotFound when the
	// transfer was deleted meanwhile.
	Update(ctx context.Context, r *TransferRecord) error
	// Delete removes the transfer and its iteration history.
	Delete(ctx context.Context, id string) error
	// PutIteration inserts or replaces iteration it.Seq of a transfer.
	PutIteration(ctx context.Context, transferID string, it Iteration) error
	// History returns iterations newest first.
	History(ctx context.Context, transferID string, pageSize int, pageToken string) (IterationPage, error)
}

// ObjectMeta describes one object at a transfer endpoint.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string // without quotes
	LastModified time.Time
	// HTTP attributes and user metadata copied with the object.
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Metadata           map[string]string // x-amz-meta-*, lower-case keys
}

// Endpoint is one side of a transfer, opened with its credentials. Errors the
// remote answers are *EndpointError; a missing object is errs.NotFound.
type Endpoint interface {
	// List yields the current objects whose key starts with prefix, in key
	// order, paging TransferListPage at a time.
	List(ctx context.Context, prefix string) iter.Seq2[ObjectMeta, error]
	Head(ctx context.Context, key string) (ObjectMeta, error)
	// Get streams an object; the caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error)
	// Put writes an object of exactly meta.Size bytes with meta's attributes
	// (multipart from TransferMultipartThreshold on) and returns its ETag.
	Put(ctx context.Context, key string, body io.Reader, meta ObjectMeta) (string, error)
	// ReplaceMetadata rewrites an object's attributes in place (CopyObject
	// with MetadataDirective REPLACE) and returns its ETag.
	ReplaceMetadata(ctx context.Context, key string, meta ObjectMeta) (string, error)
	Delete(ctx context.Context, key string) error
}

// EndpointError is an error answered by a transfer endpoint.
type EndpointError struct {
	Code    string
	Message string
}

func (e *EndpointError) Error() string { return e.Code + ": " + e.Message }

// Endpoints opens transfer endpoints (backend/s3endpoint, backend/azblob).
type Endpoints interface {
	Open(ctx context.Context, side Side) (Endpoint, error)
}

// ManagedObjects remembers which destination objects a transfer wrote, so
// that touch_unmanaged=false leaves every other object alone. An object is
// managed while its ETag is the one the transfer wrote.
type ManagedObjects interface {
	Managed(ctx context.Context, destination, key, etag string) (bool, error)
	MarkManaged(ctx context.Context, destination, key, etag string) error
	Forget(ctx context.Context, destination, key string) error
}
