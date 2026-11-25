package domain

import (
	"context"
	"fmt"
	"maps"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// CreateVersionCmd is the input of CreateVersion.
type CreateVersionCmd struct {
	// Metadata.parent_id is the secret.
	Metadata *commonpb.ResourceMetadata
	Spec     *mysteryboxpb.SecretVersionSpec
	// Request is echoed in Operation.request. It must already be redacted:
	// operations are readable by anyone who may poll them.
	Request proto.Message
}

// ListVersionsQuery selects a page of a secret's versions.
type ListVersionsQuery struct {
	SecretID    string
	PageSize    int64
	PageToken   string
	ShowDeleted bool
}

// GetVersion returns a version without its payload. Versions scheduled for
// deletion are hidden unless showDeleted is set.
func (s *Service) GetVersion(ctx context.Context, id string, showDeleted bool) (*mysteryboxpb.SecretVersion, error) {
	return s.version(ctx, id, visibility[showDeleted])
}

// ListVersions returns one page of a secret's versions, oldest first.
func (s *Service) ListVersions(ctx context.Context, q ListVersionsQuery) (VersionPage, error) {
	size, err := pageSize(q.PageSize)
	if err != nil {
		return VersionPage{}, err
	}
	if _, err := s.secret(ctx, q.SecretID, withDeleted); err != nil {
		return VersionPage{}, err
	}
	page, err := s.versions.List(ctx, q.SecretID, size, q.PageToken)
	if err != nil {
		return VersionPage{}, err
	}
	out := page.Items[:0]
	for _, v := range page.Items {
		if s.purgeDue(v.GetStatus()) {
			if err := s.purgeVersion(ctx, v.GetMetadata().GetId()); err != nil {
				return VersionPage{}, err
			}
			continue
		}
		if q.ShowDeleted || !versionDeleted(v) {
			out = append(out, v)
		}
	}
	page.Items = out
	return page, nil
}

// CreateVersion adds an immutable version to an active secret. With
// spec.set_primary, or when the secret has no primary yet, it becomes primary.
func (s *Service) CreateVersion(ctx context.Context, cmd CreateVersionCmd) (*operations.Operation, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	secretID := md.GetParentId()
	if secretID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if err := s.validatePayload("spec.payload", cmd.Spec.GetPayload()); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindSecretVersion)
	if err != nil {
		return nil, fmt.Errorf("new secret version id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create secret version",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			sec, err := s.secret(ctx, secretID, activeOnly)
			if err != nil {
				return fmt.Errorf("secret %s: %w", secretID, err)
			}
			if err := s.checkVersionLimit(ctx, secretID); err != nil {
				return err
			}
			if err := s.addVersion(ctx, id, md, cmd.Spec); err != nil {
				return err
			}
			if cmd.Spec.GetSetPrimary() || sec.GetSpec().GetPrimaryVersionId() == "" {
				return s.setPrimary(ctx, sec, id)
			}
			return nil
		},
	})
}

// DeleteVersion schedules a version for deletion. Any version may be deleted,
// the primary included; payload reads of a deleted primary then fail.
func (s *Service) DeleteVersion(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete secret version",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			v, err := s.version(ctx, id, activeOnly)
			if err != nil {
				return err
			}
			now := s.clk.Now()
			v.Status = &mysteryboxpb.SecretVersionStatus{
				State:     mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION,
				DeletedAt: timestamppb.New(now),
				PurgeAt:   timestamppb.New(now.Add(s.limits.PurgeDelay)),
			}
			return s.versions.Update(ctx, v)
		},
	})
}

// UndeleteVersion restores a version scheduled for deletion. Its secret must
// be active.
func (s *Service) UndeleteVersion(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Undelete secret version",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			v, err := s.version(ctx, id, withDeleted)
			if err != nil {
				return err
			}
			if !versionDeleted(v) {
				return errs.BadState(id, "the version is not scheduled for deletion")
			}
			if _, err := s.secret(ctx, v.GetMetadata().GetParentId(), activeOnly); err != nil {
				return fmt.Errorf("secret of version %s: %w", id, err)
			}
			v.Status = &mysteryboxpb.SecretVersionStatus{State: mysteryboxpb.SecretVersionStatus_ACTIVE}
			return s.versions.Update(ctx, v)
		},
	})
}

// checkVersionLimit fails when the secret already holds the maximum number of
// versions (purged ones no longer count).
func (s *Service) checkVersionLimit(ctx context.Context, secretID string) error {
	vs, err := s.versions.All(ctx, secretID)
	if err != nil {
		return fmt.Errorf("versions of %s: %w", secretID, err)
	}
	n := 0
	for _, v := range vs {
		if s.purgeDue(v.GetStatus()) {
			if err := s.purgeVersion(ctx, v.GetMetadata().GetId()); err != nil {
				return err
			}
			continue
		}
		n++
	}
	if n >= s.limits.MaxVersionsPerSecret {
		return &errs.Error{
			Kind: errs.KindQuotaExceeded, ResourceID: secretID,
			Message: fmt.Sprintf("a secret holds at most %d versions", s.limits.MaxVersionsPerSecret),
		}
	}
	return nil
}

// addVersion seals the payload and stores the version and its payload. The
// stored spec keeps only the description: payload and set_primary are input-only.
func (s *Service) addVersion(ctx context.Context, id string, md *commonpb.ResourceMetadata, spec *mysteryboxpb.SecretVersionSpec) error {
	sealed, err := s.sealPayload(ctx, id, spec.GetPayload())
	if err != nil {
		return err
	}
	now := timestamppb.New(s.clk.Now())
	v := &mysteryboxpb.SecretVersion{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        md.GetParentId(),
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec:   &mysteryboxpb.SecretVersionSpec{Description: spec.GetDescription()},
		Status: &mysteryboxpb.SecretVersionStatus{State: mysteryboxpb.SecretVersionStatus_ACTIVE},
	}
	if err := s.payloads.Put(ctx, id, md.GetParentId(), sealed); err != nil {
		return fmt.Errorf("store payload of %s: %w", id, err)
	}
	if err := s.versions.Create(ctx, v); err != nil {
		_ = s.payloads.Delete(ctx, id) // best-effort: the version error is what matters
		return err
	}
	return nil
}

// setPrimary makes versionID the secret's primary version.
func (s *Service) setPrimary(ctx context.Context, sec *mysteryboxpb.Secret, versionID string) error {
	if sec.GetSpec().GetPrimaryVersionId() == versionID {
		return nil
	}
	sec.Spec.PrimaryVersionId = proto.String(versionID)
	sec.Metadata.ResourceVersion++
	sec.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.secrets.Update(ctx, sec)
}
