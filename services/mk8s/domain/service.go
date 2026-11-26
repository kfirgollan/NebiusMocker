package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Listing page sizes (default 100, like the other areas).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// Versions is the control-plane version catalog, oldest first.
	Versions []string
	// Detached runs operation Work on its own goroutine (backends whose
	// calls block for long, such as k3s); the mock backend runs inline.
	Detached bool
	// RolloutStep is the pause between the batches of a node rollout, on
	// the injected clock (0: none).
	RolloutStep time.Duration
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Clusters   ClusterRepository
	NodeGroups NodeGroupRepository
	Bindings   NetworkBindingRepository
	Backend    Backend
	Peers      Peers
	Hierarchy  Hierarchy
	Quota      QuotaGate
	Ops        Operations
	IDs        IDGenerator
	Clock      clock.Clock
	Config     Config
}

// Service implements the mk8s cluster and node group services.
//
// mu serializes the short state checks and writes of operation Prepare
// steps (a node group may only be created in a RUNNING cluster, which
// cluster Delete flips to DELETING). Backend and compute calls run in
// operation Work, outside mu, one at a time per cluster (clusterLock).
type Service struct {
	mu       sync.Mutex
	d        Deps
	versions catalog

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // cluster ID → its Work lock
}

// New wires the mk8s domain.
func New(d Deps) (*Service, error) {
	if d.Clusters == nil || d.NodeGroups == nil || d.Bindings == nil || d.Backend == nil || d.Peers == nil || d.Hierarchy == nil || d.Quota == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("mk8s: service dependencies must not be nil")
	}
	cat, err := newCatalog(d.Config.Versions)
	if err != nil {
		return nil, err
	}
	return &Service{d: d, versions: cat, locks: map[string]*sync.Mutex{}}, nil
}

// clusterLock serializes the backend and node work of one cluster.
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
		"Managed Kubernetes needs the %s service, which is not enabled in this NebiusMocker (NEBIUS_MOCKER_SERVICES)", area)})
}

func (s *Service) computeNodes(field string) (peer.ComputeNodes, error) {
	if cn, ok := s.d.Peers.ComputeNodes(); ok {
		return cn, nil
	}
	return nil, notEnabled(field, "compute")
}

func (s *Service) nics(field string) (peer.NetworkInterfaces, error) {
	if n, ok := s.d.Peers.NetworkInterfaces(); ok {
		return n, nil
	}
	return nil, notEnabled(field, "vpc")
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
	if err := checkNewMetadata(in); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: in.GetParentId(), Name: in.GetName(), Labels: in.GetLabels(),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func checkNewMetadata(in *commonpb.ResourceMetadata) error {
	if in.GetId() != "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if in.GetResourceVersion() != 0 {
		return errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	return nil
}

// metadataOwner is the common shape of Cluster and NodeGroup.
type metadataOwner interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// checkUpdatable enforces an immutable parent and the expected version.
func checkUpdatable(stored metadataOwner, md *commonpb.ResourceMetadata) error {
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
func patch[T metadataOwner](stored, patchMsg T, resetMask *mask.Mask) (T, error) {
	var zero T
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patchMsg, resetMask); err != nil {
		return zero, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, umd := stored.GetMetadata(), updated.GetMetadata()
	if umd == nil {
		return zero, errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
	}
	umd.Id, umd.ParentId = md.GetId(), md.GetParentId()
	umd.ResourceVersion = md.GetResourceVersion()
	umd.CreatedAt, umd.UpdatedAt = md.GetCreatedAt(), md.GetUpdatedAt()
	if umd.GetName() == "" {
		return zero, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	return updated, nil
}

// bump marks a changed resource: next version, new updated_at.
func (s *Service) bump(md *commonpb.ResourceMetadata) {
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(s.d.Clock.Now())
}

// event is a status event that happened now.
func (s *Service) event(level commonpb.ResourceEvent_Level, code, msg string) *commonpb.RecurrentResourceEvent {
	now := timestamppb.New(s.d.Clock.Now())
	return &commonpb.RecurrentResourceEvent{
		FirstOccurredAt: now,
		LastOccurrence:  &commonpb.ResourceEvent{OccurredAt: now, Level: level, Code: code, Message: msg},
		OccurrenceCount: 1,
	}
}

// prefixFields returns err with every field violation prefixed, e.g.
// "resources.preset" → "spec.template.resources.preset".
func prefixFields(err error, prefix string) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	out := *e
	out.Violations = make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		out.Violations[i] = errs.Violation{Field: prefix + v.Field, Message: v.Message}
	}
	return &out
}
