//go:build e2e

// Package tunnel runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// Tunnels service: a nebius_tunnel_v1_tunnel resource and data source.
// Nothing is patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package tunnel

import (
	"bytes"
	"testing"
	"text/template"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"
	tunnelsvc "github.com/nebius/gosdk/services/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/tunnel"
)

type cfg struct {
	Parent, Name, Title, Description, Labels string
	DataSource                               bool
}

var cfgTmpl = template.Must(template.New("cfg").Parse(`
resource "nebius_tunnel_v1_tunnel" "t" {
  parent_id   = "{{.Parent}}"
  name        = "{{.Name}}"
  title       = "{{.Title}}"
{{- if .Description}}
  description = "{{.Description}}"
{{- end}}
  labels      = { {{.Labels}} }
}
{{if .DataSource}}
data "nebius_tunnel_v1_tunnel" "by_id" {
  id = nebius_tunnel_v1_tunnel.t.id
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

func TestTunnelLifecycle(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), tunnel.New()},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
			testkit.WithProjects(config.ProjectSeed{Name: "second"}))...)
	if len(inst.Projects) < 2 {
		t.Fatalf("seeded projects = %d, want 2", len(inst.Projects))
	}
	second := inst.Projects[1].ID
	tunnels := tunnelsvc.NewTunnelService(inst.SDK(t))
	tf := newTerraform(t, inst)

	get := func(id string) *tunnelpb.Tunnel {
		t.Helper()
		r, err := tunnels.Get(t.Context(), &tunnelpb.GetTunnelRequest{Id: id})
		if err != nil {
			t.Fatalf("get tunnel %s: %v", id, err)
		}
		return r
	}
	// check runs the empty-plan fidelity check and reads the tunnel back
	// through the Go SDK.
	check := func(step, parent, title, description string, labels map[string]string) *tunnelpb.Tunnel {
		t.Helper()
		tf.expectNoChanges(t)
		r := get(tf.stateIDs(t)["t"])
		sp := r.GetSpec()
		if r.GetMetadata().GetParentId() != parent || r.GetMetadata().GetName() != "tf-tunnel" ||
			sp.GetTitle() != title || sp.GetDescription() != description {
			t.Errorf("%s: tunnel = %v, want parent %s title %q description %q", step, r, parent, title, description)
		}
		if st := r.GetStatus(); st.GetState() != tunnelpb.TunnelStatus_CREATED ||
			st.GetConnectionState() != tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED || len(st.GetServices()) != 0 {
			t.Errorf("%s: status = %v, want CREATED, DISCONNECTED, no services", step, st)
		}
		if len(r.GetMetadata().GetLabels()) != len(labels) {
			t.Errorf("%s: labels = %v, want %v", step, r.GetMetadata().GetLabels(), labels)
		}
		for k, want := range labels {
			if r.GetMetadata().GetLabels()[k] != want {
				t.Errorf("%s: labels = %v, want %s=%s", step, r.GetMetadata().GetLabels(), k, want)
			}
		}
		return r
	}

	c := cfg{Parent: inst.ProjectID, Name: "tf-tunnel", Title: "Office", Description: "first", Labels: `env = "dev"`}
	tf.writeConfig(t, c.render(t))
	tf.init(t)
	tf.expectPlan(t, map[string]string{"t": "create"})
	tf.apply(t)
	r := check("create", inst.ProjectID, "Office", "first", map[string]string{"env": "dev"})
	firstID, firstVersion := r.GetMetadata().GetId(), r.GetMetadata().GetResourceVersion()

	// In place: title and labels. The tunnel keeps its ID and its version grows.
	c.Title, c.Labels = "Warehouse", `env = "prod", team = "net"`
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"t": "update"})
	tf.apply(t)
	r = check("update", inst.ProjectID, "Warehouse", "first", map[string]string{"env": "prod", "team": "net"})
	if r.GetMetadata().GetId() != firstID || r.GetMetadata().GetResourceVersion() <= firstVersion {
		t.Errorf("update: id %s version %d, want id %s and a version above %d", r.GetMetadata().GetId(), r.GetMetadata().GetResourceVersion(), firstID, firstVersion)
	}

	// Removing the description from the config clears it (X-ResetMask).
	c.Description = ""
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"t": "update"})
	tf.apply(t)
	check("clear description", inst.ProjectID, "Warehouse", "", map[string]string{"env": "prod", "team": "net"})

	// Data source by ID.
	c.DataSource = true
	tf.writeConfig(t, c.render(t))
	tf.apply(t)
	tf.expectNoChanges(t)
	ds := tf.stateAttrs(t)["data.nebius_tunnel_v1_tunnel.by_id"]
	st, _ := ds["status"].(map[string]any)
	if ds["id"] != firstID || ds["title"] != "Warehouse" || ds["parent_id"] != inst.ProjectID ||
		st["state"] != "CREATED" || st["connection_state"] != "CONNECTION_STATE_DISCONNECTED" {
		t.Errorf("data source = %v, want tunnel %s titled Warehouse, CREATED and DISCONNECTED", ds, firstID)
	}
	c.DataSource = false
	tf.writeConfig(t, c.render(t))
	tf.apply(t)

	// parent_id is RequiresReplace: moving to the second project replaces the tunnel.
	c.Parent = second
	tf.writeConfig(t, c.render(t))
	tf.expectPlan(t, map[string]string{"t": "replace"})
	tf.apply(t)
	r = check("replace", second, "Warehouse", "", map[string]string{"env": "prod", "team": "net"})
	if r.GetMetadata().GetId() == firstID {
		t.Errorf("parent change kept the tunnel ID %s", firstID)
	}
	if _, err := tunnels.Get(t.Context(), &tunnelpb.GetTunnelRequest{Id: firstID}); status.Code(err) != codes.NotFound {
		t.Errorf("replaced tunnel %s: get err = %v, want NotFound", firstID, err)
	}

	lastID := r.GetMetadata().GetId()
	tf.destroy(t)
	if _, err := tunnels.Get(t.Context(), &tunnelpb.GetTunnelRequest{Id: lastID}); status.Code(err) != codes.NotFound {
		t.Errorf("tunnel %s after destroy: get err = %v, want NotFound", lastID, err)
	}
}
