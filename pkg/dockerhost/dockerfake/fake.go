// Package dockerfake is an in-memory stand-in for *dockerhost.Client, for
// Docker-free unit tests of its consumers.
package dockerfake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

const firstHostPort = 32768

type container struct {
	c       dockerhost.Container
	files   map[string][]byte
	logs    string
	memory  int64
	started bool
}

// Fake implements the same method set as *dockerhost.Client.
type Fake struct {
	mu         sync.Mutex
	info       dockerhost.Info
	images     map[string]bool
	networks   map[string]dockerhost.Network
	volumes    map[string]map[string]string
	containers map[string]*container // by ID
	failNext   map[string][]error
	seq        int
	nextPort   int

	execs       []ExecCall
	execHandler func(id string, e dockerhost.ExecSpec) dockerhost.ExecResult
	specs       []dockerhost.ContainerSpec
	autoExit    map[string]autoExit // by container name prefix
}

// New returns an empty fake daemon.
func New() *Fake {
	return &Fake{
		info: dockerhost.Info{
			NCPU: 8, MemTotal: 16 << 30, OperatingSystem: "Fake Docker", Architecture: "aarch64", ServerVersion: "24.0.0",
		},
		images:     map[string]bool{},
		networks:   map[string]dockerhost.Network{},
		volumes:    map[string]map[string]string{},
		containers: map[string]*container{},
		failNext:   map[string][]error{},
		nextPort:   firstHostPort,
	}
}

// FailNext makes the next call to method (for example "CreateContainer") return err.
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[method] = append(f.failNext[method], err)
}

// popFail must be called with f.mu held.
func (f *Fake) popFail(method string) error {
	q := f.failNext[method]
	if len(q) == 0 {
		return nil
	}
	f.failNext[method] = q[1:]
	return q[0]
}

// find resolves an ID or name; must be called with f.mu held.
func (f *Fake) find(idOrName string) (*container, error) {
	if c, ok := f.containers[idOrName]; ok {
		return c, nil
	}
	for _, c := range f.containers {
		if c.c.Name == idOrName {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: container %s", dockerhost.ErrNotFound, idOrName)
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || (v != "" && got != v) {
			return false
		}
	}
	return true
}

// SetInfo replaces the daemon info.
func (f *Fake) SetInfo(info dockerhost.Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info = info
}

// SetRunning marks a container running (or stopped, exit code 0).
//
//nolint:revive // flag-parameter: a two-state test driver is clearer than SetRunning/SetStopped pairs.
func (f *Fake) SetRunning(id string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return
	}
	c.c.Running = running
	c.c.Status = "exited"
	if running {
		c.c.Status = "running"
	}
}

// SetExited marks a container exited with code and replaces its logs.
func (f *Fake) SetExited(id string, code int, logs string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return
	}
	c.c.Running, c.c.Status, c.c.ExitCode, c.logs = false, "exited", code, logs
}

// SetMemoryUsage sets what MemoryUsage returns for the container.
func (f *Fake) SetMemoryUsage(id string, bytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, err := f.find(id); err == nil {
		c.memory = bytes
	}
}

// SetFile sets what ReadFile returns for path in the container.
func (f *Fake) SetFile(id, path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, err := f.find(id); err == nil {
		c.files[path] = slices.Clone(data)
	}
}

// Files returns a copy of the files in the container (CopyTo and SetFile writes).
func (f *Fake) Files(id string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return nil
	}
	out := make(map[string][]byte, len(c.files))
	for p, d := range c.files {
		out[p] = slices.Clone(d)
	}
	return out
}

// Containers returns a snapshot of all containers, sorted by name.
func (f *Fake) Containers() []dockerhost.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerhost.Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, cloneContainer(c.c))
	}
	slices.SortFunc(out, func(a, b dockerhost.Container) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Images returns the pulled image references, sorted.
func (f *Fake) Images() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.images))
}

// Networks returns the networks, sorted by name.
func (f *Fake) Networks() []dockerhost.Network {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Collect(maps.Values(f.networks))
	slices.SortFunc(out, func(a, b dockerhost.Network) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Volumes returns the volume names, sorted.
func (f *Fake) Volumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.volumes))
}

func cloneContainer(c dockerhost.Container) dockerhost.Container {
	c.Labels = maps.Clone(c.Labels)
	c.Networks = maps.Clone(c.Networks)
	ports := make(map[string][]dockerhost.PortBinding, len(c.Ports))
	for k, v := range c.Ports {
		ports[k] = slices.Clone(v)
	}
	c.Ports = ports
	return c
}

// Ping implements the client method.
func (f *Fake) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.popFail("Ping")
}

// Info implements the client method.
func (f *Fake) Info(context.Context) (dockerhost.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("Info"); err != nil {
		return dockerhost.Info{}, err
	}
	return f.info, nil
}

// Host implements the client method.
func (f *Fake) Host() string { return "fake://docker" }

// EnsureImage implements the client method.
func (f *Fake) EnsureImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("EnsureImage"); err != nil {
		return err
	}
	f.images[ref] = true
	return nil
}

// EnsureNetwork implements the client method.
func (f *Fake) EnsureNetwork(_ context.Context, name string, labels map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("EnsureNetwork"); err != nil {
		return "", err
	}
	if n, ok := f.networks[name]; ok {
		return n.ID, nil
	}
	f.seq++
	n := dockerhost.Network{ID: fmt.Sprintf("net%04d", f.seq), Name: name, Labels: maps.Clone(labels)}
	f.networks[name] = n
	return n.ID, nil
}

