package k3s

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestScaleDownDrainsBeforeRemoval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	h.kube.addPods("n2",
		kubePod{Namespace: "apps", Name: "web-1"},
		kubePod{Namespace: "apps", Name: "web-2"},
		kubePod{Namespace: "kube-system", Name: "ds-agent", DaemonSet: true},
		kubePod{Namespace: "kube-system", Name: "static-etcd", Mirror: true},
	)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1"))

	cordon, evictWeb1 := h.docker.eventIndex("cordon:n2"), h.docker.eventIndex("evict:n2:apps/web-1")
	evictWeb2, remove := h.docker.eventIndex("evict:n2:apps/web-2"), h.docker.eventIndex("remove:n2")
	if cordon < 0 || cordon >= evictWeb1 || cordon >= evictWeb2 || evictWeb1 >= remove || evictWeb2 >= remove {
		t.Fatalf("event order = %v, want cordon, then both evictions, then removal of n2", h.docker.eventsWithPrefix(""))
	}
	for _, e := range h.docker.eventsWithPrefix("evict:") {
		if strings.Contains(e, "ds-agent") || strings.Contains(e, "static-etcd") {
			t.Errorf("event %q: DaemonSet and mirror pods must not be evicted", e)
		}
	}
	if got := h.docker.eventsWithPrefix("cordon:"); !slices.Equal(got, []string{"cordon:n2"}) {
		t.Errorf("cordoned nodes = %v, want only n2", got)
	}
}

func TestDrainRetriesBlockedEviction(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	h.kube.addPods("n2", kubePod{Namespace: "apps", Name: "db-0"})
	h.kube.blockEvictions("apps", "db-0", 3)
	g := testGroupSpec(testCluster, "n1")
	g.DrainTimeout = time.Minute
	start := h.clk.Now()
	h.ensureNodes(t, g)

	if got := len(h.docker.eventsWithPrefix("evict:n2:apps/db-0")); got != 4 {
		t.Errorf("evictions of db-0 = %d, want 4 (3 refused, then accepted)", got)
	}
	if left := h.kube.podNames("n2"); len(left) != 0 {
		t.Errorf("pods left on n2 = %v, want none", left)
	}
	if h.docker.eventIndex("remove:n2") < h.docker.eventIndex("evict:n2:apps/db-0") {
		t.Errorf("n2 was removed before its pod was evicted")
	}
	if waited := h.clk.Now().Sub(start); waited >= time.Minute {
		t.Errorf("drain used %s, want it to end before the 1m timeout", waited)
	}
}

func TestDrainTimeoutRemovesNodeAnyway(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	h.kube.addPods("n2", kubePod{Namespace: "apps", Name: "stuck"},
		kubePod{Namespace: "kube-system", Name: "ds", DaemonSet: true}, kubePod{Namespace: "kube-system", Name: "static", Mirror: true})
	h.kube.blockEvictions("apps", "stuck", -1)
	g := testGroupSpec(testCluster, "n1")
	g.DrainTimeout = 30 * time.Second
	start := h.clk.Now()
	got := h.ensureNodes(t, g)

	if len(got) != 1 || !got[0].Ready {
		t.Fatalf("EnsureNodes = %+v, want n1 ready", got)
	}
	if _, ok := h.agents(t)["n2"]; ok {
		t.Error("n2 still has an agent after the drain timeout")
	}
	if waited := h.clk.Now().Sub(start); waited < 30*time.Second {
		t.Errorf("gave up after %s, want the full 30s drain timeout", waited)
	}
	if n := len(h.docker.eventsWithPrefix("evict:n2:apps/stuck")); n < 2 {
		t.Errorf("evictions attempted = %d, want retries", n)
	}
	// After the timeout the pod is deleted (falling back to pod deletion), before the node goes.
	if del, rm := h.docker.eventIndex("delete:n2:apps/stuck"), h.docker.eventIndex("remove:n2"); del < 0 || del > rm {
		t.Errorf("event order = %v, want the stuck pod deleted before n2 is removed", h.docker.eventsWithPrefix(""))
	}
	if got := h.docker.eventsWithPrefix("delete:"); len(got) != 1 {
		t.Errorf("deleted pods = %v, want only the blocked pod (not the DaemonSet or mirror pod)", got)
	}
}

