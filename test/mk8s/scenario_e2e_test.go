//go:build e2e_mk8s

package mk8s

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

// cpuGroup is what the scenario changes in the CPU node group.
type cpuGroup struct {
	nodes int
	// workload is a template label and the value of its taint.
	workload string
	preset   string
	// maxPods is the template's max_pods; 0 leaves it unset (110).
	maxPods int
}

var cpuStart = cpuGroup{nodes: 2, workload: "batch", preset: "4vcpu-16gb"}

// hcl is the scenario's Terraform configuration.
func hcl(project string, cpu cpuGroup) string {
	return fmt.Sprintf(`
resource "nebius_vpc_v1_network" "net" {
  parent_id = %[1]q
  name      = "k3s-e2e"
}

resource "nebius_vpc_v1_subnet" "sub" {
  parent_id          = %[1]q
  name               = "k3s-e2e"
  network_id         = nebius_vpc_v1_network.net.id
  ipv4_private_pools = { pools = [{ cidrs = [{ cidr = "/24" }] }] }
}

resource "nebius_mk8s_v1_cluster" "k" {
  parent_id = %[1]q
  name      = "k3s-e2e"
  control_plane = {
    subnet_id = nebius_vpc_v1_subnet.sub.id
    version   = "1.36"
    endpoints = { public_endpoint = {} }
  }
}

resource "nebius_mk8s_v1_node_group" "cpu" {
  parent_id        = nebius_mk8s_v1_cluster.k.id
  name             = "cpu"
  fixed_node_count = %[2]d
  # Surge first, so drained pods have a node to move to; a drain blocked by
  # a PodDisruptionBudget falls back to pod deletion after a minute.
  strategy = {
    max_surge       = { count = 1 }
    max_unavailable = { count = 0 }
    drain_timeout   = "1m"
  }
  template = {
    metadata  = { labels = { "team" = "e2e", "workload" = %[3]q } }
    taints    = [{ key = "dedicated", value = %[3]q, effect = "PREFER_NO_SCHEDULE" }]
    resources = { platform = "cpu-d3", preset = %[4]q }
    boot_disk = { type = "NETWORK_SSD", size_gibibytes = 64 }
    network_interfaces = [{ subnet_id = nebius_vpc_v1_subnet.sub.id }]
%[5]s
  }
}

resource "nebius_mk8s_v1_node_group" "gpu" {
  parent_id        = nebius_mk8s_v1_cluster.k.id
  name             = "gpu"
  fixed_node_count = 1
  template = {
    taints    = [{ key = "nvidia.com/gpu", value = "present", effect = "NO_SCHEDULE" }]
    resources = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb" }
    boot_disk = { type = "NETWORK_SSD", size_gibibytes = 64 }
    network_interfaces = [{ subnet_id = nebius_vpc_v1_subnet.sub.id }]
  }
}

`, project, cpu.nodes, cpu.workload, cpu.preset, maxPodsLine(cpu.maxPods))
}

func maxPodsLine(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("    max_pods = %d", n)
}

