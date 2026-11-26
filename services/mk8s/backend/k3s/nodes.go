package k3s

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// nodePasswordPath is where a k3s agent keeps the password it registers its
// node with.
//
//nolint:gosec // G101: a file path inside the agent container, not a credential.
const nodePasswordPath = "/etc/rancher/node/password"

// EnsureNodes implements domain.Backend: one k3s agent container per node,
// admitted while the Docker host has room; nodes that do not fit, or whose
// agent does not become ready, are reported not ready with the reason.
func (b *Backend) EnsureNodes(ctx context.Context, g domain.BackendNodeGroup) ([]domain.NodeReadiness, error) {
	st, ok := b.cluster(g.ClusterID)
	if len(g.Nodes) > 0 && !ok {
		return nil, errs.BadState(g.ClusterID, "the cluster's k3s control plane is not running")
	}
	image := ""
	if len(g.Nodes) > 0 {
		var found bool
		if image, found = b.versions[g.Version]; !found {
			return nil, errs.BadRequest(errs.Violation{
				Field:   "spec.version",
				Message: fmt.Sprintf("the k3s backend has no image for Kubernetes %s (NEBIUS_MOCKER_MK8S_K3S_IMAGES)", g.Version),
			})
		}
	}
	existing, err := b.docker.ListContainers(ctx, map[string]string{
		LabelBackend: backendName, LabelCluster: g.ClusterID, LabelNodeGroup: g.NodeGroupID, LabelRole: roleAgent,
	})
	if err != nil {
		return nil, fmt.Errorf("list agents of %s: %w", g.NodeGroupID, err)
	}
	want := map[string]agentSpec{}
	for _, n := range g.Nodes {
		want[n.Name] = agentSpec{Image: image, Labels: g.Labels, Taints: g.Taints, GPUs: n.GPUs}
	}
	have := map[string]dockerhost.Container{}
	for _, c := range existing {
		name := c.Labels[LabelNode]
		spec, wanted := want[name]
		if wanted && c.Running && c.Labels[labelSpec] == specHash(spec) {
			have[name] = c
			continue
		}
		// Not wanted any more, stopped, or started with another image,
		// labels, taints or GPU count: remove it (a recreated one follows).
		if err := b.removeAgent(ctx, st, c); err != nil {
			return nil, err
		}
	}
	if len(g.Nodes) == 0 {
		return nil, nil
	}

	reasons, err := b.startAgents(ctx, g, want, have)
	if err != nil {
		return nil, err
	}
	if err := b.waitAgents(ctx, st, g, reasons); err != nil {
		return nil, err
	}
	out := make([]domain.NodeReadiness, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = domain.NodeReadiness{Name: n.Name, Ready: reasons[n.Name] == "", Reason: reasons[n.Name]}
	}
	return out, nil
}

func (b *Backend) removeAgent(ctx context.Context, st *clusterState, c dockerhost.Container) error {
	if err := b.docker.RemoveContainer(ctx, c.ID); err != nil {
		return fmt.Errorf("remove agent %s: %w", c.Name, err)
	}
	if st != nil {
		if err := st.kube.DeleteNode(ctx, c.Labels[LabelNode]); err != nil {
			return fmt.Errorf("delete node %s: %w", c.Labels[LabelNode], err)
		}
	}
	b.log.InfoContext(ctx, "mk8s k3s: agent removed", "node", c.Labels[LabelNode], "container", c.Name)
	return nil
}

