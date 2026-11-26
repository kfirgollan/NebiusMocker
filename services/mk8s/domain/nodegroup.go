package domain

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// CreateNodeGroupCmd is the input of CreateNodeGroup.
type CreateNodeGroupCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *mk8spb.NodeGroupSpec
	Request  proto.Message
}

// UpdateNodeGroupCmd is the input of UpdateNodeGroup (full replace guarded
// by a reset mask).
type UpdateNodeGroupCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *mk8spb.NodeGroupSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// GetNodeGroup returns a node group.
func (s *Service) GetNodeGroup(ctx context.Context, id string) (*mk8spb.NodeGroup, error) {
	return s.d.NodeGroups.Get(ctx, id)
}

// GetNodeGroupByName returns the node group with the name in a cluster.
func (s *Service) GetNodeGroupByName(ctx context.Context, clusterID, name string) (*mk8spb.NodeGroup, error) {
	return s.d.NodeGroups.GetByName(ctx, clusterID, name)
}

// ListNodeGroups returns one page of a cluster's node groups in creation order.
func (s *Service) ListNodeGroups(ctx context.Context, clusterID string, size int64, pageToken string) (Page[*mk8spb.NodeGroup], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*mk8spb.NodeGroup]{}, err
	}
	return s.d.NodeGroups.List(ctx, clusterID, n, pageToken)
}

// ngPlan is a validated node group create or update.
type ngPlan struct {
	ng *mk8spb.NodeGroup // what to store
	// changed is false for an update that changes nothing.
	changed bool
	// rollout recreates every node (template or version change).
	rollout nodeRollout
	// claim is what the nodes this plan adds would consume (nil: none).
	claim *peer.QuotaClaim
}

// CreateNodeGroup creates a node group in a RUNNING cluster and its nodes.
func (s *Service) CreateNodeGroup(ctx context.Context, cmd CreateNodeGroupCmd) (*operations.Operation, error) {
	id, err := s.d.IDs.New(KindNodeGroup)
	if err != nil {
		return nil, fmt.Errorf("new node group id: %w", err)
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Create node group", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			p, err := s.planCreate(ctx, cmd.Metadata, cmd.Spec, "", &checker{})
			if err != nil {
				return err
			}
			p.ng.Metadata, err = s.newMetadata(id, cmd.Metadata)
			if err != nil {
				return err
			}
			if err := s.checkNodeQuota(ctx, p); err != nil {
				return err
			}
			return s.d.NodeGroups.Create(ctx, p.ng)
		},
		Work: s.reconcileWork(id, keepNodes),
	})
}