// kubeNodes is `kubectl get nodes -o json`, trimmed.
type kubeNodes struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Taints []struct{ Key, Value, Effect string } `json:"taints"`
		} `json:"spec"`
		Status struct {
			Capacity    map[string]string               `json:"capacity"`
			Allocatable map[string]string               `json:"allocatable"`
			Conditions  []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func (n kubeNodes) ready() []string {
	var out []string
	for _, it := range n.Items {
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				out = append(out, it.Metadata.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestK3sScenario(t *testing.T) {
	e := startEnv(t)
	tf := tfharness.New(t, e.addr, e.inst.Token)
	tf.UseTLSWithoutVerify()
	tf.Write(t, hcl(e.inst.ProjectID, cpuStart))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{
		"nebius_vpc_v1_network.net": "create", "nebius_vpc_v1_subnet.sub": "create", "nebius_mk8s_v1_cluster.k": "create",
		"nebius_mk8s_v1_node_group.cpu": "create", "nebius_mk8s_v1_node_group.gpu": "create",
	})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	st := tf.State(t)
	clusterID := st["nebius_mk8s_v1_cluster.k"]["id"].(string)
	cpuID := st["nebius_mk8s_v1_node_group.cpu"]["id"].(string)
	gpuID := st["nebius_mk8s_v1_node_group.gpu"]["id"].(string)

	cluster, err := e.sdk.Services().MK8S().V1().Cluster().Get(t.Context(), &mk8spb.GetClusterRequest{Id: clusterID})
	if err != nil {
		t.Fatalf("Cluster.Get: %v", err)
	}
	ep := cluster.GetStatus().GetControlPlane().GetEndpoints().GetPublicEndpoint()
	if !strings.HasPrefix(ep, "https://host.docker.internal:") {
		t.Fatalf("public endpoint = %q, want the published k3s port", ep)
	}
	if !strings.Contains(cluster.GetStatus().GetControlPlane().GetAuth().GetClusterCaCertificate(), "BEGIN CERTIFICATE") {
		t.Fatal("cluster CA is not a PEM certificate")
	}

	// The documented user path: a profile, get-credentials, kubectl.
	e.tokenProfile(t, "admin", e.inst.Token)
	e.user.sh(t, "nebius mk8s cluster get-credentials --id "+clusterID+" --external")
	kc := e.user.sh(t, "cat /root/.kube/config")
	if !strings.Contains(kc, "get-token") || !strings.Contains(kc, ep) {
		t.Fatalf("kubeconfig does not use the cluster endpoint and the nebius exec plugin:\n%s", kc)
	}

	nodes := waitNodes(t, e, "", 3)
	cpuNames := e.nodeNames(t, cpuID)
	gpuNames := e.nodeNames(t, gpuID)
	want := slices.Sorted(slices.Values(append(slices.Clone(cpuNames), gpuNames...)))
	if got := nodes.ready(); !slices.Equal(got, want) {
		t.Fatalf("ready nodes = %v, want the node groups' compute instances %v", got, want)
	}
	checkNodeSpec(t, nodes, cpuNames, gpuNames)

	hostCheck(t, e, cluster, len(want))
	workloads(t, e, len(want), gpuNames)
	viewer(t, e, clusterID)
	scale(t, e, tf, cpuID, clusterID)
	inPlace(t, e, tf, cpuID)
	rollout(t, e, tf, cpuID)
	maxPods(t, e, tf, cpuID, gpuNames)

	tf.Destroy(t)
	if left := e.d.k3sContainers(t, "nebius-mocker.cluster="+clusterID); len(left) > 0 {
		t.Fatalf("containers left after the cluster was destroyed: %v", left)
	}
}

// nodeNames returns the compute instances of node group ngID: the mocker
// names Kubernetes nodes after them.
func (e *env) nodeNames(t *testing.T, ngID string) []string {
	t.Helper()
	list, err := e.sdk.Services().Compute().V1().Instance().List(t.Context(), &computepb.ListInstancesRequest{ParentId: e.inst.ProjectID})
	if err != nil {
		t.Fatalf("Instance.List: %v", err)
	}
	var names []string
	for _, in := range list.GetItems() {
		if in.GetMetadata().GetLabels()["mk8s.nebius.ai/node-group-id"] == ngID {
			names = append(names, in.GetMetadata().GetName())
		}
	}
	slices.Sort(names)
	return names
}

// waitNodes waits until kubectl (as profile, "" for the current context)
// sees n Ready nodes.
func waitNodes(t *testing.T, e *env, kubeconfig string, n int) kubeNodes {
	t.Helper()
	var nodes kubeNodes
	eventually(t, 4*time.Minute, func() error {
		raw, err := e.user.try(t.Context(), "kubectl "+kubeconfig+" get nodes -o json")
		if err != nil {
			return fmt.Errorf("kubectl get nodes: %w: %s", err, raw)
		}
		nodes = kubeNodes{}
		if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
			return fmt.Errorf("kubectl get nodes: %w", err)
		}
		if r := nodes.ready(); len(r) != n || len(nodes.Items) != n {
			return fmt.Errorf("%d nodes, %d ready (%v), want %d", len(nodes.Items), len(r), r, n)
		}
		return nil
	})
	return nodes
}

func checkNodeSpec(t *testing.T, nodes kubeNodes, cpu, gpu []string) {
	t.Helper()
	for _, it := range nodes.Items {
		name := it.Metadata.Name
		if it.Status.Capacity["pods"] != "110" || it.Status.Allocatable["pods"] != "110" {
			t.Errorf("node %s pods capacity %q allocatable %q, want 110 (the MK8s default with max_pods unset)",
				name, it.Status.Capacity["pods"], it.Status.Allocatable["pods"])
		}
		switch {
		case slices.Contains(cpu, name):
			if it.Metadata.Labels["team"] != "e2e" || it.Metadata.Labels["workload"] != "batch" {
				t.Errorf("node %s labels = %v, want the template's team=e2e, workload=batch", name, it.Metadata.Labels)
			}
			if len(it.Spec.Taints) != 1 || it.Spec.Taints[0].Key != "dedicated" || it.Spec.Taints[0].Value != "batch" ||
				it.Spec.Taints[0].Effect != "PreferNoSchedule" {
				t.Errorf("node %s taints = %+v, want dedicated=batch:PreferNoSchedule", name, it.Spec.Taints)
			}
			if _, ok := it.Status.Capacity["nvidia.com/gpu"]; ok {
				t.Errorf("CPU node %s advertises GPUs", name)
			}
		case slices.Contains(gpu, name):
			if it.Status.Capacity["nvidia.com/gpu"] != "1" {
				t.Errorf("GPU node %s capacity nvidia.com/gpu = %q, want 1 (the preset's GPU count)", name, it.Status.Capacity["nvidia.com/gpu"])
			}
		default:
			t.Errorf("unexpected node %s", name)
		}
	}
}
