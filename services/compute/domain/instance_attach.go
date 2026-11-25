package domain

import (
	"context"
	"errors"
	"fmt"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// checkFilesystems validates spec.filesystems: an existing filesystem ID and
// a mount tag, unique per instance (docs: compute/storage/use).
func checkFilesystems(spec *computepb.InstanceSpec) error {
	tags, ids := map[string]bool{}, map[string]bool{}
	for i, fs := range spec.GetFilesystems() {
		field := fmt.Sprintf("spec.filesystems[%d]", i)
		id := fs.GetExistingFilesystem().GetId()
		switch {
		case id == "":
			return errs.BadRequest(errs.Violation{Field: field + ".existing_filesystem.id", Message: "required"})
		case ids[id]:
			return errs.BadRequest(errs.Violation{Field: field + ".existing_filesystem.id", Message: fmt.Sprintf("filesystem %s is attached twice", id)})
		case fs.GetMountTag() == "":
			return errs.BadRequest(errs.Violation{Field: field + ".mount_tag", Message: "required"})
		case tags[fs.GetMountTag()]:
			return errs.BadRequest(errs.Violation{Field: field + ".mount_tag", Message: fmt.Sprintf("mount_tag %q is used twice", fs.GetMountTag())})
		}
		ids[id], tags[fs.GetMountTag()] = true, true
	}
	return nil
}

// checkGpuCluster requires a preset that allows GPU clustering when the
// instance joins a GPU cluster.
func checkGpuCluster(spec *computepb.InstanceSpec, pp PlatformPreset) error {
	if spec.GetGpuCluster().GetId() == "" || pp.Preset.GetAllowGpuClustering() {
		return nil
	}
	return errs.BadRequest(errs.Violation{Field: "spec.gpu_cluster.id", Message: fmt.Sprintf(
		"preset %s of platform %s does not allow GPU clustering", pp.Preset.GetName(), pp.Platform.GetMetadata().GetName())})
}

func filesystemIDs(spec *computepb.InstanceSpec) map[string]*computepb.AttachedFilesystemSpec {
	out := map[string]*computepb.AttachedFilesystemSpec{}
	for _, fs := range spec.GetFilesystems() {
		out[fs.GetExistingFilesystem().GetId()] = fs
	}
	return out
}

// attachFilesystems attaches the filesystems of next that prev does not
// have. It returns the IDs it attached, for rollback; on error it detaches
// them itself.
func (s *InstanceService) attachFilesystems(ctx context.Context, inst *computepb.Instance, prev, next *computepb.InstanceSpec) ([]string, error) {
	id, project := inst.GetMetadata().GetId(), inst.GetMetadata().GetParentId()
	had := filesystemIDs(prev)
	var added []string
	for i, fs := range next.GetFilesystems() {
		fsID := fs.GetExistingFilesystem().GetId()
		if had[fsID] != nil {
			continue
		}
		if err := s.d.Filesystems.Attach(ctx, fsID, id, project, fs.GetAttachMode()); err != nil {
			err = rebaseFields(err, "spec.filesystems.", fmt.Sprintf("spec.filesystems[%d].", i))
			return nil, errors.Join(err, s.detachFilesystems(ctx, id, added))
		}
		added = append(added, fsID)
	}
	return added, nil
}

func (s *InstanceService) detachFilesystems(ctx context.Context, instanceID string, ids []string) error {
	var errList []error
	for _, fsID := range ids {
		errList = append(errList, s.d.Filesystems.Detach(ctx, fsID, instanceID))
	}
	return errors.Join(errList...)
}

// removedFilesystems lists the filesystems of prev that next no longer has.
func removedFilesystems(prev, next *computepb.InstanceSpec) []string {
	keep := filesystemIDs(next)
	var out []string
	for _, fs := range prev.GetFilesystems() {
		if fsID := fs.GetExistingFilesystem().GetId(); keep[fsID] == nil {
			out = append(out, fsID)
		}
	}
	return out
}

// joinGpuCluster joins the instance's GPU cluster, if any, and records the
// topology path in its status.
func (s *InstanceService) joinGpuCluster(ctx context.Context, inst *computepb.Instance) error {
	clusterID := inst.GetSpec().GetGpuCluster().GetId()
	if clusterID == "" {
		return nil
	}
	path, err := s.d.GpuClusters.Join(ctx, clusterID, inst.GetMetadata().GetId(), inst.GetMetadata().GetParentId(),
		inst.GetSpec().GetResources().GetPlatform())
	if err != nil {
		return err
	}
	inst.Status.GpuClusterTopology = &computepb.InstanceStatus_InfinibandTopologyPath{
		InfinibandTopologyPath: &computepb.InstanceStatusInfinibandTopologyPath{Path: path},
	}
	return nil
}

func (s *InstanceService) leaveGpuCluster(ctx context.Context, inst *computepb.Instance) error {
	clusterID := inst.GetSpec().GetGpuCluster().GetId()
	if clusterID == "" {
		return nil
	}
	return s.d.GpuClusters.Leave(ctx, clusterID, inst.GetMetadata().GetId())
}
