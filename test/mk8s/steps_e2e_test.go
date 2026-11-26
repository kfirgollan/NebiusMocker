//go:build e2e_mk8s

package mk8s

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

// pauseImage is a tiny multi-arch image for the workloads.
const pauseImage = "registry.k8s.io/pause:3.10"

const workloadsYAML = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: pause, image: ` + pauseImage + `}]
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: probe}
spec:
  selector: {matchLabels: {app: probe}}
  template:
    metadata: {labels: {app: probe}}
    spec:
      tolerations: [{operator: Exists}]
      containers: [{name: pause, image: ` + pauseImage + `}]
---
apiVersion: v1
kind: Pod
metadata: {name: cuda}
spec:
  tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
  containers:
  - name: pause
    image: ` + pauseImage + `
    resources: {limits: {nvidia.com/gpu: 1}}
`

// workloads deploys a Deployment, a DaemonSet that must land on all n
// nodes, and a pod requesting a GPU, which only the GPU node can take.
func workloads(t *testing.T, e *env, n int, gpuNodes []string) {
	t.Helper()
	e.user.sh(t, "cat <<'EOF' | kubectl apply -f -\n"+workloadsYAML+"EOF")
	e.user.sh(t, "kubectl rollout status deployment/web --timeout=240s")
	e.user.sh(t, "kubectl rollout status daemonset/probe --timeout=240s")
	var ds struct {
		Status struct {
			Desired int `json:"desiredNumberScheduled"`
			Ready   int `json:"numberReady"`
		} `json:"status"`
	}
	kubectlJSON(t, e, &ds, "get daemonset probe")
	if ds.Status.Desired != n || ds.Status.Ready != n {
		t.Fatalf("daemonset probe: %d desired, %d ready, want %d on every node", ds.Status.Desired, ds.Status.Ready, n)
	}
	e.user.sh(t, "kubectl wait pod/cuda --for=condition=Ready --timeout=240s")
	var pod struct {
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	}
	kubectlJSON(t, e, &pod, "get pod cuda")
	if !slices.Contains(gpuNodes, pod.Spec.NodeName) {
		t.Fatalf("GPU pod runs on %q, want a GPU node %v", pod.Spec.NodeName, gpuNodes)
	}
}

func kubectlJSON(t *testing.T, e *env, out any, args string) {
	t.Helper()
	raw := e.user.sh(t, "kubectl "+args+" -o json")
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("kubectl %s: %v\n%s", args, err, raw)
	}
}

// viewer checks the documented IAM → Kubernetes mapping with a service
// account in the viewers group, authenticated the way users do: key
// exchange from a CLI profile with the service account's credentials file.
func viewer(t *testing.T, e *env, clusterID string) {
	t.Helper()
	var sa struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	raw := e.user.sh(t, "nebius --profile admin iam service-account create --name k3s-viewer --format json 2>/dev/null")
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		t.Fatalf("service-account create: %v\n%s", err, raw)
	}
	e.user.sh(t, fmt.Sprintf(`group=$(nebius --profile admin iam group get-by-name --name viewers --parent-id %s --format json | jq -r .metadata.id)
