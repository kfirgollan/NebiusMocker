package dockerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// EnsureImage pulls ref unless the daemon already has it.
func (c *Client) EnsureImage(ctx context.Context, ref string) error {
	err := c.do(ctx, request{method: http.MethodGet, path: "/images/" + escapePath(ref) + "/json"}, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	repo, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	return c.do(ctx, request{method: http.MethodPost, path: "/images/create", query: q}, func(body io.Reader) error {
		return readPullStream(ref, body)
	})
}

// splitImageRef splits "repo:tag" (default latest) and "repo@sha256:..".
func splitImageRef(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	lastSlash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > lastSlash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// readPullStream consumes the JSON-lines pull progress and fails on any error line.
func readPullStream(ref string, body io.Reader) error {
	dec := json.NewDecoder(io.LimitReader(body, maxBody))
	for {
		var line struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: pull %s: read stream: %w", ref, err)
		}
		switch {
		case line.Error != "":
			return fmt.Errorf("docker: pull %s: %s", ref, line.Error)
		case line.ErrorDetail != nil:
			return fmt.Errorf("docker: pull %s: %s", ref, line.ErrorDetail.Message)
		}
	}
}

// ImageExists reports whether the daemon already has ref.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, request{method: http.MethodGet, path: "/images/" + escapePath(ref) + "/json"}, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// BuildImage builds an image tagged tag from dockerfile alone (no other files
// in the build context) and labels it.
func (c *Client) BuildImage(ctx context.Context, tag, dockerfile string, labels map[string]string) error {
	archive, err := buildTar([]File{{Path: "/Dockerfile", Content: []byte(dockerfile)}})
	if err != nil {
		return err
	}
	rawLabels, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("docker: encode labels: %w", err)
	}
	q := url.Values{"t": {tag}, "labels": {string(rawLabels)}, "rm": {"1"}}
	return c.do(ctx, request{
		method: http.MethodPost, path: "/build", query: q,
		body: bytes.NewReader(archive), contentType: "application/x-tar",
	}, func(body io.Reader) error {
		return readBuildStream(tag, body)
	})
}

// readBuildStream consumes the JSON-lines build output and fails on an error line.
func readBuildStream(tag string, body io.Reader) error {
	dec := json.NewDecoder(io.LimitReader(body, maxBody))
	for {
		var line struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: build %s: read stream: %w", tag, err)
		}
		switch {
		case line.Error != "":
			return fmt.Errorf("docker: build %s: %s", tag, line.Error)
		case line.ErrorDetail != nil:
			return fmt.Errorf("docker: build %s: %s", tag, line.ErrorDetail.Message)
		}
	}
}

// Network is a Docker network.
type Network struct {
	ID     string
	Name   string
	Labels map[string]string
}

// EnsureNetwork creates a labelled bridge network, or returns the existing one's ID.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	id, err := c.networkID(ctx, name)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return id, err
	}
	body, err := jsonBody(map[string]any{"Name": name, "Driver": "bridge", "Labels": labels})
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = c.doJSON(ctx, request{
		method: http.MethodPost, path: "/networks/create", body: body, contentType: "application/json",
	}, &created)
	if errors.Is(err, ErrConflict) { // lost a creation race
		return c.networkID(ctx, name)
	}
	return created.ID, err
}

func (c *Client) networkID(ctx context.Context, name string) (string, error) {
	var n struct {
		ID string `json:"Id"`
	}
	err := c.doJSON(ctx, request{method: http.MethodGet, path: "/networks/" + url.PathEscape(name)}, &n)
	return n.ID, err
}

// RemoveNetwork deletes a network; a missing network is not an error.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, request{method: http.MethodDelete, path: "/networks/" + url.PathEscape(name)}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ListNetworks returns the networks carrying all the given labels.
func (c *Client) ListNetworks(ctx context.Context, labels map[string]string) ([]Network, error) {
	filters, err := labelFilters(labels)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID     string            `json:"Id"`
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	err = c.doJSON(ctx, request{method: http.MethodGet, path: "/networks", query: url.Values{"filters": {filters}}}, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(raw))
	for _, n := range raw {
		out = append(out, Network{ID: n.ID, Name: n.Name, Labels: n.Labels})
	}
	return out, nil
}