// planCreate validates a new node group in its cluster and builds it
// (without metadata). replaces is a node group a recreate deletes first.
func (s *Service) planCreate(ctx context.Context, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, replaces string, chk *checker) (*ngPlan, error) {
	if err := chk.check(checkNewMetadata(md)); err != nil {
		return nil, err
	}
	spec = proto.CloneOf(spec)
	strategy, err := s.checkSpec(spec, chk)
	if err != nil {
		return nil, err
	}
	parent := md.GetParentId()
	if parent == "" || chk.isUnknown("metadata.parent_id") {
		return nil, chk.stop(errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"}))
	}
	c, err := s.d.Clusters.Get(ctx, parent)
	if err := chk.stop(err); err != nil {
		return nil, err
	}
	if st := c.GetStatus().GetState(); st != mk8spb.ClusterStatus_RUNNING {
		if err := chk.check(errs.BadState(parent, fmt.Sprintf("cluster is %s: node groups can be created only in a RUNNING cluster", st))); err != nil {
			return nil, err
		}
	}
	if err := chk.check(s.nameFree(ctx, parent, md.GetName(), replaces)); err != nil {
		return nil, err
	}
	nv := createNodeVersion(spec.GetVersion(), s.versions.clusterMinor(c))
	res, err := s.checkWithCluster(ctx, c, spec, nv, chk)
	if err != nil {
		return nil, err
	}
	target := targetOnCreate(spec)
	return &ngPlan{changed: true, claim: nodeClaim(c, spec.GetTemplate(), res, target), ng: &mk8spb.NodeGroup{Spec: spec, Status: &mk8spb.NodeGroupStatus{
		State: mk8spb.NodeGroupStatus_PROVISIONING, Reconciling: true,
		TargetNodeCount: target, Version: nodeStatusVersion(c, nv.minor, nv.image), Strategy: strategy,
	}}}, nil
}

// checkSpec runs the checks that need no cluster: unsupported features and
// the deployment strategy, whose effective value it returns.
func (s *Service) checkSpec(spec *mk8spb.NodeGroupSpec, chk *checker) (*mk8spb.NodeGroupDeploymentStrategy, error) {
	if spec.GetTemplate() == nil {
		if err := chk.stop(errs.BadRequest(errs.Violation{Field: "spec.template", Message: "required"})); err != nil {
			return nil, err
		}
	}
	if err := chk.check(checkNodeGroupSupported(spec)); err != nil {
		return nil, err
	}
	strategy, err := effectiveStrategy(spec.GetStrategy())
	return strategy, chk.check(err)
}

// nameFree fails when another node group of the cluster has the name.
func (s *Service) nameFree(ctx context.Context, clusterID, name, replaces string) error {
	g, err := s.d.NodeGroups.GetByName(ctx, clusterID, name)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return nil
	case err != nil:
		return err
	case g.GetMetadata().GetId() == replaces:
		return nil
	default:
		return errs.AlreadyExists(g.GetMetadata().GetId(), fmt.Sprintf("cluster %s already has a node group named %q", clusterID, name))
	}
}

// createNodeVersion is the version a new node group runs: the spec's, or
// the cluster's minor; the latest node image unless the spec pins one.
func createNodeVersion(spec, clusterMinor string) nodeVersion {
	nv, ok := parseNodeVersion(spec)
	if !ok { // "" (the format is checked by buf.validate)
		nv = nodeVersion{minor: clusterMinor}
	}
	if nv.image == 0 {
		nv.image = latestNodeImage
	}
	return nv
}

// checkWithCluster normalizes the template in spec and runs the checks that
// need the cluster: version skew, subnets, node image and the compute
// template.
func (s *Service) checkWithCluster(ctx context.Context, c *mk8spb.Cluster, spec *mk8spb.NodeGroupSpec, nv nodeVersion, chk *checker) (peer.NodeResources, error) {
	if err := chk.check(s.versions.checkNodeVersion(nv, s.versions.clusterMinor(c))); err != nil {
		return peer.NodeResources{}, err
	}
	if spec.GetTemplate() == nil {
		return peer.NodeResources{}, nil
	}
	t := spec.GetTemplate()
	normalizeTemplate(t, c.GetSpec().GetControlPlane().GetSubnetId())
	if err := chk.check(s.checkSubnets(ctx, c, t)); err != nil {
		return peer.NodeResources{}, err
	}
	family, err := checkImage(t)
	if err != nil || chk.anyUnknown("spec.template") {
		return peer.NodeResources{}, chk.check(err)
	}
	cn, err := s.computeNodes("spec.template.resources")
	if err != nil {
		return peer.NodeResources{}, chk.check(err)
	}
	res, err := cn.CheckTemplate(ctx, c.GetMetadata().GetParentId(), toPeerTemplate(t, family))
	return res, chk.check(prefixFields(err, "spec.template."))
}

// checkSubnets requires every node subnet to be in the cluster's network.
func (s *Service) checkSubnets(ctx context.Context, c *mk8spb.Cluster, t *mk8spb.NodeTemplate) error {
	clusterSubnet := c.GetSpec().GetControlPlane().GetSubnetId()
	n, err := s.nics("spec.template.network_interfaces")
	if err != nil {
		return err
	}
	csn, err := n.Subnet(ctx, clusterSubnet)
	if err != nil {
		return fmt.Errorf("cluster subnet %s: %w", clusterSubnet, err)
	}
	for i, nic := range t.GetNetworkInterfaces() {
		if nic.GetSubnetId() == clusterSubnet {
			continue
		}
		field := fmt.Sprintf("spec.template.network_interfaces[%d].subnet_id", i)
		sn, err := n.Subnet(ctx, nic.GetSubnetId())
		switch {
		case errs.IsKind(err, errs.KindNotFound):
			return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("subnet %s not found", nic.GetSubnetId())})
		case err != nil:
			return err
		case sn.NetworkID != csn.NetworkID:
			return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
				"subnet %s is in network %s; node subnets must be in the cluster's network %s", sn.ID, sn.NetworkID, csn.NetworkID)})
		}
	}
	return nil
}

