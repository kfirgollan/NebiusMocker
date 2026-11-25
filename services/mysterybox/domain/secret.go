package domain

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// CreateSecretCmd is the input of CreateSecret.
type CreateSecretCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *mysteryboxpb.SecretSpec
	// Request is echoed in Operation.request. It must already be redacted.
	Request proto.Message
}

// UpdateSecretCmd is the input of UpdateSecret (full replace guarded by a
// reset mask).
type UpdateSecretCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *mysteryboxpb.SecretSpec
	// ResetMask lists fields to clear when absent from the request (X-ResetMask).
	ResetMask *mask.Mask
	Request   proto.Message
}

// ListSecretsQuery selects a page of a project's secrets.
type ListSecretsQuery struct {
	ParentID    string
	PageSize    int64
	PageToken   string
	ShowDeleted bool
}

// SystemSecretCmd is the input of CreateSystemSecret.
type SystemSecretCmd struct {
	ParentID    string
	Name        string
	Description string
	Labels      map[string]string
	Payload     []*mysteryboxpb.Payload
}

// GetSecret returns a secret (never its payload). Secrets scheduled for
// deletion are hidden unless showDeleted is set.
func (s *Service) GetSecret(ctx context.Context, id string, showDeleted bool) (*mysteryboxpb.Secret, error) {
	return s.secret(ctx, id, visibility[showDeleted])
}

// GetSecretByName returns the active secret with the name in a project.
func (s *Service) GetSecretByName(ctx context.Context, parentID, name string) (*mysteryboxpb.Secret, error) {
	sec, err := s.secrets.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.secret(ctx, sec.GetMetadata().GetId(), activeOnly)
}

// ListSecrets returns one page of a project's secrets in creation order.
func (s *Service) ListSecrets(ctx context.Context, q ListSecretsQuery) (SecretPage, error) {
	size, err := pageSize(q.PageSize)
	if err != nil {
		return SecretPage{}, err
	}
	page, err := s.secrets.List(ctx, q.ParentID, size, q.PageToken)
	if err != nil {
		return SecretPage{}, err
	}
	out := page.Items[:0]
	for _, sec := range page.Items {
		if s.purgeDue(sec.GetStatus()) {
			if err := s.purgeSecret(ctx, sec.GetMetadata().GetId()); err != nil {
				return SecretPage{}, err
			}
			continue
		}
		if q.ShowDeleted || !secretDeleted(sec) {
			out = append(out, sec)
		}
	}
	page.Items = out
	return page, nil
}

// CreateSecret creates a secret in a project. spec.secret_version, when set,
// becomes the secret's first (and primary) version.
func (s *Service) CreateSecret(ctx context.Context, cmd CreateSecretCmd) (*operations.Operation, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if cmd.Spec.GetPrimaryVersionId() != "" {
		return nil, errs.BadRequest(errs.Violation{
			Field: "spec.primary_version_id", Message: "must not be set on create; use spec.secret_version",
		})
	}
	first := cmd.Spec.GetSecretVersion()
	if first != nil {
		if err := s.validatePayload("spec.secret_version.payload", first.GetPayload()); err != nil {
			return nil, err
		}
	}
	if err := s.checkParent(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindSecret)
	if err != nil {
		return nil, fmt.Errorf("new secret id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create secret",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.createSecret(ctx, id, md, cmd.Spec.GetDescription(), first)
		},
	})
}

// CreateSystemSecret creates a secret with one primary version holding
// payload, synchronously and without an operation. It is the entry point for
// other parts of the mocker that store a secret on a user's behalf (e.g.
// IAM access keys delivered to MysteryBox). The caller decides authorization.
// The returned secret's spec.primary_version_id names the new version.
func (s *Service) CreateSystemSecret(ctx context.Context, cmd SystemSecretCmd) (*mysteryboxpb.Secret, error) {
	if err := s.validatePayload("payload", cmd.Payload); err != nil {
		return nil, err
	}
	if err := s.checkParent(ctx, cmd.ParentID); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindSecret)
	if err != nil {
		return nil, fmt.Errorf("new secret id: %w", err)
	}
	md := &commonpb.ResourceMetadata{ParentId: cmd.ParentID, Name: cmd.Name, Labels: cmd.Labels}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.createSecret(ctx, id, md, cmd.Description, &mysteryboxpb.SecretVersionSpec{Payload: cmd.Payload}); err != nil {
		return nil, err
	}
	return s.secret(ctx, id, activeOnly)
}