// startAgents creates the missing agents of g, in g.Nodes order, while the
// host admits them. It returns the not-ready reason of every node of g ("":
// the agent runs).
func (b *Backend) startAgents(ctx context.Context, g domain.BackendNodeGroup, want map[string]agentSpec, have map[string]dockerhost.Container) (map[string]string, error) {
	reasons := map[string]string{}
	var missing []domain.BackendNode
	for _, n := range g.Nodes {
		if _, ok := have[n.Name]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return reasons, nil
	}
	if err := b.docker.EnsureImage(ctx, want[missing[0].Name].Image); err != nil {
		return nil, fmt.Errorf("pull %s: %w", want[missing[0].Name].Image, err)
	}
	// Admission is a critical section over the whole Docker host: two node
	// groups admitting at once would both see the same free capacity.
	select {
	case b.admission <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	defer func() { <-b.admission }()
	load, err := b.load(ctx)
	if err != nil {
		return nil, err
	}
	limits := capacityLimits{
		AgentMemory: b.cfg.AgentMemory, AgentCPU: b.cfg.AgentCPU, MemoryReserve: b.cfg.MemoryReserve, MaxAgents: b.cfg.MaxAgents,
	}
	for _, n := range missing {
		if why := admitAgent(limits, load); why != "" {
			reasons[n.Name] = why
			continue
		}
		if err := b.startAgent(ctx, g, n, want[n.Name]); err != nil {
			return nil, err
		}
		load.Agents++
		load.Used += b.cfg.AgentMemory
	}
	return reasons, nil
}

func (b *Backend) startAgent(ctx context.Context, g domain.BackendNodeGroup, n domain.BackendNode, spec agentSpec) error {
	cmd := []string{
		"agent",
		"--server=https://" + serverName(g.ClusterID) + ":6443",
		"--node-name=" + n.Name,
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		if kubeletLabel(k) {
			cmd = append(cmd, "--node-label="+k+"="+spec.Labels[k])
		}
	}
	for _, t := range spec.Taints {
		cmd = append(cmd, "--node-taint="+taintString(t))
	}
	labels := b.clusterLabels(g.ClusterID, roleAgent)
	labels[LabelNodeGroup] = g.NodeGroupID
	labels[LabelNode] = n.Name
	labels[LabelInstance] = n.InstanceID
	labels[labelSpec] = specHash(spec)
	c := dockerhost.ContainerSpec{
		Name:       agentName(g.ClusterID, n.Name),
		Image:      spec.Image,
		Cmd:        cmd,
		Env:        []string{"K3S_TOKEN=" + joinToken(b.secret, g.ClusterID)},
		Labels:     labels,
		Privileged: true,
		Tmpfs:      map[string]string{"/run": "", "/var/run": ""},
		Network:    networkName(g.ClusterID),
		Aliases:    []string{n.Name},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Files: []dockerhost.File{{
			Path: nodePasswordPath, Mode: 0o600, Content: []byte(nodePassword(b.secret, g.ClusterID, n.Name)),
		}},
	}
	id, err := b.docker.CreateContainer(ctx, c)
	if err != nil {
		return fmt.Errorf("create agent %s: %w", c.Name, err)
	}
	if err := b.docker.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start agent %s: %w", c.Name, err)
	}
	b.log.InfoContext(ctx, "mk8s k3s: agent started", "cluster", g.ClusterID, "node_group", g.NodeGroupID, "node", n.Name)
	return nil
}

func taintString(t domain.NodeTaint) string {
	if t.Value == "" {
		return t.Key + ":" + t.Effect
	}
	return t.Key + "=" + t.Value + ":" + t.Effect
}

// load measures the Docker host: totals, the memory its running containers
// use, and the agents of every cluster.
func (b *Backend) load(ctx context.Context) (hostLoad, error) {
	info, err := b.docker.Info(ctx)
	if err != nil {
		return hostLoad{}, fmt.Errorf("docker info: %w", err)
	}
	all, err := b.docker.ListContainers(ctx, nil)
	if err != nil {
		return hostLoad{}, fmt.Errorf("list containers: %w", err)
	}
	h := hostLoad{CPUs: info.NCPU, Memory: info.MemTotal}
	var used atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, c := range all {
		ours := c.Labels[LabelBackend] == backendName
		estimate := int64(0)
		switch {
		case ours && c.Labels[LabelRole] == roleAgent:
			h.Agents++
			estimate = b.cfg.AgentMemory
		case ours && c.Labels[LabelRole] == roleServer:
			estimate = b.cfg.ServerMemory
		}
		if !c.Running {
			continue
		}
		g.Go(func() error {
			u, err := b.docker.MemoryUsage(gctx, c.ID)
			if err != nil {
				// A container that stopped meanwhile uses nothing.
				b.log.DebugContext(gctx, "mk8s k3s: memory usage", "container", c.Name, "error", err)
			}
			used.Add(max(u, estimate))
			return nil
		})
	}
	_ = g.Wait() // the goroutines never fail
	h.Used = used.Load()
	return h, nil
}

