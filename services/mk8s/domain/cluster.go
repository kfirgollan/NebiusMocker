package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// defaultEtcdClusterSize is the etcd size of a spec that sets 0
// (cluster.proto: "3 by default").
const defaultEtcdClusterSize = 3

const subnetField = "spec.control_plane.subnet_id"

// CreateClusterCmd is the input of CreateCluster.
type CreateClusterCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *mk8spb.ClusterSpec
	Request  proto.Message
}

// UpdateClusterCmd is the input of UpdateCluster (full replace guarded by a
// reset mask).
type UpdateClusterCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *mk8spb.ClusterSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// GetCluster returns a cluster.
func (s *Service) GetCluster(ctx context.Context, id string) (*mk8spb.Cluster, error) {
	return s.d.Clusters.Get(ctx, id)
}

// GetClusterByName returns the cluster with the name in a project.
func (s *Service) GetClusterByName(ctx context.Context, parentID, name string) (*mk8spb.Cluster, error) {
	return s.d.Clusters.GetByName(ctx, parentID, name)
}

// ListClusters returns one page of a project's clusters in creation order.
func (s *Service) ListClusters(ctx context.Context, parentID string, size int64, pageToken string) (Page[*mk8spb.Cluster], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*mk8spb.Cluster]{}, err
	}
	return s.d.Clusters.List(ctx, parentID, n, pageToken)
}

// ControlPlaneVersions lists the catalog: every version is public and
// supported.
func (s *Service) ControlPlaneVersions() []*mk8spb.ClusterControlPlaneVersion {
	out := make([]*mk8spb.ClusterControlPlaneVersion, 0, len(s.versions.versions))
	for _, v := range s.versions.versions {
		out = append(out, &mk8spb.ClusterControlPlaneVersion{Version: v})
	}
	return out
}

// Cluster resolves a cluster for other areas (peer.MK8sClusters).
func (s *Service) Cluster(ctx context.Context, id string) (peer.MK8sCluster, error) {
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return peer.MK8sCluster{}, err
	}
	md := c.GetMetadata()
	return peer.MK8sCluster{
		ID: md.GetId(), ProjectID: md.GetParentId(), Name: md.GetName(),
		Running: c.GetStatus().GetState() == mk8spb.ClusterStatus_RUNNING,
	}, nil
}

// CreateCluster creates a cluster: PROVISIONING until the backend's control
// plane is ready, then RUNNING with its endpoints and CA.
func (s *Service) CreateCluster(ctx context.Context, cmd CreateClusterCmd) (*operations.Operation, error) {
	id, err := s.d.IDs.New(KindCluster)
	if err != nil {
		return nil, fmt.Errorf("new cluster id: %w", err)
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Create cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error { return s.prepareCreateCluster(ctx, id, cmd) },
		Work:    func(ctx context.Context) error { return s.ensureCluster(ctx, id) },
	})
}

func (s *Service) prepareCreateCluster(ctx context.Context, id string, cmd CreateClusterCmd) error {
	md, err := s.newMetadata(id, cmd.Metadata)
	if err != nil {
		return err
	}
	if err := s.checkProject(ctx, md.GetParentId()); err != nil {
		return err
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &mk8spb.ClusterSpec{} // cluster_service.proto: no spec means the default network
	}
	if spec.GetControlPlane() == nil {
		spec.ControlPlane = &mk8spb.ControlPlaneSpec{}
	}
	cp := spec.GetControlPlane()
	if err := s.checkControlPlane(cp, ""); err != nil {
		return err
	}
	sn, err := s.resolveSubnet(ctx, md.GetParentId(), cp.GetSubnetId())
	if err != nil {
		return err
	}
	cp.SubnetId = sn.ID
	c := &mk8spb.Cluster{Metadata: md, Spec: spec, Status: &mk8spb.ClusterStatus{
		State: mk8spb.ClusterStatus_PROVISIONING, Reconciling: true,
		ControlPlane: &mk8spb.ControlPlaneStatus{EtcdClusterSize: etcdSize(cp)},
	}}
	return s.storeNewCluster(ctx, c, sn.NetworkID)
}

// storeNewCluster binds the cluster to its network and stores it.
func (s *Service) storeNewCluster(ctx context.Context, c *mk8spb.Cluster, networkID string) error {
	nets, ok := s.d.Peers.Networks()
	if !ok {
		return notEnabled(subnetField, "vpc")
	}
	id := c.GetMetadata().GetId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := nets.Bind(ctx, networkID, id); err != nil {
		return fmt.Errorf("%s: %w", subnetField, err)
	}
	if err := s.d.Bindings.Set(ctx, id, networkID); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id))
	}
	if err := s.d.Clusters.Create(ctx, c); err != nil {
		return errors.Join(err, nets.Unbind(ctx, networkID, id), s.d.Bindings.Delete(ctx, id))
	}
	return nil
}

