package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gosdknid "github.com/nebius/gosdk/nid"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// MembershipService implements IAM group memberships.
type MembershipService struct {
	memberships MembershipRepository
	groups      GroupRepository
	ops         Operations
	ids         IDGenerator
	clk         clock.Clock
}

// NewMembershipService wires a MembershipService. All dependencies are required.
func NewMembershipService(memberships MembershipRepository, groups GroupRepository, ops Operations, ids IDGenerator, clk clock.Clock) (*MembershipService, error) {
	if memberships == nil || groups == nil || ops == nil || ids == nil || clk == nil {
		return nil, errors.New("iam: membership service dependencies must not be nil")
	}
	return &MembershipService{memberships: memberships, groups: groups, ops: ops, ids: ids, clk: clk}, nil
}

// CreateMembershipCmd is the input of Create.
type CreateMembershipCmd struct {
	Metadata         *commonpb.ResourceMetadata
	Spec             *iampb.GroupMembershipSpec
	RevokeAfterHours int64
	Request          proto.Message
}

// Get returns a membership by ID.
func (s *MembershipService) Get(ctx context.Context, id string) (*iampb.GroupMembership, error) {
	return s.memberships.Get(ctx, id)
}

// ListMembers returns one page of a group's memberships.
func (s *MembershipService) ListMembers(ctx context.Context, groupID string, pageSize int64, pageToken string) (MembershipPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return MembershipPage{}, err
	}
	return s.memberships.ListByGroup(ctx, groupID, size, pageToken)
}

// ListMemberOf returns the groups a principal belongs to.
func (s *MembershipService) ListMemberOf(ctx context.Context, subjectID string) ([]*iampb.Group, error) {
	ms, err := s.memberships.ListByMember(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	out := make([]*iampb.Group, 0, len(ms))
	for _, m := range ms {
		g, err := s.groups.Get(ctx, m.GetMetadata().GetParentId())
		if err != nil {
			return nil, fmt.Errorf("group of membership %s: %w", m.GetMetadata().GetId(), err)
		}
		out = append(out, g)
	}
	return out, nil
}

// Create adds a member to a group.
func (s *MembershipService) Create(ctx context.Context, cmd CreateMembershipCmd) (*operations.Operation, error) {
	m, err := s.newMembership(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  m.GetMetadata().GetId(),
		Description: "Create group membership",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.memberships.Create(ctx, m) },
	})
}

// Delete removes a member from a group when the operation completes.
func (s *MembershipService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete group membership",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.memberships.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.memberships.Delete(ctx, id) },
	})
}

func (s *MembershipService) newMembership(ctx context.Context, cmd CreateMembershipCmd) (*iampb.GroupMembership, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	groupID := md.GetParentId()
	if _, err := s.groups.Get(ctx, groupID); err != nil {
		return nil, fmt.Errorf("membership group %s: %w", groupID, err)
	}
	memberID := cmd.Spec.GetMemberId()
	if err := validateMember(memberID); err != nil {
		return nil, err
	}
	if cmd.RevokeAfterHours < 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "revoke_after_hours", Message: "must not be negative"})
	}
	id, err := s.ids.New(KindGroupMembership)
	if err != nil {
		return nil, fmt.Errorf("new membership id: %w", err)
	}
	now := s.clk.Now()
	m := &iampb.GroupMembership{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: groupID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: timestamppb.New(now), UpdatedAt: timestamppb.New(now),
		},
		Spec:   &iampb.GroupMembershipSpec{MemberId: memberID},
		Status: &iampb.GroupMembershipStatus{GroupMemberMetadata: &commonpb.ResourceMetadata{Id: memberID}},
	}
	if cmd.RevokeAfterHours > 0 {
		m.RevokeAt = timestamppb.New(now.Add(time.Duration(cmd.RevokeAfterHours) * time.Hour))
	}
	return m, nil
}

// validateMember checks that id is a user account or service account NID.
// The mocker does not model user accounts yet, so existence is not checked.
func validateMember(id string) error {
	if id == "" {
		return errs.BadRequest(errs.Violation{Field: "spec.member_id", Message: "required"})
	}
	n, err := gosdknid.Parse(id)
	if err != nil {
		return errs.BadRequest(errs.Violation{Field: "spec.member_id", Message: "not a valid Nebius ID"})
	}
	if t := string(n.Type); t != KindTenantUserAccount && t != KindServiceAccount {
		return errs.BadRequest(errs.Violation{
			Field: "spec.member_id", Message: "must be a tenant user account or a service account, got " + t,
		})
	}
	return nil
}

// memberCounts counts a group's members by kind.
func memberCounts(ctx context.Context, memberships MembershipRepository, groupID string) (users, serviceAccounts int32, err error) {
	token := ""
	for {
		page, err := memberships.ListByGroup(ctx, groupID, maxPageSize, token)
		if err != nil {
			return 0, 0, err
		}
		for _, m := range page.Items {
			if strings.HasPrefix(m.GetSpec().GetMemberId(), KindServiceAccount+"-") {
				serviceAccounts++
			} else {
				users++
			}
		}
		if token = page.NextPageToken; token == "" {
			return users, serviceAccounts, nil
		}
	}
}

func pageSizeOf(pageSize int64) (int, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	if pageSize == 0 {
		return defaultPageSize, nil
	}
	return int(pageSize), nil
}
