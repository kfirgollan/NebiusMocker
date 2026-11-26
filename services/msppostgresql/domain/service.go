package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Listing page sizes (default 100, max 1000).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// MSP holds the version, preset and disk catalogs.
	MSP config.MSPConfig
	// Detached runs operation Work on its own goroutine (backends whose
	// calls block for long, such as docker); the mock backend runs inline.
	Detached bool
}

// Deps are the dependencies of a Service. All but Log are required.
type Deps struct {
	Clusters  ClusterRepository
	Backups   BackupRepository
	Bindings  NetworkBindingRepository
	Backend   Backend
	Peers     Peers
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
	// Log defaults to a discarding logger.
	Log *slog.Logger
}

// Service implements the cluster and backup services.
//
// mu serializes the short state checks and writes of operation Prepare
// steps. Backend calls run in operation Work, outside mu, one at a time per
// cluster (clusterLock).
type Service struct {
	mu  sync.Mutex
	d   Deps
	log *slog.Logger

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // cluster ID → its Work lock
}

// New wires the Managed PostgreSQL domain.
func New(d Deps) (*Service, error) {
	if d.Clusters == nil || d.Backups == nil || d.Bindings == nil || d.Backend == nil || d.Peers == nil || d.Hierarchy == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("msppostgresql: service dependencies must not be nil")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d, log: log, locks: map[string]*sync.Mutex{}}, nil
}

// clusterLock serializes the backend work of one cluster.
func (s *Service) clusterLock(clusterID string) func() {
	s.locksMu.Lock()
	l, ok := s.locks[clusterID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[clusterID] = l
	}
	s.locksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// forgetLock drops the lock of a deleted cluster.
func (s *Service) forgetLock(clusterID string) {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	delete(s.locks, clusterID)
}

func pageSize(n int64) (int, error) {
	if n < 0 || n > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if n == 0 {
		return defaultPageSize, nil
	}
	return int(n), nil
}

// notEnabled is the error for a capability whose module is disabled.
func notEnabled(field, area string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
		"Managed PostgreSQL needs the %s service, which is not enabled in this NebiusMocker (NEBIUS_MOCKER_SERVICES)", area)})
}

// checkProject requires an existing project as a cluster's parent.
func (s *Service) checkProject(ctx context.Context, parentID string) error {
	const field = "metadata.parent_id"
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	if parentID == s.d.Config.ProjectID {
		return nil
	}
	if _, err := s.d.Hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("cluster parent %s: %w", parentID, err)
	}
	return nil
}

// newMetadata builds the metadata of a resource being created.
func (s *Service) newMetadata(id string, in *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if in.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if in.GetResourceVersion() != 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: in.GetParentId(), Name: in.GetName(), Labels: in.GetLabels(),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// checkUpdatable enforces an immutable parent and the expected version.
func checkUpdatable(stored *pgpb.Cluster, md *commonpb.ResourceMetadata) error {
	smd := stored.GetMetadata()
	if p := md.GetParentId(); p != "" && p != smd.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
		return errs.Conflict(smd.GetId(), fmt.Sprintf(
			"resource_version %d does not match the current version %d", v, smd.GetResourceVersion()))
	}
	return nil
}

// patch applies the X-ResetMask update patchMsg to a copy of stored, then
// restores the server-owned metadata and the status.
func patch(stored, patchMsg *pgpb.Cluster, resetMask *mask.Mask) (*pgpb.Cluster, error) {
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patchMsg, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, umd := stored.GetMetadata(), updated.GetMetadata()
	if umd == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
	}
	umd.Id, umd.ParentId = md.GetId(), md.GetParentId()
	umd.ResourceVersion = md.GetResourceVersion()
	umd.CreatedAt, umd.UpdatedAt = md.GetCreatedAt(), md.GetUpdatedAt()
	if umd.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}

// bump marks a changed resource: next version, new updated_at.
func (s *Service) bump(md *commonpb.ResourceMetadata) {
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(s.d.Clock.Now())
}
