package k3s

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// groupWith is a one-node group "n1" with labels and taints.
func groupWith(labels map[string]string, taints ...domain.NodeTaint) domain.BackendNodeGroup {
	g := testGroupSpec(testCluster, "n1")
	g.Labels, g.Taints = labels, taints
	return g
}

func TestFirstApplicationRecordsAnnotations(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, groupWith(map[string]string{"team": "ml", "node-role.kubernetes.io/gpu": ""},
		domain.NodeTaint{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}))
	n := h.kube.n1()
	if got, want := n.Annotations[annotationLabels], `["node-role.kubernetes.io/gpu","team"]`; got != want {
		t.Errorf("annotation %s = %s, want %s", annotationLabels, got, want)
	}
	if got, want := n.Annotations[annotationTaints], `[{"key":"dedicated","value":"gpu","effect":"NoSchedule"}]`; got != want {
		t.Errorf("annotation %s = %s, want %s", annotationTaints, got, want)
	}
	// A second reconcile with nothing changed does not touch the node again.
	before, _, _ := h.kube.snapshot()
	h.ensureNodes(t, groupWith(map[string]string{"team": "ml", "node-role.kubernetes.io/gpu": ""},
		domain.NodeTaint{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}))
	if after, _, _ := h.kube.snapshot(); len(after) != len(before) {
		t.Errorf("an unchanged group made %d more UpdateNode calls", len(after)-len(before))
	}
}

func TestLabelsChangeInPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, groupWith(map[string]string{"team": "ml", "tier": "gold", "node-role.kubernetes.io/gpu": "1"}))
	before := h.agents(t)["n1"].ID
	creates := h.docker.createCount()

	got := h.ensureNodes(t, groupWith(map[string]string{"team": "other", "new": "x", "node-role.kubernetes.io/gpu": "1"}))
	if len(got) != 1 || !got[0].Ready {
		t.Fatalf("EnsureNodes = %+v, want n1 ready", got)
	}
	if h.agents(t)["n1"].ID != before || h.docker.createCount() != creates {
		t.Fatalf("a label change recreated the agent")
	}
	n := h.kube.n1()
	want := map[string]string{"kubernetes.io/hostname": "n1", "team": "other", "new": "x", "node-role.kubernetes.io/gpu": "1"}
	if diff := cmp.Diff(want, n.Labels); diff != "" {
		t.Errorf("node labels mismatch (-want +got):\n%s", diff)
	}
	if got, want := n.Annotations[annotationLabels], `["new","node-role.kubernetes.io/gpu","team"]`; got != want {
		t.Errorf("annotation %s = %s, want %s", annotationLabels, got, want)
	}
	if h.docker.eventIndex("cordon:n1") >= 0 {
		t.Error("a label change drained the node")
	}
}

func TestForeignLabelsAndTaintsSurvive(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, groupWith(map[string]string{"team": "ml"}, domain.NodeTaint{Key: "dedicated", Value: "a", Effect: "NoSchedule"}))
	h.kube.addForeign("n1", "kubectl-label", "yes", kubeTaint{Key: "node.kubernetes.io/unreachable", Effect: "NoExecute"})

	h.ensureNodes(t, groupWith(map[string]string{"team": "other"}, domain.NodeTaint{Key: "dedicated", Value: "b", Effect: "NoSchedule"}))
	n := h.kube.n1()
	if n.Labels["kubectl-label"] != "yes" || n.Labels["team"] != "other" {
		t.Errorf("labels = %v, want the foreign label kept and team=other", n.Labels)
	}
	want := []kubeTaint{
		{Key: "node.kubernetes.io/unreachable", Effect: "NoExecute"},
		{Key: "dedicated", Value: "b", Effect: "NoSchedule"},
	}
	if diff := cmp.Diff(want, n.Taints); diff != "" {
		t.Errorf("node taints mismatch (-want +got):\n%s", diff)
	}

	// Dropping the group's labels and taints removes only its own.
	h.ensureNodes(t, groupWith(nil))
	n = h.kube.n1()
	if _, ok := n.Labels["team"]; ok || n.Labels["kubectl-label"] != "yes" {
		t.Errorf("labels after dropping the group's = %v, want team gone and the foreign label kept", n.Labels)
	}
	if diff := cmp.Diff(want[:1], n.Taints); diff != "" {
		t.Errorf("node taints after dropping the group's (-want +got):\n%s", diff)
	}
}

func TestTaintsChangeInPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, groupWith(nil, domain.NodeTaint{Key: "a", Value: "1", Effect: "NoSchedule"}))
	before := h.agents(t)["n1"].ID

	tests := []struct {
		name   string
		taints []domain.NodeTaint
		want   []kubeTaint
	}{
		{"value", []domain.NodeTaint{{Key: "a", Value: "2", Effect: "NoSchedule"}}, []kubeTaint{{Key: "a", Value: "2", Effect: "NoSchedule"}}},
		{"effect", []domain.NodeTaint{{Key: "a", Value: "2", Effect: "NoExecute"}}, []kubeTaint{{Key: "a", Value: "2", Effect: "NoExecute"}}},
		{"added", []domain.NodeTaint{{Key: "a", Value: "2", Effect: "NoExecute"}, {Key: "b", Effect: "NoSchedule"}}, []kubeTaint{
			{Key: "a", Value: "2", Effect: "NoExecute"}, {Key: "b", Effect: "NoSchedule"},
		}},
		{"removed", nil, nil},
	}
	for _, tt := range tests {
		h.ensureNodes(t, groupWith(nil, tt.taints...))
		if diff := cmp.Diff(tt.want, h.kube.n1().Taints, cmp.FilterValues(func(a, b []kubeTaint) bool { return len(a) == 0 && len(b) == 0 }, cmp.Ignore())); diff != "" {
			t.Errorf("taints after %s (-want +got):\n%s", tt.name, diff)
		}
		if h.agents(t)["n1"].ID != before {
			t.Fatalf("a taint change (%s) recreated the agent", tt.name)
		}
	}
}

func TestRestKubeUpdateNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		patch nodePatch
		want  string
	}{
		{
			name: "labels set and removed, annotations, taints",
			patch: nodePatch{
				Labels:      map[string]*string{"a": ptr("1"), "gone": nil, "empty": ptr("")},
				Annotations: map[string]string{annotationLabels: `["a"]`},
				Taints:      []kubeTaint{{Key: "k", Value: "v", Effect: "NoSchedule"}, {Key: "k2", Effect: "NoExecute"}},
			},
			want: `{"metadata":{"labels":{"a":"1","gone":null,"empty":""},"annotations":{"nebius-mocker/node-group-labels":"[\"a\"]"}},
				"spec":{"taints":[{"key":"k","value":"v","effect":"NoSchedule"},{"key":"k2","effect":"NoExecute"}]}}`,
		},
		{
			name:  "no taints given leaves spec out",
			patch: nodePatch{Labels: map[string]*string{"a": ptr("1")}},
			want:  `{"metadata":{"labels":{"a":"1"}}}`,
		},
		{
			name:  "empty taints clears them",
			patch: nodePatch{Taints: []kubeTaint{}},
			want:  `{"metadata":{},"spec":{"taints":[]}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ks, k := newKubeServer(t, nil)
			if err := k.UpdateNode(t.Context(), "n1", tt.patch); err != nil {
				t.Fatalf("UpdateNode: %v", err)
			}
			got := ks.last(t)
			if got.Method != http.MethodPatch || got.Path != "/api/v1/nodes/n1" || got.ContentType != "application/merge-patch+json" {
				t.Fatalf("UpdateNode request = %+v, want a merge patch of /api/v1/nodes/n1", got)
			}
			assertJSON(t, got.Body, tt.want)
		})
	}
}

func TestRestKubeNodesReadsAnnotationsAndTaints(t *testing.T) {
	t.Parallel()
	_, k := newKubeServer(t, map[string]cannedResponse{"GET /api/v1/nodes": {http.StatusOK, `{"items":[
		{"metadata":{"name":"n1","annotations":{"a":"b"}},"spec":{"taints":[{"key":"k","value":"v","effect":"NoSchedule"}]},
		 "status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`}})
	nodes, err := k.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	want := kubeNode{Ready: true, Annotations: map[string]string{"a": "b"}, Taints: []kubeTaint{{Key: "k", Value: "v", Effect: "NoSchedule"}}}
	if diff := cmp.Diff(want, nodes["n1"]); diff != "" {
		t.Errorf("Nodes()[n1] mismatch (-want +got):\n%s", diff)
	}
}
