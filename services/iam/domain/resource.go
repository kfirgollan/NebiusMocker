package domain

import (
	"fmt"
	"maps"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// resource is any Nebius resource message (metadata/spec/status).
type resource interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// requireNoID rejects a client-supplied metadata.id on Create.
func requireNoID(md *commonpb.ResourceMetadata) error {
	if md.GetId() != "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	return nil
}

// newMetadata builds the server-side metadata of a new resource.
func newMetadata(id, parentID string, md *commonpb.ResourceMetadata, now time.Time) *commonpb.ResourceMetadata {
	ts := timestamppb.New(now)
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
		ResourceVersion: 1, CreatedAt: ts, UpdatedAt: ts,
	}
}

// checkMetadataUpdate enforces an immutable parent_id and optimistic concurrency.
func checkMetadataUpdate(stored, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetResourceVersion() {
		return errs.Conflict(stored.GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, stored.GetResourceVersion()))
	}
	return nil
}

// patchResource applies patch onto a copy of stored with reset-mask semantics
// and restores the server-owned metadata. Callers restore spec/status fields
// the client may not change.
func patchResource[T resource](stored, patch T, resetMask *mask.Mask) (T, error) {
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		var zero T
		return zero, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, smd := updated.GetMetadata(), stored.GetMetadata()
	if md == nil {
		// PatchWithResetMask keeps metadata when masked away only if present;
		// re-clone so the server-owned fields below have somewhere to go.
		updated = proto.CloneOf(stored)
		md = updated.GetMetadata()
	}
	md.Id = smd.GetId()
	md.ParentId = smd.GetParentId()
	md.ResourceVersion = smd.GetResourceVersion()
	md.CreatedAt = smd.GetCreatedAt()
	md.UpdatedAt = smd.GetUpdatedAt()
	return updated, nil
}

// bumpVersion marks a changed resource: next resource_version, new updated_at.
func bumpVersion(md *commonpb.ResourceMetadata, now time.Time) {
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(now)
}
