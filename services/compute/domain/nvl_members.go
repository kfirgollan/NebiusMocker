package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// NVLMembers implements NVLMembership over an NVLInstanceGroupRepository.
type NVLMembers struct {
	groups    NVLInstanceGroupRepository
	platforms NVLPlatforms
}

var _ NVLMembership = (*NVLMembers)(nil)

// NewNVLMembers returns NVLMembers over groups, checking instance platforms
// against the group type with platforms.
func NewNVLMembers(groups NVLInstanceGroupRepository, platforms NVLPlatforms) (*NVLMembers, error) {
	if groups == nil || platforms == nil {
		return nil, errors.New("compute: nvl membership needs a group repository and the platforms")
	}
	return &NVLMembers{groups: groups, platforms: platforms}, nil
}

const nvlGroupField = "spec.nvl_instance_group_id"

// Join implements NVLMembership.
func (m *NVLMembers) Join(ctx context.Context, groupID, instanceID, projectID, platform string) error {
	if groupID == "" {
		return errs.BadRequest(errs.Violation{Field: nvlGroupField, Message: "required"})
	}
	if instanceID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "instance id required to join an nvl instance group"})
	}
	_, err := m.groups.Mutate(ctx, groupID, func(g *computepb.NVLInstanceGroup) error {
		if g.GetMetadata().GetParentId() != projectID {
			return errs.BadRequest(errs.Violation{
				Field: nvlGroupField, Message: fmt.Sprintf("nvl instance group %s is not in project %s", groupID, projectID),
			})
		}
		want := g.GetSpec().GetType()
		if got := m.platforms.NVLInstanceGroupType(platform); got != want {
			return errs.BadRequest(errs.Violation{Field: nvlGroupField, Message: fmt.Sprintf(
				"nvl instance group %s is of type %s: platform %q cannot join it (%s)", groupID, want, platform, platformNVLHint(got))})
		}
		if g.GetStatus() == nil {
			g.Status = &computepb.NVLInstanceGroupStatus{}
		}
		members := g.GetStatus().GetInstances()
		if _, ok := members[instanceID]; ok {
			return nil
		}
		if size := g.GetSpec().GetSize(); int64(len(members)) >= size {
			return errs.BadState(groupID, fmt.Sprintf("nvl instance group %s is full: it holds at most %d instances", groupID, size))
		}
		if members == nil {
			g.Status.Instances = map[string]*computepb.NVLInstanceGroupStatus_InstanceInfo{}
		}
		// The state is filled in from the instance on every read.
		g.Status.Instances[instanceID] = &computepb.NVLInstanceGroupStatus_InstanceInfo{InstanceState: computepb.InstanceStatus_CREATING}
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return &errs.Error{
			Kind: errs.KindNotFound, ResourceID: groupID,
			Message: fmt.Sprintf("%s: nvl instance group %s not found", nvlGroupField, groupID),
		}
	}
	return err
}

func platformNVLHint(t computepb.NVLInstanceGroupSpec_NVLInstanceGroupType) string {
	if t == computepb.NVLInstanceGroupSpec_UNSPECIFIED {
		return "the platform has no NVLink domains"
	}
	return "the platform's instances join " + t.String() + " groups"
}

// Leave implements NVLMembership.
func (m *NVLMembers) Leave(ctx context.Context, groupID, instanceID string) error {
	if groupID == "" {
		return nil
	}
	_, err := m.groups.Mutate(ctx, groupID, func(g *computepb.NVLInstanceGroup) error {
		delete(g.GetStatus().GetInstances(), instanceID)
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

// Members implements NVLMembership.
func (m *NVLMembers) Members(ctx context.Context, groupID string) ([]string, error) {
	g, err := m.groups.Get(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(g.GetStatus().GetInstances())), nil
}
