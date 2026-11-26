package contract

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const rolloutStep = 10 * time.Minute

// A template change rolls the nodes out in batches that honour the
// strategy (max_surge 1, max_unavailable 0), one batch per RolloutStep on
// the mock clock. Between batches the node group status and the compute
// instances show the progress.
func TestMK8sNodeGroupRollingUpdate(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	e := startMK8s(t, testkit.WithClock(clk), testkit.WithOperationDelay(0),
		testkit.WithConfig(func(c *config.Config) { c.MK8s.RolloutStep = rolloutStep }))
	cluster := e.newCluster(t, "rolling")
	spec := fixedGroup(3)
	spec.Strategy = &mk8spb.NodeGroupDeploymentStrategy{
		MaxSurge:       &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Count{Count: 1}},
		MaxUnavailable: &mk8spb.PercentOrCount{Value: &mk8spb.PercentOrCount_Count{Count: 0}},
	}
	id := e.newGroup(t, cluster, "g", spec)
	old := instanceIDs(e.nodes(t, id))

	g := e.getGroup(t, id)
	next := proto.CloneOf(g.GetSpec())
	next.Template = cpuTemplate("4vcpu-16gb")
	op, err := e.groups.Update(t.Context(), &mk8spb.UpdateNodeGroupRequest{Metadata: g.GetMetadata(), Spec: next})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	for outdated := int64(2); outdated >= 1; outdated-- {
		e.awaitPause(t, clk)
		st := e.getGroup(t, id).GetStatus()
		ids := instanceIDs(e.nodes(t, id))
		kept := 0
		for _, o := range old {
			if slices.Contains(ids, o) {
				kept++
			}
		}
		if st.GetOutdatedNodeCount() != outdated || st.GetNodeCount() != 3 || st.GetReadyNodeCount() != 3 ||
			!st.GetReconciling() || len(ids) != 3 || int64(kept) != outdated {
			t.Fatalf("batch with %d outdated left: status %v, %d instances, %d old", outdated, st, len(ids), kept)
		}
		if got, err := e.groups.Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: id}); err != nil || got.GetStatus().GetState() != mk8spb.NodeGroupStatus_RUNNING {
			t.Fatalf("node group during the rollout: %v, %v", got.GetStatus(), err)
		}
		if op.Done() {
			t.Fatal("update finished before the rollout")
		}
		clk.Advance(rolloutStep)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("update operation: %v", err)
	}
	st := e.getGroup(t, id).GetStatus()
	ids := instanceIDs(e.nodes(t, id))
	if st.GetOutdatedNodeCount() != 0 || st.GetNodeCount() != 3 || st.GetReconciling() || len(ids) != 3 {
		t.Fatalf("after the rollout: status %v, instances %v", st, ids)
	}
	for _, o := range old {
		if slices.Contains(ids, o) {
			t.Errorf("outdated node %s survived the rollout", o)
		}
	}
}

// awaitPause waits (in real time, bounded) until the rollout is parked on
// the mock clock between two batches.
func (e *mk8sEnv) awaitPause(t *testing.T, clk *clock.Manual) {
	t.Helper()
	for range 2000 {
		if clk.HasWaiter(clk.Now().Add(rolloutStep)) {
			return
		}
		_, _ = e.groups.Get(t.Context(), &mk8spb.GetNodeGroupRequest{Id: "mk8snodegroup-e00none"}) // a round trip: the rollout goroutine runs meanwhile
	}
	t.Fatal("the rollout never paused between batches")
}

func instanceIDs(insts []*computepb.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.GetMetadata().GetId())
	}
	return out
}
