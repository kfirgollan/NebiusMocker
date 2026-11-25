package domain

import (
	"context"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// KindInstance is the NID type prefix of compute instances.
const KindInstance = "computeinstance"

// kindServiceAccount is the IAM service account NID prefix (spec.service_account_id).
const kindServiceAccount = "serviceaccount"

// InstancePage is one page of an instance listing.
type InstancePage struct {
	Items         []*computepb.Instance
	NextPageToken string
}

// InstanceRepository stores instances. Changes that touch an instance and
// the disks it attaches run in one transaction (Txn), so attachment
// bookkeeping cannot race with a concurrent disk delete.
type InstanceRepository interface {
	Get(ctx context.Context, id string) (*computepb.Instance, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.Instance, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (InstancePage, error)
	// Txn runs fn in one serializable transaction. An error from fn
	// discards every write made through tx.
	Txn(ctx context.Context, fn func(tx InstanceTx) error) error
	// StoppedBefore returns the IDs of the instances with a stop time (see
	// InstanceTx.SetStoppedAt) before t.
	StoppedBefore(ctx context.Context, t time.Time) ([]string, error)
}

// InstanceTx reads and writes instances and disks inside one transaction.
// Reads return errs.NotFound for missing records.
type InstanceTx interface {
	Instance(id string) (*computepb.Instance, error)
	// CreateInstance fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	CreateInstance(inst *computepb.Instance) error
	// PutInstance replaces an existing instance.
	PutInstance(inst *computepb.Instance) error
	DeleteInstance(id string) error

	// StoppedAt returns when a STOPPED instance that still holds a dynamic
	// public address stopped (ok = false when no stop time is recorded).
	StoppedAt(id string) (at time.Time, ok bool, err error)
	// SetStoppedAt records the stop time of an instance.
	SetStoppedAt(id string, at time.Time) error
	// ClearStoppedAt removes the stop time of an instance, if any.
	ClearStoppedAt(id string) error

	Disk(id string) (*computepb.Disk, error)
	// CreateDisk fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	CreateDisk(d *computepb.Disk) error
	// PutDisk replaces an existing disk.
	PutDisk(d *computepb.Disk) error
	DeleteDisk(id string) error
}

// PlatformResolver resolves an instance's platform and preset in its
// project's region (satisfied by *PlatformService).
type PlatformResolver interface {
	Resolve(ctx context.Context, projectID, platform, preset string) (PlatformPreset, error)
}

// Networking finds the VPC capability that gives NICs their addresses, at
// call time (the vpc module may be disabled).
type Networking interface {
	NetworkInterfaces() (peer.NetworkInterfaces, bool)
}

// CallerPermissions checks the caller's own permission on a resource, under
// the configured authorization mode (BatchGet authorizes each ID).
type CallerPermissions interface {
	// Require returns nil when the caller holds perm on resourceID, an
	// errs.PermissionDenied/Unauthenticated error when not, or the lookup
	// error (e.g. errs.NotFound).
	Require(ctx context.Context, perm, resourceID string) error
}
