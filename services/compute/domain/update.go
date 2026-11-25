package domain

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// checkUpdatable enforces immutable parent_id and optimistic concurrency.
func checkUpdatable(stored *computepb.Image, md *commonpb.ResourceMetadata) error {
	if p := md.GetParentId(); p != "" && p != stored.GetMetadata().GetParentId() {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "field is immutable"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != stored.GetMetadata().GetResourceVersion() {
		return errs.Conflict(stored.GetMetadata().GetId(), fmt.Sprintf(
			"resource_version %d does not match current version %d", v, stored.GetMetadata().GetResourceVersion(),
		))
	}
	return nil
}

// patchImage applies the request to a copy of stored using the reset mask,
// rejects changes to IMMUTABLE spec fields and restores server-owned fields.
//
// An immutable field that the patch leaves empty keeps its stored value, so
// clients that resend the full spec (or reset everything they omit) are
// accepted as long as they do not try to change it.
func patchImage(stored *computepb.Image, cmd UpdateImageCmd) (*computepb.Image, error) {
	patch := &computepb.Image{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	if updated.GetMetadata() == nil {
		updated.Metadata = &commonpb.ResourceMetadata{}
	}
	md := stored.GetMetadata()
	updated.Metadata.Id = md.GetId()
	updated.Metadata.ParentId = md.GetParentId()
	updated.Metadata.ResourceVersion = md.GetResourceVersion()
	updated.Metadata.CreatedAt = md.GetCreatedAt()
	updated.Metadata.UpdatedAt = md.GetUpdatedAt()
	if updated.GetSpec() == nil {
		updated.Spec = &computepb.ImageSpec{}
	}
	if err := keepImmutable(stored.GetSpec(), updated.GetSpec()); err != nil {
		return nil, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}

// keepImmutable restores empty immutable fields of next from prev and fails if
// one was changed to a different non-empty value.
func keepImmutable(prev, next *computepb.ImageSpec) error {
	var v []errs.Violation
	str := func(field, p string, n *string) {
		switch {
		case *n == "":
			*n = p
		case *n != p:
			v = append(v, errs.Violation{Field: "spec." + field, Message: "field is immutable"})
		}
	}
	str("image_family", prev.GetImageFamily(), &next.ImageFamily)
	str("version", prev.GetVersion(), &next.Version)
	str("image_family_human_readable", prev.GetImageFamilyHumanReadable(), &next.ImageFamilyHumanReadable)
	switch a := next.GetCpuArchitecture(); {
	case a == computepb.ImageSpec_UNSPECIFIED:
		next.CpuArchitecture = prev.GetCpuArchitecture()
	case a != prev.GetCpuArchitecture():
		v = append(v, errs.Violation{Field: "spec.cpu_architecture", Message: "field is immutable"})
	}
	switch {
	case next.GetSource() == nil:
		next.Source = prev.GetSource()
	case !sameSource(prev, next):
		v = append(v, errs.Violation{Field: "spec.source", Message: "field is immutable"})
	}
	if len(v) > 0 {
		return errs.BadRequest(v...)
	}
	return nil
}

func sameSource(a, b *computepb.ImageSpec) bool {
	return proto.Equal(
		&computepb.ImageSpec{Source: a.GetSource()},
		&computepb.ImageSpec{Source: b.GetSource()},
	)
}
