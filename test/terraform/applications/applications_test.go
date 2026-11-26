//go:build e2e

// Package applications runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// Applications service: a nebius_applications_v1alpha1_k8s_release deployed
// to a Managed Kubernetes cluster that Terraform created too. Nothing is
// patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package applications

import (
	"bytes"
	"fmt"
	"testing"
	"text/template"
	"time"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	applicationssvc "github.com/nebius/gosdk/services/nebius/applications/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/applications"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

type cfg struct {
	Proj, Labels, Namespace, Values string
	Set                             string
	DataSource                      bool
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`
resource "nebius_mk8s_v1_cluster" "c" {
  parent_id = "{{.Proj}}"
  name      = "tf-apps-cluster"
  control_plane = {
    version           = "1.35"
    endpoints         = { public_endpoint = {} }
    etcd_cluster_size = 1
  }
}

resource "nebius_mk8s_v1_node_group" "g" {
  parent_id        = nebius_mk8s_v1_cluster.c.id
  name             = "tf-apps-pool"
  fixed_node_count = 2
  template = {
    resources = {
      platform = "cpu-d3"
      preset   = "2vcpu-8gb"
    }
    boot_disk = {
      size_gibibytes = 64
      type           = "NETWORK_SSD"
    }
    network_interfaces = [{ public_ip_address = {} }]
  }
}

resource "nebius_applications_v1alpha1_k8s_release" "r" {
  parent_id        = "{{.Proj}}"
  cluster_id       = nebius_mk8s_v1_cluster.c.id
  product_slug     = "nebius/soperator"
  application_name = "soperator"
  namespace        = "{{.Namespace}}"
  labels           = { {{.Labels}} }
  values = yamlencode({
{{.Values}}
  })
  set = { {{.Set}} }
  depends_on = [nebius_mk8s_v1_node_group.g]
}
{{if .DataSource}}
data "nebius_applications_v1alpha1_k8s_release" "by_id" {
  id = nebius_applications_v1alpha1_k8s_release.r.id
}
{{end}}`))

func (c *cfg) render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := cfgTmpl.Execute(&b, c); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestK8sReleaseLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New(), applications.New()},
		mockerOptions(testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(200*time.Millisecond),
			testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	releases := applicationssvc.NewK8SReleaseService(inst.SDK(t))
	tf := newTerraform(t, inst)

	get := func(id string) *applicationsv1alpha1pb.K8SRelease {
		t.Helper()
		r, err := releases.Get(t.Context(), &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: id})
		if err != nil {
			t.Fatalf("get release %s: %v", id, err)
		}
		return r
	}
	// checkRelease runs the empty-plan fidelity check (values and set are
	// input-only: the mock never returns them, so they must not drift) and
	// reads the release back through the Go SDK.
	checkRelease := func(step, namespace string, labels map[string]string) *applicationsv1alpha1pb.K8SRelease {
		t.Helper()
		tf.expectNoChanges(t)
		ids := tf.stateIDs(t)
		r := get(ids["r"])
		sp := r.GetSpec()
		if r.GetStatus().GetState() != applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED || sp.GetClusterId() != ids["c"] ||
			sp.GetProductSlug() != "nebius/soperator" || sp.GetApplicationName() != "soperator" || sp.GetNamespace() != namespace ||
			r.GetMetadata().GetParentId() != inst.ProjectID {
			t.Errorf("%s: release = %v, want DEPLOYED soperator/%s on cluster %s", step, r, namespace, ids["c"])
		}
		if sp.GetValues() != "" || len(sp.GetSet()) != 0 {
			t.Errorf("%s: Get returned input-only fields: values %q set %v", step, sp.GetValues(), sp.GetSet())
		}
		for k, want := range labels {
			if r.GetMetadata().GetLabels()[k] != want {
				t.Errorf("%s: labels = %v, want %s=%s", step, r.GetMetadata().GetLabels(), k, want)
			}
		}
		return r
	}

	c := cfg{
		Proj: inst.ProjectID, Labels: `env = "dev"`, Namespace: "soperator",
		Values: `    slurm = { workers = 2 }`, Set: `"image.tag" = "v1"`,
	}
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"c": "create", "g": "create", "r": "create"})
	tf.apply(t)
	r := checkRelease("create", "soperator", map[string]string{"env": "dev"})
	firstID := r.GetMetadata().GetId()

	// Data source by ID.
	c.DataSource = true
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	checkRelease("data source", "soperator", nil)
	ds := tf.stateAttrs(t)["data.nebius_applications_v1alpha1_k8s_release.by_id"]
	if ds["id"] != firstID || ds["namespace"] != "soperator" || ds["product_slug"] != "nebius/soperator" ||
		ds["application_name"] != "soperator" || ds["cluster_id"] != tf.stateIDs(t)["c"] {
		t.Errorf("data source = %v, want release %s", ds, firstID)
	}
	if st, _ := ds["status"].(map[string]any); st["state"] != "DEPLOYED" {
		t.Errorf("data source status = %v, want DEPLOYED", ds["status"])
	}

	// In place: new values, set and labels. The release keeps its ID.
	c.Values, c.Set, c.Labels = `    slurm = { workers = 4 }`, fmt.Sprintf(`%q = "v2"`, "image.tag"), `env = "prod", team = "ml"`
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"c": "no-op", "g": "no-op", "r": "update"})
	tf.apply(t)
	r = checkRelease("update", "soperator", map[string]string{"env": "prod", "team": "ml"})
	if r.GetMetadata().GetId() != firstID {
		t.Errorf("update replaced the release: %s -> %s", firstID, r.GetMetadata().GetId())
	}

	// The namespace is immutable: the release is replaced.
	c.Namespace = "slurm"
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"c": "no-op", "g": "no-op", "r": "replace"})
	tf.apply(t)
	r = checkRelease("replace", "slurm", nil)
	if r.GetMetadata().GetId() == firstID {
		t.Errorf("namespace change kept the release ID %s", firstID)
	}
	if _, err := releases.Get(t.Context(), &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: firstID}); err == nil {
		t.Errorf("replaced release %s still exists", firstID)
	}

	lastID := r.GetMetadata().GetId()
	tf.destroy(t)
	if _, err := releases.Get(t.Context(), &applicationsv1alpha1pb.GetK8SReleaseRequest{Id: lastID}); err == nil {
		t.Errorf("release %s still exists after destroy", lastID)
	}
}
