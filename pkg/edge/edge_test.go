package edge_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/edge"
)

// selfSigned returns a certificate for 127.0.0.1 and a pool trusting it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Real{}.Now() // the TLS stack checks validity against the real clock
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "edge-test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func timeoutCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), 10*time.Second)
}

func newLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// startEdge serves POST /x ("hello") and returns the bound address.
func startEdge(t *testing.T, s *edge.Server) string {
	t.Helper()
	s.HandleHTTP("POST /x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Proto", r.Proto)
		_, _ = io.WriteString(w, "hello "+string(body))
	}))
	addr, err := s.Start(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := timeoutCtx(t)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("Stop = %v, want nil", err)
		}
	})
	return addr.String()
}

func checkHealth(t *testing.T, addr string, creds credentials.TransportCredentials) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	ctx, cancel := timeoutCtx(t)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check = %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health status = %v, want SERVING", resp.GetStatus())
	}
}

// httpResult is a fully read response.
type httpResult struct {
	status, major int
	proto, body   string
}

func do(t *testing.T, c *http.Client, method, url, body string) httpResult {
	t.Helper()
	ctx, cancel := timeoutCtx(t)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: resp.StatusCode, major: resp.ProtoMajor, proto: resp.Proto, body: string(b)}
}

func checkHTTP(t *testing.T, c *http.Client, base string, wantMajor int) {
	t.Helper()
	r := do(t, c, http.MethodPost, base+"/x", "world")
	if r.status != http.StatusOK || r.body != "hello world" {
		t.Errorf("POST /x = %d %q, want 200 \"hello world\"", r.status, r.body)
	}
	if r.major != wantMajor {
		t.Errorf("POST /x protocol = %s, want HTTP/%d", r.proto, wantMajor)
	}
	if got := do(t, c, http.MethodGet, base+"/x", "").status; got != http.StatusMethodNotAllowed {
		t.Errorf("GET /x = %d, want 405", got)
	}
	if got := do(t, c, http.MethodPost, base+"/nope", "").status; got != http.StatusNotFound {
		t.Errorf("POST /nope = %d, want 404", got)
	}
}

func TestEdgePlaintext(t *testing.T) {
	t.Parallel()
	addr := startEdge(t, edge.New(newLogger()))

	t.Run("grpc h2c", func(t *testing.T) {
		t.Parallel()
		checkHealth(t, addr, insecure.NewCredentials())
	})
	t.Run("http/1.1", func(t *testing.T) {
		t.Parallel()
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		checkHTTP(t, c, "http://"+addr, 1)
	})
	t.Run("http h2c prior knowledge", func(t *testing.T) {
		t.Parallel()
		protocols := new(http.Protocols)
		protocols.SetUnencryptedHTTP2(true)
		c := &http.Client{Transport: &http.Transport{Protocols: protocols, DisableKeepAlives: true}}
		checkHTTP(t, c, "http://"+addr, 2)
	})
}

func TestEdgeTLS(t *testing.T) {
	t.Parallel()
	cert, pool := selfSigned(t)
	addr := startEdge(t, edge.NewTLS(newLogger(), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		checkHealth(t, addr, credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	})
	t.Run("https h2 via ALPN", func(t *testing.T) {
		t.Parallel()
		c := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true, DisableKeepAlives: true,
		}}
		checkHTTP(t, c, "https://"+addr, 2)
	})
	t.Run("https http/1.1 via ALPN", func(t *testing.T) {
		t.Parallel()
		c := &http.Client{Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
			DisableKeepAlives: true,
		}}
		checkHTTP(t, c, "https://"+addr, 1)
	})
}

func TestEdgeStopWithoutStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := timeoutCtx(t)
	defer cancel()
	if err := edge.New(newLogger()).Stop(ctx); err != nil {
		t.Errorf("Stop before Start = %v, want nil", err)
	}
}

func TestEdgeRefusesAfterStop(t *testing.T) {
	t.Parallel()
	s := edge.New(newLogger())
	addr := startEdge(t, s)
	// A client connecting after Stop is refused.
	ctx, cancel := timeoutCtx(t)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	var d net.Dialer
	if conn, err := d.DialContext(ctx, "tcp", addr); err == nil {
		conn.Close()
		t.Errorf("dial after Stop succeeded, want connection refused")
	}
}