// DiscardSystemSecret removes a secret and all its versions and payloads for
// good, without a soft-delete period. It rolls back a CreateSystemSecret whose
// caller could not store its own resource; a missing secret is not an error.
func (s *Service) DiscardSystemSecret(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeSecret(ctx, id)
}

// UpdateSecret changes name, labels, description or the primary version.
// spec.secret_version is input-only and immutable: an unchanged copy (clients
// such as Terraform resend it on every update) is accepted, a changed one is
// rejected.
func (s *Service) UpdateSecret(ctx context.Context, cmd UpdateSecretCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update secret",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.applyUpdate(ctx, cmd)
		},
	})
}

// DeleteSecret schedules a secret for deletion. It keeps its versions (they
// come back on Undelete) and releases its name.
func (s *Service) DeleteSecret(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete secret",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			sec, err := s.secret(ctx, id, activeOnly)
			if err != nil {
				return err
			}
			now := s.clk.Now()
			sec.Status.State = mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION
			sec.Status.DeletedAt = timestamppb.New(now)
			sec.Status.PurgeAt = timestamppb.New(now.Add(s.limits.PurgeDelay))
			return s.secrets.Update(ctx, sec)
		},
	})
}

// UndeleteSecret restores a secret scheduled for deletion, optionally under a
// new name (its old one may have been taken meanwhile).
func (s *Service) UndeleteSecret(ctx context.Context, id, newName string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Undelete secret",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			sec, err := s.secret(ctx, id, withDeleted)
			if err != nil {
				return err
			}
			if !secretDeleted(sec) {
				return errs.BadState(id, "the secret is not scheduled for deletion")
			}
			if newName != "" {
				sec.Metadata.Name = newName
			}
			sec.Status.State = mysteryboxpb.SecretStatus_ACTIVE
			sec.Status.DeletedAt = nil
			sec.Status.PurgeAt = nil
			return s.secrets.Update(ctx, sec)
		},
	})
}

// checkParent requires an existing project as the parent.
func (s *Service) checkParent(ctx context.Context, parentID string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project"})
	}
	if _, err := s.hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("secret parent %s: %w", parentID, err)
	}
	return nil
}

// createSecret stores a new secret and, when first is set, its first version
// as primary. s.mu must be held.
func (s *Service) createSecret(ctx context.Context, id string, md *commonpb.ResourceMetadata, description string, first *mysteryboxpb.SecretVersionSpec) error {
	now := timestamppb.New(s.clk.Now())
	sec := &mysteryboxpb.Secret{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        md.GetParentId(),
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec: &mysteryboxpb.SecretSpec{Description: description},
		Status: &mysteryboxpb.SecretStatus{
			State:             mysteryboxpb.SecretStatus_ACTIVE,
			EffectiveKmsKeyId: s.sealer.KeyID(),
		},
	}
	if first == nil {
		return s.secrets.Create(ctx, sec)
	}
	versionID, err := s.ids.New(KindSecretVersion)
	if err != nil {
		return fmt.Errorf("new secret version id: %w", err)
	}
	sec.Spec.PrimaryVersionId = proto.String(versionID)
	if err := s.secrets.Create(ctx, sec); err != nil {
		return err
	}
	if err := s.addVersion(ctx, versionID, &commonpb.ResourceMetadata{ParentId: id}, first); err != nil {
		_ = s.secrets.Delete(ctx, id) // best-effort rollback; the version error is what matters
		return err
	}
	if err := s.recordInline(ctx, id, first); err != nil {
		_ = s.purgeSecret(ctx, id) // best-effort rollback; the record error is what matters
		return err
	}
	return nil
}

// inlineSpec is the immutable part of spec.secret_version that Update compares:
// set_primary is input-only and has no effect on the first version.
func inlineSpec(v *mysteryboxpb.SecretVersionSpec) *mysteryboxpb.SecretVersionSpec {
	return &mysteryboxpb.SecretVersionSpec{Description: v.GetDescription(), Payload: v.GetPayload()}
}

