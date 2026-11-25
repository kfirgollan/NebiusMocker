package contract

import (
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
)

// dialTCPStub connects to a tls:// public endpoint as a user's client would:
// the address's host and port, trusting the local CA. It returns the line the
// stub sends.
func dialTCPStub(t *testing.T, env *aiEnv, addr string) string {
	t.Helper()
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "tls" {
		t.Fatalf("address %q is not a tls:// URL (%v)", addr, err)
	}
	// *.ai.localhost resolves to loopback on a developer machine; dial the
	// loopback address directly and verify the certificate for the name.
	d := tls.Dialer{Config: &tls.Config{RootCAs: env.ca, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	line, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read from %s: %v", addr, err)
	}
	return string(line)
}

func TestAIDataPlaneTCP(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	parent := env.inst.ProjectID
	id := aiCreate(t, env.svc, parent, "db", func(s *aipb.EndpointSpec) {
		s.AuthToken = ""
		s.Ports = []*aipb.EndpointSpec_Port{
			{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP},
			{ContainerPort: 5432, Protocol: aipb.EndpointSpec_Port_TCP},
		}
	})

	urls := aiGet(t, env.svc, id).GetStatus().GetPublicEndpoints()
	if len(urls) != 2 {
		t.Fatalf("public_endpoints = %v, want 2", urls)
	}
	if want := env.mod.DataPlaneURL() + "/" + id; urls[0] != want {
		t.Errorf("public_endpoints[0] = %q, want %q", urls[0], want)
	}
	tcpAddr := urls[1]
	prefix := "tls://" + id + "-p5432.ai.localhost:"
	if !strings.HasPrefix(tcpAddr, prefix) || len(tcpAddr) == len(prefix) {
		t.Fatalf("public_endpoints[1] = %q, want %s<port>", tcpAddr, prefix)
	}
	if res := callDataPlane(t, env.http, "GET", urls[0]+"/ping", ""); res.status != 200 {
		t.Errorf("HTTP port: status %d, want 200", res.status)
	}

	if got, want := dialTCPStub(t, env, tcpAddr), "endpoint was called: "+id+" port 5432\n"; got != want {
		t.Errorf("running: line = %q, want %q", got, want)
	}

	// An unknown endpoint ID on the same listener.
	unknown := strings.Replace(tcpAddr, id, "aiendpoint-e00doesnotexist00", 1)
	if got := dialTCPStub(t, env, unknown); !strings.HasPrefix(got, "error 404") {
		t.Errorf("unknown endpoint: line = %q, want error 404", got)
	}
	// A port the endpoint does not expose as TCP (80 is HTTP).
	httpAsTCP := strings.Replace(tcpAddr, "-p5432.", "-p80.", 1)
	if got := dialTCPStub(t, env, httpAsTCP); !strings.HasPrefix(got, "error 404") {
		t.Errorf("non-TCP port: line = %q, want error 404", got)
	}

	op, err := env.svc.Stop(t.Context(), &aipb.StopEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Stop wait: %v", err)
	}
	if got := dialTCPStub(t, env, tcpAddr); !strings.HasPrefix(got, "error 503") {
		t.Errorf("stopped: line = %q, want error 503", got)
	}

	op, err = env.svc.Start(t.Context(), &aipb.StartEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Start wait: %v", err)
	}
	if got, want := dialTCPStub(t, env, tcpAddr), "endpoint was called: "+id+" port 5432\n"; got != want {
		t.Errorf("restarted: line = %q, want %q", got, want)
	}
}

func TestAIEndpointRejectsUDP(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	spec := validSpec()
	spec.AuthToken = ""
	spec.Ports = []*aipb.EndpointSpec_Port{
		{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP},
		{ContainerPort: 53, Protocol: aipb.EndpointSpec_Port_UDP},
	}
	_, err := env.svc.Create(t.Context(), aiCreateReq(env.inst.ProjectID, "dns", spec))
	assertCode(t, err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "spec.ports[1].protocol") {
		t.Errorf("error %q does not name spec.ports[1].protocol", err)
	}
}