// UpdateNodeGroup replaces the mutable parts of a node group. Size changes
// scale it; template (other than taints and node labels) and version
// changes recreate its nodes.
func (s *Service) UpdateNodeGroup(ctx context.Context, cmd UpdateNodeGroupCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	var p *ngPlan
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Update node group", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.rolloutDetached(),
		Prepare: func(ctx context.Context) error {
			var err error
			if p, err = s.planUpdate(ctx, cmd.Metadata, cmd.Spec, cmd.ResetMask, &checker{}); err != nil || !p.changed {
				return err
			}
			if err := s.checkNodeQuota(ctx, p); err != nil {
				return err
			}
			return s.d.NodeGroups.Update(ctx, p.ng)
		},
		Work: func(ctx context.Context) error {
			if !p.changed {
				return nil
			}
			return s.reconcileWork(id, p.rollout)(ctx)
		},
	})
}

// planUpdate validates an update of a stored node group and builds the
// result.
func (s *Service) planUpdate(ctx context.Context, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, resetMask *mask.Mask, chk *checker) (*ngPlan, error) {
	stored, updated, err := s.patchNodeGroup(ctx, md, spec, resetMask, chk)
	if err != nil {
		return nil, err
	}
	strategy, err := s.checkSpec(updated.GetSpec(), chk)
	if err != nil {
		return nil, err
	}
	c, err := s.d.Clusters.Get(ctx, stored.GetMetadata().GetParentId())
	if err := chk.stop(err); err != nil {
		return nil, err
	}
	nv := updateNodeVersion(stored, updated.GetSpec(), s.versions.clusterMinor(c))
	res, err := s.checkWithCluster(ctx, c, updated.GetSpec(), nv, chk)
	if err != nil {
		return nil, err
	}
	st := updated.GetStatus()
	cur := statusNode(stored.GetStatus().GetVersion())
	p := &ngPlan{ng: updated}
	if nv != cur || templateNeedsRollout(stored.GetSpec().GetTemplate(), updated.GetSpec().GetTemplate()) {
		p.rollout = recreateNodes
	}
	st.Strategy = strategy
	st.TargetNodeCount = targetOnUpdate(updated.GetSpec(), stored.GetStatus().GetNodeCount())
	p.claim = nodeClaim(c, updated.GetSpec().GetTemplate(), res, st.GetTargetNodeCount()-stored.GetStatus().GetTargetNodeCount())
	st.Version = nodeStatusVersion(c, nv.minor, nv.image)
	if proto.Equal(updated, stored) {
		return p, nil // nothing to do: keep resource_version and updated_at
	}
	p.changed = true
	if !proto.Equal(updated.GetSpec(), stored.GetSpec()) || !proto.Equal(updated.GetMetadata(), stored.GetMetadata()) {
		s.bump(updated.GetMetadata())
	}
	st.Reconciling = true
	if p.rollout == recreateNodes {
		st.OutdatedNodeCount = st.GetNodeCount()
	}
	return p, nil
}