func TestRolloutBatchStartsNewNodeBeforeDrainingOld(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "old-1", "old-2"))
	h.kube.addPods("old-2", kubePod{Namespace: "apps", Name: "web"})

	got := h.ensureNodes(t, testGroupSpec(testCluster, "old-1", "new-1"))
	if names, ready := readyNames(got); !slices.Equal(names, []string{"old-1", "new-1"}) || !slices.Equal(ready, []bool{true, true}) {
		t.Fatalf("EnsureNodes = %+v, want old-1 and new-1 ready", got)
	}
	idx := map[string]int{}
	for _, e := range []string{"create:new-1", "ready:new-1", "cordon:old-2", "evict:old-2:apps/web", "remove:old-2"} {
		idx[e] = h.docker.eventIndex(e)
		if idx[e] < 0 {
			t.Fatalf("event %q missing in %v", e, h.docker.eventsWithPrefix(""))
		}
	}
	order := []string{"create:new-1", "ready:new-1", "cordon:old-2", "evict:old-2:apps/web", "remove:old-2"}
	if !slices.IsSortedFunc(order, func(a, b string) int { return idx[a] - idx[b] }) {
		t.Errorf("event order = %v, want %v", h.docker.eventsWithPrefix(""), order)
	}
}

func TestStaleAgentDrainedBeforeRecreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	g := testGroupSpec(testCluster, "n1")
	h.ensureNodes(t, g)
	h.kube.addPods("n1", kubePod{Namespace: "apps", Name: "web"})
	g.Nodes[0].GPUs = 1 // a GPU change needs a new agent
	h.ensureNodes(t, g)
	if c, e, r := h.docker.eventIndex("cordon:n1"), h.docker.eventIndex("evict:n1:apps/web"), h.docker.eventIndex("remove:n1"); c < 0 || e < c || r < e {
		t.Fatalf("event order = %v, want cordon, evict, remove of the stale agent", h.docker.eventsWithPrefix(""))
	}
}

// An empty group may be a rollout batch that removes before it adds, so its
// nodes are drained like any other.
func TestEmptyNodesDrainsThenRemoves(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	h.ensureNodes(t, testGroupSpec(testCluster, "n1", "n2"))
	h.kube.addPods("n1", kubePod{Namespace: "apps", Name: "web"})
	if _, err := h.b.EnsureNodes(t.Context(), domain.BackendNodeGroup{ClusterID: testCluster, NodeGroupID: testGroup, Version: "1.35"}); err != nil {
		t.Fatalf("EnsureNodes with no nodes: %v", err)
	}
	cordon, evict, remove := h.docker.eventIndex("cordon:n1"), h.docker.eventIndex("evict:n1:apps/web"), h.docker.eventIndex("remove:n1")
	if cordon < 0 || evict < cordon || remove < evict {
		t.Errorf("event order = %v, want cordon, evict, remove of n1", h.docker.eventsWithPrefix(""))
	}
	if h.docker.eventIndex("cordon:n2") < 0 {
		t.Errorf("n2 was not cordoned: %v", h.docker.eventsWithPrefix(""))
	}
	if n := len(h.agents(t)); n != 0 {
		t.Errorf("agent containers = %d, want none", n)
	}
	if _, _, deleted := h.kube.snapshot(); len(deleted) != 2 {
		t.Errorf("deleted Kubernetes nodes = %v, want both", deleted)
	}
}

