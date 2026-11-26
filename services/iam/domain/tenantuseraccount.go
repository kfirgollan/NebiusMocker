package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// MemberDirectory resolves the principals that can be group members: tenant
// user accounts (ordinary or invited) and service accounts.
type MemberDirectory struct {
	users       TenantUserAccountRepository
	attrs       UserAttributesRepository
	invitations InvitationRepository
	sas         ServiceAccountRepository
	clk         clock.Clock
}

// NewMemberDirectory returns a MemberDirectory. All dependencies are required.
func NewMemberDirectory(users TenantUserAccountRepository, attrs UserAttributesRepository, invitations InvitationRepository, sas ServiceAccountRepository, clk clock.Clock) (*MemberDirectory, error) {
	if users == nil || attrs == nil || invitations == nil || sas == nil || clk == nil {
		return nil, errors.New("iam: member directory dependencies must not be nil")
	}
	return &MemberDirectory{users: users, attrs: attrs, invitations: invitations, sas: sas, clk: clk}, nil
}

// checkGroupMember verifies that memberID may join a group of tenantID: a
// tenant user account must exist in the tenant, and an invited one must not
// have an expired invitation. Service accounts are not checked.
func (d *MemberDirectory) checkGroupMember(ctx context.Context, tenantID, memberID string) error {
	if !strings.HasPrefix(memberID, KindTenantUserAccount+"-") {
		return nil
	}
	tua, err := d.users.Get(ctx, memberID)
	if err != nil {
		return fmt.Errorf("group member %s: %w", memberID, err)
	}
	if tua.GetMetadata().GetParentId() != tenantID {
		return fmt.Errorf("group member %s: %w", memberID, errs.NotFound(memberID))
	}
	if invID := tua.GetStatus().GetInvitationId(); invID != "" {
		inv, err := d.invitations.Get(ctx, invID)
		if err != nil {
			return fmt.Errorf("invitation %s of %s: %w", invID, memberID, err)
		}
		if invitationView(inv, d.clk.Now()).GetStatus().GetState() == iampb.InvitationStatus_EXPIRED {
			return errs.BadState(memberID, "the invitation of this user has expired; resend it first")
		}
	}
	return nil
}

// userAttributes returns what the mock knows about a tenant user account:
// the invited email and, once the user exists for real, sub = the account ID.
func (d *MemberDirectory) userAttributes(ctx context.Context, tua *iampb.TenantUserAccount) (*iampb.UserAttributes, error) {
	stored, err := d.attrs.Attributes(ctx, tua.GetMetadata().GetId())
	if err != nil {
		return nil, err
	}
	out := proto.CloneOf(stored)
	if tua.GetStatus().GetInvitationId() == "" {
		id := tua.GetMetadata().GetId()
		out.Sub = &id
	}
	return out, nil
}

// TenantUserAccountService implements nebius.iam.v1.TenantUserAccountService
// and TenantUserAccountWithAttributesService.
type TenantUserAccountService struct {
	users TenantUserAccountRepository
	dir   *MemberDirectory
	ops   Operations
	clk   clock.Clock
}

// NewTenantUserAccountService wires a TenantUserAccountService.
func NewTenantUserAccountService(users TenantUserAccountRepository, dir *MemberDirectory, ops Operations, clk clock.Clock) (*TenantUserAccountService, error) {
	if users == nil || dir == nil || ops == nil || clk == nil {
		return nil, errors.New("iam: tenant user account service dependencies must not be nil")
	}
	return &TenantUserAccountService{users: users, dir: dir, ops: ops, clk: clk}, nil
}

// Get returns a tenant user account.
func (s *TenantUserAccountService) Get(ctx context.Context, id string) (*iampb.TenantUserAccount, error) {
	return s.users.Get(ctx, id)
}

// List returns one page of a tenant's accounts.
func (s *TenantUserAccountService) List(ctx context.Context, tenantID string, pageSize int64, pageToken, filter string) (TenantUserAccountPage, error) {
	if err := rejectFilter(filter); err != nil {
		return TenantUserAccountPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return TenantUserAccountPage{}, err
	}
	return s.users.List(ctx, tenantID, size, pageToken)
}

