package domain

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// GpuClusterMembers implements GpuClusterMembership over a GpuClusterRepository.
type GpuClusterMembers struct {
	clusters GpuClusterRepository
	fabrics  FabricCatalog
}

var _ GpuClusterMembership = (*GpuClusterMembers)(nil)

// NewGpuClusterMembers returns GpuClusterMembers over clusters, checking
// platforms against fabrics.
func NewGpuClusterMembers(clusters GpuClusterRepository, fabrics FabricCatalog) (*GpuClusterMembers, error) {
	if clusters == nil {
		return nil, errors.New("compute: gpu cluster membership needs a gpu cluster repository")
	}
	return &GpuClusterMembers{clusters: clusters, fabrics: fabrics}, nil
}

const gpuClusterField = "spec.gpu_cluster.id"

// Join implements GpuClusterMembership.
func (m *GpuClusterMembers) Join(ctx context.Context, clusterID, instanceID, projectID, platform string) ([]string, error) {
	if clusterID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: gpuClusterField, Message: "required"})
	}
	if instanceID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "instance id required to join a gpu cluster"})
	}
	var path []string
	_, err := m.clusters.Mutate(ctx, clusterID, func(c *computepb.GpuCluster) error {
		if c.GetMetadata().GetParentId() != projectID {
			return errs.BadRequest(errs.Violation{
				Field: gpuClusterField, Message: fmt.Sprintf("gpu cluster %s is not in project %s", clusterID, projectID),
			})
		}
		fabric := c.GetSpec().GetInfinibandFabric()
		f, ok := m.fabrics.Lookup(fabric)
		if !ok {
			return errs.BadState(clusterID, fmt.Sprintf("gpu cluster %s is on unknown fabric %q", clusterID, fabric))
		}
		if !f.Allows(platform) {
			return errs.BadRequest(errs.Violation{Field: gpuClusterField, Message: fmt.Sprintf(
				"fabric %s of gpu cluster %s accepts only platform %s, not %q", fabric, clusterID, f.platformList(), platform)})
		}
		path = addMember(c, instanceID)
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil, &errs.Error{
			Kind: errs.KindNotFound, ResourceID: clusterID,
			Message: fmt.Sprintf("%s: gpu cluster %s not found", gpuClusterField, clusterID),
		}
	}
	if err != nil {
		return nil, err
	}
	return path, nil
}

// Fabric implements GpuClusterMembership.
func (m *GpuClusterMembers) Fabric(ctx context.Context, clusterID string) (string, error) {
	c, err := m.clusters.Get(ctx, clusterID)
	if err != nil {
		return "", err
	}
	return c.GetSpec().GetInfinibandFabric(), nil
}

// Leave implements GpuClusterMembership.
func (m *GpuClusterMembers) Leave(ctx context.Context, clusterID, instanceID string) error {
	if clusterID == "" {
		return nil
	}
	_, err := m.clusters.Mutate(ctx, clusterID, func(c *computepb.GpuCluster) error {
		removeMember(c, instanceID)
		return nil
	})
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

// addMember records instanceID in c's status and returns its topology path.
// An existing member keeps its path.
func addMember(c *computepb.GpuCluster, instanceID string) []string {
	if c.GetStatus() == nil {
		c.Status = &computepb.GpuClusterStatus{}
	}
	st := c.GetStatus()
	if !slices.Contains(st.GetInstances(), instanceID) {
		st.Instances = append(st.Instances, instanceID)
	}
	topo := st.GetInfinibandTopologyPath()
	if topo == nil {
		topo = &computepb.GpuClusterStatusInfinibandTopologyPath{}
		st.Topology = &computepb.GpuClusterStatus_InfinibandTopologyPath{InfinibandTopologyPath: topo}
	}
	for _, p := range topo.GetInstances() {
		if p.GetInstanceId() == instanceID {
			return slices.Clone(p.GetPath())
		}
	}
	path := TopologyPath(c.GetSpec().GetInfinibandFabric(), instanceID)
	topo.Instances = append(topo.Instances, &computepb.GpuClusterStatusInfinibandTopologyPathInstance{
		InstanceId: instanceID, Path: slices.Clone(path),
	})
	return path
}

// removeMember drops instanceID from c's status. The topology oneof is
// cleared with the last member, as on a new cluster.
func removeMember(c *computepb.GpuCluster, instanceID string) {
	st := c.GetStatus()
	if st == nil {
		return
	}
	st.Instances = slices.DeleteFunc(st.GetInstances(), func(id string) bool { return id == instanceID })
	if topo := st.GetInfinibandTopologyPath(); topo != nil {
		topo.Instances = slices.DeleteFunc(topo.GetInstances(), func(p *computepb.GpuClusterStatusInfinibandTopologyPathInstance) bool {
			return p.GetInstanceId() == instanceID
		})
		if len(topo.GetInstances()) == 0 {
			st.Topology = nil
		}
	}
}

// TopologyPath is the synthetic, deterministic InfiniBand path of an instance
// on fabric: the fabric, a spine and a leaf switch derived from the instance
// ID. Nothing is simulated; the path only gives clients a stable,
// realistic-looking value.
func TopologyPath(fabric, instanceID string) []string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instanceID)) // hash.Hash.Write never fails
	n := h.Sum32()
	return []string{
		fabric,
		fmt.Sprintf("%s-spine-%02d", fabric, n%4),
		fmt.Sprintf("%s-leaf-%03d", fabric, n%64),
	}
}