// checkControlPlane validates the control-plane spec; current is the
// running version of an existing cluster ("" on create).
func (s *Service) checkControlPlane(cp *mk8spb.ControlPlaneSpec, current string) error {
	if err := checkClusterSupported(cp); err != nil {
		return err
	}
	v := cp.GetVersion()
	if v == "" {
		v = current
		if v == "" {
			v = s.versions.newest()
		}
	} else if err := s.versions.require("spec.control_plane.version", v); err != nil {
		return err
	}
	if current != "" {
		if err := checkClusterUpgrade(current, v); err != nil {
			return err
		}
	}
	return s.d.Backend.CheckVersion(v)
}

// resolveSubnet returns the control-plane subnet: the project's default
// subnet when subnetID is empty.
func (s *Service) resolveSubnet(ctx context.Context, projectID, subnetID string) (peer.Subnet, error) {
	if subnetID == "" {
		ds, ok := s.d.Peers.DefaultSubnets()
		if !ok {
			return peer.Subnet{}, notEnabled(subnetField, "vpc")
		}
		sn, err := ds.DefaultSubnet(ctx, projectID)
		if errs.IsKind(err, errs.KindNotFound) {
			return peer.Subnet{}, errs.BadRequest(errs.Violation{Field: subnetField, Message: fmt.Sprintf(
				"project %s has no default subnet: set subnet_id", projectID)})
		}
		return sn, err
	}
	n, err := s.nics(subnetField)
	if err != nil {
		return peer.Subnet{}, err
	}
	sn, err := n.Subnet(ctx, subnetID)
	if err != nil {
		return peer.Subnet{}, fmt.Errorf("%s: %w", subnetField, err)
	}
	if sn.ProjectID != projectID {
		return peer.Subnet{}, errs.BadRequest(errs.Violation{Field: subnetField, Message: fmt.Sprintf(
			"subnet %s belongs to %s, not to the cluster's project %s", subnetID, sn.ProjectID, projectID)})
	}
	return sn, nil
}

func etcdSize(cp *mk8spb.ControlPlaneSpec) int64 {
	if n := cp.GetEtcdClusterSize(); n != 0 {
		return n
	}
	return defaultEtcdClusterSize
}

// ensureCluster reconciles the backend control plane with the stored
// cluster and records the result in its status.
func (s *Service) ensureCluster(ctx context.Context, id string) error {
	unlock := s.clusterLock(id)
	defer unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	ep, err := s.d.Backend.EnsureCluster(ctx, s.backendCluster(c))
	if err != nil {
		c.Status.Events = []*commonpb.RecurrentResourceEvent{s.event(commonpb.ResourceEvent_ERROR, "ProvisioningFailed", err.Error())}
		return errors.Join(err, s.d.Clusters.Update(ctx, c))
	}
	c.Status = &mk8spb.ClusterStatus{
		State: mk8spb.ClusterStatus_RUNNING,
		ControlPlane: &mk8spb.ControlPlaneStatus{
			Version: ep.KubernetesVersion + cpInfraSuffix,
			Endpoints: &mk8spb.ControlPlaneStatusEndpoints{
				PublicEndpoint: ep.PublicEndpoint, PrivateEndpoint: ep.PrivateEndpoint,
			},
			EtcdClusterSize: etcdSize(c.GetSpec().GetControlPlane()),
			Auth:            &mk8spb.ControlPlaneStatusAuth{ClusterCaCertificate: ep.CACertificate},
		},
	}
	return s.d.Clusters.Update(ctx, c)
}

func (s *Service) backendCluster(c *mk8spb.Cluster) BackendCluster {
	cp := c.GetSpec().GetControlPlane()
	bc := BackendCluster{
		ID: c.GetMetadata().GetId(), ProjectID: c.GetMetadata().GetParentId(), Name: c.GetMetadata().GetName(),
		Version: s.versions.clusterMinor(c), EtcdClusterSize: etcdSize(cp),
	}
	if cidrs := c.GetSpec().GetKubeNetwork().GetServiceCidrs(); len(cidrs) > 0 {
		bc.ServiceCIDR = cidrs[0]
	}
	if pe := cp.GetEndpoints().GetPublicEndpoint(); pe != nil {
		bc.PublicEndpoint = true
		bc.AllowedCIDRs = append([]string(nil), pe.GetAllowedCidrs()...)
	}
	return bc
}

