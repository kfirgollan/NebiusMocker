package domain

import (
	"context"
	"fmt"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// stuckRolloutChecks is how many times a rollout that can neither add nor
// remove a node re-reads node readiness (one RolloutStep apart) before it
// fails.
const stuckRolloutChecks = 10

// rollingUpdate replaces a node group's outdated nodes in batches that
// honour status.strategy, as Kubernetes rolling updates do:
//
//   - max_surge (rounded up for a percentage) new nodes may exist above the
//     target, so new nodes are added first;
//   - max_unavailable (rounded down) of the target may be missing, so old
//     nodes are removed while at least target − max_unavailable nodes are
//     ready;
//   - both 0 after rounding means one node may be unavailable.
//
// Each batch creates compute instances, hands the nodes to the backend
// (which drains removed nodes), deletes the removed instances and records
// the progress (node_count, ready_node_count, outdated_node_count), then
// waits RolloutStep on the clock. The cluster lock is held. It returns the
// new nodes and their readiness; on error, the nodes that exist.
func (s *Service) rollingUpdate(ctx context.Context, cn peer.ComputeNodes, c *mk8spb.Cluster, g *mk8spb.NodeGroup, old []peer.NodeInstance) ([]peer.NodeInstance, []NodeReadiness, error) {
	r := &rollout{s: s, cn: cn, c: c, g: g, old: old}
	target := int(g.GetStatus().GetTargetNodeCount())
	surge, unavailable := rolloutBudget(g.GetStatus().GetStrategy(), target)
	if err := r.ensure(ctx); err != nil {
		return r.all(), nil, err
	}
	stuck := 0
	for first := true; len(r.old) > 0 || len(r.fresh) < target; first = false {
		if !first {
			if err := s.pause(ctx); err != nil {
				return r.all(), r.ready, err
			}
		}
		added, err := r.add(ctx, min(target-len(r.fresh), target+surge-len(r.old)-len(r.fresh)))
		if err != nil {
			return r.all(), r.ready, err
		}
		removed, err := r.remove(ctx, min(len(r.old), r.available()-(target-unavailable)))
		if err != nil {
			return r.all(), r.ready, err
		}
		if added || removed {
			stuck = 0
			continue
		}
		stuck++
		if stuck > stuckRolloutChecks {
			return r.all(), r.ready, errs.BadState(g.GetMetadata().GetId(), fmt.Sprintf(
				"rollout cannot proceed: %d of %d nodes are ready, and max_unavailable allows %d missing",
				r.available(), len(r.all()), unavailable))
		}
		if err := r.ensure(ctx); err != nil { // re-read readiness
			return r.all(), r.ready, err
		}
	}
	return r.fresh, r.ready, nil
}

// rollout is the state of one rolling update.
type rollout struct {
	s  *Service
	cn peer.ComputeNodes
	c  *mk8spb.Cluster
	g  *mk8spb.NodeGroup
	// old are the outdated nodes still present, oldest first; fresh the new ones.
	old, fresh []peer.NodeInstance
	// ready is the readiness of all() from the last EnsureNodes.
	ready []NodeReadiness
}

func (r *rollout) all() []peer.NodeInstance {
	return append(append([]peer.NodeInstance(nil), r.old...), r.fresh...)
}

func (r *rollout) available() int {
	n := 0
	for _, x := range r.ready {
		if x.Ready {
			n++
		}
	}
	return n
}

// add creates n new nodes (none when n ≤ 0) and reports whether it did.
func (r *rollout) add(ctx context.Context, n int) (bool, error) {
	if n <= 0 {
		return false, nil
	}
	for range n {
		node, err := r.s.createNode(ctx, r.cn, r.c, r.g, r.all())
		if err != nil {
			return false, err
		}
		r.fresh = append(r.fresh, node)
	}
	return true, r.ensure(ctx)
}

// remove takes the n oldest outdated nodes out of the backend, then deletes
// their instances, and reports whether it did.
func (r *rollout) remove(ctx context.Context, n int) (bool, error) {
	if n <= 0 {
		return false, nil
	}
	victims := r.old[:n]
	r.old = r.old[n:]
	if err := r.ensure(ctx); err != nil {
		r.old = append(victims, r.old...)
		return false, err
	}
	if err := deleteNodes(ctx, r.cn, victims); err != nil {
		return false, err
	}
	return true, r.progress(ctx)
}

// ensure hands the current nodes to the backend and records the progress.
func (r *rollout) ensure(ctx context.Context) error {
	ready, err := r.s.d.Backend.EnsureNodes(ctx, backendGroup(r.g, r.all()))
	if err != nil {
		return err
	}
	r.ready = ready
	return r.progress(ctx)
}

// progress stores the rollout's counts in the node group status.
func (r *rollout) progress(ctx context.Context) error {
	st := r.g.GetStatus()
	st.NodeCount = int64(len(r.old) + len(r.fresh))
	st.ReadyNodeCount = int64(min(r.available(), len(r.old)+len(r.fresh)))
	st.OutdatedNodeCount = int64(len(r.old))
	st.Reconciling = true
	return r.s.d.NodeGroups.Update(ctx, r.g)
}

// pause waits RolloutStep on the clock between batches.
func (s *Service) pause(ctx context.Context) error {
	if s.d.Config.RolloutStep <= 0 {
		return nil
	}
	select {
	case <-s.d.Clock.After(s.d.Config.RolloutStep):
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// rolloutBudget resolves the strategy for target nodes.
func rolloutBudget(st *mk8spb.NodeGroupDeploymentStrategy, target int) (surge, unavailable int) {
	surge = resolveCount(st.GetMaxSurge(), target, roundUp)
	unavailable = resolveCount(st.GetMaxUnavailable(), target, roundDown)
	if surge == 0 && unavailable == 0 {
		unavailable = 1
	}
	return surge, unavailable
}

// rounding says how a percentage of the target becomes a node count.
type rounding int

const (
	roundDown rounding = iota
	roundUp
)

// resolveCount is a count, or a percentage of target rounded as r says.
func resolveCount(p *mk8spb.PercentOrCount, target int, r rounding) int {
	if _, ok := p.GetValue().(*mk8spb.PercentOrCount_Percent); !ok {
		return int(p.GetCount())
	}
	n := target * int(p.GetPercent())
	if r == roundUp {
		return (n + 99) / 100
	}
	return n / 100
}
