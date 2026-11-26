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
	tf.Write(t, hcl(e.inst.ProjectID, 4))
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

	tf.Write(t, hcl(e.inst.ProjectID, 1))
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
