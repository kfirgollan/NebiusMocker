package domain

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// DefaultInvitationValidity is how long an invitation stays acceptable when
// the request gives no expires_in (3 days, as in Nebius).
const DefaultInvitationValidity = 72 * time.Hour

// InvitationService implements nebius.iam.v1.InvitationService, plus the mock's
// stand-in for the invitee accepting the invitation in their browser.
type InvitationService struct {
	invitations InvitationRepository
	users       TenantUserAccountRepository
	attrs       UserAttributesRepository
	memberships MembershipRepository
	tenants     ContainerRepository
	federations FederationGetter
	ops         Operations
	ids         IDGenerator
	clk         clock.Clock
}

// InvitationDeps are the dependencies of an InvitationService. All are required.
type InvitationDeps struct {
	Invitations InvitationRepository
	Users       TenantUserAccountRepository
	Attributes  UserAttributesRepository
	Memberships MembershipRepository
	Tenants     ContainerRepository
	Federations FederationGetter
	Ops         Operations
	IDs         IDGenerator
	Clock       clock.Clock
}

// NewInvitationService wires an InvitationService.
func NewInvitationService(d InvitationDeps) (*InvitationService, error) {
	if d.Invitations == nil || d.Users == nil || d.Attributes == nil || d.Memberships == nil || d.Tenants == nil ||
		d.Federations == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: invitation service dependencies must not be nil")
	}
	return &InvitationService{
		invitations: d.Invitations, users: d.Users, attrs: d.Attributes, memberships: d.Memberships, tenants: d.Tenants,
		federations: d.Federations, ops: d.Ops, ids: d.IDs, clk: d.Clock,
	}, nil
}

// CreateInvitationCmd is the input of Create.
type CreateInvitationCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.InvitationSpec
	NoSend    bool
	ExpiresIn *durationpb.Duration
	Request   proto.Message
}

// UpdateInvitationCmd is the input of Update.
type UpdateInvitationCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.InvitationSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// AcceptedInvitation is the outcome of accepting an invitation.
type AcceptedInvitation struct {
	InvitationID        string
	TenantUserAccountID string
	State               iampb.InvitationStatus_State
}

// invitationView is the client-visible form of a stored invitation: one that
// is waiting for the invitee past its expiry reports EXPIRED.
func invitationView(inv *iampb.Invitation, now time.Time) *iampb.Invitation {
	out := proto.CloneOf(inv)
	st := out.GetStatus()
	waiting := st.GetState() == iampb.InvitationStatus_CREATED || st.GetState() == iampb.InvitationStatus_PENDING
	if exp := st.GetExpiresAt(); waiting && exp != nil && !now.Before(exp.AsTime()) {
		st.State = iampb.InvitationStatus_EXPIRED
	}
	return out
}

// Get returns an invitation as clients see it.
func (s *InvitationService) Get(ctx context.Context, id string) (*iampb.Invitation, error) {
	inv, err := s.invitations.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return invitationView(inv, s.clk.Now()), nil
}

// List returns one page of a tenant's invitations.
func (s *InvitationService) List(ctx context.Context, tenantID string, pageSize int64, pageToken, filter string) (InvitationPage, error) {
	if err := rejectFilter(filter); err != nil {
		return InvitationPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return InvitationPage{}, err
	}
	page, err := s.invitations.List(ctx, tenantID, size, pageToken)
	if err != nil {
		return InvitationPage{}, err
	}
	now := s.clk.Now()
	for i, inv := range page.Items {
		page.Items[i] = invitationView(inv, now)
	}
	return page, nil
}

// Create validates the request, creates the invitee's tenant user account and
// the invitation.
func (s *InvitationService) Create(ctx context.Context, cmd CreateInvitationCmd) (*operations.Operation, error) {
	inv, tua, validity, err := s.newInvitation(ctx, cmd)
	if err != nil {
		return nil, err
	}
	email := inv.GetSpec().GetEmail()
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  inv.GetMetadata().GetId(),
		Description: "Create invitation",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			if err := s.users.Create(ctx, tua); err != nil {
				return err
			}
			rollback := func(err error) error {
				return errors.Join(err, s.users.Delete(ctx, tua.GetMetadata().GetId()))
			}
			if err := s.attrs.SetAttributes(ctx, tua.GetMetadata().GetId(), &iampb.UserAttributes{Email: &email}); err != nil {
				return rollback(err)
			}
			if err := s.invitations.Create(ctx, inv, validity); err != nil {
				return rollback(err)
			}
			return nil
		},
	})
}

