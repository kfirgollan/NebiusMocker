package domain

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// FederatedCredentialsService implements nebius.iam.v1.FederatedCredentialsService:
// trust from a subject of an external OIDC issuer to a service account, which
// the token exchange honours (subject_token = the service account ID,
// actor_token = the issuer's JWT).
type FederatedCredentialsService struct {
	creds    FederatedCredentialsRepository
	sas      ServiceAccountRepository
	projects ContainerRepository
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
	revoker  TokenRevoker
}

// FederatedCredentialsDeps are the dependencies of a FederatedCredentialsService.
// All are required.
type FederatedCredentialsDeps struct {
	Credentials     FederatedCredentialsRepository
	ServiceAccounts ServiceAccountRepository
	Projects        ContainerRepository
	Ops             Operations
	IDs             IDGenerator
	Clock           clock.Clock
	Revoker         TokenRevoker
}

// NewFederatedCredentialsService wires a FederatedCredentialsService.
func NewFederatedCredentialsService(d FederatedCredentialsDeps) (*FederatedCredentialsService, error) {
	if d.Credentials == nil || d.ServiceAccounts == nil || d.Projects == nil || d.Ops == nil || d.IDs == nil ||
		d.Clock == nil || d.Revoker == nil {
		return nil, errors.New("iam: federated credentials service dependencies must not be nil")
	}
	return &FederatedCredentialsService{
		creds: d.Credentials, sas: d.ServiceAccounts, projects: d.Projects, ops: d.Ops, ids: d.IDs, clk: d.Clock,
		revoker: d.Revoker,
	}, nil
}

// CreateFederatedCredentialsCmd is the input of Create.
type CreateFederatedCredentialsCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.FederatedCredentialsSpec
	Request  proto.Message
}

// UpdateFederatedCredentialsCmd is the input of Update.
type UpdateFederatedCredentialsCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.FederatedCredentialsSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns federated credentials by ID.
func (s *FederatedCredentialsService) Get(ctx context.Context, id string) (*iampb.FederatedCredentials, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.creds.Get(ctx, id)
}

// GetByName returns federated credentials by name within their project.
func (s *FederatedCredentialsService) GetByName(ctx context.Context, parentID, name string) (*iampb.FederatedCredentials, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if name == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	}
	return s.creds.GetByName(ctx, parentID, name)
}

// List returns one page of a project's federated credentials.
func (s *FederatedCredentialsService) List(ctx context.Context, parentID string, pageSize int64, pageToken, filter string) (FederatedCredentialsPage, error) {
	if parentID == "" {
		return FederatedCredentialsPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if err := rejectFilter(filter); err != nil {
		return FederatedCredentialsPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return FederatedCredentialsPage{}, err
	}
	return s.creds.List(ctx, parentID, size, pageToken)
}

// Create registers federated credentials for a service account of the parent project.
func (s *FederatedCredentialsService) Create(ctx context.Context, cmd CreateFederatedCredentialsCmd) (*operations.Operation, error) {
	c, err := s.newCredentials(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  c.GetMetadata().GetId(),
		Description: "Create federated credentials",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.creds.Create(ctx, c) },
	})
}

// Update changes name, labels and the spec. A spec change revokes the access
// tokens already exchanged through the credentials: they were issued under a
// trust that no longer holds.
func (s *FederatedCredentialsService) Update(ctx context.Context, cmd UpdateFederatedCredentialsCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update federated credentials",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

// Delete removes federated credentials and revokes the tokens exchanged through them.
func (s *FederatedCredentialsService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete federated credentials",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.creds.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			if err := s.creds.Delete(ctx, id); err != nil {
				return err
			}
			s.revoker.RevokeKey(id)
			return nil
		},
	})
}

