package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Managed node instances (peer.ComputeNodes): the instances behind mk8s node
// groups are ordinary instances that a node group owns. They are created
// RUNNING without an operation (the node group's operation is the visible
// one) and users cannot update, start, stop or delete them.

var _ peer.ComputeNodes = (*InstanceService)(nil)

// bootDiskSuffix is appended to the node name to name its managed boot disk.
const bootDiskSuffix = "-boot"

// refuseManaged fails with errs.BadState when instance id is a node of an
// mk8s node group: only the node group changes it.
func (s *InstanceService) refuseManaged(ctx context.Context, id string) error {
	rec, ok, err := s.d.Nodes.Owner(ctx, id)
	if err != nil || !ok {
		return err
	}
	return errs.BadState(id, fmt.Sprintf(
		"instance %s is a node of mk8s node group %s (cluster %s) and is managed by it: change the node group instead",
		id, rec.NodeGroupID, rec.ClusterID))
}

// CheckTemplate implements peer.ComputeNodes.
func (s *InstanceService) CheckTemplate(ctx context.Context, projectID string, t peer.NodeTemplate) (peer.NodeResources, error) {
	if projectID == "" {
		return peer.NodeResources{}, errs.BadRequest(errs.Violation{Field: "project_id", Message: "required"})
	}
	spec, err := nodeSpec("node", t)
	if err != nil {
		return peer.NodeResources{}, err
	}
	pp, err := s.checkSpec(ctx, projectID, spec)
	if err != nil {
		return peer.NodeResources{}, mapNodeFields(err)
	}
	inst := &computepb.Instance{Metadata: &commonpb.ResourceMetadata{ParentId: projectID}}
	boot := resolvedSlot{diskSlot: diskSlots(spec)[0]}
	if _, err := s.newManagedDisk(ctx, inst, boot); err != nil {
		return peer.NodeResources{}, mapNodeFields(err)
	}
	return presetResources(pp), nil
}

// CreateNode implements peer.ComputeNodes.
func (s *InstanceService) CreateNode(ctx context.Context, n peer.NodeSpec) (peer.NodeInstance, error) {
	switch {
	case n.ProjectID == "":
		return peer.NodeInstance{}, errs.BadRequest(errs.Violation{Field: "project_id", Message: "required"})
	case n.Name == "":
		return peer.NodeInstance{}, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	case n.NodeGroupID == "" || n.ClusterID == "":
		return peer.NodeInstance{}, errs.BadRequest(errs.Violation{Field: "node_group_id", Message: "the node group and cluster are required"})
	}
	spec, err := nodeSpec(n.Name, n.Template)
	if err != nil {
		return peer.NodeInstance{}, err
	}
	s.nodes.Lock()
	defer s.nodes.Unlock()
	if inst, ok, err := s.existingNode(ctx, n); err != nil || ok {
		if err != nil {
			return peer.NodeInstance{}, err
		}
		return s.nodeInstance(ctx, inst)
	}
	inst, err := s.newInstance(ctx, CreateInstanceCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: n.ProjectID, Name: n.Name, Labels: n.Labels},
		Spec:     spec,
	})
	if err != nil {
		return peer.NodeInstance{}, err
	}
	rec := NodeRecord{InstanceID: inst.GetMetadata().GetId(), NodeGroupID: n.NodeGroupID, ClusterID: n.ClusterID, Name: n.Name}
	if err := s.d.Nodes.Put(ctx, rec); err != nil {
		return peer.NodeInstance{}, err
	}
	setInstanceState(inst, computepb.InstanceStatus_RUNNING)
	if err := s.prepareCreate(ctx, inst); err != nil {
		return peer.NodeInstance{}, errors.Join(err, s.d.Nodes.Delete(ctx, rec.InstanceID))
	}
	return s.nodeInstance(ctx, inst)
}

// existingNode finds the node n.Name of n's node group. A record whose
// instance is gone is dropped.
func (s *InstanceService) existingNode(ctx context.Context, n peer.NodeSpec) (*computepb.Instance, bool, error) {
	rec, ok, err := s.d.Nodes.ByName(ctx, n.NodeGroupID, n.Name)
	if err != nil || !ok {
		return nil, false, err
	}
	inst, err := s.d.Instances.Get(ctx, rec.InstanceID)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil, false, s.d.Nodes.Delete(ctx, rec.InstanceID)
	}
	return inst, err == nil, err
}

// DeleteNode implements peer.ComputeNodes.
func (s *InstanceService) DeleteNode(ctx context.Context, instanceID string) error {
	if err := s.remove(ctx, instanceID); err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	return s.d.Nodes.Delete(ctx, instanceID)
}

