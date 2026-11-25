package domain

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// ServiceAccountService implements nebius.iam.v1.ServiceAccountService.
type ServiceAccountService struct {
	sas      ServiceAccountRepository
	projects ContainerRepository
	keys     AccessKeyRepository
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
}

// ServiceAccountDeps are the dependencies of a ServiceAccountService. All are required.
type ServiceAccountDeps struct {
	ServiceAccounts ServiceAccountRepository
	Projects        ContainerRepository
	AccessKeys      AccessKeyRepository
	Ops             Operations
	IDs             IDGenerator
	Clock           clock.Clock
}

// NewServiceAccountService wires a ServiceAccountService.
func NewServiceAccountService(d ServiceAccountDeps) (*ServiceAccountService, error) {
	if d.ServiceAccounts == nil || d.Projects == nil || d.AccessKeys == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: service account service dependencies must not be nil")
	}
	return &ServiceAccountService{
		sas: d.ServiceAccounts, projects: d.Projects, keys: d.AccessKeys, ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreateServiceAccountCmd is the input of Create.
type CreateServiceAccountCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.ServiceAccountSpec
	Request  proto.Message
}

// UpdateServiceAccountCmd is the input of Update (full replace guarded by a reset mask).
type UpdateServiceAccountCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.ServiceAccountSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a service account by ID.
func (s *ServiceAccountService) Get(ctx context.Context, id string) (*iampb.ServiceAccount, error) {
	return s.sas.Get(ctx, id)
}

// GetByName returns a service account by name within its project.
func (s *ServiceAccountService) GetByName(ctx context.Context, parentID, name string) (*iampb.ServiceAccount, error) {
	return s.sas.GetByName(ctx, parentID, name)
}

// List returns one page of a project's service accounts in creation order.
func (s *ServiceAccountService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (ServiceAccountPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return ServiceAccountPage{}, err
	}
	return s.sas.List(ctx, parentID, size, pageToken)
}

// Create stores a new, active service account.
func (s *ServiceAccountService) Create(ctx context.Context, cmd CreateServiceAccountCmd) (*operations.Operation, error) {
	sa, err := s.newServiceAccount(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  sa.GetMetadata().GetId(),
		Description: "Create service account",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.sas.Create(ctx, sa) },
	})
}

// Update changes name, labels and description (X-ResetMask semantics).
func (s *ServiceAccountService) Update(ctx context.Context, cmd UpdateServiceAccountCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update service account",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

// Delete removes the service account and its access keys.
func (s *ServiceAccountService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete service account",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.sas.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.deleteCascade(ctx, id) },
	})
}

// deleteCascade removes the account's access keys first: a key must never
// outlive the principal it authenticates.
func (s *ServiceAccountService) deleteCascade(ctx context.Context, id string) error {
	keys, err := s.keys.ListByAccount(ctx, id)
	if err != nil {
		return fmt.Errorf("access keys of %s: %w", id, err)
	}
	for _, k := range keys {
		if err := s.keys.Delete(ctx, k.GetMetadata().GetId()); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return fmt.Errorf("delete access key %s: %w", k.GetMetadata().GetId(), err)
		}
	}
	return s.sas.Delete(ctx, id)
}

func (s *ServiceAccountService) newServiceAccount(ctx context.Context, cmd CreateServiceAccountCmd) (*iampb.ServiceAccount, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.projects.Get(ctx, parentID); err != nil {
		return nil, fmt.Errorf("service account parent %s: %w", parentID, err)
	}
	if md.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	id, err := s.ids.New(KindServiceAccount)
	if err != nil {
		return nil, fmt.Errorf("new service account id: %w", err)
	}
	spec := cmd.Spec
	if spec == nil {
		spec = &iampb.ServiceAccountSpec{}
	}
	return &iampb.ServiceAccount{
		Metadata: newMetadata(id, parentID, md, s.clk.Now()),
		Spec:     proto.CloneOf(spec),
		Status:   &iampb.ServiceAccountStatus{Active: true},
	}, nil
}

func (s *ServiceAccountService) applyUpdate(ctx context.Context, cmd UpdateServiceAccountCmd) error {
	stored, err := s.sas.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.ServiceAccount{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetMetadata().GetName() == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	if updated.GetSpec() == nil {
		updated.Spec = &iampb.ServiceAccountSpec{}
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	return s.sas.Update(ctx, updated)
}
