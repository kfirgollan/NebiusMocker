package domain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Listing page sizes (tunnel_service.proto leaves them to the service).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Tunnels   TunnelRepository
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
}

// Service implements the tunnel service. mu serializes every mutation, so
// the read-check-write sequences are atomic.
type Service struct {
	mu        sync.Mutex
	tunnels   TunnelRepository
	hierarchy Hierarchy
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
	cfg       Config
}

// New wires the Tunnels domain.
func New(d Deps) (*Service, error) {
	if d.Tunnels == nil || d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("tunnel: service dependencies must not be nil")
	}
	return &Service{tunnels: d.Tunnels, hierarchy: d.Hierarchy, ops: d.Ops, ids: d.IDs, clk: d.Clock, cfg: d.Config}, nil
}

// status is what every tunnel reports: it exists, and no agent is connected
// (the mock has no edge an agent could dial), so it exposes no services.
func status() *tunnelpb.TunnelStatus {
	return &tunnelpb.TunnelStatus{
		State:           tunnelpb.TunnelStatus_CREATED,
		ConnectionState: tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED,
	}
}

// CreateCmd is the input of Create.
type CreateCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *tunnelpb.TunnelSpec
	Request  proto.Message
}

// UpdateCmd is the input of Update: a full replace of name, labels and spec,
// where ResetMask (X-ResetMask) names the fields to clear.
type UpdateCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *tunnelpb.TunnelSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// ListCmd is the input of List.
type ListCmd struct {
	ParentID  string
	PageSize  int64
	PageToken string
	Filter    string
}

// Get returns a tunnel.
func (s *Service) Get(ctx context.Context, id string) (*tunnelpb.Tunnel, error) {
	return s.tunnels.Get(ctx, id)
}

// List returns one page of the project's tunnels, in creation order.
func (s *Service) List(ctx context.Context, cmd ListCmd) (Page[*tunnelpb.Tunnel], error) {
	var zero Page[*tunnelpb.Tunnel]
	if cmd.Filter != "" {
		return zero, errs.BadRequest(errs.Violation{Field: "filter", Message: "filtering is not implemented in NebiusMocker yet"})
	}
	size, err := pageSize(cmd.PageSize)
	if err != nil {
		return zero, err
	}
	if err := s.checkParent(ctx, cmd.ParentID, "parent_id"); err != nil {
		return zero, err
	}
	all, err := s.tunnels.List(ctx, cmd.ParentID)
	if err != nil {
		return zero, err
	}
	return paginate(all, size, cmd.PageToken)
}

// Create stores a new tunnel.
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (*operations.Operation, error) {
	id, err := s.ids.New(KindTunnel)
	if err != nil {
		return nil, fmt.Errorf("new tunnel id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create tunnel",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.create(ctx, id, cmd)
		},
	})
}

func (s *Service) create(ctx context.Context, id string, cmd CreateCmd) error {
	md := cmd.Metadata
	if err := s.checkParent(ctx, md.GetParentId(), "metadata.parent_id"); err != nil {
		return err
	}
	if md.GetId() != "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if md.GetResourceVersion() != 0 {
		return errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &tunnelpb.TunnelSpec{}
	}
	now := timestamppb.New(s.clk.Now())
	return s.tunnels.Create(ctx, &tunnelpb.Tunnel{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: md.GetParentId(), Name: md.GetName(), Labels: md.GetLabels(),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   spec,
		Status: status(),
	})
}

// Update changes a tunnel's name, labels, title and description.
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update tunnel",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.update(ctx, cmd)
		},
	})
}

func (s *Service) update(ctx context.Context, cmd UpdateCmd) error {
	stored, err := s.tunnels.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	smd := stored.GetMetadata()
	if p := cmd.Metadata.GetParentId(); p != "" && p != smd.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := cmd.Metadata.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
		return errs.Conflict(smd.GetId(), fmt.Sprintf("resource_version %d does not match current version %d", v, smd.GetResourceVersion()))
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	patch := &tunnelpb.Tunnel{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		return errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	umd := updated.GetMetadata()
	if umd == nil {
		return errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
	}
	// Server-owned fields survive whatever the patch or reset mask said.
	umd.Id, umd.ParentId = smd.GetId(), smd.GetParentId()
	umd.ResourceVersion, umd.CreatedAt, umd.UpdatedAt = smd.GetResourceVersion(), smd.GetCreatedAt(), smd.GetUpdatedAt()
	if updated.GetSpec() == nil {
		updated.Spec = &tunnelpb.TunnelSpec{}
	}
	updated.Status = status()
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	umd.ResourceVersion++
	umd.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.tunnels.Update(ctx, updated)
}

// Delete removes a tunnel.
func (s *Service) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete tunnel",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.tunnels.Delete(ctx, id)
		},
	})
}

// checkParent requires an existing project as a tunnel's parent.
func (s *Service) checkParent(ctx context.Context, parentID, field string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	if parentID == s.cfg.ProjectID {
		return nil
	}
	if _, err := s.hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("%s %s: %w", field, parentID, err)
	}
	return nil
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

// paginate returns one page of items after the offset in pageToken.
func paginate[T any](items []T, size int, pageToken string) (Page[T], error) {
	offset := 0
	if pageToken != "" {
		b, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, perr := strconv.Atoi(string(b))
		if err != nil || perr != nil || n < 0 {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
		}
		offset = n
	}
	if offset >= len(items) {
		return Page[T]{Items: []T{}}, nil
	}
	end := min(offset+size, len(items))
	page := Page[T]{Items: items[offset:end]}
	if end < len(items) {
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return page, nil
}
