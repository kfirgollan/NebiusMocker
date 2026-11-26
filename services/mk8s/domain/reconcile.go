package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Labels the service puts on node instances and Kubernetes nodes; they win
// over the template's.
const (
	LabelClusterID     = "mk8s.nebius.ai/cluster-id"
	LabelNodeGroupID   = "mk8s.nebius.ai/node-group-id"
	LabelNodeGroupName = "mk8s.nebius.ai/node-group-name"
)

const (
	maxNodeNamePrefix = 40
	nodeSuffixLen     = 5
	nodeNameAttempts  = 5
)

const (
	gib = 1 << 30
	mib = 1 << 20
	kib = 1 << 10
)

// nodeRollout says whether a reconciliation keeps the existing nodes or
// recreates them (a template or version change).
type nodeRollout int

const (
	keepNodes nodeRollout = iota
	recreateNodes
)

var nodeNameRE = regexp.MustCompile(`[^a-z0-9-]+`)

// reconcileWork is operation Work that makes a node group's nodes match its
// stored spec and status.target_node_count.
func (s *Service) reconcileWork(id string, mode nodeRollout) func(context.Context) error {
	return func(ctx context.Context) error {
		g, err := s.d.NodeGroups.Get(ctx, id)
		if err != nil {
			return err
		}
		unlock := s.clusterLock(g.GetMetadata().GetParentId())
		defer unlock()
		if g, err = s.d.NodeGroups.Get(ctx, id); err != nil { // the cluster may have been deleted meanwhile
			return err
		}
		c, err := s.d.Clusters.Get(ctx, g.GetMetadata().GetParentId())
		if err != nil {
			return err
		}
		return s.reconcile(ctx, c, g, mode)
	}
}

// reconcile creates and deletes node instances to reach the target count,
// hands the nodes to the backend and records their state. The cluster lock
// is held.
func (s *Service) reconcile(ctx context.Context, c *mk8spb.Cluster, g *mk8spb.NodeGroup, mode nodeRollout) error {
	cn, err := s.computeNodes("spec.template.resources")
	if err != nil {
		return err
	}
	id := g.GetMetadata().GetId()
	nodes, err := cn.ListNodes(ctx, id)
	if err != nil {
		return err
	}
	if mode == recreateNodes {
		fresh, ready, err := s.rollingUpdate(ctx, cn, c, g, nodes)
		if err != nil {
			return s.failNodes(ctx, g, fresh, err)
		}
		s.setRunning(g, fresh, ready)
		return s.d.NodeGroups.Update(ctx, g)
	}
	nodes, err = s.scale(ctx, cn, c, g, nodes)
	if err != nil {
		return s.failNodes(ctx, g, nodes, err)
	}
	ready, err := s.d.Backend.EnsureNodes(ctx, backendGroup(g, nodes))
	if err != nil {
		return s.failNodes(ctx, g, nodes, err)
	}
	s.setRunning(g, nodes, ready)
	return s.d.NodeGroups.Update(ctx, g)
}

