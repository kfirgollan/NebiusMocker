// Package dockerhost is a small Docker Engine API client over a unix socket or
// TCP. It covers only what the mk8s k3s backend needs: images, networks and
// containers (create, start, copy files in and out, inspect, stats, logs).
package dockerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// apiVersion is the path prefix every request carries; Docker 24+ serves it.
const apiVersion = "/v1.43"

const (
	// maxErrorBody bounds how much of an error response is buffered.
	maxErrorBody = 64 << 10
	// maxBody bounds buffered successful responses (logs, archives, JSON).
	maxBody = 64 << 20
)

// Sentinel errors; callers use errors.Is.
var (
	// ErrNotFound is returned for HTTP 404.
	ErrNotFound = errors.New("docker: not found")
	// ErrConflict is returned for HTTP 409 (for example a name already in use).
	ErrConflict = errors.New("docker: conflict")
	// ErrUnavailable is returned when the daemon cannot be reached.
	ErrUnavailable = errors.New("docker: daemon unavailable")
)

// Client talks to one Docker daemon.
type Client struct {
	http *http.Client
	host string
}

// New parses host: "unix:///path/docker.sock" or "tcp://host:port". The empty
// string picks the first of /var/run/docker.sock and
// $HOME/.docker/run/docker.sock that exists, else /var/run/docker.sock.
func New(host string) (*Client, error) {
	if host == "" {
		host = defaultHost()
	}
	var network, addr string
	switch {
	case strings.HasPrefix(host, "unix://"):
		network, addr = "unix", strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "tcp://"):
		network, addr = "tcp", strings.TrimPrefix(host, "tcp://")
	default:
		return nil, fmt.Errorf("docker: unsupported host %q (want unix:///path or tcp://host:port)", host)
	}
	if addr == "" {
		return nil, fmt.Errorf("docker: host %q has no address", host)
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
			}
			return conn, nil
		},
		DisableKeepAlives: true,
	}
	return &Client{http: &http.Client{Transport: transport}, host: host}, nil
}

func defaultHost() string {
	candidates := []string{"/var/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return "unix://" + p
		}
	}
	return "unix:///var/run/docker.sock"
}

// Host returns the resolved endpoint, for messages.
func (c *Client) Host() string { return c.host }

// request describes one API call.
type request struct {
	method      string
	path        string // already escaped, without the version prefix
	query       url.Values
	body        io.Reader
	contentType string
	accept      []int // extra non-2xx statuses treated as success
}

// do sends r and hands the body of a successful response to read (nil discards
// it). The body is always closed.
func (c *Client) do(ctx context.Context, r request, read func(io.Reader) error) error {
	target := "http://docker" + apiVersion + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target, r.body)
	if err != nil {
		return fmt.Errorf("docker: %s %s: %w", r.method, r.path, err)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("docker: %s %s on %s: %w", r.method, r.path, c.host, err)
		}
		return fmt.Errorf("docker: %s %s: %w", r.method, r.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && !slices.Contains(r.accept, resp.StatusCode) {
		return statusError(r, resp)
	}
	if read == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	return read(resp.Body)
}

func statusError(r request, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(raw))
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
		msg = payload.Message
	}
	base := fmt.Sprintf("docker: %s %s: %d: %s", r.method, r.path, resp.StatusCode, msg)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w (%s)", ErrNotFound, base)
	case http.StatusConflict:
		return fmt.Errorf("%w (%s)", ErrConflict, base)
	default:
		return errors.New(base)
	}
}

// doJSON sends r and decodes the JSON response into out (nil to ignore it).
func (c *Client) doJSON(ctx context.Context, r request, out any) error {
	return c.do(ctx, r, func(body io.Reader) error {
		if out == nil {
			return nil
		}
		if err := json.NewDecoder(io.LimitReader(body, maxBody)).Decode(out); err != nil {
			return fmt.Errorf("docker: %s %s: decode response: %w", r.method, r.path, err)
		}
		return nil
	})
}

// jsonBody encodes v as a request body.
func jsonBody(v any) (io.Reader, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("docker: encode request: %w", err)
	}
	return strings.NewReader(string(raw)), nil
}

// Ping checks that the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodGet, path: "/_ping"}, nil)
}

// Info is the subset of the daemon's /info the mocker reports.
type Info struct {
	NCPU            int
	MemTotal        int64
	OperatingSystem string
	Architecture    string
	ServerVersion   string
}

// Info returns daemon capacity and version.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.doJSON(ctx, request{method: http.MethodGet, path: "/info"}, &info)
	return info, err
}

// labelFilters renders {"label":["k=v","k"]} deterministically.
func labelFilters(labels map[string]string) (string, error) {
	list := make([]string, 0, len(labels))
	for k, v := range labels {
		if v == "" {
			list = append(list, k)
		} else {
			list = append(list, k+"="+v)
		}
	}
	slices.Sort(list)
	raw, err := json.Marshal(map[string][]string{"label": list})
	if err != nil {
		return "", fmt.Errorf("docker: encode filters: %w", err)
	}
	return string(raw), nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
