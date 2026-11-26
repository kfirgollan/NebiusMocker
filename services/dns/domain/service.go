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

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Listing page sizes (zone_service.proto, record_service.proto: default 100).
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
	Zones     ZoneRepository
	Records   RecordRepository
	Networks  Networks
	Hierarchy Hierarchy
	Quota     QuotaGate
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
}

// Service implements the DNS zone and record services. mu serializes every
// DNS mutation, so the per-zone checks (duplicates, limits, "zone has
// records") are atomic with the writes they guard.
type Service struct {
	mu        sync.Mutex
	zones     ZoneRepository
	records   RecordRepository
	networks  Networks
	hierarchy Hierarchy
	gate      QuotaGate
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
	cfg       Config
}

// New wires the DNS domain.
func New(d Deps) (*Service, error) {
	if d.Zones == nil || d.Records == nil || d.Networks == nil || d.Hierarchy == nil || d.Quota == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("dns: service dependencies must not be nil")
	}
	return &Service{
		zones: d.Zones, records: d.Records, networks: d.Networks, hierarchy: d.Hierarchy, gate: d.Quota,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, cfg: d.Config,
	}, nil
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

// checkParent requires an existing project as a zone's parent.
func (s *Service) checkParent(ctx context.Context, parentID string) error {
	const field = "metadata.parent_id"
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
		return fmt.Errorf("zone parent %s: %w", parentID, err)
	}
	return nil
}

// metadataOwner is the common shape of Zone and Record.
type metadataOwner interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// checkVersion enforces an expected resource_version (0 means "any"). Per
// the DNS protos a mismatch is FAILED_PRECONDITION.
func checkVersion(r metadataOwner, want int64) error {
	md := r.GetMetadata()
	if want != 0 && want != md.GetResourceVersion() {
		return errs.BadState(md.GetId(), fmt.Sprintf("resource_version %d does not match the current version %d", want, md.GetResourceVersion()))
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
	now := timestamppb.New(s.clk.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: in.GetParentId(), Name: in.GetName(), Labels: in.GetLabels(),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// checkUpdatable enforces an immutable parent and the expected version.
func checkUpdatable(stored metadataOwner, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetMetadata().GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	return checkVersion(stored, md.GetResourceVersion())
}

// patch applies the mutable fields in patchMsg to a copy of stored using the
// X-ResetMask, then restores the server-owned metadata. The caller restores
// immutable spec fields and the status.
func patch[T metadataOwner](stored, patchMsg T, resetMask *mask.Mask) (T, error) {
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patchMsg, resetMask); err != nil {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, umd := stored.GetMetadata(), updated.GetMetadata()
	if umd == nil {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
	}
	umd.Id, umd.ParentId = md.GetId(), md.GetParentId()
	umd.ResourceVersion = md.GetResourceVersion()
	umd.CreatedAt, umd.UpdatedAt = md.GetCreatedAt(), md.GetUpdatedAt()
	return updated, nil
}

// bump marks a changed resource: next version, new updated_at.
func (s *Service) bump(md *commonpb.ResourceMetadata) {
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(s.clk.Now())
}

func immutable(field string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: "field is immutable"})
}
