package oidc_test

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/iam/backend/oidc"
)

// issuer is an httptest OIDC issuer counting its JWKS fetches.
type issuer struct {
	srv       *httptest.Server
	jwks      atomic.Value // string
	fetches   atomic.Int32
	issuerFor func(base string) string
	jwksURI   func(base string) string
	status    atomic.Int32
}

func newIssuer(t *testing.T, start func(http.Handler) *httptest.Server) *issuer {
	t.Helper()
	is := &issuer{
		issuerFor: func(base string) string { return base },
		jwksURI:   func(base string) string { return base + "/keys" },
	}
	is.jwks.Store(`{"keys":[{"kty":"RSA","kid":"a"}]}`)
	is.status.Store(http.StatusOK)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		base := is.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.issuerFor(base), "jwks_uri": is.jwksURI(base)})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		is.fetches.Add(1)
		w.WriteHeader(int(is.status.Load()))
		_, _ = w.Write([]byte(is.jwks.Load().(string)))
	})
	is.srv = start(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func newClient(t *testing.T, clk clock.Clock, o oidc.Options) *oidc.Client {
	t.Helper()
	c, err := oidc.New(clk, o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeySetFetchAndCache(t *testing.T) {
	t.Parallel()
	is := newIssuer(t, httptest.NewServer)
	clk := clock.NewManual(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	c := newClient(t, clk, oidc.Options{CacheTTL: time.Minute})

	doc, err := c.KeySet(t.Context(), is.srv.URL)
	if err != nil || !strings.Contains(string(doc), `"kid":"a"`) {
		t.Fatalf("KeySet = %s, %v", doc, err)
	}
	// Cached: a rotated key is not seen until the TTL passes or a refresh.
	is.jwks.Store(`{"keys":[{"kty":"RSA","kid":"b"}]}`)
	if doc, _ := c.KeySet(t.Context(), is.srv.URL); !strings.Contains(string(doc), `"kid":"a"`) || is.fetches.Load() != 1 {
		t.Fatalf("second KeySet = %s after %d fetches, want the cached set", doc, is.fetches.Load())
	}
	if doc, _ := c.RefreshKeySet(t.Context(), is.srv.URL); !strings.Contains(string(doc), `"kid":"b"`) || is.fetches.Load() != 2 {
		t.Fatalf("refreshed KeySet = %s after %d fetches, want the new set", doc, is.fetches.Load())
	}
	is.jwks.Store(`{"keys":[{"kty":"RSA","kid":"c"}]}`)
	clk.Advance(time.Minute)
	if doc, _ := c.KeySet(t.Context(), is.srv.URL); !strings.Contains(string(doc), `"kid":"c"`) || is.fetches.Load() != 3 {
		t.Fatalf("KeySet after the TTL = %s after %d fetches, want a refetch", doc, is.fetches.Load())
	}
	// A trailing slash on the issuer URL finds the same discovery document.
	if _, err := c.KeySet(t.Context(), is.srv.URL+"/"); err != nil {
		t.Errorf("KeySet with a trailing slash: %v", err)
	}
}

func TestKeySetErrors(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	for name, tt := range map[string]struct {
		setup   func(is *issuer)
		mention string
	}{
		"issuer mismatch":   {func(is *issuer) { is.issuerFor = func(string) string { return "https://other.example" } }, "names issuer"},
		"relative jwks_uri": {func(is *issuer) { is.jwksURI = func(string) string { return "/keys" } }, "jwks_uri"},
		"jwks HTTP 500":     {func(is *issuer) { is.status.Store(http.StatusInternalServerError) }, "HTTP 500"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is := newIssuer(t, httptest.NewServer)
			tt.setup(is)
			_, err := newClient(t, clk, oidc.Options{}).KeySet(t.Context(), is.srv.URL)
			if err == nil || !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("KeySet = %v, want an error mentioning %q", err, tt.mention)
			}
		})
	}
	t.Run("no discovery document", func(t *testing.T) {
		t.Parallel()
		is := newIssuer(t, httptest.NewServer)
		_, err := newClient(t, clk, oidc.Options{}).KeySet(t.Context(), is.srv.URL+"/nope")
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Errorf("KeySet = %v, want HTTP 404", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { close(block); srv.Close() })
		_, err := newClient(t, clk, oidc.Options{Timeout: 50 * time.Millisecond}).KeySet(t.Context(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Errorf("KeySet = %v, want a deadline error", err)
		}
	})
}

func TestKeySetTrustsExtraRoots(t *testing.T) {
	t.Parallel()
	is := newIssuer(t, httptest.NewTLSServer)
	clk := clock.NewManual(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if _, err := newClient(t, clk, oidc.Options{}).KeySet(t.Context(), is.srv.URL); err == nil {
		t.Fatal("KeySet trusted a self-signed issuer without ExtraRoots")
	}
	cert, err := x509.ParseCertificate(is.srv.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newClient(t, clk, oidc.Options{ExtraRoots: []*x509.Certificate{cert}}).KeySet(t.Context(), is.srv.URL); err != nil {
		t.Errorf("KeySet with the issuer's certificate as an extra root: %v", err)
	}
}