// RemoveNetwork implements the client method.
func (f *Fake) RemoveNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("RemoveNetwork"); err != nil {
		return err
	}
	delete(f.networks, name)
	return nil
}

// ListNetworks implements the client method.
func (f *Fake) ListNetworks(_ context.Context, labels map[string]string) ([]dockerhost.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("ListNetworks"); err != nil {
		return nil, err
	}
	var out []dockerhost.Network
	for _, n := range f.networks {
		if hasLabels(n.Labels, labels) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b dockerhost.Network) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// EnsureVolume implements the client method.
func (f *Fake) EnsureVolume(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("EnsureVolume"); err != nil {
		return err
	}
	if _, ok := f.volumes[name]; !ok {
		f.volumes[name] = maps.Clone(labels)
	}
	return nil
}

// RemoveVolume implements the client method.
func (f *Fake) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("RemoveVolume"); err != nil {
		return err
	}
	delete(f.volumes, name)
	return nil
}

// ListVolumes implements the client method.
func (f *Fake) ListVolumes(_ context.Context, labels map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("ListVolumes"); err != nil {
		return nil, err
	}
	var out []string
	for name, l := range f.volumes {
		if hasLabels(l, labels) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// CreateContainer implements the client method.
func (f *Fake) CreateContainer(_ context.Context, spec dockerhost.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("CreateContainer"); err != nil {
		return "", err
	}
	for _, c := range f.containers {
		if c.c.Name == spec.Name {
			return "", fmt.Errorf("%w: name %q in use", dockerhost.ErrConflict, spec.Name)
		}
	}
	f.seq++
	id := fmt.Sprintf("ctr%04d", f.seq)
	ports := map[string][]dockerhost.PortBinding{}
	for _, p := range spec.Ports {
		b := p
		if b.HostPort == "" {
			b.HostPort = strconv.Itoa(f.nextPort)
			f.nextPort++
		}
		if b.HostIP == "" {
			b.HostIP = "0.0.0.0"
		}
		ports[p.ContainerPort] = append(ports[p.ContainerPort], b)
	}
	networks := map[string]string{}
	if spec.Network != "" {
		networks[spec.Network] = fmt.Sprintf("172.18.0.%d", 1+f.seq)
	}
	c := &container{
		c: dockerhost.Container{
			ID: id, Name: spec.Name, Image: spec.Image, Labels: maps.Clone(spec.Labels),
			Status: "created", Ports: ports, Networks: networks,
		},
		files: map[string][]byte{},
	}
	for _, file := range spec.Files {
		c.files[file.Path] = slices.Clone(file.Content)
	}
	f.containers[id] = c
	f.specs = append(f.specs, spec)
	return id, nil
}

// CopyTo implements the client method.
func (f *Fake) CopyTo(_ context.Context, id string, files []dockerhost.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("CopyTo"); err != nil {
		return err
	}
	c, err := f.find(id)
	if err != nil {
		return err
	}
	for _, file := range files {
		c.files[file.Path] = slices.Clone(file.Content)
	}
	return nil
}

// StartContainer implements the client method.
func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("StartContainer"); err != nil {
		return err
	}
	c, err := f.find(id)
	if err != nil {
		return err
	}
	c.started = true
	c.c.Running, c.c.Status = true, "running"
	for prefix, ae := range f.autoExit {
		if strings.HasPrefix(c.c.Name, prefix) {
			c.c.Running, c.c.Status, c.c.ExitCode, c.logs = false, "exited", ae.code, ae.logs
		}
	}
	return nil
}

// RemoveContainer implements the client method.
func (f *Fake) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("RemoveContainer"); err != nil {
		return err
	}
	if c, err := f.find(id); err == nil {
		delete(f.containers, c.c.ID)
	}
	return nil
}

// InspectContainer implements the client method.
func (f *Fake) InspectContainer(_ context.Context, idOrName string) (dockerhost.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("InspectContainer"); err != nil {
		return dockerhost.Container{}, err
	}
	c, err := f.find(idOrName)
	if err != nil {
		return dockerhost.Container{}, err
	}
	return cloneContainer(c.c), nil
}

// ListContainers implements the client method.
func (f *Fake) ListContainers(_ context.Context, labels map[string]string) ([]dockerhost.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("ListContainers"); err != nil {
		return nil, err
	}
	var out []dockerhost.Container
	for _, c := range f.containers {
		if !hasLabels(c.c.Labels, labels) {
			continue
		}
		s := cloneContainer(c.c)
		s.Ports, s.Networks, s.ExitCode = nil, nil, 0 // the real list endpoint omits them
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b dockerhost.Container) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ReadFile implements the client method.
func (f *Fake) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("ReadFile"); err != nil {
		return nil, err
	}
	c, err := f.find(id)
	if err != nil {
		return nil, err
	}
	data, ok := c.files[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s in container %s", dockerhost.ErrNotFound, path, id)
	}
	return slices.Clone(data), nil
}

// MemoryUsage implements the client method.
func (f *Fake) MemoryUsage(_ context.Context, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("MemoryUsage"); err != nil {
		return 0, err
	}
	c, err := f.find(id)
	if err != nil {
		return 0, err
	}
	return c.memory, nil
}

// Logs implements the client method.
func (f *Fake) Logs(_ context.Context, id string, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("Logs"); err != nil {
		return "", err
	}
	c, err := f.find(id)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		return c.logs, nil
	}
	lines := strings.SplitAfter(c.logs, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines[max(0, len(lines)-tail):], ""), nil
}
