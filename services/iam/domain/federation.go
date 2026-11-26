package domain

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// FederationService implements nebius.iam.v1.FederationService: SAML identity
// providers of a tenant.
type FederationService struct {
	feds    FederationRepository
	certs   FederationCertificateRepository
	tenants TenantRepository
	users   FederationUserCounter
	ops     Operations
	ids     IDGenerator
	clk     clock.Clock
}

// FederationDeps are the dependencies of a FederationService. All are required.
type FederationDeps struct {
	Federations  FederationRepository
	Certificates FederationCertificateRepository
	Tenants      TenantRepository
	Users        FederationUserCounter
	Ops          Operations
	IDs          IDGenerator
	Clock        clock.Clock
}

// NewFederationService wires a FederationService.
func NewFederationService(d FederationDeps) (*FederationService, error) {
	if d.Federations == nil || d.Certificates == nil || d.Tenants == nil || d.Users == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: federation service dependencies must not be nil")
	}
	return &FederationService{
		feds: d.Federations, certs: d.Certificates, tenants: d.Tenants, users: d.Users, ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreateFederationCmd is the input of Create.
type CreateFederationCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.FederationSpec
	Request  proto.Message
}

// UpdateFederationCmd is the input of Update.
type UpdateFederationCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.FederationSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a federation with its live status counts.
func (s *FederationService) Get(ctx context.Context, id string) (*iampb.Federation, error) {
	f, err := s.feds.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return f, s.fillStatus(ctx, f)
}

// GetByName returns a federation by name within its tenant.
func (s *FederationService) GetByName(ctx context.Context, parentID, name string) (*iampb.Federation, error) {
	if name == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	}
	f, err := s.feds.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return f, s.fillStatus(ctx, f)
}

// List returns one page of a tenant's federations in creation order.
func (s *FederationService) List(ctx context.Context, parentID string, pageSize int64, pageToken, filter string) (FederationPage, error) {
	if err := rejectFilter(filter); err != nil {
		return FederationPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return FederationPage{}, err
	}
	page, err := s.feds.List(ctx, parentID, size, pageToken)
	if err != nil {
		return FederationPage{}, err
	}
	for _, f := range page.Items {
		if err := s.fillStatus(ctx, f); err != nil {
			return FederationPage{}, err
		}
	}
	return page, nil
}

// fillStatus sets the computed counts; the stored status holds only the state.
func (s *FederationService) fillStatus(ctx context.Context, f *iampb.Federation) error {
	id := f.GetMetadata().GetId()
	users, err := s.users.CountByFederation(ctx, id)
	if err != nil {
		return fmt.Errorf("users of federation %s: %w", id, err)
	}
	certs, err := s.certs.ListAll(ctx, id)
	if err != nil {
		return fmt.Errorf("certificates of federation %s: %w", id, err)
	}
	if f.GetStatus() == nil {
		f.Status = &iampb.FederationStatus{State: iampb.FederationStatus_ACTIVE}
	}
	f.Status.UsersCount = int32(users)             //nolint:gosec // user counts are far below int32
	f.Status.CertificatesCount = int32(len(certs)) //nolint:gosec // certificate counts are far below int32
	return nil
}

// Create registers a new, active federation in a tenant.
func (s *FederationService) Create(ctx context.Context, cmd CreateFederationCmd) (*operations.Operation, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if md.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	if err := validateFederationSpec(cmd.Spec); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindFederation)
	if err != nil {
		return nil, fmt.Errorf("new federation id: %w", err)
	}
	f := &iampb.Federation{
		Metadata: newMetadata(id, parentID, md, s.clk.Now()),
		Spec:     proto.CloneOf(cmd.Spec),
		Status:   &iampb.FederationStatus{State: iampb.FederationStatus_ACTIVE},
	}
	return s.submit(ctx, id, "Create federation", cmd.Request, func(ctx context.Context) error {
		if _, err := s.tenants.Get(ctx, parentID); err != nil {
			return fmt.Errorf("federation parent %s: %w", parentID, err)
		}
		return s.feds.Create(ctx, f)
	}, nil)
}

// Update changes name, labels and spec; the parent is immutable.
func (s *FederationService) Update(ctx context.Context, cmd UpdateFederationCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.submit(ctx, id, "Update federation", cmd.Request, func(ctx context.Context) error {
		return s.applyUpdate(ctx, cmd)
	}, nil)
}

// Activate lets the federation authenticate users again.
func (s *FederationService) Activate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Activate federation", request, func(ctx context.Context) error {
		return s.setState(ctx, id, iampb.FederationStatus_ACTIVE)
	}, nil)
}

// Deactivate stops the federation from authenticating users.
func (s *FederationService) Deactivate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Deactivate federation", request, func(ctx context.Context) error {
		return s.setState(ctx, id, iampb.FederationStatus_INACTIVE)
	}, nil)
}

// Delete removes an inactive federation and its certificates.
func (s *FederationService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Delete federation", request, func(ctx context.Context) error {
		f, err := s.feds.Get(ctx, id)
		if err != nil {
			return err
		}
		if f.GetStatus().GetState() == iampb.FederationStatus_ACTIVE {
			return errs.BadState(id, "federation is active: deactivate it before deleting it")
		}
		return nil
	}, func(ctx context.Context) error {
		if err := s.certs.DeleteByFederation(ctx, id); err != nil {
			return err
		}
		return s.feds.Delete(ctx, id)
	})
}

func (s *FederationService) submit(ctx context.Context, id, desc string, request proto.Message, prepare, work func(context.Context) error) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: desc, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request, Prepare: prepare, Work: work,
	})
}

func (s *FederationService) setState(ctx context.Context, id string, state iampb.FederationStatus_State) error {
	f, err := s.feds.Get(ctx, id)
	if err != nil {
		return err
	}
	if f.GetStatus().GetState() == state {
		return nil
	}
	f.Status = &iampb.FederationStatus{State: state}
	bumpVersion(f.GetMetadata(), s.clk.Now())
	return s.feds.Update(ctx, f)
}

func (s *FederationService) applyUpdate(ctx context.Context, cmd UpdateFederationCmd) error {
	stored, err := s.feds.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.Federation{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetMetadata().GetName() == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	if err := validateFederationSpec(updated.GetSpec()); err != nil {
		return err
	}
	// The status is server-owned (the request's deprecated status is ignored).
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	return s.feds.Update(ctx, updated)
}

// validateFederationSpec checks the required SAML settings. The deprecated
// spec.active is stored as given and has no effect.
func validateFederationSpec(spec *iampb.FederationSpec) error {
	saml := spec.GetSamlSettings()
	if saml == nil {
		return errs.BadRequest(errs.Violation{Field: "spec.saml_settings", Message: "required"})
	}
	var vs []errs.Violation
	if saml.GetIdpIssuer() == "" {
		vs = append(vs, errs.Violation{Field: "spec.saml_settings.idp_issuer", Message: "required"})
	}
	switch u, err := url.Parse(saml.GetSsoUrl()); {
	case saml.GetSsoUrl() == "":
		vs = append(vs, errs.Violation{Field: "spec.saml_settings.sso_url", Message: "required"})
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		vs = append(vs, errs.Violation{Field: "spec.saml_settings.sso_url", Message: "must be an absolute http or https URL"})
	}
	if len(vs) > 0 {
		return errs.BadRequest(vs...)
	}
	return nil
}