// GetWithAttributes returns an account with its attributes.
func (s *TenantUserAccountService) GetWithAttributes(ctx context.Context, id string) (*iampb.TenantUserAccountWithAttributes, error) {
	tua, err := s.users.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withAttributes(ctx, tua)
}

// TenantUserAccountWithAttributesPage is one page of accounts with attributes.
type TenantUserAccountWithAttributesPage struct {
	Items         []*iampb.TenantUserAccountWithAttributes
	NextPageToken string
}

// ListWithAttributes returns one page of a tenant's accounts with attributes.
func (s *TenantUserAccountService) ListWithAttributes(ctx context.Context, tenantID string, pageSize int64, pageToken, filter string) (TenantUserAccountWithAttributesPage, error) {
	page, err := s.List(ctx, tenantID, pageSize, pageToken, filter)
	if err != nil {
		return TenantUserAccountWithAttributesPage{}, err
	}
	out := TenantUserAccountWithAttributesPage{NextPageToken: page.NextPageToken}
	for _, tua := range page.Items {
		item, err := s.withAttributes(ctx, tua)
		if err != nil {
			return TenantUserAccountWithAttributesPage{}, err
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (s *TenantUserAccountService) withAttributes(ctx context.Context, tua *iampb.TenantUserAccount) (*iampb.TenantUserAccountWithAttributes, error) {
	attrs, err := s.dir.userAttributes(ctx, tua)
	if err != nil {
		return nil, err
	}
	return &iampb.TenantUserAccountWithAttributes{
		TenantUserAccount:  tua,
		AttributesOptional: &iampb.TenantUserAccountWithAttributes_Attributes{Attributes: attrs},
	}, nil
}

// Block stops the account from being authorized to use tenant resources.
func (s *TenantUserAccountService) Block(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.setState(ctx, id, "Block tenant user account", request, iampb.TenantUserAccountStatus_BLOCKED)
}

// Unblock makes a blocked account usable again.
func (s *TenantUserAccountService) Unblock(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.setState(ctx, id, "Unblock tenant user account", request, iampb.TenantUserAccountStatus_ACTIVE)
}

func (s *TenantUserAccountService) setState(ctx context.Context, id, desc string, request proto.Message, state iampb.TenantUserAccountStatus_State) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: desc, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error {
			tua, err := s.users.Get(ctx, id)
			if err != nil {
				return err
			}
			if tua.GetStatus().GetState() == state {
				return nil
			}
			tua.Status.State = state
			bumpVersion(tua.GetMetadata(), s.clk.Now())
			return s.users.Update(ctx, tua)
		},
	})
}

// newTenantUserAccount builds the account an invitation creates.
func newTenantUserAccount(id, tenantID, invitationID string, md *commonpb.ResourceMetadata, clk clock.Clock) *iampb.TenantUserAccount {
	return &iampb.TenantUserAccount{
		Metadata: newMetadata(id, tenantID, md, clk.Now()),
		Spec:     &iampb.TenantUserAccountSpec{},
		Status: &iampb.TenantUserAccountStatus{
			State: iampb.TenantUserAccountStatus_ACTIVE, InvitationId: invitationID,
			UserAccountState: iampb.UserAccountStatus_STATE_UNSPECIFIED,
		},
	}
}

// SeedTenantUserAccount makes sure the account of a static principal exists in
// the tenant: active, and already a real (non-invited) user.
func SeedTenantUserAccount(ctx context.Context, users TenantUserAccountRepository, clk clock.Clock, tenantID, id string) error {
	tua := newTenantUserAccount(id, tenantID, "", nil, clk)
	tua.Status.UserAccountState = iampb.UserAccountStatus_ACTIVE
	if err := users.Ensure(ctx, tua); err != nil {
		return fmt.Errorf("seed tenant user account %s: %w", id, err)
	}
	return nil
}
