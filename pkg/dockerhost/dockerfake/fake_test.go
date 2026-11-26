package dockerfake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/dockerfake"
)

// api lists every method both the real client and the fake provide.
type api interface {
	Host() string
	Ping(ctx context.Context) error
	Info(ctx context.Context) (dockerhost.Info, error)
	EnsureImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
	RemoveNetwork(ctx context.Context, name string) error
	ListNetworks(ctx context.Context, labels map[string]string) ([]dockerhost.Network, error)
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	ListVolumes(ctx context.Context, labels map[string]string) ([]string, error)
	CreateContainer(ctx context.Context, spec dockerhost.ContainerSpec) (string, error)
	CopyTo(ctx context.Context, id string, files []dockerhost.File) error
	StartContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, idOrName string) (dockerhost.Container, error)
	ListContainers(ctx context.Context, labels map[string]string) ([]dockerhost.Container, error)
	ReadFile(ctx context.Context, id, path string) ([]byte, error)
	MemoryUsage(ctx context.Context, id string) (int64, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Exec(ctx context.Context, id string, e dockerhost.ExecSpec) (dockerhost.ExecResult, error)
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	BuildImage(ctx context.Context, tag, dockerfile string, labels map[string]string) error
}

var (
	_ api = (*dockerfake.Fake)(nil)
	_ api = (*dockerhost.Client)(nil)
)

func TestLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	id, err := f.CreateContainer(ctx, dockerhost.ContainerSpec{
		Name: "k3s-a", Image: "rancher/k3s", Labels: map[string]string{"l": "1"}, Network: "n",
		Ports: []dockerhost.PortBinding{{ContainerPort: "6443/tcp"}, {ContainerPort: "80/tcp", HostPort: "8080"}},
		Files: []dockerhost.File{{Path: "/etc/a", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("CreateContainer = %v", err)
	}
	if _, err := f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: "k3s-a"}); !errors.Is(err, dockerhost.ErrConflict) {
		t.Fatalf("duplicate CreateContainer err = %v, want ErrConflict", err)
	}
	c, _ := f.InspectContainer(ctx, "k3s-a")
	if c.Running || c.Status != "created" {
		t.Fatalf("after create: %+v", c)
	}
	if got := c.Ports["6443/tcp"][0].HostPort; got != "32768" {
		t.Errorf("auto HostPort = %q, want 32768", got)
	}
	if got := c.Ports["80/tcp"][0].HostPort; got != "8080" {
		t.Errorf("explicit HostPort = %q, want 8080", got)
	}
	if err := f.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer = %v", err)
	}
	f.SetMemoryUsage(id, 42)
	if m, _ := f.MemoryUsage(ctx, id); m != 42 {
		t.Errorf("MemoryUsage = %d, want 42", m)
	}
	data, err := f.ReadFile(ctx, id, "/etc/a")
	if err != nil || string(data) != "x" {
		t.Errorf("ReadFile = %q, %v", data, err)
	}
	f.SetFile(id, "/out", []byte("y"))
	if got := f.Files(id)["/out"]; string(got) != "y" {
		t.Errorf("Files[/out] = %q", got)
	}
	f.SetExited(id, 3, "a\nb\nc\n")
	if logs, _ := f.Logs(ctx, id, 2); logs != "b\nc\n" {
		t.Errorf("Logs tail 2 = %q", logs)
	}
	f.SetRestartCount(id, 2)
	if c, _ := f.InspectContainer(ctx, id); c.RestartCount != 2 {
		t.Errorf("RestartCount = %d, want 2", c.RestartCount)
	}
	list, _ := f.ListContainers(ctx, map[string]string{"l": "1"})
	if len(list) != 1 || list[0].Running || list[0].RestartCount != 0 {
		t.Errorf("ListContainers = %+v", list)
	}
	if err := f.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("RemoveContainer = %v", err)
	}
	if _, err := f.InspectContainer(ctx, id); !errors.Is(err, dockerhost.ErrNotFound) {
		t.Errorf("Inspect after remove err = %v, want ErrNotFound", err)
	}
	if err := f.RemoveContainer(ctx, id); err != nil {
		t.Errorf("second RemoveContainer = %v, want nil", err)
	}
}

func TestFailNextAndNetworks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	boom := errors.New("boom")
	f.FailNext("EnsureImage", boom)
	if err := f.EnsureImage(ctx, "x"); !errors.Is(err, boom) {
		t.Fatalf("EnsureImage err = %v, want boom", err)
	}
	if err := f.EnsureImage(ctx, "x"); err != nil {
		t.Fatalf("second EnsureImage = %v", err)
	}
	a, _ := f.EnsureNetwork(ctx, "n", map[string]string{"k": "v"})
	b, _ := f.EnsureNetwork(ctx, "n", nil)
	if a != b || a == "" {
		t.Errorf("EnsureNetwork ids %q, %q, want equal non-empty", a, b)
	}
	if ns, _ := f.ListNetworks(ctx, map[string]string{"k": "v"}); len(ns) != 1 {
		t.Errorf("ListNetworks = %v", ns)
	}
	_ = f.EnsureVolume(ctx, "v1", map[string]string{"k": "v"})
	if vs, _ := f.ListVolumes(ctx, map[string]string{"k": "v"}); len(vs) != 1 || vs[0] != "v1" {
		t.Errorf("ListVolumes = %v", vs)
	}
}
