package domain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Limits bound what a secret may hold. Zero fields take the defaults.
type Limits struct {
	// MaxEntriesPerVersion is the number of payload entries one version may hold.
	MaxEntriesPerVersion int
	// MaxValueBytes is the size of one string or binary value.
	MaxValueBytes int
	// MaxKeyBytes is the length of one payload key.
	MaxKeyBytes int
	// MaxVersionsPerSecret counts every version not yet purged, including
	// those scheduled for deletion.
	MaxVersionsPerSecret int
	// PurgeDelay is how long a soft-deleted secret or version can be restored.
	PurgeDelay time.Duration
}

// Default limits. The value size matches the API's validation (64 KiB); the
// purge delay matches the documented 30 days. Upstream does not publish the
// other limits, so these are generous, configurable assumptions.
const (
	DefaultMaxEntriesPerVersion = 100
	DefaultMaxValueBytes        = 65536
	DefaultMaxKeyBytes          = 256
	DefaultMaxVersionsPerSecret = 100
	DefaultPurgeDelay           = 30 * 24 * time.Hour
)

func (l Limits) withDefaults() Limits {
	if l.MaxEntriesPerVersion <= 0 {
		l.MaxEntriesPerVersion = DefaultMaxEntriesPerVersion
	}
	if l.MaxValueBytes <= 0 {
		l.MaxValueBytes = DefaultMaxValueBytes
	}
	if l.MaxKeyBytes <= 0 {
		l.MaxKeyBytes = DefaultMaxKeyBytes
	}
	if l.MaxVersionsPerSecret <= 0 {
		l.MaxVersionsPerSecret = DefaultMaxVersionsPerSecret
	}
	if l.PurgeDelay <= 0 {
		l.PurgeDelay = DefaultPurgeDelay
	}
	return l
}

// Deps are the dependencies of a Service. All but Limits are required.
type Deps struct {
	Secrets   SecretRepository
	Versions  VersionRepository
	Payloads  PayloadRepository
	Sealer    PayloadSealer
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Quota     QuotaGate
	Limits    Limits
}

// Service implements secrets, versions and payload reads.
//
// Writes that span a secret and its versions (version limits, primary
// switching, deletion) are serialized by mu: the operation engine locks per
// resource ID, and a new version has an ID of its own.
type Service struct {
	secrets   SecretRepository
	versions  VersionRepository
	payloads  PayloadRepository
	sealer    PayloadSealer
	hierarchy Hierarchy
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
	quota     QuotaGate
	limits    Limits

	mu sync.Mutex
}

// NewService wires a Service.
func NewService(d Deps) (*Service, error) {
	if d.Secrets == nil || d.Versions == nil || d.Payloads == nil || d.Sealer == nil ||
		d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Quota == nil {
		return nil, errors.New("mysterybox: service dependencies must not be nil")
	}
	return &Service{
		secrets: d.Secrets, versions: d.Versions, payloads: d.Payloads, sealer: d.Sealer,
		hierarchy: d.Hierarchy, ops: d.Ops, ids: d.IDs, clk: d.Clock, quota: d.Quota, limits: d.Limits.withDefaults(),
	}, nil
}

// Limits returns the effective limits.
func (s *Service) Limits() Limits { return s.limits }

func pageSize(n int64) (int, error) {
	if n < 0 || n > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	if n == 0 {
		return defaultPageSize, nil
	}
	return int(n), nil
}

// softDeleted is the shared view of SecretStatus and SecretVersionStatus.
type softDeleted interface {
	GetPurgeAt() *timestamppb.Timestamp
}

// purgeDue reports whether a soft-deleted resource has passed its purge time.
func (s *Service) purgeDue(st softDeleted) bool {
	p := st.GetPurgeAt()
	return p != nil && !s.clk.Now().Before(p.AsTime())
}

// lookup selects which resources a read sees.
type lookup int

const (
	// activeOnly hides resources scheduled for deletion.
	activeOnly lookup = iota
	// withDeleted also returns resources scheduled for deletion.
	withDeleted
)

// visibility maps a show_scheduled_for_deletion flag to a lookup.
var visibility = map[bool]lookup{false: activeOnly, true: withDeleted}

// secret returns a secret that is not purged. With activeOnly, a
// secret scheduled for deletion is reported as not found. A secret past its
// purge time is removed (with its versions) and reported as not found.
func (s *Service) secret(ctx context.Context, id string, vis lookup) (*mysteryboxpb.Secret, error) {
	sec, err := s.secrets.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.purgeDue(sec.GetStatus()) {
		if err := s.purgeSecret(ctx, id); err != nil {
			return nil, err
		}
		return nil, errs.NotFound(id)
	}
	if vis == activeOnly && secretDeleted(sec) {
		return nil, errs.NotFound(id)
	}
	return sec, nil
}

// version is secret for versions.
func (s *Service) version(ctx context.Context, id string, vis lookup) (*mysteryboxpb.SecretVersion, error) {
	v, err := s.versions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.purgeDue(v.GetStatus()) {
		if err := s.purgeVersion(ctx, id); err != nil {
			return nil, err
		}
		return nil, errs.NotFound(id)
	}
	if vis == activeOnly && versionDeleted(v) {
		return nil, errs.NotFound(id)
	}
	return v, nil
}

func secretDeleted(sec *mysteryboxpb.Secret) bool {
	return sec.GetStatus().GetState() == mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION
}

func versionDeleted(v *mysteryboxpb.SecretVersion) bool {
	return v.GetStatus().GetState() == mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION
}

// purgeSecret removes a secret, its versions and their payloads for good.
func (s *Service) purgeSecret(ctx context.Context, id string) error {
	vs, err := s.versions.All(ctx, id)
	if err != nil {
		return fmt.Errorf("purge secret %s: %w", id, err)
	}
	for _, v := range vs {
		if err := s.purgeVersion(ctx, v.GetMetadata().GetId()); err != nil {
			return err
		}
	}
	if err := ignoreNotFound(s.payloads.Delete(ctx, id)); err != nil { // the inline version record
		return fmt.Errorf("purge secret %s: %w", id, err)
	}
	return ignoreNotFound(s.secrets.Delete(ctx, id))
}

// purgeVersion removes a version and its payload for good.
func (s *Service) purgeVersion(ctx context.Context, id string) error {
	if err := ignoreNotFound(s.payloads.Delete(ctx, id)); err != nil {
		return fmt.Errorf("purge payload of %s: %w", id, err)
	}
	return ignoreNotFound(s.versions.Delete(ctx, id))
}

// ignoreNotFound treats an already-removed resource as removed: lazy purges
// may race with one another.
func ignoreNotFound(err error) error {
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}