func TestRestKubeDeletePod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "deleted", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ks, k := newKubeServer(t, map[string]cannedResponse{"DELETE /api/v1/namespaces/apps/pods/web": {tt.status, `{}`}})
			if err := k.DeletePod(t.Context(), "apps", "web"); (err != nil) != tt.wantErr {
				t.Fatalf("DeletePod() = %v, want error %v", err, tt.wantErr)
			}
			if req := ks.last(t); req.Method != http.MethodDelete || req.Path != "/api/v1/namespaces/apps/pods/web" {
				t.Fatalf("DeletePod request = %+v", req)
			}
		})
	}
}

func TestRestKubeCordon(t *testing.T) {
	t.Parallel()
	ks, k := newKubeServer(t, nil)
	if err := k.Cordon(t.Context(), "n1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	got := ks.last(t)
	if got.Method != http.MethodPatch || got.Path != "/api/v1/nodes/n1" || got.ContentType != "application/merge-patch+json" {
		t.Fatalf("Cordon request = %+v, want a merge patch of /api/v1/nodes/n1", got)
	}
	assertJSON(t, got.Body, `{"spec":{"unschedulable":true}}`)
}

func TestRestKubePodsOn(t *testing.T) {
	t.Parallel()
	ks, k := newKubeServer(t, map[string]cannedResponse{"GET /api/v1/pods": {http.StatusOK, `{"items":[
		{"metadata":{"name":"web","namespace":"apps","ownerReferences":[{"kind":"ReplicaSet","controller":true}]}},
		{"metadata":{"name":"ds","namespace":"kube-system","ownerReferences":[{"kind":"DaemonSet","controller":true}]}},
		{"metadata":{"name":"owned-ds-not-controller","namespace":"x","ownerReferences":[{"kind":"DaemonSet"}]}},
		{"metadata":{"name":"static","namespace":"kube-system","annotations":{"kubernetes.io/config.mirror":"abc"}}},
		{"metadata":{"name":"going","namespace":"apps","deletionTimestamp":"2026-01-01T00:00:00Z"}}]}`}})
	got, err := k.PodsOn(t.Context(), "n 1")
	if err != nil {
		t.Fatalf("PodsOn: %v", err)
	}
	want := []kubePod{
		{Namespace: "apps", Name: "web"},
		{Namespace: "kube-system", Name: "ds", DaemonSet: true},
		{Namespace: "x", Name: "owned-ds-not-controller"},
		{Namespace: "kube-system", Name: "static", Mirror: true},
		{Namespace: "apps", Name: "going", Terminating: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("PodsOn mismatch (-want +got):\n%s", diff)
	}
	req := ks.last(t)
	q, err := url.ParseQuery(req.Query)
	if err != nil || req.Method != http.MethodGet || req.Path != "/api/v1/pods" || q.Get("fieldSelector") != "spec.nodeName=n 1" {
		t.Errorf("PodsOn request = %+v, want GET /api/v1/pods?fieldSelector=spec.nodeName=<node>", req)
	}
}

func TestRestKubeEvict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		want    error
		wantErr bool
	}{
		{name: "accepted", status: http.StatusCreated},
		{name: "pod gone", status: http.StatusNotFound},
		{name: "blocked by a budget", status: http.StatusTooManyRequests, want: errEvictionBlocked, wantErr: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ks, k := newKubeServer(t, map[string]cannedResponse{"POST /api/v1/namespaces/apps/pods/web/eviction": {tt.status, `{}`}})
			err := k.Evict(t.Context(), "apps", "web")
			if (err != nil) != tt.wantErr || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Evict() = %v, want error %v (%v)", err, tt.wantErr, tt.want)
			}
			req := ks.last(t)
			if req.Method != http.MethodPost || req.Path != "/api/v1/namespaces/apps/pods/web/eviction" {
				t.Fatalf("Evict request = %+v", req)
			}
			assertJSON(t, req.Body, `{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"web","namespace":"apps"}}`)
		})
	}
}