nebius --profile admin iam group-membership create --parent-id "$group" --member-id %s >/dev/null
nebius --profile admin iam auth-public-key generate --service-account-id %s --output /root/viewer-credentials.json >/dev/null`,
		e.inst.TenantID, sa.Metadata.ID, sa.Metadata.ID))
	e.profile(t, "viewer", "--service-account-file /root/viewer-credentials.json")
	const kc = "--kubeconfig /root/viewer.kubeconfig"
	e.user.sh(t, "nebius --profile viewer mk8s cluster get-credentials --id "+clusterID+" --external "+kc)

	var who struct {
		Status struct {
			UserInfo struct {
				Username string   `json:"username"`
				Groups   []string `json:"groups"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	raw = e.user.sh(t, "kubectl "+kc+" auth whoami -o json")
	if err := json.Unmarshal([]byte(raw), &who); err != nil {
		t.Fatalf("auth whoami: %v\n%s", err, raw)
	}
	if who.Status.UserInfo.Username != sa.Metadata.ID || !slices.Contains(who.Status.UserInfo.Groups, "nebius:viewer") {
		t.Fatalf("viewer identity = %+v, want %s in nebius:viewer", who.Status.UserInfo, sa.Metadata.ID)
	}
	if out := e.user.sh(t, "kubectl "+kc+" get pods -A -o name"); !strings.Contains(out, "pod/cuda") {
		t.Fatalf("viewer cannot list pods:\n%s", out)
	}
	out, err := e.user.try(t.Context(), "kubectl "+kc+" create deployment denied --image="+pauseImage)
	if err == nil || !strings.Contains(out, "forbidden") {
		t.Fatalf("viewer created a deployment (err=%v):\n%s", err, out)
	}
	if out := e.user.sh(t, "kubectl "+kc+" auth can-i create pods || true"); lastLine(out) != "no" {
		t.Fatalf("viewer auth can-i create pods = %q, want no", out)
	}
}

// scale grows the CPU group past the injected host capacity (maxAgents),
// checks the status says so honestly, then shrinks it; agents, Kubernetes
// nodes and compute instances follow.
func scale(t *testing.T, e *env, tf *tfharness.Run, cpuID, clusterID string) {
	t.Helper()
	// 2 CPU + 1 GPU agents run; 4 CPU nodes would make 5 > maxAgents.
	tf.Write(t, hcl(e.inst.ProjectID, cpuGroup{4, "batch", "4vcpu-16gb", 0}))
	tf.ExpectPlan(t, scalePlan)
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	ng := e.nodeGroup(t, cpuID, func(s *mk8spb.NodeGroupStatus) bool {
		return s.GetNodeCount() == 4 && s.GetReadyNodeCount() == 3
	})
	if !mentions(ng, "NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS") {
		t.Fatalf("node group status does not name the host capacity limit: %v", ng.GetStatus())
	}
	if names := e.nodeNames(t, cpuID); len(names) != 4 {
		t.Fatalf("compute instances of the CPU group = %v, want 4", names)
	}
	if agents := e.d.k3sContainers(t, "nebius-mocker.node-group="+cpuID); len(agents) != 3 {
		t.Fatalf("agents of the CPU group = %v, want 3 (host capacity %d agents)", agents, maxAgents)
	}
	waitNodes(t, e, "", maxAgents)

	tf.Write(t, hcl(e.inst.ProjectID, cpuGroup{1, "batch", "4vcpu-16gb", 0}))
	tf.ExpectPlan(t, scalePlan)
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	e.nodeGroup(t, cpuID, func(s *mk8spb.NodeGroupStatus) bool {
		return s.GetNodeCount() == 1 && s.GetReadyNodeCount() == 1
	})
	nodes := waitNodes(t, e, "", 2)
	names := e.nodeNames(t, cpuID)
	if len(names) != 1 || !slices.Contains(nodes.ready(), names[0]) {
		t.Fatalf("after scale-down: instances %v, ready nodes %v", names, nodes.ready())
	}
	if agents := e.d.k3sContainers(t, "nebius-mocker.node-group="+cpuID); len(agents) != 1 {
		t.Fatalf("agents of the CPU group after scale-down = %v, want 1", agents)
	}
	_ = clusterID
}

// scalePlan is the plan of a fixed_node_count change: in place.
var scalePlan = map[string]string{
	"nebius_vpc_v1_network.net": "no-op", "nebius_vpc_v1_subnet.sub": "no-op", "nebius_mk8s_v1_cluster.k": "no-op",
	"nebius_mk8s_v1_node_group.cpu": "update", "nebius_mk8s_v1_node_group.gpu": "no-op",
}

