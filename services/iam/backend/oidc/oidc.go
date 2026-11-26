// Package oidc fetches the signing keys of OIDC issuers over HTTP(S): the
// discovery document at <issuer>/.well-known/openid-configuration, then the
// JSON Web Key Set at its jwks_uri. It serves the federated token exchange,
// for the mocker's own fake issuer and for any issuer a user runs.
package oidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// Defaults of Options.
const (
	DefaultTimeout  = 10 * time.Second
	DefaultCacheTTL = 5 * time.Minute
	// maxDocumentBytes bounds a discovery document or JWKS.
	maxDocumentBytes = 1 << 20
)

// DiscoveryPath is appended to an issuer URL to find its discovery document.
const DiscoveryPath = "/.well-known/openid-configuration"

// Options configure a Client. Zero values take the defaults.
type Options struct {
	// Timeout bounds each fetch (discovery and JWKS together).
	Timeout time.Duration
	// CacheTTL is how long a fetched key set is reused, on the mocker's clock.
	CacheTTL time.Duration
	// ExtraRoots are trusted for https issuers besides the system roots (the
	// mocker's own certificate chain, so its fake issuer works under TLS).
	ExtraRoots []*x509.Certificate
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// Client fetches and caches issuers' key sets. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	clk     clock.Clock
	timeout time.Duration
	ttl     time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	jwks      []byte
	fetchedAt time.Time
}

var _ domain.OIDCKeySource = (*Client)(nil)

// New returns a Client on clk.
func New(clk clock.Clock, o Options) (*Client, error) {
	if clk == nil {
		return nil, errors.New("oidc: clock must not be nil")
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = DefaultCacheTTL
	}
	rt := o.Transport
	if rt == nil {
		t, err := transport(o.ExtraRoots)
		if err != nil {
			return nil, err
		}
		rt = t
	}
	return &Client{
		http: &http.Client{Transport: rt}, clk: clk, timeout: o.Timeout, ttl: o.CacheTTL, cache: map[string]cached{},
	}, nil
}

func transport(extra []*x509.Certificate) (http.RoundTripper, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("oidc: http.DefaultTransport is not an *http.Transport")
	}
	t := base.Clone()
	if len(extra) == 0 {
		return t, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, c := range extra {
		pool.AddCert(c)
	}
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return t, nil
}

// KeySet implements domain.OIDCKeySource: the issuer's JWKS document, from
// the cache while it is younger than the cache TTL.
func (c *Client) KeySet(ctx context.Context, issuerURL string) ([]byte, error) {
	c.mu.Lock()
	e, ok := c.cache[issuerURL]
	c.mu.Unlock()
	if ok && c.clk.Now().Sub(e.fetchedAt) < c.ttl {
		return e.jwks, nil
	}
	return c.RefreshKeySet(ctx, issuerURL)
}

// RefreshKeySet implements domain.OIDCKeySource: it fetches the issuer's
// JWKS document and caches it.
func (c *Client) RefreshKeySet(ctx context.Context, issuerURL string) ([]byte, error) {
	now := c.clk.Now()
	jwks, err := c.fetch(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[issuerURL] = cached{jwks: jwks, fetchedAt: now}
	c.mu.Unlock()
	return jwks, nil
}

// discovery is the part of an OIDC discovery document the mocker uses.
type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

func (c *Client) fetch(ctx context.Context, issuerURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	discoveryURL := strings.TrimSuffix(issuerURL, "/") + DiscoveryPath
	body, err := c.get(ctx, discoveryURL)
	if err != nil {
		return nil, err
	}
	var d discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("discovery document %s is not JSON: %w", discoveryURL, err)
	}
	if strings.TrimSuffix(d.Issuer, "/") != strings.TrimSuffix(issuerURL, "/") {
		return nil, fmt.Errorf("discovery document %s names issuer %q, want %q", discoveryURL, d.Issuer, issuerURL)
	}
	u, err := url.Parse(d.JWKSURI)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("discovery document %s: jwks_uri %q is not an absolute http(s) URL", discoveryURL, d.JWKSURI)
	}
	return c.get(ctx, d.JWKSURI)
}

func (c *Client) get(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only; a close error changes nothing
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", target, err)
	}
	if len(body) > maxDocumentBytes {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", target, maxDocumentBytes)
	}
	return body, nil
}
