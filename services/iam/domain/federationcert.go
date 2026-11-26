package domain

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// FederationCertificateService implements
// nebius.iam.v1.FederationCertificateService: the X.509 certificates a
// federation's identity provider signs SAML assertions with.
type FederationCertificateService struct {
	certs FederationCertificateRepository
	feds  FederationRepository
	ops   Operations
	ids   IDGenerator
	clk   clock.Clock
}

// FederationCertificateDeps are the dependencies of a
// FederationCertificateService. All are required.
type FederationCertificateDeps struct {
	Certificates FederationCertificateRepository
	Federations  FederationRepository
	Ops          Operations
	IDs          IDGenerator
	Clock        clock.Clock
}

// NewFederationCertificateService wires a FederationCertificateService.
func NewFederationCertificateService(d FederationCertificateDeps) (*FederationCertificateService, error) {
	if d.Certificates == nil || d.Federations == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: federation certificate service dependencies must not be nil")
	}
	return &FederationCertificateService{certs: d.Certificates, feds: d.Federations, ops: d.Ops, ids: d.IDs, clk: d.Clock}, nil
}

// CreateFederationCertificateCmd is the input of Create.
type CreateFederationCertificateCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.FederationCertificateSpec
	Request  proto.Message
}

// UpdateFederationCertificateCmd is the input of Update, and one element of UpdateBulk.
type UpdateFederationCertificateCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.FederationCertificateSpec
	ResetMask *mask.Mask
}

// Get returns a certificate as clients see it (see view).
func (s *FederationCertificateService) Get(ctx context.Context, id string) (*iampb.FederationCertificate, error) {
	c, err := s.certs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(c), nil
}

// ListByFederation returns one page of a federation's certificates.
func (s *FederationCertificateService) ListByFederation(ctx context.Context, federationID string, pageSize int64, pageToken string) (FederationCertificatePage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return FederationCertificatePage{}, err
	}
	if federationID == "" {
		return FederationCertificatePage{}, errs.BadRequest(errs.Violation{Field: "federation_id", Message: "required"})
	}
	if _, err := s.feds.Get(ctx, federationID); err != nil {
		return FederationCertificatePage{}, err
	}
	page, err := s.certs.List(ctx, federationID, size, pageToken)
	if err != nil {
		return FederationCertificatePage{}, err
	}
	for i, c := range page.Items {
		page.Items[i] = s.view(c)
	}
	return page, nil
}

// Create registers a certificate for a federation.
func (s *FederationCertificateService) Create(ctx context.Context, cmd CreateFederationCertificateCmd) (*operations.Operation, error) {
	if err := requireNoID(cmd.Metadata); err != nil {
		return nil, err
	}
	federationID := cmd.Metadata.GetParentId()
	if federationID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	c, err := s.newCertificate(federationID, cmd.Metadata, cmd.Spec)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, c.GetMetadata().GetId(), "Create federation certificate", cmd.Request, func(ctx context.Context) error {
		if _, err := s.feds.Get(ctx, federationID); err != nil {
			return fmt.Errorf("certificate federation %s: %w", federationID, err)
		}
		return s.certs.Create(ctx, c)
	})
}

// Update changes name, labels and description; the data is immutable.
func (s *FederationCertificateService) Update(ctx context.Context, cmd UpdateFederationCertificateCmd, request proto.Message) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.submit(ctx, id, "Update federation certificate", request, func(ctx context.Context) error {
		stored, err := s.certs.Get(ctx, id)
		if err != nil {
			return err
		}
		updated, err := s.patched(stored, cmd)
		if err != nil {
			return err
		}
		if proto.Equal(updated, stored) {
			return nil
		}
		return s.certs.Update(ctx, updated)
	})
}

// Delete removes a certificate.
func (s *FederationCertificateService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete federation certificate", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error {
			_, err := s.certs.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.certs.Delete(ctx, id) },
	})
}

// UpdateBulk makes the update list the federation's complete certificate set:
// an element with metadata.id updates that certificate, one without creates a
// new one, and every certificate not mentioned is removed. Everything is
// validated first and then applied in one transaction.
func (s *FederationCertificateService) UpdateBulk(ctx context.Context, federationID string, updates []UpdateFederationCertificateCmd, request proto.Message) (*operations.Operation, error) {
	if federationID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "federation_id", Message: "required"})
	}
	return s.submit(ctx, federationID, "Update federation certificates", request, func(ctx context.Context) error {
		if _, err := s.feds.Get(ctx, federationID); err != nil {
			return err
		}
		existing, err := s.certs.ListAll(ctx, federationID)
		if err != nil {
			return err
		}
		remove, update, create, err := s.planBulk(federationID, existing, updates)
		if err != nil {
			return err
		}
		return s.certs.ReplaceAll(ctx, federationID, remove, update, create)
	})
}

// planBulk validates a bulk update against the federation's current
// certificates and splits it into removals, updates and creations.
func (s *FederationCertificateService) planBulk(federationID string, existing []*iampb.FederationCertificate, updates []UpdateFederationCertificateCmd) (remove []string, update, create []*iampb.FederationCertificate, err error) {
	byID := make(map[string]*iampb.FederationCertificate, len(existing))
	for _, c := range existing {
		byID[c.GetMetadata().GetId()] = c
	}
	kept := make(map[string]bool, len(updates))
	names := make(map[string]bool, len(updates))
	for i, u := range updates {
		field := fmt.Sprintf("updates[%d]", i)
		c, isUpdate, err := s.planElement(federationID, byID, kept, u)
		if err != nil {
			return nil, nil, nil, prefixViolations(err, field)
		}
		if name := c.GetMetadata().GetName(); name != "" {
			if names[name] {
				return nil, nil, nil, errs.AlreadyExists("", "certificate name "+name+" is used twice in the update")
			}
			names[name] = true
		}
		if isUpdate {
			update = append(update, c)
		} else {
			create = append(create, c)
		}
	}
	for _, c := range existing {
		if id := c.GetMetadata().GetId(); !kept[id] {
			remove = append(remove, id)
		}
	}
	return remove, update, create, nil
}