// nodeGroup polls the node group until ok(status).
func (e *env) nodeGroup(t *testing.T, id string, ok func(*mk8spb.NodeGroupStatus) bool) *mk8spb.NodeGroup {
	t.Helper()
	var ng *mk8spb.NodeGroup
	eventually(t, 4*time.Minute, func() error {
		var err error
		ng, err = e.sdk.Services().MK8S().V1().NodeGroup().Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: id})
		if err != nil {
			return err
		}
		if !ok(ng.GetStatus()) {
			return fmt.Errorf("node group status: %v", ng.GetStatus())
		}
		return nil
	})
	return ng
}

func mentions(ng *mk8spb.NodeGroup, s string) bool {
	b, _ := json.Marshal(ng.GetStatus()) //nolint:errchkjson // a proto struct always marshals
	return strings.Contains(string(b), s)
}

const pdbYAML = `
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web}
spec:
  minAvailable: 1
  selector: {matchLabels: {app: web}}
`

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	} `json:"items"`
}

// rollout changes the CPU group's preset, which replaces its nodes in
// batches. Before an old node's agent goes, the backend drains
// it: the web pods (under a PodDisruptionBudget) are evicted and rescheduled
// onto the new node. Evidence that the drain came first: the old pods were
// stopped by their own kubelet ("Killing" events from the old node), which
// cannot happen once the agent is gone.
func rollout(t *testing.T, e *env, tf *tfharness.Run, cpuID string) {
	t.Helper()
	e.user.sh(t, "cat <<'EOF' | kubectl apply -f -\n"+pdbYAML+"EOF")
	e.user.sh(t, "kubectl rollout status deployment/web --timeout=240s")
	oldNodes := e.nodeNames(t, cpuID)
	var before podList
	kubectlJSON(t, e, &before, "get pods -l app=web")
	for _, p := range before.Items {
		if !slices.Contains(oldNodes, p.Spec.NodeName) {
			t.Fatalf("web pod %s runs on %s, want a CPU node %v before the rollout", p.Metadata.Name, p.Spec.NodeName, oldNodes)
		}
	}

	tf.Write(t, hcl(e.inst.ProjectID, cpuGroup{1, "stream", "2vcpu-8gb", 0}))
	tf.ExpectPlan(t, scalePlan)
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	e.nodeGroup(t, cpuID, func(s *mk8spb.NodeGroupStatus) bool {
		return s.GetNodeCount() == 1 && s.GetReadyNodeCount() == 1 && s.GetOutdatedNodeCount() == 0
	})
	newNodes := e.nodeNames(t, cpuID)
	if len(newNodes) != 1 || slices.Contains(oldNodes, newNodes[0]) {
		t.Fatalf("CPU nodes after the rollout = %v, want one new node (old %v)", newNodes, oldNodes)
	}
	e.user.sh(t, "kubectl rollout status deployment/web --timeout=240s")
	var after podList
	kubectlJSON(t, e, &after, "get pods -l app=web")
	for _, p := range after.Items {
		if p.Spec.NodeName != newNodes[0] {
			t.Errorf("web pod %s runs on %q after the rollout, want the new node %s", p.Metadata.Name, p.Spec.NodeName, newNodes[0])
		}
	}
	for _, p := range before.Items {
		var ev struct {
			Items []struct {
				Reason string `json:"reason"`
				Source struct {
					Host string `json:"host"`
				} `json:"source"`
			} `json:"items"`
		}
		kubectlJSON(t, e, &ev, "get events --field-selector involvedObject.name="+p.Metadata.Name)
		drained := false
		for _, it := range ev.Items {
			if it.Reason == "Killing" && it.Source.Host == p.Spec.NodeName {
				drained = true
			}
		}
		if !drained {
			t.Errorf("old web pod %s on %s has no Killing event from its kubelet: it was not drained before the node went (events %+v)",
				p.Metadata.Name, p.Spec.NodeName, ev.Items)
		}
	}
	if label := e.user.sh(t, "kubectl get node "+newNodes[0]+" -o jsonpath='{.metadata.labels.workload}'"); strings.TrimSpace(label) != "stream" {
		t.Errorf("new node label workload = %q, want stream", label)
	}
}

