package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// attachField is the instance field that names an attached filesystem.
const attachField = "spec.filesystems.existing_filesystem.id"

// FilesystemAttach implements FilesystemAttachments over a FilesystemRepository.
type FilesystemAttach struct{ filesystems FilesystemRepository }

var _ FilesystemAttachments = (*FilesystemAttach)(nil)

// NewFilesystemAttachments returns a FilesystemAttach over filesystems.
func NewFilesystemAttachments(filesystems FilesystemRepository) (*FilesystemAttach, error) {
	if filesystems == nil {
		return nil, errors.New("compute: filesystem attachments need a filesystem repository")
	}
	return &FilesystemAttach{filesystems: filesystems}, nil
}

// Attach implements FilesystemAttachments. The checks and the write happen
// in one store transaction, so an Attach and a filesystem Delete never both
// succeed.
func (a *FilesystemAttach) Attach(ctx context.Context, fsID, instanceID, projectID string, mode computepb.AttachedFilesystemSpec_AttachMode) error {
	if fsID == "" {
		return errs.BadRequest(errs.Violation{Field: attachField, Message: "required"})
	}
	if instanceID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "the instance ID is required to attach a filesystem"})
	}
	if mode != computepb.AttachedFilesystemSpec_READ_WRITE {
		return errs.BadRequest(errs.Violation{
			Field: "spec.filesystems.attach_mode", Message: fmt.Sprintf("the only supported attach mode for filesystems is READ_WRITE, got %s", mode),
		})
	}
	_, err := a.filesystems.Mutate(ctx, fsID, func(fs *computepb.Filesystem) error {
		if fs.GetMetadata().GetParentId() != projectID {
			return errs.BadRequest(errs.Violation{
				Field:   attachField,
				Message: fmt.Sprintf("filesystem %s is not in project %s: filesystems attach only to instances of their project", fsID, projectID),
			})
		}
		if st := fs.GetStatus().GetState(); st != computepb.FilesystemStatus_READY {
			return errs.BadState(fsID, fmt.Sprintf("filesystem %s is %s, not READY", fsID, st))
		}
		if !slices.Contains(fs.GetStatus().GetReadWriteAttachments(), instanceID) {
			fs.Status.ReadWriteAttachments = append(fs.Status.ReadWriteAttachments, instanceID)
		}
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return &errs.Error{Kind: errs.KindNotFound, ResourceID: fsID, Message: fmt.Sprintf("%s: filesystem %s not found", attachField, fsID)}
	}
	return err
}

// Detach implements FilesystemAttachments.
func (a *FilesystemAttach) Detach(ctx context.Context, fsID, instanceID string) error {
	_, err := a.filesystems.Mutate(ctx, fsID, func(fs *computepb.Filesystem) error {
		st := fs.GetStatus()
		if st == nil {
			return nil
		}
		st.ReadWriteAttachments = slices.DeleteFunc(st.GetReadWriteAttachments(), func(x string) bool { return x == instanceID })
		st.ReadOnlyAttachments = slices.DeleteFunc(st.GetReadOnlyAttachments(), func(x string) bool { return x == instanceID })
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

// CheckFilesystemDeletable reports why a filesystem cannot be deleted, or nil.
func CheckFilesystemDeletable(fs *computepb.Filesystem) error {
	id, st := fs.GetMetadata().GetId(), fs.GetStatus()
	switch {
	case fs.GetSpec().GetForbidDeletion():
		return errs.BadState(id, "filesystem cannot be deleted because forbid_deletion is set")
	case len(st.GetReadWriteAttachments()) > 0 || len(st.GetReadOnlyAttachments()) > 0:
		return errs.BadState(id, fmt.Sprintf("filesystem is attached to instances %v: detach it before deleting",
			slices.Concat(st.GetReadWriteAttachments(), st.GetReadOnlyAttachments())))
	}
	return nil
}
