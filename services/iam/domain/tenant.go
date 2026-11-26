package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// DefaultRegion is the region of seeded tenants.
const DefaultRegion = "eu-north1"

// Permissions a caller needs on a tenant or project to see it in a listing.
var (
	permTenantsGet  = authz.Perm("iam", "tenants", authz.VerbGet)
	permProjectsGet = authz.Perm("iam", "projects", authz.VerbGet)
)

// newContainer builds an active tenant or project.
func newContainer(id, parentID, name, region string, labels map[string]string, now time.Time) *iampb.Container {
	ts := timestamppb.New(now)
	return &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: name, Labels: labels, ResourceVersion: 1, CreatedAt: ts, UpdatedAt: ts,
		},
		Spec: &iampb.ContainerSpec{Region: region},
		Status: &iampb.ContainerStatus{
			ContainerState:  iampb.State_ACTIVE,
			SuspensionState: iampb.SuspensionState_NONE,
			Region:          region,
		},
	}
}

// SeedTenant creates the tenant with the given ID if it does not exist yet.
// Tenants cannot be created through the public API, so the mocker seeds them.
func SeedTenant(ctx context.Context, tenants ContainerRepository, clk clock.Clock, id, name string) error {
	if err := tenants.Ensure(ctx, newContainer(id, "", name, DefaultRegion, nil, clk.Now())); err != nil {
		return fmt.Errorf("seed tenant %s: %w", id, err)
	}
	return nil
}

// SeedProject creates the project under tenantID in region if it does not
// exist yet. Projects are the parent of most cloud resources (registries,
// buckets, …).
func SeedProject(ctx context.Context, projects ContainerRepository, clk clock.Clock, id, tenantID, name, region string) error {
	if err := projects.Ensure(ctx, newContainer(id, tenantID, name, region, nil, clk.Now())); err != nil {
		return fmt.Errorf("seed project %s: %w", id, err)
	}
	return nil
}

// TenantService implements the read side of nebius.iam.v1/v2.TenantService.
// Tenants are seeded; a caller lists only the tenants it may read.
type TenantService struct {
	tenants TenantStore
	vis     Visibility
	ops     Operations
	clk     clock.Clock
}

// TenantStore is a TenantRepository that can also replace a tenant (the only
// write a tenant admits besides seeding).
type TenantStore interface {
	TenantRepository
	// Update fails with errs.NotFound if the tenant does not exist and with
	// errs.AlreadyExists if the name is taken by another tenant.
	Update(ctx context.Context, t *iampb.Container) error
}

// UpdateTenantCmd is the input of Update (full replace guarded by a reset mask).
type UpdateTenantCmd struct {
	Metadata *commonpb.ResourceMetadata
	// NamePrefix is the request's name_prefix, which the mock rejects.
	NamePrefix string
	ResetMask  *mask.Mask
	Request    proto.Message
}

// NewTenantService wires a TenantService.
func NewTenantService(tenants TenantStore, vis Visibility, ops Operations, clk clock.Clock) (*TenantService, error) {
	if tenants == nil || vis == nil || ops == nil || clk == nil {
		return nil, errors.New("iam: tenant service dependencies must not be nil")
	}
	return &TenantService{tenants: tenants, vis: vis, ops: ops, clk: clk}, nil
}

// Update changes the tenant's name and labels (X-ResetMask semantics). The
// spec is empty; name_prefix (a server-chosen name suffix) is not supported.
func (s *TenantService) Update(ctx context.Context, cmd UpdateTenantCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	if cmd.NamePrefix != "" {
		return nil, errs.BadRequest(errs.Violation{
			Field: "name_prefix", Message: "name_prefix is not supported by NebiusMocker: set metadata.name instead",
		})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update tenant",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

func (s *TenantService) applyUpdate(ctx context.Context, cmd UpdateTenantCmd) error {
	stored, err := s.tenants.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetMetadata().GetName() == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	// The tenant's spec and status are server-owned.
	updated.Spec = proto.CloneOf(stored.GetSpec())
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	return s.tenants.Update(ctx, updated)
}

// Get returns a tenant by ID.
func (s *TenantService) Get(ctx context.Context, id string) (*iampb.Container, error) {
	return s.tenants.Get(ctx, id)
}

// GetByName returns a tenant by name. Tenants are roots, so parentID exists in
// the request only for compatibility. Upstream says only an empty value is
// allowed, but the nebius_iam_v2_tenant data source refuses to read by name
// without a parent_id. The mock therefore also accepts the tenant's own ID,
// so `parent_id = <tenant>` works, and rejects anything else.
func (s *TenantService) GetByName(ctx context.Context, parentID, name string) (*iampb.Container, error) {
	if name == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	}
	t, err := s.tenants.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if parentID != "" && parentID != t.GetMetadata().GetId() {
		return nil, errs.BadRequest(errs.Violation{
			Field: "parent_id", Message: "parent_id must be empty (or the tenant's own ID): tenants have no parent",
		})
	}
	return t, nil
}

// List returns one page of the tenants the caller may read (iam.tenants.get
// on the tenant), in creation order.
func (s *TenantService) List(ctx context.Context, pageSize int64, pageToken, filter string) (ContainerPage, error) {
	size, err := listArgs(pageSize, filter)
	if err != nil {
		return ContainerPage{}, err
	}
	fetch := func(ctx context.Context, n int, tok string) ([]*iampb.Container, string, error) {
		p, err := s.tenants.List(ctx, n, tok)
		return p.Items, p.NextPageToken, err
	}
	items, next, err := visiblePage(ctx, size, pageToken, fetch, func(t *iampb.Container) (bool, error) {
		return s.vis.CanSee(ctx, permTenantsGet, t.GetMetadata().GetId())
	})
	if err != nil {
		return ContainerPage{}, err
	}
	return ContainerPage{Items: items, NextPageToken: next}, nil
}

// listArgs validates the common List arguments. Filters are not supported:
// they are rejected rather than ignored.
func listArgs(pageSize int64, filter string) (int, error) {
	if filter != "" {
		return 0, errs.BadRequest(errs.Violation{Field: "filter", Message: "filters are not supported by NebiusMocker; list without a filter"})
	}
	return pageSizeOf(pageSize)
}
