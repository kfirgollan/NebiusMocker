package k3s

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// Annotations on a Node recording which labels and taints came from its
// node group, so a template change can remove the ones the group dropped
// without touching labels and taints others set (kubectl, controllers).
const (
	annotationLabels = "nebius-mocker/node-group-labels"
	annotationTaints = "nebius-mocker/node-group-taints"
)

// kubeTaint is a Node taint.
type kubeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// nodePatch is a JSON merge patch of a Node's metadata and taints.
type nodePatch struct {
	// Labels to set; a nil value removes the label.
	Labels      map[string]*string
	Annotations map[string]string
	// Taints replaces spec.taints when non-nil.
	Taints []kubeTaint
}

func (p nodePatch) empty() bool {
	return len(p.Labels) == 0 && len(p.Annotations) == 0 && p.Taints == nil
}

// applyNode brings a ready node to the group's labels and taints, in place
// (a template's metadata and taints change nodes without recreating them),
// and advertises its GPUs.
func (*Backend) applyNode(ctx context.Context, st *clusterState, n domain.BackendNode, kn kubeNode, g domain.BackendNodeGroup) error {
	p := labelPatch(kn, g.Labels)
	taints, changed := taintList(kn, g.Taints)
	if changed {
		p.Taints = taints
		p.Annotations[annotationTaints] = taintRecord(g.Taints)
	}
	if !p.empty() {
		if err := st.kube.UpdateNode(ctx, n.Name, p); err != nil {
			return fmt.Errorf("update node %s: %w", n.Name, err)
		}
	}
	if n.GPUs > 0 && kn.Capacity[gpuResource] != strconv.Itoa(n.GPUs) {
		if err := st.kube.SetExtendedResource(ctx, n.Name, gpuResource, n.GPUs); err != nil {
			return fmt.Errorf("advertise GPUs of node %s: %w", n.Name, err)
		}
	}
	return nil
}

// labelPatch sets the group's labels and removes the ones it set before
// and no longer has.
func labelPatch(kn kubeNode, want map[string]string) nodePatch {
	p := nodePatch{Labels: map[string]*string{}, Annotations: map[string]string{}}
	for k, v := range want {
		if got, ok := kn.Labels[k]; !ok || got != v {
			p.Labels[k] = &v
		}
	}
	var before []string
	_ = json.Unmarshal([]byte(kn.Annotations[annotationLabels]), &before) // absent or foreign: nothing recorded
	for _, k := range before {
		if _, keep := want[k]; !keep {
			if _, ok := kn.Labels[k]; ok {
				p.Labels[k] = nil
			}
		}
	}
	keys := slices.Sorted(maps.Keys(want))
	if rec := mustJSON(keys); kn.Annotations[annotationLabels] != rec {
		p.Annotations[annotationLabels] = rec
	}
	return p
}

// taintList is the node's taints with the group's replaced by want; the
// group's previous taints come from the annotation, or, on a node that has
// none yet, are the ones the agent registered with (want itself).
func taintList(kn kubeNode, want []domain.NodeTaint) ([]kubeTaint, bool) {
	desired := make([]kubeTaint, 0, len(want))
	for _, t := range want {
		desired = append(desired, kubeTaint{Key: t.Key, Value: t.Value, Effect: t.Effect})
	}
	var before []kubeTaint
	if err := json.Unmarshal([]byte(kn.Annotations[annotationTaints]), &before); err != nil {
		before = desired
	}
	isGroup := func(t kubeTaint) bool {
		return slices.ContainsFunc(before, func(b kubeTaint) bool { return b.Key == t.Key && b.Effect == t.Effect })
	}
	out := make([]kubeTaint, 0, len(kn.Taints)+len(desired))
	for _, t := range kn.Taints {
		if !isGroup(t) {
			out = append(out, t)
		}
	}
	out = append(out, desired...)
	return out, !slices.Equal(out, kn.Taints) || kn.Annotations[annotationTaints] != taintRecord(want)
}

// taintRecord is the annotationTaints value for the group's taints.
func taintRecord(want []domain.NodeTaint) string {
	out := make([]kubeTaint, 0, len(want))
	for _, t := range want {
		out = append(out, kubeTaint{Key: t.Key, Value: t.Value, Effect: t.Effect})
	}
	return mustJSON(out)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v) //nolint:errchkjson // strings and plain structs always marshal
	return string(b)
}