// patchNodeGroup loads a node group and applies an update to a copy.
func (s *Service) patchNodeGroup(ctx context.Context, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, resetMask *mask.Mask, chk *checker) (stored, updated *mk8spb.NodeGroup, err error) {
	stored, err = s.d.NodeGroups.Get(ctx, md.GetId())
	if err := chk.stop(err); err != nil {
		return nil, nil, err
	}
	if err := chk.check(checkUpdatable(stored, md)); err != nil {
		return nil, nil, err
	}
	if stored.GetStatus().GetState() == mk8spb.NodeGroupStatus_DELETING {
		return nil, nil, chk.stop(errs.BadState(md.GetId(), "the node group is being deleted"))
	}
	updated, err = patch(stored, &mk8spb.NodeGroup{
		Metadata: &commonpb.ResourceMetadata{Name: md.GetName(), Labels: md.GetLabels()},
		Spec:     spec,
	}, resetMask)
	if err := chk.stop(err); err != nil {
		return nil, nil, err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &mk8spb.NodeGroupSpec{}
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if updated.GetStatus() == nil {
		updated.Status = &mk8spb.NodeGroupStatus{}
	}
	return stored, updated, nil
}

// updateNodeVersion is the version a node group runs after an update. An
// empty spec version that stays empty keeps the current version (a cluster
// upgrade does not move node groups); a new minor without an image suffix
// gets the latest image, the same minor keeps its image.
func updateNodeVersion(stored *mk8spb.NodeGroup, spec *mk8spb.NodeGroupSpec, clusterMinor string) nodeVersion {
	cur := statusNode(stored.GetStatus().GetVersion())
	nv, ok := parseNodeVersion(spec.GetVersion())
	switch {
	case ok:
	case stored.GetSpec().GetVersion() == "" && cur.minor != "":
		return cur
	default:
		nv = nodeVersion{minor: clusterMinor}
	}
	if nv.image == 0 {
		nv.image = latestNodeImage
		if nv.minor == cur.minor {
			nv.image = cur.image
		}
	}
	return nv
}

// rolloutDetached reports whether operations that may roll nodes out run
// detached: for a slow backend, or when batches wait RolloutStep on the
// clock (inline work would block the RPC).
func (s *Service) rolloutDetached() bool {
	return s.d.Config.Detached || s.d.Config.RolloutStep > 0
}

// UpgradeNodeGroup moves a node group to the latest node image of its
// minor (and the cluster's patch version), recreating its nodes.
func (s *Service) UpgradeNodeGroup(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	rollout := false
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Upgrade node group", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.rolloutDetached(),
		Prepare: func(ctx context.Context) error {
			var err error
			rollout, err = s.prepareUpgrade(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			if !rollout {
				return nil
			}
			return s.reconcileWork(id, recreateNodes)(ctx)
		},
	})
}

func (s *Service) prepareUpgrade(ctx context.Context, id string) (bool, error) {
	g, err := s.d.NodeGroups.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if g.GetStatus().GetState() == mk8spb.NodeGroupStatus_DELETING {
		return false, errs.BadState(id, "the node group is being deleted")
	}
	c, err := s.d.Clusters.Get(ctx, g.GetMetadata().GetParentId())
	if err != nil {
		return false, err
	}
	next := nodeStatusVersion(c, statusNode(g.GetStatus().GetVersion()).minor, latestNodeImage)
	if next == g.GetStatus().GetVersion() {
		return false, nil
	}
	g.Status.Version, g.Status.Reconciling, g.Status.OutdatedNodeCount = next, true, g.GetStatus().GetNodeCount()
	return true, s.d.NodeGroups.Update(ctx, g)
}

// DeleteNodeGroup deletes a node group and its nodes.
func (s *Service) DeleteNodeGroup(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Delete node group", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			g, err := s.d.NodeGroups.Get(ctx, id)
			if err != nil {
				return err
			}
			g.Status.State, g.Status.Reconciling = mk8spb.NodeGroupStatus_DELETING, true
			return s.d.NodeGroups.Update(ctx, g)
		},
		Work: func(ctx context.Context) error {
			g, err := s.d.NodeGroups.Get(ctx, id)
			if err != nil {
				return err
			}
			unlock := s.clusterLock(g.GetMetadata().GetParentId())
			defer unlock()
			return s.removeNodeGroupLocked(ctx, g)
		},
	})
}

// isKubernetesLabel reports a system label key that node templates may not
// set (node_group.proto: "kubernetes.io" and "k8s.io" labels are ignored).
func isKubernetesLabel(key string) bool {
	return strings.Contains(key, "kubernetes.io") || strings.Contains(key, "k8s.io")
}
