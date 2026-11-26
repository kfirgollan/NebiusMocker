package k3s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// kubeAPI is what the backend asks of a cluster's Kubernetes API server, as
// the k3s admin. It is a seam: unit tests use a fake.
type kubeAPI interface {
	// Ready returns nil once GET /readyz answers ok.
	Ready(ctx context.Context) error
	// Version returns the running "<major>.<minor>.<patch>".
	Version(ctx context.Context) (string, error)
	// Nodes returns the cluster's nodes by name.
	Nodes(ctx context.Context) (map[string]kubeNode, error)
	// LabelNode merges labels into the node's labels.
	LabelNode(ctx context.Context, name string, labels map[string]string) error
	// SetExtendedResource advertises count units of resource in the node's
	// capacity (Kubernetes "advertise extended resources for a node").
	SetExtendedResource(ctx context.Context, node, resource string, count int) error
	// DeleteNode removes a node; a missing one is not an error.
	DeleteNode(ctx context.Context, name string) error
}

// kubeNode is the part of a Node the backend reads.
type kubeNode struct {
	Ready    bool
	Labels   map[string]string
	Capacity map[string]string
}

// adminKubeconfig is the part of /etc/rancher/k3s/k3s.yaml the backend uses.
type adminKubeconfig struct {
	CA   []byte
	Cert []byte
	Key  []byte
}

func parseAdminKubeconfig(data []byte) (adminKubeconfig, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				CA string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				Cert string `yaml:"client-certificate-data"`
				Key  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return adminKubeconfig{}, fmt.Errorf("parse k3s kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return adminKubeconfig{}, errors.New("parse k3s kubeconfig: no cluster or user")
	}
	var out adminKubeconfig
	for _, f := range []struct {
		dst *[]byte
		src string
	}{{&out.CA, kc.Clusters[0].Cluster.CA}, {&out.Cert, kc.Users[0].User.Cert}, {&out.Key, kc.Users[0].User.Key}} {
		b, err := base64.StdEncoding.DecodeString(f.src)
		if err != nil || len(b) == 0 {
			return adminKubeconfig{}, errors.New("parse k3s kubeconfig: bad certificate data")
		}
		*f.dst = b
	}
	return out, nil
}

// restKube is kubeAPI over HTTPS with the k3s admin client certificate.
type restKube struct {
	base string
	http *http.Client
}

// requestTimeout bounds one Kubernetes API request.
const requestTimeout = 15 * time.Second

func newRestKube(base string, kc adminKubeconfig) (*restKube, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(kc.CA) {
		return nil, errors.New("k3s kubeconfig: CA is not PEM")
	}
	cert, err := tls.X509KeyPair(kc.Cert, kc.Key)
	if err != nil {
		return nil, fmt.Errorf("k3s kubeconfig: client certificate: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
	}}
	return &restKube{base: strings.TrimSuffix(base, "/"), http: &http.Client{Transport: tr, Timeout: requestTimeout}}, nil
}

func (k *restKube) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, r)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: the body is fully read below
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errKubeNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data[:min(len(data), 512)]))
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(data)
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

var errKubeNotFound = errors.New("not found")

func (k *restKube) Ready(ctx context.Context) error {
	var body string
	if err := k.do(ctx, http.MethodGet, "/readyz", "", nil, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body) != "ok" {
		return fmt.Errorf("readyz: %q", body)
	}
	return nil
}

func (k *restKube) Version(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := k.do(ctx, http.MethodGet, "/version", "", nil, &v); err != nil {
		return "", err
	}
	return parseGitVersion(v.GitVersion)
}

// parseGitVersion turns "v1.35.10+k3s1" into "1.35.10".
func parseGitVersion(s string) (string, error) {
	v := strings.TrimPrefix(s, "v")
	v, _, _ = strings.Cut(v, "+")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("unexpected Kubernetes version %q", s)
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("unexpected Kubernetes version %q", s)
		}
	}
	return v, nil
}

func (k *restKube) Nodes(ctx context.Context) (map[string]kubeNode, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Capacity   map[string]string `json:"capacity"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.do(ctx, http.MethodGet, "/api/v1/nodes", "", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]kubeNode, len(list.Items))
	for _, it := range list.Items {
		n := kubeNode{Labels: it.Metadata.Labels, Capacity: it.Status.Capacity}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		out[it.Metadata.Name] = n
	}
	return out, nil
}

func (k *restKube) LabelNode(ctx context.Context, name string, labels map[string]string) error {
	patch := map[string]any{"metadata": map[string]any{"labels": labels}}
	return k.do(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(name), "application/merge-patch+json", patch, nil)
}

func (k *restKube) SetExtendedResource(ctx context.Context, node, resource string, count int) error {
	// JSON Patch escapes "/" in the resource name as "~1".
	path := "/status/capacity/" + strings.ReplaceAll(strings.ReplaceAll(resource, "~", "~0"), "/", "~1")
	patch := []map[string]any{{"op": "add", "path": path, "value": strconv.Itoa(count)}}
	return k.do(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(node)+"/status", "application/json-patch+json", patch, nil)
}

func (k *restKube) DeleteNode(ctx context.Context, name string) error {
	err := k.do(ctx, http.MethodDelete, "/api/v1/nodes/"+url.PathEscape(name), "", nil, nil)
	if errors.Is(err, errKubeNotFound) {
		return nil
	}
	return err
}
