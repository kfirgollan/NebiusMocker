//go:build e2e

package ai

import (
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	tfjson "github.com/hashicorp/terraform-json"
)

// tcpEndpointConfig renders an endpoint with an HTTP port and a TCP port.
func tcpEndpointConfig(projectID string) string {
	return `
resource "nebius_ai_v1_endpoint" "e" {
  parent_id  = "` + projectID + `"
  name       = "web"
  image      = "nginx:alpine"
  platform   = "cpu-d3"
  preset     = "4vcpu-16gb"
  disk       = { type = "NETWORK_SSD", size_bytes = 21474836480 }
  ports = [
    { container_port = 80, protocol = "HTTP" },
    { container_port = 5432, protocol = "TCP" },
  ]
  auth_token = "` + endpointToken + `"
}
`
}

// statePublicEndpoints returns status.public_endpoints of resource "e" from
// the Terraform state.
func (r *tfRun) statePublicEndpoints(t *testing.T) []string {
	t.Helper()
	st, err := r.tf.Show(t.Context())
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	if st.Values == nil || st.Values.RootModule == nil {
		t.Fatal("terraform state is empty")
	}
	for _, res := range st.Values.RootModule.Resources {
		if res.Mode != tfjson.ManagedResourceMode || res.Name != "e" {
			continue
		}
		status, _ := res.AttributeValues["status"].(map[string]any)
		list, _ := status["public_endpoints"].([]any)
		var out []string
		for _, v := range list {
			s, _ := v.(string)
			out = append(out, s)
		}
		return out
	}
	t.Fatal("no endpoint e in terraform state")
	return nil
}

// TestEndpointTCPPort exposes a TCP port through Terraform and reaches it over
// TLS, verifying the server against the mock's CA.
func TestEndpointTCPPort(t *testing.T) {
	t.Parallel()
	inst := startMock(t)
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, tcpEndpointConfig(inst.ProjectID))
	tf.init(t)
	tf.apply(t)
	id := tf.stateIDs(t)["e"]
	if id == "" {
		t.Fatal("no endpoint ID in state")
	}

	endpoints := tf.statePublicEndpoints(t)
	i := slices.IndexFunc(endpoints, func(u string) bool { return strings.HasPrefix(u, "tls://") })
	if i < 0 {
		t.Fatalf("public_endpoints = %v, want a tls:// entry for the TCP port", endpoints)
	}
	u, err := url.Parse(endpoints[i])
	if err != nil {
		t.Fatalf("parse %q: %v", endpoints[i], err)
	}
	if want := id + "-p5432."; !strings.HasPrefix(u.Hostname(), want) {
		t.Fatalf("TCP address %q, want host starting with %q", endpoints[i], want)
	}
	if !slices.ContainsFunc(endpoints, func(e string) bool { return strings.HasPrefix(e, "https://") }) {
		t.Errorf("public_endpoints = %v, want the https:// entry for the HTTP port too", endpoints)
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{RootCAs: caPool(t, inst.CAFile), ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(t.Context(), "tcp", u.Host)
	if err != nil {
		t.Fatalf("TLS dial %s: %v", u.Host, err)
	}
	defer func() { _ = conn.Close() }()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read from %s: %v", u.Host, err)
	}
	if want := "endpoint was called: " + id + " port 5432\n"; string(got) != want {
		t.Errorf("TCP reply = %q, want %q", got, want)
	}

	tf.expectNoChanges(t)
	tf.destroy(t)
}

// TestEndpointUDPPortRejected proves a UDP port fails the apply and the error
// names the protocol field.
func TestEndpointUDPPortRejected(t *testing.T) {
	t.Parallel()
	inst := startMock(t)
	tf := newTerraform(t, inst.Instance)

	tf.writeConfig(t, `
resource "nebius_ai_v1_endpoint" "e" {
  parent_id = "`+inst.ProjectID+`"
  name      = "web"
  image     = "nginx:alpine"
  platform  = "cpu-d3"
  preset    = "4vcpu-16gb"
  disk      = { type = "NETWORK_SSD", size_bytes = 21474836480 }
  ports     = [{ container_port = 53, protocol = "UDP" }]
}
`)
	tf.init(t)
	if err := tf.tf.Apply(t.Context()); err == nil {
		t.Fatal("apply succeeded, want UDP ports to be rejected")
	}
	if out := tf.out.String(); !strings.Contains(out, "protocol") || !strings.Contains(strings.ToUpper(out), "UDP") {
		t.Errorf("terraform output does not name the protocol field and UDP:\n%s", out)
	}
}