// inPlace changes the CPU group's template label and taint: Nebius applies
// them to the existing nodes, so the node, its agent and its pods stay.
func inPlace(t *testing.T, e *env, tf *tfharness.Run, cpuID string) {
	t.Helper()
	nodes := e.nodeNames(t, cpuID)
	agents := e.d.k3sContainers(t, "nebius-mocker.node-group="+cpuID)
	e.user.sh(t, "kubectl label node "+nodes[0]+" team.example.com/owner=alice") // a user's own label stays
	tf.Write(t, hcl(e.inst.ProjectID, cpuGroup{1, "stream", "4vcpu-16gb", 0}))
	tf.ExpectPlan(t, scalePlan)
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	if got := e.nodeNames(t, cpuID); !slices.Equal(got, nodes) {
		t.Fatalf("CPU nodes after a label change = %v, want the same %v (no rollout)", got, nodes)
	}
	if got := e.d.k3sContainers(t, "nebius-mocker.node-group="+cpuID); !slices.Equal(got, agents) {
		t.Fatalf("CPU agents after a label change = %v, want the same %v", got, agents)
	}
	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Taints []struct{ Key, Value, Effect string } `json:"taints"`
		} `json:"spec"`
	}
	eventually(t, time.Minute, func() error {
		kubectlJSON(t, e, &node, "get node "+nodes[0])
		l := node.Metadata.Labels
		if l["workload"] != "stream" || l["team.example.com/owner"] != "alice" {
			return fmt.Errorf("labels = %v, want workload=stream and the user's team.example.com/owner=alice", l)
		}
		if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "dedicated" || node.Spec.Taints[0].Value != "stream" {
			return fmt.Errorf("taints = %+v, want dedicated=stream:PreferNoSchedule only", node.Spec.Taints)
		}
		return nil
	})
}

// maxPods sets the CPU group's max_pods. It is a template field, so the
// node is rolled out: the new node's kubelet runs with the limit, which
// Kubernetes reports as the Node's pods capacity and allocatable. The GPU
// group, without max_pods, keeps the default 110.
func maxPods(t *testing.T, e *env, tf *tfharness.Run, cpuID string, gpuNodes []string) {
	t.Helper()
	const limit = 32
	oldNodes := e.nodeNames(t, cpuID)
	tf.Write(t, hcl(e.inst.ProjectID, cpuGroup{1, "stream", "2vcpu-8gb", limit}))
	tf.ExpectPlan(t, scalePlan)
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	ng := e.nodeGroup(t, cpuID, func(s *mk8spb.NodeGroupStatus) bool {
		return s.GetNodeCount() == 1 && s.GetReadyNodeCount() == 1 && s.GetOutdatedNodeCount() == 0
	})
	if got := ng.GetSpec().GetTemplate().GetMaxPods(); got != limit {
		t.Fatalf("spec.template.max_pods = %d, want %d", got, limit)
	}
	newNodes := e.nodeNames(t, cpuID)
	if len(newNodes) != 1 || slices.Contains(oldNodes, newNodes[0]) {
		t.Fatalf("CPU nodes after the max_pods change = %v, want one new node (old %v)", newNodes, oldNodes)
	}
	nodes := waitNodes(t, e, "", 1+len(gpuNodes))
	want := map[string]string{newNodes[0]: fmt.Sprint(limit)}
	for _, n := range gpuNodes {
		want[n] = "110"
	}
	for _, it := range nodes.Items {
		w, ok := want[it.Metadata.Name]
		if !ok {
			t.Errorf("unexpected node %s after the max_pods rollout", it.Metadata.Name)
			continue
		}
		if it.Status.Capacity["pods"] != w || it.Status.Allocatable["pods"] != w {
			t.Errorf("node %s pods capacity %q allocatable %q, want %s", it.Metadata.Name,
				it.Status.Capacity["pods"], it.Status.Allocatable["pods"], w)
		}
	}
}
