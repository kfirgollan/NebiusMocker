package domain_test

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
)

func count(n int64) *mk8spb.PercentOrCount {
	return &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Count{Count: n}}
}

func percent(n int64) *mk8spb.PercentOrCount {
	return &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Percent{Percent: n}}
}

// A template change replaces nodes in batches that honour the strategy: the
// backend sees each batch (node counts per EnsureNodes call), and the nodes
// never exceed target+max_surge or drop below target−max_unavailable.
func TestRollingUpdateBatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		strategy *mk8spb.NodeGroupDeploymentStrategy
		// sizes are the backend's node counts: the start, then each batch.
		sizes []int
	}{
		{"default: one unavailable, no surge", nil, []int{3, 2, 3, 2, 3, 2, 3}},
		{"surge 1, none unavailable", &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: count(1), MaxUnavailable: count(0)}, []int{3, 4, 3, 4, 3, 4, 3}},
		{"surge 50% rounds up to 2", &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: percent(50), MaxUnavailable: count(0)}, []int{3, 5, 3, 4, 3}},
		{"all unavailable: recreate", &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: count(0), MaxUnavailable: percent(100)}, []int{3, 0, 3}},
		{"both 0% after rounding: one unavailable", &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: percent(0), MaxUnavailable: percent(10)}, []int{3, 2, 3, 2, 3, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", "")
			spec := fixedSpec(3)
			spec.Strategy = tt.strategy
			id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", spec))
			before := e.compute.instanceIDs(id)
			created := len(e.backend.sizesOf(id))

			g := e.group(t, id)
			next := proto.CloneOf(g.GetSpec())
			next.Template.Resources = preset("cpu-d3", "4vcpu-16gb")
			done(t)(e.update(t.Context(), g, next))

			if got := e.backend.sizesOf(id)[created:]; !slices.Equal(got, tt.sizes) {
				t.Errorf("backend node counts during the rollout = %v, want %v", got, tt.sizes)
			}
			after := e.compute.instanceIDs(id)
			for _, old := range before {
				if slices.Contains(after, old) {
					t.Errorf("outdated node %s survived the rollout", old)
				}
			}
			if got := e.backend.group(id).DrainTimeout; got != 10*time.Minute {
				t.Errorf("backend drain timeout = %s, want the default 10m", got)
			}
			st := e.group(t, id).GetStatus()
			if len(after) != 3 || st.GetNodeCount() != 3 || st.GetReadyNodeCount() != 3 || st.GetOutdatedNodeCount() != 0 || st.GetReconciling() {
				t.Errorf("after the rollout: %d instances, status %v", len(after), st)
			}
		})
	}
}

// A rollout that can neither add nor remove a node (new nodes never get
// ready and nothing may be unavailable) fails instead of spinning.
func TestRollingUpdateStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	spec := fixedSpec(2)
	spec.Strategy = &mk8spb.NodeGroupDeploymentStrategy{MaxSurge: count(1), MaxUnavailable: count(0)}
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", spec))
	e.backend.mu.Lock()
	e.backend.notReady = "no capacity"
	e.backend.mu.Unlock()

	g := e.group(t, id)
	next := proto.CloneOf(g.GetSpec())
	next.Template.Resources = preset("cpu-d3", "4vcpu-16gb")
	op, err := e.update(t.Context(), g, next)
	if err != nil {
		t.Fatal(err)
	}
	if op.Err == nil {
		t.Fatal("stuck rollout succeeded")
	}
	st := e.group(t, id).GetStatus()
	if st.GetOutdatedNodeCount() != 2 || st.GetNodeCount() != 3 || st.GetReadyNodeCount() != 0 {
		t.Errorf("stuck rollout status = %v, want 2 outdated of 3 nodes, none ready", st)
	}
}