// ListNodes implements peer.ComputeNodes.
func (s *InstanceService) ListNodes(ctx context.Context, nodeGroupID string) ([]peer.NodeInstance, error) {
	recs, err := s.d.Nodes.List(ctx, nodeGroupID)
	if err != nil {
		return nil, err
	}
	out := make([]peer.NodeInstance, 0, len(recs))
	for _, rec := range recs {
		inst, err := s.d.Instances.Get(ctx, rec.InstanceID)
		if errs.IsKind(err, errs.KindNotFound) {
			continue // being deleted
		}
		if err != nil {
			return nil, err
		}
		n, err := s.nodeInstance(ctx, inst)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// nodeInstance renders a node instance for the capability.
func (s *InstanceService) nodeInstance(ctx context.Context, inst *computepb.Instance) (peer.NodeInstance, error) {
	res := inst.GetSpec().GetResources()
	pp, err := s.d.Platforms.Resolve(ctx, inst.GetMetadata().GetParentId(), res.GetPlatform(), res.GetPreset())
	if err != nil {
		return peer.NodeInstance{}, err
	}
	out := peer.NodeInstance{ID: inst.GetMetadata().GetId(), Name: inst.GetMetadata().GetName(), Resources: presetResources(pp)}
	if nics := inst.GetStatus().GetNetworkInterfaces(); len(nics) > 0 {
		out.PrivateIP = bareAddress(nics[0].GetIpAddress().GetAddress())
		out.PublicIP = bareAddress(nics[0].GetPublicIpAddress().GetAddress())
	}
	return out, nil
}

// bareAddress drops the prefix length of an address ("10.0.0.4/32").
func bareAddress(a string) string {
	addr, _, _ := strings.Cut(a, "/")
	return addr
}

func presetResources(pp PlatformPreset) peer.NodeResources {
	r := pp.Preset.GetResources()
	return peer.NodeResources{GPUs: int(r.GetGpuCount()), VCPUs: int(r.GetVcpuCount()), Memory: int64(r.GetMemoryGibibytes())}
}

// nodeSpec builds the instance spec a node of template t has.
func nodeSpec(name string, t peer.NodeTemplate) (*computepb.InstanceSpec, error) {
	diskType, ok := computepb.DiskSpec_DiskType_value[t.BootDisk.Type]
	if t.BootDisk.Type != "" && !ok {
		return nil, errs.BadRequest(errs.Violation{Field: "boot_disk.type", Message: fmt.Sprintf("unknown disk type %q", t.BootDisk.Type)})
	}
	disk := &computepb.DiskSpec{
		Type:           computepb.DiskSpec_DiskType(diskType),
		Size:           &computepb.DiskSpec_SizeBytes{SizeBytes: t.BootDisk.SizeBytes},
		BlockSizeBytes: t.BootDisk.BlockSizeBytes,
		Source:         &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: t.ImageFamily}},
	}
	if t.BootDisk.Encrypted {
		disk.DiskEncryption = &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED}
	}
	nic := &computepb.NetworkInterfaceSpec{Name: "eth0", SubnetId: t.SubnetID, IpAddress: &computepb.IPAddress{}}
	if t.PublicIP {
		nic.PublicIpAddress = &computepb.PublicIPAddress{}
	}
	for _, sg := range t.SecurityGroupIDs {
		nic.SecurityGroups = append(nic.SecurityGroups, &computepb.SecurityGroup{Id: sg})
	}
	spec := &computepb.InstanceSpec{
		Resources: &computepb.ResourcesSpec{Platform: t.Platform, Size: &computepb.ResourcesSpec_Preset{Preset: t.Preset}},
		BootDisk: &computepb.AttachedDiskSpec{
			AttachMode: computepb.AttachedDiskSpec_READ_WRITE,
			Type:       &computepb.AttachedDiskSpec_ManagedDisk{ManagedDisk: &computepb.ManagedDisk{Name: name + bootDiskSuffix, Spec: disk}},
		},
		NetworkInterfaces: []*computepb.NetworkInterfaceSpec{nic},
		ServiceAccountId:  t.ServiceAccountID,
		CloudInitUserData: t.CloudInitUserData,
	}
	if t.GpuClusterID != "" {
		spec.GpuCluster = &computepb.InstanceGpuClusterSpec{Id: t.GpuClusterID}
	}
	if t.Preemptible {
		spec.Preemptible = &computepb.PreemptibleSpec{OnPreemption: computepb.PreemptibleSpec_STOP}
		spec.RecoveryPolicy = computepb.InstanceRecoveryPolicy_FAIL
	}
	for i, fs := range t.Filesystems {
		if fs.ReadOnly {
			return nil, errs.BadRequest(errs.Violation{
				Field: fmt.Sprintf("filesystems[%d].attach_mode", i), Message: "READ_ONLY is not supported: compute attaches filesystems READ_WRITE only",
			})
		}
		spec.Filesystems = append(spec.Filesystems, &computepb.AttachedFilesystemSpec{
			AttachMode: computepb.AttachedFilesystemSpec_READ_WRITE, MountTag: fs.MountTag,
			Type: &computepb.AttachedFilesystemSpec_ExistingFilesystem{ExistingFilesystem: &computepb.ExistingFilesystem{Id: fs.ID}},
		})
	}
	return spec, nil
}

// mapNodeFields renames the violation fields of an instance-spec error to
// the mk8s NodeTemplate's (spec.resources.preset becomes resources.preset,
// the boot disk's spec.size becomes boot_disk.size_bytes, ...).
func mapNodeFields(err error) error {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		return err
	}
	out := *e
	out.Violations = make([]errs.Violation, len(e.Violations))
	for i, v := range e.Violations {
		out.Violations[i] = errs.Violation{Field: nodeField(v.Field), Message: v.Message}
	}
	return &out
}

func nodeField(f string) string {
	if rest, ok := strings.CutPrefix(f, "spec.boot_disk.managed_disk.spec."); ok {
		switch {
		case rest == "size":
			rest = "size_bytes"
		case strings.HasPrefix(rest, "source"):
			rest = "image_family"
		case strings.HasPrefix(rest, "disk_encryption"):
			rest = "encrypted"
		}
		return "boot_disk." + rest
	}
	return strings.TrimPrefix(f, "spec.")
}