// scale creates missing nodes and deletes surplus ones, newest first.
func (s *Service) scale(ctx context.Context, cn peer.ComputeNodes, c *mk8spb.Cluster, g *mk8spb.NodeGroup, nodes []peer.NodeInstance) ([]peer.NodeInstance, error) {
	target := int(g.GetStatus().GetTargetNodeCount())
	for len(nodes) > target {
		last := nodes[len(nodes)-1]
		if err := cn.DeleteNode(ctx, last.ID); err != nil {
			return nodes, fmt.Errorf("delete node %s: %w", last.Name, err)
		}
		nodes = nodes[:len(nodes)-1]
	}
	for len(nodes) < target {
		n, err := s.createNode(ctx, cn, c, g, nodes)
		if err != nil {
			return nodes, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// createNode creates one node instance with a fresh name.
func (s *Service) createNode(ctx context.Context, cn peer.ComputeNodes, c *mk8spb.Cluster, g *mk8spb.NodeGroup, nodes []peer.NodeInstance) (peer.NodeInstance, error) {
	t := g.GetSpec().GetTemplate()
	family, err := checkImage(t)
	if err != nil {
		return peer.NodeInstance{}, err
	}
	var lastErr error
	for range nodeNameAttempts {
		name, err := s.nodeName(g.GetMetadata().GetName(), nodes)
		if err != nil {
			return peer.NodeInstance{}, err
		}
		n, err := cn.CreateNode(ctx, peer.NodeSpec{
			ProjectID: c.GetMetadata().GetParentId(), Name: name,
			ClusterID: c.GetMetadata().GetId(), NodeGroupID: g.GetMetadata().GetId(),
			Labels: instanceLabels(g), Template: toPeerTemplate(t, family),
		})
		if !errs.IsKind(err, errs.KindAlreadyExists) {
			if err != nil {
				return peer.NodeInstance{}, fmt.Errorf("create node %s: %w", name, err)
			}
			return n, nil
		}
		lastErr = err // the name is taken in the project: draw another
	}
	return peer.NodeInstance{}, fmt.Errorf("create node: %w", lastErr)
}

// nodeName is "<node group name>-<5 random characters>", unique among nodes.
func (s *Service) nodeName(groupName string, nodes []peer.NodeInstance) (string, error) {
	prefix := strings.Trim(nodeNameRE.ReplaceAllString(strings.ToLower(groupName), "-"), "-")
	prefix = strings.TrimRight(prefix[:min(len(prefix), maxNodeNamePrefix)], "-")
	if prefix == "" {
		prefix = "node"
	}
	for range nodeNameAttempts {
		id, err := s.d.IDs.New(KindNode)
		if err != nil {
			return "", fmt.Errorf("new node name: %w", err)
		}
		id = strings.ToLower(id)
		name := prefix + "-" + id[max(0, len(id)-nodeSuffixLen):]
		taken := false
		for _, n := range nodes {
			taken = taken || n.Name == name
		}
		if !taken {
			return name, nil
		}
	}
	return "", errs.Internal("could not draw a unique node name")
}

func deleteNodes(ctx context.Context, cn peer.ComputeNodes, nodes []peer.NodeInstance) error {
	for _, n := range nodes {
		if err := cn.DeleteNode(ctx, n.ID); err != nil {
			return fmt.Errorf("delete node %s: %w", n.Name, err)
		}
	}
	return nil
}

// setRunning records the reconciled nodes in the status.
func (s *Service) setRunning(g *mk8spb.NodeGroup, nodes []peer.NodeInstance, ready []NodeReadiness) {
	st := g.GetStatus()
	var readyCount int64
	var notReady []string
	for _, r := range ready {
		if r.Ready {
			readyCount++
			continue
		}
		notReady = append(notReady, r.Name+": "+r.Reason)
	}
	st.State = mk8spb.NodeGroupStatus_RUNNING
	st.NodeCount, st.ReadyNodeCount, st.OutdatedNodeCount = int64(len(nodes)), readyCount, 0
	st.Reconciling = readyCount < st.GetNodeCount()
	st.Events = nil
	if len(notReady) > 0 {
		st.Events = []*commonpb.RecurrentResourceEvent{s.event(commonpb.ResourceEvent_WARN, "NodesNotReady",
			fmt.Sprintf("%d of %d nodes are not ready: %s", len(notReady), len(nodes), strings.Join(notReady, "; ")))}
	}
}

// failNodes records a failed reconciliation (the nodes that exist, and an
// ERROR event) and returns err.
func (s *Service) failNodes(ctx context.Context, g *mk8spb.NodeGroup, nodes []peer.NodeInstance, err error) error {
	st := g.GetStatus()
	st.NodeCount = int64(len(nodes))
	st.ReadyNodeCount = min(st.GetReadyNodeCount(), st.GetNodeCount())
	st.Events = []*commonpb.RecurrentResourceEvent{s.event(commonpb.ResourceEvent_ERROR, "ReconciliationFailed", err.Error())}
	return errors.Join(err, s.d.NodeGroups.Update(ctx, g))
}

// removeNodeGroupLocked deletes a node group's nodes, empties it in the
// backend and deletes its record. The cluster lock is held.
func (s *Service) removeNodeGroupLocked(ctx context.Context, g *mk8spb.NodeGroup) error {
	id := g.GetMetadata().GetId()
	if cn, ok := s.d.Peers.ComputeNodes(); ok { // without compute there are no nodes
		nodes, err := cn.ListNodes(ctx, id)
		if err != nil {
			return err
		}
		if err := deleteNodes(ctx, cn, nodes); err != nil {
			return err
		}
	}
	if _, err := s.d.Backend.EnsureNodes(ctx, backendGroup(g, nil)); err != nil {
		return err
	}
	return s.d.NodeGroups.Delete(ctx, id)
}

func serviceLabels(g *mk8spb.NodeGroup) map[string]string {
	return map[string]string{
		LabelClusterID:     g.GetMetadata().GetParentId(),
		LabelNodeGroupID:   g.GetMetadata().GetId(),
		LabelNodeGroupName: g.GetMetadata().GetName(),
	}
}

// instanceLabels are the node instances' metadata labels.
func instanceLabels(g *mk8spb.NodeGroup) map[string]string {
	out := maps.Clone(g.GetSpec().GetTemplate().GetInstanceMetadata().GetLabels())
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, serviceLabels(g))
	return out
}

// nodeLabels are the Kubernetes Node labels.
func nodeLabels(g *mk8spb.NodeGroup) map[string]string {
	out := map[string]string{}
	for k, v := range g.GetSpec().GetTemplate().GetMetadata().GetLabels() {
		if !isKubernetesLabel(k) {
			out[k] = v
		}
	}
	maps.Copy(out, serviceLabels(g))
	return out
}

var taintEffects = map[mk8spb.NodeTaint_Effect]string{
	mk8spb.NodeTaint_NO_EXECUTE:         "NoExecute",
	mk8spb.NodeTaint_NO_SCHEDULE:        "NoSchedule",
	mk8spb.NodeTaint_PREFER_NO_SCHEDULE: "PreferNoSchedule",
}

func backendGroup(g *mk8spb.NodeGroup, nodes []peer.NodeInstance) BackendNodeGroup {
	t := g.GetSpec().GetTemplate()
	bg := BackendNodeGroup{
		ClusterID: g.GetMetadata().GetParentId(), NodeGroupID: g.GetMetadata().GetId(), Name: g.GetMetadata().GetName(),
		Version: statusNode(g.GetStatus().GetVersion()).minor, Labels: nodeLabels(g),
		DrainTimeout: g.GetStatus().GetStrategy().GetDrainTimeout().AsDuration(),
	}
	for _, tn := range t.GetTaints() {
		bg.Taints = append(bg.Taints, NodeTaint{Key: tn.GetKey(), Value: tn.GetValue(), Effect: taintEffects[tn.GetEffect()]})
	}
	for _, n := range nodes {
		bg.Nodes = append(bg.Nodes, BackendNode{
			Name: n.Name, InstanceID: n.ID, PrivateIP: n.PrivateIP, PublicIP: n.PublicIP,
			Platform: t.GetResources().GetPlatform(), Preset: t.GetResources().GetPreset(), GPUs: n.Resources.GPUs,
		})
	}
	return bg
}

// toPeerTemplate is the compute part of a node template.
func toPeerTemplate(t *mk8spb.NodeTemplate, family string) peer.NodeTemplate {
	d := t.GetBootDisk()
	pt := peer.NodeTemplate{
		Platform: t.GetResources().GetPlatform(), Preset: t.GetResources().GetPreset(),
		BootDisk: peer.NodeDisk{
			SizeBytes: diskBytes(d), BlockSizeBytes: d.GetBlockSizeBytes(), Type: d.GetType().String(),
			Encrypted: d.GetDiskEncryption().GetType() == mk8spb.DiskEncryption_DISK_ENCRYPTION_MANAGED,
		},
		ImageFamily: family, ServiceAccountID: t.GetServiceAccountId(), GpuClusterID: t.GetGpuCluster().GetId(),
		CloudInitUserData: t.GetCloudInitUserData(), Preemptible: t.GetPreemptible() != nil,
	}
	if nics := t.GetNetworkInterfaces(); len(nics) > 0 {
		pt.SubnetID, pt.PublicIP = nics[0].GetSubnetId(), nics[0].GetPublicIpAddress() != nil
		for _, sg := range nics[0].GetSecurityGroups() {
			pt.SecurityGroupIDs = append(pt.SecurityGroupIDs, sg.GetId())
		}
	}
	for _, fs := range t.GetFilesystems() {
		pt.Filesystems = append(pt.Filesystems, peer.NodeFilesystem{
			ID: fs.GetExistingFilesystem().GetId(), MountTag: fs.GetMountTag(),
			ReadOnly: fs.GetAttachMode() == mk8spb.AttachedFilesystemSpec_READ_ONLY,
		})
	}
	return pt
}

func diskBytes(d *mk8spb.DiskSpec) int64 {
	switch {
	case d.GetSizeGibibytes() != 0:
		return d.GetSizeGibibytes() * gib
	case d.GetSizeMebibytes() != 0:
		return d.GetSizeMebibytes() * mib
	case d.GetSizeKibibytes() != 0:
		return d.GetSizeKibibytes() * kib
	default:
		return d.GetSizeBytes()
	}
}
