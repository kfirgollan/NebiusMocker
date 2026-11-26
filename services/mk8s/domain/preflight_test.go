package domain_test

import (
	"slices"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func diagPaths(r domain.PreflightResult) []string {
	var out []string
	for _, d := range r.Diagnostics {
		if d.GetSeverity() != commonpb.PreflightCheckDiagnostic_ERROR || d.GetSummary() == "" {
			continue
		}
		out = append(out, d.GetPath())
	}
	return out
}

func TestPreflightCreate(t *testing.T) {
	t.Parallel()
	badPreset := func() *mk8spb.NodeGroupSpec {
		s := fixedSpec(1)
		s.Template.Resources = preset("cpu-d3", "1vcpu-1gb")
		s.Template.MaxPods = 32
		return s
	}
	tests := []struct {
		name      string
		action    commonpb.PreflightCheckContext_Action
		unknown   string
		parent    string // "" uses the cluster
		spec      *mk8spb.NodeGroupSpec
		wantPaths []string
	}{
		{"valid", commonpb.PreflightCheckContext_CREATE, "", "", fixedSpec(1), nil},
		{"inferred create", commonpb.PreflightCheckContext_UNSPECIFIED, "", "", fixedSpec(1), nil},
		{
			"every problem is reported", commonpb.PreflightCheckContext_CREATE, "", "", badPreset(),
			[]string{"spec.template.max_pods", "spec.template.resources.preset"},
		},
		{
			"unknown preset is not checked", commonpb.PreflightCheckContext_CREATE, "spec.template.resources.preset", "", badPreset(),
			[]string{"spec.template.max_pods"},
		},
		{"everything unknown", commonpb.PreflightCheckContext_CREATE, "*", "", badPreset(), nil},
		{
			"unknown cluster skips cluster checks", commonpb.PreflightCheckContext_CREATE, "metadata.parent_id", "mk8scluster-e00later", badPreset(),
			[]string{"spec.template.max_pods"},
		},
		{"missing cluster", commonpb.PreflightCheckContext_CREATE, "", "mk8scluster-e00nope", fixedSpec(1), []string{""}},
		{"delete", commonpb.PreflightCheckContext_DELETE, "", "", badPreset(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.createCluster(t, "c", "")
			parent := tt.parent
			if parent == "" {
				parent = c.GetMetadata().GetId()
			}
			res, err := e.svc.PreflightCheckNodeGroup(t.Context(), domain.PreflightCmd{
				Context:  &commonpb.PreflightCheckContext{Action: tt.action, UnknownPaths: tt.unknown},
				Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: "g"}, Spec: tt.spec,
			})
			if err != nil {
				t.Fatalf("PreflightCheckNodeGroup: %v", err)
			}
			if got := diagPaths(res); !slices.Equal(got, tt.wantPaths) {
				t.Errorf("diagnostic paths = %q, want %q (diagnostics: %v)", got, tt.wantPaths, res.Diagnostics)
			}
			if e.groups.len() != 0 || e.compute.total() != 0 {
				t.Error("a preflight check changed state")
			}
		})
	}
}

func TestPreflightUpdateAndRecreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createCluster(t, "c", "")
	other := e.createCluster(t, "other", "")
	id := done(t)(e.createGroup(t.Context(), c.GetMetadata().GetId(), "g", fixedSpec(2)))
	g := e.group(t, id)
	run := func(md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, action commonpb.PreflightCheckContext_Action) domain.PreflightResult {
		t.Helper()
		res, err := e.svc.PreflightCheckNodeGroup(t.Context(), domain.PreflightCmd{
			Context: &commonpb.PreflightCheckContext{Action: action}, Metadata: md, Spec: spec,
		})
		if err != nil {
			t.Fatalf("PreflightCheckNodeGroup: %v", err)
		}
		return res
	}

	res := run(g.GetMetadata(), fixedSpec(5), commonpb.PreflightCheckContext_UNSPECIFIED)
	if len(res.Diagnostics) != 0 || res.PathsRequireRecreate != "" {
		t.Errorf("valid update = %+v, want clean", res)
	}
	tooNew := fixedSpec(1)
	tooNew.Version = "1.37"
	if got := diagPaths(run(g.GetMetadata(), tooNew, commonpb.PreflightCheckContext_UPDATE)); !slices.Equal(got, []string{"spec.version"}) {
		t.Errorf("bad version update paths = %q, want spec.version", got)
	}

	moved := &commonpb.ResourceMetadata{Id: id, ParentId: other.GetMetadata().GetId(), Name: "g"}
	res = run(moved, fixedSpec(2), commonpb.PreflightCheckContext_UPDATE)
	if res.PathsRequireRecreate != "metadata.parent_id" || len(res.Diagnostics) != 0 {
		t.Errorf("moving update = %+v, want paths_require_recreate metadata.parent_id and no diagnostics", res)
	}

	// A recreate of the node group may reuse its own name.
	if res := run(g.GetMetadata(), fixedSpec(2), commonpb.PreflightCheckContext_RECREATE); len(res.Diagnostics) != 0 {
		t.Errorf("recreate = %+v, want clean", res)
	}
	taken := &commonpb.ResourceMetadata{ParentId: c.GetMetadata().GetId(), Name: "g"}
	if got := diagPaths(run(taken, fixedSpec(1), commonpb.PreflightCheckContext_CREATE)); !slices.Equal(got, []string{""}) {
		t.Errorf("create with a taken name = %q, want one resource-level diagnostic", got)
	}
	if res := run(&commonpb.ResourceMetadata{Id: "mk8snodegroup-e00nope"}, fixedSpec(1), commonpb.PreflightCheckContext_UPDATE); len(res.Diagnostics) != 1 {
		t.Errorf("update of a missing node group = %+v, want one diagnostic", res)
	}
	_, err := e.svc.PreflightCheckNodeGroup(t.Context(), domain.PreflightCmd{
		Context: &commonpb.PreflightCheckContext{UnknownPaths: "spec.(("}, Metadata: g.GetMetadata(), Spec: fixedSpec(1),
	})
	assertErr(t, err, errs.KindBadRequest, "context.unknown_paths")
}