// recordInline keeps a sealed copy of the inline version spec under the
// secret's ID, so Update can enforce its immutability even after that version
// is purged. It is sealed like any payload.
func (s *Service) recordInline(ctx context.Context, secretID string, first *mysteryboxpb.SecretVersionSpec) error {
	plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(inlineSpec(first))
	if err != nil {
		return fmt.Errorf("encode inline version: %w", err)
	}
	sealed, err := s.sealer.Seal(ctx, plain, []byte(secretID))
	if err != nil {
		return fmt.Errorf("seal inline version: %w", err)
	}
	if err := s.payloads.Put(ctx, secretID, secretID, sealed); err != nil {
		return fmt.Errorf("store inline version of %s: %w", secretID, err)
	}
	return nil
}

// checkInline enforces that spec.secret_version is immutable. Clients such as
// Terraform resend it on every update: an identical copy is accepted, any
// difference (or one on a secret created without it) is rejected. An empty
// payload means the client omitted it (Terraform write-only payloads), so only
// the description is compared then.
func (s *Service) checkInline(ctx context.Context, secretID string, req *mysteryboxpb.SecretVersionSpec) error {
	if req == nil {
		return nil
	}
	immutable := errs.BadRequest(errs.Violation{Field: "spec.secret_version", Message: "field is immutable; set it only on create"})
	sealed, err := s.payloads.Get(ctx, secretID)
	if errs.IsKind(err, errs.KindNotFound) {
		return immutable
	}
	if err != nil {
		return fmt.Errorf("inline version of %s: %w", secretID, err)
	}
	plain, err := s.sealer.Open(ctx, sealed, []byte(secretID))
	if err != nil {
		return errs.Internal("cannot decrypt the inline version of " + secretID)
	}
	var stored mysteryboxpb.SecretVersionSpec
	if err := proto.Unmarshal(plain, &stored); err != nil {
		return errs.Internal("corrupt inline version of " + secretID)
	}
	want := inlineSpec(req)
	if len(want.GetPayload()) == 0 {
		want.Payload = stored.GetPayload()
	}
	if !proto.Equal(&stored, want) {
		return immutable
	}
	return nil
}

func (s *Service) applyUpdate(ctx context.Context, cmd UpdateSecretCmd) error {
	stored, err := s.secret(ctx, cmd.Metadata.GetId(), activeOnly)
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	if err := s.checkInline(ctx, stored.GetMetadata().GetId(), cmd.Spec.GetSecretVersion()); err != nil {
		return err
	}
	updated, err := patchSecret(stored, cmd)
	if err != nil {
		return err
	}
	if p := cmd.Spec.GetPrimaryVersionId(); p != "" && p != stored.GetSpec().GetPrimaryVersionId() {
		if err := s.checkPrimaryCandidate(ctx, stored.GetMetadata().GetId(), p); err != nil {
			return err
		}
		updated.Spec.PrimaryVersionId = proto.String(p)
	}
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.secrets.Update(ctx, updated)
}

// checkPrimaryCandidate requires an active version of the secret.
func (s *Service) checkPrimaryCandidate(ctx context.Context, secretID, versionID string) error {
	v, err := s.version(ctx, versionID, activeOnly)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return errs.BadRequest(errs.Violation{Field: "spec.primary_version_id", Message: "no active version " + versionID})
	case err != nil:
		return err
	case v.GetMetadata().GetParentId() != secretID:
		return errs.BadRequest(errs.Violation{
			Field: "spec.primary_version_id", Message: "version " + versionID + " belongs to another secret",
		})
	}
	return nil
}

// checkUpdatable enforces immutable parent_id and optimistic concurrency.
func checkUpdatable(stored *mysteryboxpb.Secret, md *commonpb.ResourceMetadata) error {
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

// patchSecret applies name, labels and description to a copy of stored using
// the reset mask. Everything else (the primary version, the input-only first
// version, status) is restored from stored; the caller checks the first
// version and handles the primary.
func patchSecret(stored *mysteryboxpb.Secret, cmd UpdateSecretCmd) (*mysteryboxpb.Secret, error) {
	patch := &mysteryboxpb.Secret{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &mysteryboxpb.SecretSpec{Description: cmd.Spec.GetDescription()},
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
		updated.Spec = &mysteryboxpb.SecretSpec{}
	}
	updated.Spec.PrimaryVersionId = stored.GetSpec().PrimaryVersionId
	updated.Spec.SecretVersion = nil
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}