func (s *InvitationService) newInvitation(ctx context.Context, cmd CreateInvitationCmd) (*iampb.Invitation, *iampb.TenantUserAccount, time.Duration, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, nil, 0, err
	}
	tenantID := md.GetParentId()
	if tenantID == "" {
		return nil, nil, 0, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if err := validateInvitationEmail(cmd.Spec.GetEmail()); err != nil {
		return nil, nil, 0, err
	}
	validity := DefaultInvitationValidity
	if d := cmd.ExpiresIn; d != nil {
		if err := d.CheckValid(); err != nil || d.AsDuration() <= 0 {
			return nil, nil, 0, errs.BadRequest(errs.Violation{Field: "expires_in", Message: "must be a positive duration"})
		}
		validity = d.AsDuration()
	}
	if _, err := s.tenants.Get(ctx, tenantID); err != nil {
		return nil, nil, 0, fmt.Errorf("invitation tenant %s: %w", tenantID, err)
	}
	invID, err := s.ids.New(KindInvitation)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("new invitation id: %w", err)
	}
	tuaID, err := s.ids.New(KindTenantUserAccount)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("new tenant user account id: %w", err)
	}
	now := s.clk.Now()
	state := iampb.InvitationStatus_PENDING
	if cmd.NoSend {
		state = iampb.InvitationStatus_CREATED
	}
	inv := &iampb.Invitation{
		Metadata: newMetadata(invID, tenantID, md, now),
		Spec:     proto.CloneOf(cmd.Spec),
		Status: &iampb.InvitationStatus{
			TenantUserAccountId: tuaID, ExpiresAt: timestamppb.New(now.Add(validity)), State: state,
		},
	}
	return inv, newTenantUserAccount(tuaID, tenantID, invID, nil, s.clk), validity, nil
}

// validateInvitationEmail requires a bare address such as "ada@example.com".
func validateInvitationEmail(email string) error {
	if email == "" {
		return errs.BadRequest(errs.Violation{Field: "spec.email", Message: "required"})
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return errs.BadRequest(errs.Violation{Field: "spec.email", Message: "must be a valid email address"})
	}
	return nil
}

// Update changes name, labels, description and email. The email of an
// accepted invitation is fixed.
func (s *InvitationService) Update(ctx context.Context, cmd UpdateInvitationCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Update invitation", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: cmd.Request,
		Prepare: func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

func (s *InvitationService) applyUpdate(ctx context.Context, cmd UpdateInvitationCmd) error {
	stored, err := s.invitations.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	patch := &iampb.Invitation{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &iampb.InvitationSpec{Description: cmd.Spec.GetDescription()},
	}
	if e := cmd.Spec.GetEmail(); e != "" {
		patch.Spec.Contact = &iampb.InvitationSpec_Email{Email: e}
	}
	updated, err := patchResource(stored, patch, cmd.ResetMask)
	if err != nil {
		return err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	email := updated.GetSpec().GetEmail()
	if err := validateInvitationEmail(email); err != nil {
		return err
	}
	emailChanged := email != stored.GetSpec().GetEmail()
	if emailChanged && stored.GetStatus().GetState() == iampb.InvitationStatus_ACCEPTED {
		return errs.BadState(stored.GetMetadata().GetId(), "the email of an accepted invitation cannot be changed")
	}
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	if err := s.invitations.Update(ctx, updated); err != nil {
		return err
	}
	if emailChanged {
		return s.attrs.SetAttributes(ctx, stored.GetStatus().GetTenantUserAccountId(), &iampb.UserAttributes{Email: &email})
	}
	return nil
}

// Resend sends the invitation again: it becomes PENDING and stays valid for
// its original window from now. Accepted invitations cannot be resent.
func (s *InvitationService) Resend(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Resend invitation", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error {
			inv, err := s.invitations.Get(ctx, id)
			if err != nil {
				return err
			}
			if inv.GetStatus().GetState() == iampb.InvitationStatus_ACCEPTED {
				return errs.BadState(id, "the invitation is already accepted")
			}
			validity, err := s.invitations.Validity(ctx, id)
			if err != nil {
				return err
			}
			now := s.clk.Now()
			inv.Status.State = iampb.InvitationStatus_PENDING
			inv.Status.ExpiresAt = timestamppb.New(now.Add(validity))
			bumpVersion(inv.GetMetadata(), now)
			return s.invitations.Update(ctx, inv)
		},
	})
}