func (s *FederatedCredentialsService) newCredentials(ctx context.Context, cmd CreateFederatedCredentialsCmd) (*iampb.FederatedCredentials, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a project ID (" + KindProject + "-…)"})
	}
	if _, err := s.projects.Get(ctx, parentID); err != nil {
		return nil, fmt.Errorf("federated credentials parent %s: %w", parentID, err)
	}
	if err := s.checkSpec(ctx, parentID, cmd.Spec); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindFederatedCredentials)
	if err != nil {
		return nil, fmt.Errorf("new federated credentials id: %w", err)
	}
	return &iampb.FederatedCredentials{
		Metadata: newMetadata(id, parentID, md, s.clk.Now()),
		Spec:     proto.CloneOf(cmd.Spec),
		Status:   &iampb.FederatedCredentialsStatus{},
	}, nil
}

func (s *FederatedCredentialsService) applyUpdate(ctx context.Context, cmd UpdateFederatedCredentialsCmd) error {
	stored, err := s.creds.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.FederatedCredentials{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	specChanged := !proto.Equal(updated.GetSpec(), stored.GetSpec())
	if specChanged {
		if err := s.checkSpec(ctx, stored.GetMetadata().GetParentId(), updated.GetSpec()); err != nil {
			return err
		}
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	if err := s.creds.Update(ctx, updated); err != nil {
		return err
	}
	if specChanged {
		s.revoker.RevokeKey(updated.GetMetadata().GetId())
	}
	return nil
}

// checkSpec validates a spec as stored: an OIDC provider with an absolute
// http(s) issuer URL and, if set, a usable JWKS; a federated subject; and a
// service account of project parentID as the subject.
func (s *FederatedCredentialsService) checkSpec(ctx context.Context, parentID string, spec *iampb.FederatedCredentialsSpec) error {
	if err := checkOIDCProvider(spec.GetOidcProvider()); err != nil {
		return err
	}
	if spec.GetFederatedSubjectId() == "" {
		return errs.BadRequest(errs.Violation{
			Field:   "spec.federated_subject_id",
			Message: "required: NebiusMocker does not trust every subject of an issuer; set the sub claim to trust",
		})
	}
	return s.checkSubject(ctx, parentID, spec.GetSubjectId())
}

func checkOIDCProvider(p *iampb.OidcCredentialsProvider) error {
	if p == nil {
		return errs.BadRequest(errs.Violation{Field: "spec.oidc_provider", Message: "required"})
	}
	if err := checkIssuerURL(p.GetIssuerUrl()); err != nil {
		return errs.BadRequest(errs.Violation{Field: "spec.oidc_provider.issuer_url", Message: err.Error()})
	}
	if j := p.GetJwkSetJson(); j != "" {
		if _, err := auth.ParseJWKSet([]byte(j)); err != nil {
			return errs.BadRequest(errs.Violation{Field: "spec.oidc_provider.jwk_set_json", Message: err.Error()})
		}
	}
	return nil
}

func checkIssuerURL(raw string) error {
	if raw == "" {
		return errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("must not have a query, fragment or user info")
	}
	return nil
}

func (s *FederatedCredentialsService) checkSubject(ctx context.Context, parentID, saID string) error {
	if saID == "" {
		return errs.BadRequest(errs.Violation{Field: "spec.subject_id", Message: "required"})
	}
	if !strings.HasPrefix(saID, KindServiceAccount+"-") {
		return errs.BadRequest(errs.Violation{Field: "spec.subject_id", Message: "must be a service account ID (" + KindServiceAccount + "-…)"})
	}
	sa, err := s.sas.Get(ctx, saID)
	if errs.IsKind(err, errs.KindNotFound) {
		return errs.BadRequest(errs.Violation{Field: "spec.subject_id", Message: "service account " + saID + " does not exist"})
	}
	if err != nil {
		return fmt.Errorf("federated credentials subject %s: %w", saID, err)
	}
	if p := sa.GetMetadata().GetParentId(); p != parentID {
		return errs.BadRequest(errs.Violation{
			Field: "spec.subject_id", Message: fmt.Sprintf("service account %s is in project %s, not in %s", saID, p, parentID),
		})
	}
	return nil
}

// issuerMatches compares a credential's issuer_url with a token's iss claim:
// exactly, tolerating a single trailing slash on either side.
func issuerMatches(issuerURL, iss string) bool {
	return issuerURL == iss || strings.TrimSuffix(issuerURL, "/") == strings.TrimSuffix(iss, "/")
}
