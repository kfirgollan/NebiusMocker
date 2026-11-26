package domain

import (
	"context"
	"slices"
	"strings"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

// KindNVLInstanceGroup is the NID type prefix of NVLink instance groups.
const KindNVLInstanceGroup = "computenvlinstancegroup"

// NVLInstanceGroupPage is one page of an NVL instance group listing.
type NVLInstanceGroupPage struct {
	Items         []*computepb.NVLInstanceGroup
	NextPageToken string
}

// NVLInstanceGroupRepository stores NVL instance groups. The stored
// status.instances holds the member IDs; their instance_state is filled in
// from the instances when a group is read.
type NVLInstanceGroupRepository interface {
	Get(ctx context.Context, id string) (*computepb.NVLInstanceGroup, error)
	GetByName(ctx context.Context, parentID, name string) (*computepb.NVLInstanceGroup, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (NVLInstanceGroupPage, error)
	// Create fails with errs.AlreadyExists if the ID or the name in the parent is taken.
	Create(ctx context.Context, g *computepb.NVLInstanceGroup) error
	// Mutate is DiskRepository.Mutate for NVL instance groups.
	Mutate(ctx context.Context, id string, fn func(*computepb.NVLInstanceGroup) error) (*computepb.NVLInstanceGroup, error)
	// DeleteIf atomically loads the group and deletes it unless check
	// returns an error, which aborts the deletion. A missing group is
	// errs.NotFound.
	DeleteIf(ctx context.Context, id string, check func(*computepb.NVLInstanceGroup) error) error
}

// InstanceReader reads instances (satisfied by the instance repository).
type InstanceReader interface {
	// Get returns the instance or errs.NotFound.
	Get(ctx context.Context, id string) (*computepb.Instance, error)
}

// NVLPlatforms tells which NVL instance group type a platform's instances can
// join (satisfied by *PlatformService).
type NVLPlatforms interface {
	// NVLInstanceGroupType is UNSPECIFIED for platforms without NVLink domains.
	NVLInstanceGroupType(platform string) computepb.NVLInstanceGroupSpec_NVLInstanceGroupType
}

// NVLMembership is how instances join and leave NVL instance groups
// (InstanceSpec.nvl_instance_group_id). Satisfied by *NVLMembers.
type NVLMembership interface {
	// Join atomically checks that the group exists (errs.NotFound, naming
	// spec.nvl_instance_group_id), is in projectID (errs.BadRequest), that
	// platform belongs to the group's type (errs.BadRequest) and that the
	// group has room for one more instance (errs.BadState), then adds
	// instanceID to status.instances. Joining twice is a no-op.
	Join(ctx context.Context, groupID, instanceID, projectID, platform string) error
	// Leave removes instanceID from the group. A missing group or member is
	// not an error.
	Leave(ctx context.Context, groupID, instanceID string) error
	// Members returns the group's member instance IDs, sorted, or
	// errs.NotFound when the group does not exist.
	Members(ctx context.Context, groupID string) ([]string, error)
}

// nvlTypes are the NVL instance group types a platform catalog may name.
var nvlTypes = []computepb.NVLInstanceGroupSpec_NVLInstanceGroupType{
	computepb.NVLInstanceGroupSpec_GB200, computepb.NVLInstanceGroupSpec_GB300,
}

func nvlType(name string) computepb.NVLInstanceGroupSpec_NVLInstanceGroupType {
	return computepb.NVLInstanceGroupSpec_NVLInstanceGroupType(computepb.NVLInstanceGroupSpec_NVLInstanceGroupType_value[name])
}

func validNVLType(name string) bool {
	return slices.Contains(nvlTypes, nvlType(name))
}

func nvlTypeNames() string {
	names := make([]string, 0, len(nvlTypes))
	for _, t := range nvlTypes {
		names = append(names, t.String())
	}
	return strings.Join(names, ", ")
}
