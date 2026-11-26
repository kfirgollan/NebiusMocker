package k3s

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func readyNames(rs []domain.NodeReadiness) (names []string, ready []bool) {
	for _, r := range rs {
		names = append(names, r.Name)
		ready = append(ready, r.Ready)
	}
	return names, ready
}

func TestEnsureNodesAgentSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	g := testGroupSpec(testCluster, "n1", "n2", "n3")
	g.Labels = map[string]string{"team": "ml", "node-role.kubernetes.io/gpu": "true", "kubernetes.io/os": "linux"}
	g.Taints = []domain.NodeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}, {Key: "spot", Effect: "PreferNoSchedule"}}
	g.Nodes[0].GPUs = 8

	got := h.ensureNodes(t, g)
	want := []domain.NodeReadiness{{Name: "n1", Ready: true}, {Name: "n2", Ready: true}, {Name: "n3", Ready: true}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("EnsureNodes readiness mismatch (-want +got):\n%s", diff)
	}
	agents := h.agents(t)
	if len(agents) != 3 {
		t.Fatalf("agent containers = %d, want 3", len(agents))
	}
	spec := h.docker.spec(agentName(testCluster, "n1"))
	if spec.Image != "rancher/k3s:v1.35.10-k3s1" || !spec.Privileged || spec.Network != networkName(testCluster) {
		t.Errorf("agent spec = image %q privileged %v network %q", spec.Image, spec.Privileged, spec.Network)
	}
	for k, v := range map[string]string{
		LabelBackend: "k3s", LabelCluster: testCluster, LabelNodeGroup: testGroup, LabelNode: "n1",
		LabelInstance: "computeinstance-n1", LabelRole: roleAgent,
	} {
		if spec.Labels[k] != v {
			t.Errorf("agent label %s = %q, want %q", k, spec.Labels[k], v)
		}
	}
	wantCmd := []string{
		"agent", "--server=https://" + serverName(testCluster) + ":6443", "--node-name=n1",
		"--node-label=kubernetes.io/os=linux", "--node-label=team=ml",
		"--node-taint=dedicated=gpu:NoSchedule", "--node-taint=spot:PreferNoSchedule",
	}
	if diff := cmp.Diff(wantCmd, spec.Cmd); diff != "" {
		t.Errorf("agent command mismatch (-want +got):\n%s", diff)
	}
	if got, want := fileContent(t, spec, nodePasswordPath), nodePassword(h.b.secret, testCluster, "n1"); got != want {
		t.Errorf("node password file = %q, want the node's derived password", got)
	}
	if !slices.Contains(spec.Env, "K3S_TOKEN="+joinToken(h.b.secret, testCluster)) {
		t.Errorf("agent env = %v, want the cluster join token", spec.Env)
	}

	// Labels the kubelet may not set are applied through the API, with the
	// bookkeeping annotations; GPUs are advertised.
	updates, gpus, _ := h.kube.snapshot()
	taints := []kubeTaint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}, {Key: "spot", Effect: "PreferNoSchedule"}}
	var wantUpdates []updateCall
	for _, n := range []string{"n1", "n2", "n3"} {
		wantUpdates = append(wantUpdates, updateCall{n, nodePatch{
			Labels: map[string]*string{"node-role.kubernetes.io/gpu": ptr("true")},
			Annotations: map[string]string{
				annotationLabels: `["kubernetes.io/os","node-role.kubernetes.io/gpu","team"]`,
				annotationTaints: `[{"key":"dedicated","value":"gpu","effect":"NoSchedule"},{"key":"spot","effect":"PreferNoSchedule"}]`,
			},
			Taints: taints,
		}})
	}
	if diff := cmp.Diff(wantUpdates, updates); diff != "" {
		t.Errorf("UpdateNode calls mismatch (-want +got):\n%s", diff)
	}
	if want := []gpuCall{{"n1", "nvidia.com/gpu", 8}}; !slices.Equal(gpus, want) {
		t.Errorf("SetExtendedResource calls = %v, want %v", gpus, want)
	}
}

func TestEnsureNodesIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	g := testGroupSpec(testCluster, "n1", "n2")
	g.Labels = map[string]string{"node-role.kubernetes.io/gpu": "true"}
	g.Nodes[1].GPUs = 4
	h.ensureNodes(t, g)
	creates := h.docker.createCount()
	labels1, gpus1, _ := h.kube.snapshot()
	got := h.ensureNodes(t, g)
	if names, ready := readyNames(got); !slices.Equal(names, []string{"n1", "n2"}) || !slices.Equal(ready, []bool{true, true}) {
		t.Fatalf("second EnsureNodes = %+v, want both ready", got)
	}
	labels2, gpus2, _ := h.kube.snapshot()
	if h.docker.createCount() != creates || len(labels2) != len(labels1) || len(gpus2) != len(gpus1) {
		t.Errorf("second EnsureNodes changed things: %d new containers, %d label calls, %d GPU calls",
			h.docker.createCount()-creates, len(labels2)-len(labels1), len(gpus2)-len(gpus1))
	}
}

func TestEnsureNodesScaleDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2", "n3"))
	// Another node group of the cluster is not touched.
	other := testGroupSpec(testCluster, "m1")
	other.NodeGroupID = "mk8snodegroup-other"
	h.ensureNodes(t, other)

	got := h.ensureNodes(t, testGroupSpec(testCluster, "n1"))
	if names, _ := readyNames(got); !slices.Equal(names, []string{"n1"}) {
		t.Fatalf("EnsureNodes after scale down = %+v, want only n1", got)
	}
	agents := h.agents(t)
	if len(agents) != 2 || agents["n1"].ID == "" || agents["m1"].ID == "" {
		t.Fatalf("agents = %v, want n1 and the other group's m1", slices.Sorted(maps.Keys(agents)))
	}
	_, _, deleted := h.kube.snapshot()
	slices.Sort(deleted)
	if want := []string{"n2", "n3"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted Kubernetes nodes = %v, want %v", deleted, want)
	}
}

// Only the image and the GPU count need a new agent; labels and taints are
// reconciled in place (nodemeta_test.go).
func TestEnsureNodesAgentChangeRecreates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.BackendNodeGroup)
	}{
		{name: "gpus", mutate: func(g *domain.BackendNodeGroup) { g.Nodes[0].GPUs = 2 }},
		{name: "version", mutate: func(g *domain.BackendNodeGroup) { g.Version = "1.36" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			h.ensureCluster(t, testCluster)
			g := testGroupSpec(testCluster, "n1", "n2")
			g.Labels = map[string]string{"team": "ml"}
			h.ensureNodes(t, g)
			before := h.agents(t)

			changed := g
			changed.Nodes = slices.Clone(g.Nodes)
			tt.mutate(&changed)
			got := h.ensureNodes(t, changed)
			if _, ready := readyNames(got); !slices.Equal(ready, []bool{true, true}) {
				t.Fatalf("EnsureNodes after the change = %+v, want both ready", got)
			}
			after := h.agents(t)
			if after["n1"].ID == before["n1"].ID {
				t.Errorf("n1 was not recreated after a %s change", tt.name)
			}
			// Only n1 differs for the GPU change; the image is group-wide.
			if tt.name == "gpus" && after["n2"].ID != before["n2"].ID {
				t.Errorf("n2 was recreated by a change of n1's GPUs")
			}
			if tt.name != "gpus" && after["n2"].ID == before["n2"].ID {
				t.Errorf("n2 was not recreated after a %s change", tt.name)
			}
		})
	}
}

func TestEnsureNodesCapacityNeverErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.Config.MaxAgents = 2 })
	h.ensureCluster(t, testCluster)
	got := h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2", "n3"))
	if names, ready := readyNames(got); !slices.Equal(names, []string{"n1", "n2", "n3"}) || !slices.Equal(ready, []bool{true, true, false}) {
		t.Fatalf("EnsureNodes = %+v, want n1, n2 ready and n3 not", got)
	}
	if !strings.Contains(got[2].Reason, "NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS") {
		t.Errorf("n3 reason = %q, want it to name NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS", got[2].Reason)
	}
	if n := len(h.agents(t)); n != 2 {
		t.Errorf("agent containers = %d, want 2", n)
	}
	// The next reconcile re-evaluates admission: the host is still full.
	again := h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2", "n3"))
	if again[2].Ready || again[2].Reason == "" {
		t.Errorf("n3 on the second call = %+v, want still not ready with a reason", again[2])
	}
}

func TestEnsureNodesHostMemoryAdmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.docker.SetInfo(dockerhost.Info{NCPU: 8, MemTotal: 2 << 30})
	h.ensureCluster(t, testCluster)
	// 2 GiB total - 1 GiB reserve - 768 MiB server estimate = 256 MiB: no agent (384 MiB) fits.
	got := h.ensureNodes(t, testGroupSpec(testCluster, "n1"))
	if got[0].Ready || !strings.Contains(got[0].Reason, "NEBIUS_MOCKER_MK8S_K3S_AGENT_MEMORY") {
		t.Fatalf("EnsureNodes = %+v, want n1 not ready for lack of memory", got)
	}
	if n := len(h.agents(t)); n != 0 {
		t.Errorf("agent containers = %d, want none", n)
	}
}

func TestEnsureNodesAgentExits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.docker.exitAgents["n2"] = "level=fatal msg=\"boom\"\n"
	got := h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2", "n3"))
	if names, ready := readyNames(got); !slices.Equal(names, []string{"n1", "n2", "n3"}) || !slices.Equal(ready, []bool{true, false, true}) {
		t.Fatalf("EnsureNodes = %+v, want only n2 not ready", got)
	}
	for _, want := range []string{"exited (code 3)", "boom"} {
		if !strings.Contains(got[1].Reason, want) {
			t.Errorf("n2 reason = %q, want it to contain %q", got[1].Reason, want)
		}
	}
	// The next reconcile replaces the stopped agent.
	h.docker.mu.Lock()
	delete(h.docker.exitAgents, "n2")
	h.docker.mu.Unlock()
	again := h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2", "n3"))
	if _, ready := readyNames(again); !slices.Equal(ready, []bool{true, true, true}) {
		t.Fatalf("EnsureNodes after the agent was replaced = %+v, want all ready", again)
	}
}

func TestEnsureNodesNotReadyInTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.kube.setHold("n2")
	got := h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	if _, ready := readyNames(got); !slices.Equal(ready, []bool{true, false}) {
		t.Fatalf("EnsureNodes = %+v, want only n2 not ready", got)
	}
	if !strings.Contains(got[1].Reason, "NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT") {
		t.Errorf("n2 reason = %q, want it to name NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT", got[1].Reason)
	}
}

func TestEnsureNodesEmptyRemovesAgents(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	got, err := h.b.EnsureNodes(t.Context(), testGroupSpec(testCluster))
	if err != nil || len(got) != 0 {
		t.Fatalf("EnsureNodes with no nodes = %v, %v; want nothing", got, err)
	}
	if n := len(h.agents(t)); n != 0 {
		t.Errorf("agent containers = %d, want none", n)
	}
	if _, _, deleted := h.kube.snapshot(); len(deleted) != 2 {
		t.Errorf("deleted Kubernetes nodes = %v, want both", deleted)
	}
}

func TestEnsureNodesEmptyWithoutClusterState(t *testing.T) {
	t.Parallel()
	// After a mocker restart that lost the cluster's state, leftover agents are
	// still removed (undrained: there is no API client to drain with).
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1"))
	h.b.mu.Lock()
	h.b.clusters = map[string]*clusterState{}
	h.b.mu.Unlock()
	if _, err := h.b.EnsureNodes(t.Context(), testGroupSpec(testCluster)); err != nil {
		t.Fatalf("EnsureNodes with no nodes: %v", err)
	}
	if n := len(h.agents(t)); n != 0 {
		t.Errorf("agent containers = %d, want none", n)
	}
}

func TestEnsureNodesClusterNotEnsured(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, err := h.b.EnsureNodes(t.Context(), testGroupSpec(testCluster, "n1"))
	if !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("EnsureNodes on an unknown cluster = %v, want BadState", err)
	}
}

func TestEnsureNodesUnknownVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	g := testGroupSpec(testCluster, "n1")
	g.Version = "1.99"
	if _, err := h.b.EnsureNodes(t.Context(), g); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("EnsureNodes with version 1.99 = %v, want BadRequest", err)
	}
}

// A label with an empty value (node-role.kubernetes.io/<role>: "") is still a
// label the node must get.
func TestEnsureNodesEmptyLabelValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	g := testGroupSpec(testCluster, "n1")
	g.Labels = map[string]string{"node-role.kubernetes.io/gpu": ""}
	h.ensureNodes(t, g)
	updates, _, _ := h.kube.snapshot()
	if len(updates) != 1 || !cmp.Equal(updates[0].Patch.Labels, map[string]*string{"node-role.kubernetes.io/gpu": ptr("")}) {
		t.Errorf("UpdateNode calls = %+v, want one setting the label to the empty string", updates)
	}
}

func ptr(s string) *string { return &s }