// Delete cancels an invitation. Unless it was accepted, the invitee's tenant
// user account goes with it, along with that account's group memberships.
func (s *InvitationService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete invitation", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error {
			_, err := s.invitations.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			inv, err := s.invitations.Get(ctx, id)
			if err != nil {
				return err
			}
			if inv.GetStatus().GetState() != iampb.InvitationStatus_ACCEPTED {
				if err := s.removeAccount(ctx, inv.GetStatus().GetTenantUserAccountId()); err != nil {
					return err
				}
			}
			return s.invitations.Delete(ctx, id)
		},
	})
}

// removeAccount deletes a tenant user account and its group memberships.
func (s *InvitationService) removeAccount(ctx context.Context, accountID string) error {
	ms, err := s.memberships.ListByMember(ctx, accountID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := s.memberships.Delete(ctx, m.GetMetadata().GetId()); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return err
		}
	}
	if err := s.users.Delete(ctx, accountID); err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	return nil
}

// Accept is what the invitee's browser does after following the email link: a
// pending invitation becomes ACCEPTED and its account a real user, linked to
// the federation they signed in through (optional).
func (s *InvitationService) Accept(ctx context.Context, id, federationID string) (AcceptedInvitation, error) {
	inv, err := s.invitations.Get(ctx, id)
	if err != nil {
		return AcceptedInvitation{}, err
	}
	state := invitationView(inv, s.clk.Now()).GetStatus().GetState()
	if state == iampb.InvitationStatus_CREATED {
		return AcceptedInvitation{}, errs.BadState(id, "the invitation was not sent yet; call Resend first")
	}
	if state == iampb.InvitationStatus_EXPIRED {
		return AcceptedInvitation{}, errs.BadState(id, "the invitation has expired; call Resend first")
	}
	if state != iampb.InvitationStatus_PENDING {
		return AcceptedInvitation{}, errs.BadState(id, "the invitation is "+state.String())
	}
	if federationID != "" {
		if err := s.checkFederation(ctx, inv.GetMetadata().GetParentId(), federationID); err != nil {
			return AcceptedInvitation{}, err
		}
	}
	tuaID := inv.GetStatus().GetTenantUserAccountId()
	tua, err := s.users.Get(ctx, tuaID)
	if err != nil {
		return AcceptedInvitation{}, err
	}
	now := s.clk.Now()
	tua.Status.InvitationId = ""
	tua.Status.UserAccountState = iampb.UserAccountStatus_ACTIVE
	tua.Status.FederationId = federationID
	bumpVersion(tua.GetMetadata(), now)
	inv.Status.State = iampb.InvitationStatus_ACCEPTED
	bumpVersion(inv.GetMetadata(), now)
	if err := s.users.Update(ctx, tua); err != nil {
		return AcceptedInvitation{}, err
	}
	if err := s.invitations.Update(ctx, inv); err != nil {
		return AcceptedInvitation{}, err
	}
	return AcceptedInvitation{InvitationID: id, TenantUserAccountID: tuaID, State: inv.GetStatus().GetState()}, nil
}

func (s *InvitationService) checkFederation(ctx context.Context, tenantID, federationID string) error {
	f, err := s.federations.Get(ctx, federationID)
	if err != nil {
		return fmt.Errorf("federation %s: %w", federationID, err)
	}
	if f.GetMetadata().GetParentId() != tenantID {
		return fmt.Errorf("federation %s: %w", federationID, errs.NotFound(federationID))
	}
	if f.GetStatus().GetState() != iampb.FederationStatus_ACTIVE {
		return errs.BadState(federationID, "the federation is not active")
	}
	return nil
}