// planElement turns one bulk element into the certificate to write. An
// element with metadata.id patches that certificate (recorded in kept); one
// without creates a new certificate.
func (s *FederationCertificateService) planElement(federationID string, byID map[string]*iampb.FederationCertificate, kept map[string]bool, u UpdateFederationCertificateCmd) (c *iampb.FederationCertificate, isUpdate bool, err error) {
	id := u.Metadata.GetId()
	if id == "" {
		if p := u.Metadata.GetParentId(); p != "" && p != federationID {
			return nil, false, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be " + federationID})
		}
		c, err = s.newCertificate(federationID, u.Metadata, u.Spec)
		return c, false, err
	}
	stored, ok := byID[id]
	if !ok {
		return nil, false, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "not a certificate of federation " + federationID})
	}
	if kept[id] {
		return nil, false, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "listed twice"})
	}
	kept[id] = true
	c, err = s.patched(stored, u)
	return c, true, err
}

// prefixViolations puts a bulk element's position in front of its violation fields.
func prefixViolations(err error, prefix string) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	vs := make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		vs[i] = errs.Violation{Field: prefix + "." + v.Field, Message: v.Message}
	}
	return errs.BadRequest(vs...)
}

func (s *FederationCertificateService) submit(ctx context.Context, id, desc string, request proto.Message, prepare func(context.Context) error) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: desc, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request, Prepare: prepare,
	})
}

// view is the client-visible form of a stored certificate: an active one past
// its not_after reports EXPIRED.
func (s *FederationCertificateService) view(c *iampb.FederationCertificate) *iampb.FederationCertificate {
	out := proto.CloneOf(c)
	if na := out.GetStatus().GetNotAfter(); na != nil && !na.AsTime().After(s.clk.Now()) &&
		out.GetStatus().GetState() == iampb.FederationCertificateStatus_ACTIVE {
		out.Status.State = iampb.FederationCertificateStatus_EXPIRED
	}
	return out
}

func (s *FederationCertificateService) newCertificate(federationID string, md *commonpb.ResourceMetadata, spec *iampb.FederationCertificateSpec) (*iampb.FederationCertificate, error) {
	status, err := certificateStatus(spec.GetData())
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.data", Message: err.Error()})
	}
	id, err := s.ids.New(KindFederationCertificate)
	if err != nil {
		return nil, fmt.Errorf("new federation certificate id: %w", err)
	}
	return &iampb.FederationCertificate{
		Metadata: newMetadata(id, federationID, md, s.clk.Now()),
		Spec:     proto.CloneOf(spec),
		Status:   status,
	}, nil
}

// patched applies an update to a stored certificate with reset-mask
// semantics: name, labels and description change; the data may only be
// repeated, and the status is server-owned.
func (s *FederationCertificateService) patched(stored *iampb.FederationCertificate, cmd UpdateFederationCertificateCmd) (*iampb.FederationCertificate, error) {
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return nil, err
	}
	if d := cmd.Spec.GetData(); d != "" && d != stored.GetSpec().GetData() {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.data", Message: "spec.data is immutable"})
	}
	updated, err := patchResource(stored, &iampb.FederationCertificate{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &iampb.FederationCertificateSpec{Description: cmd.Spec.GetDescription()},
	}, cmd.ResetMask)
	if err != nil {
		return nil, err
	}
	desc := updated.GetSpec().GetDescription()
	updated.Spec = proto.CloneOf(stored.GetSpec())
	updated.Spec.Description = desc
	updated.Status = proto.CloneOf(stored.GetStatus())
	if !proto.Equal(updated, stored) {
		bumpVersion(updated.GetMetadata(), s.clk.Now())
	}
	return updated, nil
}

// certificateStatus parses spec.data, which must hold exactly one PEM
// CERTIFICATE block, and describes the certificate.
func certificateStatus(data string) (*iampb.FederationCertificateStatus, error) {
	if strings.TrimSpace(data) == "" {
		return nil, errors.New("required")
	}
	block, rest := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("must be a PEM-encoded CERTIFICATE")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("PEM block is %q, want CERTIFICATE", block.Type)
	}
	if next, _ := pem.Decode(rest); next != nil {
		return nil, errors.New("must contain exactly one certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid X.509 certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	st := &iampb.FederationCertificateStatus{
		State:       iampb.FederationCertificateStatus_ACTIVE,
		Fingerprint: hex.EncodeToString(sum[:]),
		NotBefore:   timestamppb.New(cert.NotBefore),
		NotAfter:    timestamppb.New(cert.NotAfter),
	}
	switch pub := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		st.Algorithm, st.KeySize = "RSA", int64(pub.N.BitLen())
	case *ecdsa.PublicKey:
		st.Algorithm, st.KeySize = "ECDSA", int64(pub.Curve.Params().BitSize)
	case ed25519.PublicKey:
		st.Algorithm, st.KeySize = "ED25519", 256
	default:
		return nil, fmt.Errorf("unsupported public key type %T", pub)
	}
	return st, nil
}