// waitAgents waits until every admitted node of g is Ready in Kubernetes,
// then applies the labels the kubelet may not set itself and the GPU
// capacity. Nodes that do not get there in time keep a reason.
func (b *Backend) waitAgents(ctx context.Context, st *clusterState, g domain.BackendNodeGroup, reasons map[string]string) error {
	if err := b.waitReady(ctx, st, g, reasons); err != nil {
		return err
	}
	nodes, err := st.kube.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("nodes of %s: %w", g.ClusterID, err)
	}
	for _, n := range g.Nodes {
		kn, ok := nodes[n.Name]
		if reasons[n.Name] != "" || !ok {
			continue
		}
		if err := b.applyNode(ctx, st, n, kn, g.Labels); err != nil {
			return err
		}
	}
	return nil
}

// waitReady polls until no admitted node of g is pending: each is Ready,
// or got a reason (its agent exited, or StartTimeout passed).
func (b *Backend) waitReady(ctx context.Context, st *clusterState, g domain.BackendNodeGroup, reasons map[string]string) error {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	for {
		nodes, err := st.kube.Nodes(ctx)
		if err != nil {
			return fmt.Errorf("nodes of %s: %w", g.ClusterID, err)
		}
		pending := b.pendingAgents(g, nodes, reasons)
		if len(pending) == 0 {
			return nil
		}
		if !b.clk.Now().Before(deadline) {
			for _, n := range pending {
				reasons[n] = b.agentFailure(ctx, g.ClusterID, n,
					fmt.Sprintf("the k3s agent did not become ready within %s (NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT)", b.cfg.StartTimeout))
			}
			return nil
		}
		b.checkExited(ctx, g.ClusterID, pending, reasons)
		if err := b.sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

// checkExited gives the pending nodes whose agent container exited a reason.
func (b *Backend) checkExited(ctx context.Context, clusterID string, pending []string, reasons map[string]string) {
	for _, n := range pending {
		if ctr, err := b.docker.InspectContainer(ctx, agentName(clusterID, n)); err == nil && !ctr.Running {
			reasons[n] = b.agentFailure(ctx, clusterID, n, fmt.Sprintf("the k3s agent exited (code %d)", ctr.ExitCode))
		}
	}
}

// pendingAgents lists the admitted nodes of g that are not Ready yet.
func (*Backend) pendingAgents(g domain.BackendNodeGroup, nodes map[string]kubeNode, reasons map[string]string) []string {
	var pending []string
	for _, n := range g.Nodes {
		if reasons[n.Name] != "" {
			continue
		}
		if kn, ok := nodes[n.Name]; !ok || !kn.Ready {
			pending = append(pending, n.Name)
		}
	}
	return pending
}

func (*Backend) applyNode(ctx context.Context, st *clusterState, n domain.BackendNode, kn kubeNode, labels map[string]string) error {
	missing := map[string]string{}
	for k, v := range labels {
		if got, ok := kn.Labels[k]; !ok || got != v {
			missing[k] = v
		}
	}
	if len(missing) > 0 {
		if err := st.kube.LabelNode(ctx, n.Name, missing); err != nil {
			return fmt.Errorf("label node %s: %w", n.Name, err)
		}
	}
	if n.GPUs > 0 && kn.Capacity[gpuResource] != strconv.Itoa(n.GPUs) {
		if err := st.kube.SetExtendedResource(ctx, n.Name, gpuResource, n.GPUs); err != nil {
			return fmt.Errorf("advertise GPUs of node %s: %w", n.Name, err)
		}
	}
	return nil
}

func (b *Backend) agentFailure(ctx context.Context, clusterID, node, cause string) string {
	logs, err := b.docker.Logs(ctx, agentName(clusterID, node), 3)
	if err != nil || strings.TrimSpace(logs) == "" {
		return cause
	}
	b.log.WarnContext(ctx, "mk8s k3s: agent not ready", "cluster", clusterID, "node", node, "cause", cause, "logs", logs)
	return cause + "; last log lines: " + strings.TrimSpace(logs)
}
