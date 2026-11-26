package k3s

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

// kubePod is the part of a Pod draining looks at.
type kubePod struct {
	Namespace, Name string
	// DaemonSet pods are left alone (kubectl drain --ignore-daemonsets):
	// their controller would recreate them on the node at once.
	DaemonSet bool
	// Mirror pods are the kubelet's static pods; the API cannot evict them.
	Mirror bool
	// Terminating pods are already going away.
	Terminating bool
}

// errEvictionBlocked is an eviction a PodDisruptionBudget refused (HTTP
// 429); it is retried until the drain timeout.
var errEvictionBlocked = errors.New("eviction blocked by a PodDisruptionBudget")

// removeNodes drains the agents' nodes, concurrently, then removes them.
func (b *Backend) removeNodes(ctx context.Context, st *clusterState, agents []dockerhost.Container, drainTimeout time.Duration) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, c := range agents {
		g.Go(func() error {
			if st != nil {
				b.drain(gctx, st, c.Labels[LabelNode], drainTimeout)
			}
			return b.removeAgent(gctx, st, c)
		})
	}
	return g.Wait()
}

// drain does what `kubectl drain --ignore-daemonsets --delete-emptydir-data`
// does before a node goes away: cordon it, then evict its pods through the
// Eviction API, which respects PodDisruptionBudgets, retrying refused
// evictions, until no evictable pod is left. timeout (the node group's
// drain_timeout; 0: no limit) bounds it; when it runs out the node is
// removed anyway, as Nebius documents. Drain problems are logged, not
// returned: they never keep a node from being removed.
func (b *Backend) drain(ctx context.Context, st *clusterState, node string, timeout time.Duration) {
	log := b.log.With("node", node)
	if err := st.kube.Cordon(ctx, node); err != nil {
		log.WarnContext(ctx, "mk8s k3s: cordon failed; removing the node undrained", "error", err)
		return
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = b.clk.Now().Add(timeout)
	}
	for {
		pods, err := st.kube.PodsOn(ctx, node)
		if err != nil {
			log.WarnContext(ctx, "mk8s k3s: list pods to drain failed; removing the node", "error", err)
			return
		}
		left := b.evictPods(ctx, st, pods, log)
		if left == 0 {
			log.InfoContext(ctx, "mk8s k3s: node drained")
			return
		}
		if !deadline.IsZero() && !b.clk.Now().Before(deadline) {
			// drain_timeout: "falling back to pod deletion".
			log.WarnContext(ctx, "mk8s k3s: drain timeout reached; deleting the pods left", "pods_left", left, "drain_timeout", timeout)
			b.deletePods(ctx, st, pods, log)
			return
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return
		}
	}
}

// evictPods asks for the eviction of every evictable pod and returns how many
// pods are still there (evicted just now or not, terminating included).
func (b *Backend) evictPods(ctx context.Context, st *clusterState, pods []kubePod, log *slog.Logger) int {
	left := 0
	for _, p := range pods {
		if p.DaemonSet || p.Mirror {
			continue
		}
		left++
		if p.Terminating {
			continue
		}
		if err := st.kube.Evict(ctx, p.Namespace, p.Name); err != nil && !errors.Is(err, errEvictionBlocked) {
			log.WarnContext(ctx, "mk8s k3s: evict failed", "pod", p.Namespace+"/"+p.Name, "error", err)
		}
	}
	return left
}

// deletePods deletes the evictable pods without honouring
// PodDisruptionBudgets (the drain timeout passed).
func (*Backend) deletePods(ctx context.Context, st *clusterState, pods []kubePod, log *slog.Logger) {
	for _, p := range pods {
		if p.DaemonSet || p.Mirror || p.Terminating {
			continue
		}
		if err := st.kube.DeletePod(ctx, p.Namespace, p.Name); err != nil {
			log.WarnContext(ctx, "mk8s k3s: delete pod failed", "pod", p.Namespace+"/"+p.Name, "error", err)
		}
	}
}
