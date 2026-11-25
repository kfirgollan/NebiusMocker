package api

import (
	"context"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	tenantService   = "nebius.iam.v1.TenantService"
	tenantV2Service = "nebius.iam.v2.TenantService"
)

// TenantHandler implements nebius.iam.v1.TenantService (deprecated upstream,
// still called by the v1 data source and `nebius iam tenant`).
type TenantHandler struct {
	iampb.UnimplementedTenantServiceServer
	svc *domain.TenantService
}

var _ iampb.TenantServiceServer = (*TenantHandler)(nil) //nolint:staticcheck // SA1019: the v1 data source and `nebius iam tenant` still call the v1 TenantService

// NewTenantHandler returns a v1 handler backed by svc.
func NewTenantHandler(svc *domain.TenantService) *TenantHandler { return &TenantHandler{svc: svc} }

// Get returns a tenant by ID.
func (h *TenantHandler) Get(ctx context.Context, req *iampb.GetTenantRequest) (*iampb.Container, error) {
	t, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return t, nil
}

// List returns one page of the tenants the caller may read.
func (h *TenantHandler) List(ctx context.Context, req *iampb.ListTenantsRequest) (*iampb.ListTenantsResponse, error) {
	page, err := h.svc.List(ctx, req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListTenantsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// TenantV2Handler implements nebius.iam.v2.TenantService. Update stays
// UNIMPLEMENTED: tenants are seeded.
type TenantV2Handler struct {
	iamv2pb.UnimplementedTenantServiceServer
	svc *domain.TenantService
}

var _ iamv2pb.TenantServiceServer = (*TenantV2Handler)(nil)

// NewTenantV2Handler returns a v2 handler backed by svc.
func NewTenantV2Handler(svc *domain.TenantService) *TenantV2Handler {
	return &TenantV2Handler{svc: svc}
}

// Get returns a tenant by ID.
func (h *TenantV2Handler) Get(ctx context.Context, req *iamv2pb.GetTenantRequest) (*iamv2pb.Tenant, error) {
	t, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return toV2Tenant(t), nil
}

// GetByName returns a tenant by name (parent_id must be empty).
func (h *TenantV2Handler) GetByName(ctx context.Context, req *iamv2pb.GetTenantByNameRequest) (*iamv2pb.Tenant, error) {
	t, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return toV2Tenant(t), nil
}

// List returns one page of the tenants the caller may read.
func (h *TenantV2Handler) List(ctx context.Context, req *iamv2pb.ListTenantsRequest) (*iamv2pb.ListTenantsResponse, error) {
	page, err := h.svc.List(ctx, req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	items := make([]*iamv2pb.Tenant, 0, len(page.Items))
	for _, t := range page.Items {
		items = append(items, toV2Tenant(t))
	}
	return &iamv2pb.ListTenantsResponse{Items: items, NextPageToken: page.NextPageToken}, nil
}

// toV2Tenant is the v2 view of a stored tenant.
func toV2Tenant(c *iampb.Container) *iamv2pb.Tenant {
	state := iamv2pb.TenantStatus_STATE_UNSPECIFIED
	if c.GetStatus().GetContainerState() == iampb.State_ACTIVE {
		state = iamv2pb.TenantStatus_ACTIVE
	}
	return &iamv2pb.Tenant{
		Metadata: c.GetMetadata(),
		Spec:     &iamv2pb.TenantSpec{},
		Status:   &iamv2pb.TenantStatus{TenantState: state},
	}
}

// RegisterTenantAuthzRules declares the permissions of the v1 and v2
// TenantService RPCs. List is open to every authenticated caller and filtered
// to the tenants the caller may read. v2 Update has a rule so that an
// authorized caller gets UNIMPLEMENTED rather than a denial.
func RegisterTenantAuthzRules(r *authz.Rules, tenantIDOf NameLookup) {
	perm := func(verb string) authz.Permission { return authz.Perm("iam", "tenants", verb) }
	for _, svc := range []string{tenantService, tenantV2Service} {
		m := func(method string) string { return "/" + svc + "/" + method }
		r.Add(m("Get"), authz.OnID(perm(authz.VerbGet)))
		r.Add(m("List"), filteredList(perm(authz.VerbList)))
	}
	r.Add("/"+tenantV2Service+"/GetByName", byName(perm(authz.VerbGet), tenantIDOf))
	r.Add("/"+tenantV2Service+"/Update", authz.OnMetadataID(perm(authz.VerbUpdate)))
}
