package domain

import (
	"context"
	"fmt"
	"slices"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// checkRecoveryPolicy allows recovery_policy ALWAYS only for instances in an
// NVL instance group (instance.proto).
func checkRecoveryPolicy(spec *computepb.InstanceSpec) error {
	if spec.GetRecoveryPolicy() == computepb.InstanceRecoveryPolicy_ALWAYS && spec.GetNvlInstanceGroupId() == "" {
		return errs.BadRequest(errs.Violation{
			Field: "spec.recovery_policy", Message: "ALWAYS is allowed only for instances in an NVL instance group (spec.nvl_instance_group_id)",
		})
	}
	return nil
}

// joinNVLGroup adds the instance to its NVL instance group, if any.
func (s *InstanceService) joinNVLGroup(ctx context.Context, inst *computepb.Instance) error {
	groupID := inst.GetSpec().GetNvlInstanceGroupId()
	if groupID == "" {
		return nil
	}
	return s.d.NVLGroups.Join(ctx, groupID, inst.GetMetadata().GetId(), inst.GetMetadata().GetParentId(),
		inst.GetSpec().GetResources().GetPlatform())
}

func (s *InstanceService) leaveNVLGroup(ctx context.Context, inst *computepb.Instance) error {
	return s.d.NVLGroups.Leave(ctx, inst.GetSpec().GetNvlInstanceGroupId(), inst.GetMetadata().GetId())
}

// ListByNVLInstanceGroup lists the instances of an NVL instance group in
// creation order.
func (s *InstanceService) ListByNVLInstanceGroup(ctx context.Context, groupID string, size int64, token string) (InstancePage, error) {
	if groupID == "" {
		return InstancePage{}, errs.BadRequest(errs.Violation{Field: "nvl_instance_group_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return InstancePage{}, err
	}
	offset, err := decodeOffset(token)
	if err != nil {
		return InstancePage{}, err
	}
	ids, err := s.d.NVLGroups.Members(ctx, groupID)
	if errs.IsKind(err, errs.KindNotFound) {
		return InstancePage{}, &errs.Error{
			Kind: errs.KindNotFound, ResourceID: groupID, Message: fmt.Sprintf("nvl instance group %s not found", groupID),
		}
	}
	if err != nil {
		return InstancePage{}, err
	}
	members := make([]*computepb.Instance, 0, len(ids))
	for _, id := range ids {
		inst, err := s.d.Instances.Get(ctx, id)
		if errs.IsKind(err, errs.KindNotFound) {
			continue // deleted since the group was read
		}
		if err != nil {
			return InstancePage{}, err
		}
		members = append(members, inst)
	}
	slices.SortStableFunc(members, func(a, b *computepb.Instance) int {
		return a.GetMetadata().GetCreatedAt().AsTime().Compare(b.GetMetadata().GetCreatedAt().AsTime())
	})
	var page InstancePage
	if offset < len(members) {
		page.Items = members[offset:min(offset+n, len(members))]
	}
	if next := offset + n; next < len(members) {
		page.NextPageToken = encodeOffset(next)
	}
	return page, nil
}