// UpdateCluster replaces the mutable parts of a cluster and reconciles the
// control plane (a version upgrade, endpoints).
func (s *Service) UpdateCluster(ctx context.Context, cmd UpdateClusterCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	changed := false
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Update cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			var err error
			changed, err = s.prepareUpdateCluster(ctx, cmd)
			return err
		},
		Work: func(ctx context.Context) error {
			if !changed {
				return nil
			}
			return s.ensureCluster(ctx, id)
		},
	})
}

func (s *Service) prepareUpdateCluster(ctx context.Context, cmd UpdateClusterCmd) (bool, error) {
	stored, err := s.d.Clusters.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return false, err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return false, err
	}
	if stored.GetStatus().GetState() == mk8spb.ClusterStatus_DELETING {
		return false, errs.BadState(stored.GetMetadata().GetId(), "the cluster is being deleted")
	}
	updated, err := patch(stored, &mk8spb.Cluster{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return false, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if updated.GetSpec().GetControlPlane() == nil {
		updated.Spec = &mk8spb.ClusterSpec{ControlPlane: &mk8spb.ControlPlaneSpec{}, KubeNetwork: updated.GetSpec().GetKubeNetwork()}
	}
	if err := keepClusterImmutable(stored.GetSpec(), updated.GetSpec()); err != nil {
		return false, err
	}
	cp := updated.GetSpec().GetControlPlane()
	if err := s.checkControlPlane(cp, s.versions.clusterMinor(stored)); err != nil {
		return false, err
	}
	if proto.Equal(updated, stored) {
		return false, nil
	}
	s.bump(updated.GetMetadata())
	updated.Status.Reconciling = true
	return true, s.d.Clusters.Update(ctx, updated)
}

// keepClusterImmutable restores the IMMUTABLE fields an update leaves empty
// and fails when one is changed.
func keepClusterImmutable(prev, next *mk8spb.ClusterSpec) error {
	var v []errs.Violation
	cp := next.GetControlPlane()
	switch sn := cp.GetSubnetId(); {
	case sn == "":
		cp.SubnetId = prev.GetControlPlane().GetSubnetId()
	case sn != prev.GetControlPlane().GetSubnetId():
		v = append(v, errs.Violation{Field: subnetField, Message: "field is immutable"})
	}
	prevCIDRs, nextCIDRs := prev.GetKubeNetwork().GetServiceCidrs(), next.GetKubeNetwork().GetServiceCidrs()
	switch {
	case len(nextCIDRs) == 0 && len(prevCIDRs) > 0:
		next.KubeNetwork = proto.CloneOf(prev.GetKubeNetwork())
	case !slices.Equal(prevCIDRs, nextCIDRs):
		v = append(v, errs.Violation{Field: "spec.kube_network.service_cidrs", Message: "field is immutable"})
	}
	return violations(v)
}

// DeleteCluster deletes a cluster with its node groups (and their nodes)
// and whatever is installed into it.
func (s *Service) DeleteCluster(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			c, err := s.d.Clusters.Get(ctx, id)
			if err != nil {
				return err
			}
			c.Status.State, c.Status.Reconciling = mk8spb.ClusterStatus_DELETING, true
			return s.d.Clusters.Update(ctx, c)
		},
		Work: func(ctx context.Context) error { return s.removeCluster(ctx, id) },
	})
}

func (s *Service) removeCluster(ctx context.Context, id string) error {
	unlock := s.clusterLock(id)
	defer s.forgetLock(id)
	defer unlock()
	c, err := s.d.Clusters.Get(ctx, id)
	if err != nil {
		return err
	}
	groups, err := s.d.NodeGroups.All(ctx, id)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := s.removeNodeGroupLocked(ctx, g); err != nil {
			return fmt.Errorf("delete node group %s: %w", g.GetMetadata().GetId(), err)
		}
	}
	if cr, ok := s.d.Peers.ClusterReleases(); ok {
		if err := cr.DeleteClusterReleases(ctx, id); err != nil {
			return fmt.Errorf("delete the cluster's releases: %w", err)
		}
	}
	if err := s.d.Backend.DeleteCluster(ctx, id); err != nil {
		return err
	}
	if err := s.unbindNetwork(ctx, c); err != nil {
		return err
	}
	return s.d.Clusters.Delete(ctx, id)
}

// unbindNetwork releases the network recorded when the cluster was created,
// whether or not its subnet still exists.
func (s *Service) unbindNetwork(ctx context.Context, c *mk8spb.Cluster) error {
	id := c.GetMetadata().GetId()
	networkID, err := s.d.Bindings.Get(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if nets, ok := s.d.Peers.Networks(); ok {
		if err := nets.Unbind(ctx, networkID, id); err != nil {
			return err
		}
	}
	return s.d.Bindings.Delete(ctx, id)
}
