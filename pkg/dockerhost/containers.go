package dockerhost

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PortBinding publishes one container port on the host.
type PortBinding struct {
	ContainerPort string // "6443/tcp"
	HostIP        string
	HostPort      string // "" lets Docker pick
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name       string
	Image      string
	Cmd        []string
	Env        []string // "K=V"
	Labels     map[string]string
	Privileged bool
	Tmpfs      map[string]string // path -> options
	Network    string
	Aliases    []string
	Ports      []PortBinding
	ExtraHosts []string
	Memory     int64    // bytes, 0 = unlimited
	Binds      []string // HostConfig.Binds, e.g. "volume:/path"
	Files      []File
}

type wireBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// CreateContainer creates a container and copies spec.Files into it. It does not start it.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]wireBinding{}
	for _, p := range spec.Ports {
		exposed[p.ContainerPort] = struct{}{}
		bindings[p.ContainerPort] = append(bindings[p.ContainerPort], wireBinding{HostIP: p.HostIP, HostPort: p.HostPort})
	}
	payload := map[string]any{
		"Image":        spec.Image,
		"Cmd":          spec.Cmd,
		"Env":          spec.Env,
		"Labels":       spec.Labels,
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"Privileged":   spec.Privileged,
			"Tmpfs":        spec.Tmpfs,
			"PortBindings": bindings,
			"ExtraHosts":   spec.ExtraHosts,
			"Memory":       spec.Memory,
			"Binds":        spec.Binds,
			"NetworkMode":  spec.Network,
		},
	}
	if spec.Network != "" {
		payload["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{spec.Network: map[string]any{"Aliases": spec.Aliases}},
		}
	}
	body, err := jsonBody(payload)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = c.doJSON(ctx, request{
		method: http.MethodPost, path: "/containers/create", query: url.Values{"name": {spec.Name}},
		body: body, contentType: "application/json",
	}, &created)
	if err != nil {
		return "", err
	}
	if err := c.CopyTo(ctx, created.ID, spec.Files); err != nil {
		_ = c.RemoveContainer(ctx, created.ID)
		return "", err
	}
	return created.ID, nil
}

// StartContainer starts a container; an already started one is not an error.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, request{
		method: http.MethodPost, path: "/containers/" + url.PathEscape(id) + "/start",
		accept: []int{http.StatusNotModified},
	}, nil)
}

// RemoveContainer force-removes a container and its anonymous volumes; a missing one is not an error.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	err := c.do(ctx, request{
		method: http.MethodDelete, path: "/containers/" + url.PathEscape(id),
		query: url.Values{"force": {"true"}, "v": {"true"}},
	}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Container is a container's observable state.
type Container struct {
	ID, Name, Image string // Name has no leading "/"
	Labels          map[string]string
	Running         bool
	Status          string // created|running|exited|...
	ExitCode        int
	Ports           map[string][]PortBinding // "6443/tcp" -> actual host bindings
	Networks        map[string]string        // network name -> container IP (inspect only)
}

// InspectContainer returns the container by ID or name; a missing one is ErrNotFound.
func (c *Client) InspectContainer(ctx context.Context, idOrName string) (Container, error) {
	var raw struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running  bool   `json:"Running"`
			Status   string `json:"Status"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
		NetworkSettings struct {
			Ports    map[string][]wireBinding `json:"Ports"`
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	err := c.doJSON(ctx, request{method: http.MethodGet, path: "/containers/" + url.PathEscape(idOrName) + "/json"}, &raw)
	if err != nil {
		return Container{}, err
	}
	ports := make(map[string][]PortBinding, len(raw.NetworkSettings.Ports))
	for port, bs := range raw.NetworkSettings.Ports {
		for _, b := range bs {
			ports[port] = append(ports[port], PortBinding{ContainerPort: port, HostIP: b.HostIP, HostPort: b.HostPort})
		}
	}
	networks := make(map[string]string, len(raw.NetworkSettings.Networks))
	for name, n := range raw.NetworkSettings.Networks {
		networks[name] = n.IPAddress
	}
	return Container{
		Networks: networks,
		ID:       raw.ID, Name: strings.TrimPrefix(raw.Name, "/"), Image: raw.Config.Image, Labels: raw.Config.Labels,
		Running: raw.State.Running, Status: raw.State.Status, ExitCode: raw.State.ExitCode, Ports: ports,
	}, nil
}

// ListContainers lists all containers (running or not) carrying every given
// label; an empty value matches on the key alone. Ports and ExitCode are not
// filled in; use InspectContainer for them.
func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	filters, err := labelFilters(labels)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		State  string            `json:"State"`
	}
	err = c.doJSON(ctx, request{
		method: http.MethodGet, path: "/containers/json",
		query: url.Values{"all": {"true"}, "filters": {filters}},
	}, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, Container{
			ID: r.ID, Name: name, Image: r.Image, Labels: r.Labels, Running: r.State == "running", Status: r.State,
		})
	}
	return out, nil
}

// MemoryUsage returns the container's memory use, page cache excluded.
func (c *Client) MemoryUsage(ctx context.Context, id string) (int64, error) {
	var raw struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodGet, path: "/containers/" + url.PathEscape(id) + "/stats",
		query: url.Values{"stream": {"false"}, "one-shot": {"true"}},
	}, &raw)
	if err != nil {
		return 0, err
	}
	usage := raw.MemoryStats.Usage
	if v, ok := raw.MemoryStats.Stats["inactive_file"]; ok {
		usage -= v
	} else if v, ok := raw.MemoryStats.Stats["total_inactive_file"]; ok {
		usage -= v
	}
	return max(usage, 0), nil
}

// Logs returns the last tail lines of stdout and stderr (tail <= 0 means all).
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	tailArg := "all"
	if tail > 0 {
		tailArg = strconv.Itoa(tail)
	}
	var out string
	err := c.do(ctx, request{
		method: http.MethodGet, path: "/containers/" + url.PathEscape(id) + "/logs",
		query: url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {tailArg}},
	}, func(body io.Reader) error {
		s, err := decodeLogStream(io.LimitReader(body, maxBody))
		out = s
		return err
	})
	return out, err
}

// decodeLogStream joins the payloads of Docker's multiplexed (non-TTY) stream:
// frames of an 8-byte header (stream, 0, 0, 0, big-endian size) plus payload.
func decodeLogStream(r io.Reader) (string, error) {
	var sb strings.Builder
	var header [8]byte
	for {
		_, err := io.ReadFull(r, header[:])
		if errors.Is(err, io.EOF) {
			return sb.String(), nil
		}
		if err != nil {
			return sb.String(), fmt.Errorf("docker: log frame header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(&sb, r, size); err != nil {
			return sb.String(), fmt.Errorf("docker: log frame payload: %w", err)
		}
	}
}
