package k3s

import (
	"slices"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestNewPingFailure(t *testing.T) {
	t.Parallel()
	d := newRecDocker()
	d.FailNext("Ping", errBoom)
	o := testOptions(d)
	_, err := New(t.Context(), o)
	if err == nil {
		t.Fatal("New with an unreachable Docker succeeded, want an error")
	}
	for _, want := range []string{"NEBIUS_MOCKER_MK8S_BACKEND=k3s", "fake://docker", "NEBIUS_MOCKER_MK8S_K3S_DOCKER_HOST"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to contain %q", err, want)
		}
	}
}

func TestNewMissingImage(t *testing.T) {
	t.Parallel()
	o := testOptions(newRecDocker())
	o.Versions = []string{"1.35", "1.99"}
	_, err := New(t.Context(), o)
	if err == nil || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MK8S_K3S_IMAGES") || !strings.Contains(err.Error(), "1.99") {
		t.Fatalf("New() error = %v, want one naming 1.99 and NEBIUS_MOCKER_MK8S_K3S_IMAGES", err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()
	o := testOptions(newRecDocker())
	o.Log = nil
	if _, err := New(t.Context(), o); err == nil {
		t.Fatal("New without a logger succeeded, want an error")
	}
}

func TestWebhookURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		listen   string
		tls      bool
		explicit string
		want     string
		wantErr  string
	}{
		{name: "plain edge", listen: ":4566", want: "http://host.docker.internal:4566"},
		{name: "host and port", listen: "127.0.0.1:4566", want: "http://host.docker.internal:4566"},
		{name: "tls edge", listen: ":4566", tls: true, want: "https://host.docker.internal:4566"},
		{name: "ephemeral port", listen: ":0", wantErr: "NEBIUS_MOCKER_MK8S_K3S_WEBHOOK_URL"},
		{name: "no port", listen: "localhost", wantErr: "NEBIUS_MOCKER_MK8S_K3S_WEBHOOK_URL"},
		{name: "explicit wins", listen: ":0", explicit: "https://mocker.internal:9443", want: "https://mocker.internal:9443"},
		{name: "explicit wins over tls", listen: ":4566", tls: true, explicit: "http://10.0.0.5:4566", want: "http://10.0.0.5:4566"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := testOptions(newRecDocker())
			o.EdgeListen, o.EdgeTLS, o.Config.WebhookURL = tt.listen, tt.tls, tt.explicit
			b, err := New(t.Context(), o)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("New() error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if b.webhookURL != tt.want {
				t.Fatalf("webhook URL = %q, want %q", b.webhookURL, tt.want)
			}
		})
	}
}

func TestNewReapsLeftovers(t *testing.T) {
	t.Parallel()
	d := newRecDocker()
	ctx := t.Context()
	const owner = "http://host.docker.internal:4566" // testOptions: edge :4566
	ours := map[string]string{LabelBackend: backendName, LabelOwner: owner, LabelCluster: "mk8scluster-old"}
	mk := func(name string, labels map[string]string) {
		t.Helper()
		id, err := d.CreateContainer(ctx, dockerhost.ContainerSpec{Name: name, Image: "img", Labels: labels})
		if err != nil {
			t.Fatalf("CreateContainer(%s): %v", name, err)
		}
		if err := d.StartContainer(ctx, id); err != nil {
			t.Fatalf("StartContainer(%s): %v", name, err)
		}
	}
	mk("leftover-server", ours)
	mk("leftover-agent", map[string]string{LabelBackend: backendName, LabelOwner: owner, LabelRole: roleAgent})
	// Another live mocker on the same Docker host (another edge port) keeps its cluster.
	mk("other-mocker", map[string]string{LabelBackend: backendName, LabelOwner: "http://host.docker.internal:4999", LabelRole: roleServer})
	mk("unrelated", map[string]string{"app": "postgres"})
	mk("other-backend", map[string]string{LabelBackend: "harbor"})
	for _, v := range []struct {
		name   string
		labels map[string]string
	}{{"leftover-vol", ours}, {"unrelated-vol", map[string]string{"app": "postgres"}}} {
		if err := d.EnsureVolume(ctx, v.name, v.labels); err != nil {
			t.Fatalf("EnsureVolume: %v", err)
		}
	}
	for _, n := range []struct {
		name   string
		labels map[string]string
	}{{"leftover-net", ours}, {"unrelated-net", map[string]string{"app": "postgres"}}} {
		if _, err := d.EnsureNetwork(ctx, n.name, n.labels); err != nil {
			t.Fatalf("EnsureNetwork: %v", err)
		}
	}
	o := testOptions(d)
	if _, err := New(ctx, o); err != nil {
		t.Fatalf("New: %v", err)
	}
	var names []string
	for _, c := range d.Containers() {
		names = append(names, c.Name)
	}
	if want := []string{"other-backend", "other-mocker", "unrelated"}; !slices.Equal(names, want) {
		t.Errorf("containers after New = %v, want %v", names, want)
	}
	if got, want := d.Volumes(), []string{"unrelated-vol"}; !slices.Equal(got, want) {
		t.Errorf("volumes after New = %v, want %v", got, want)
	}
	nets := d.Networks()
	if len(nets) != 1 || nets[0].Name != "unrelated-net" {
		t.Errorf("networks after New = %v, want only unrelated-net", nets)
	}
}

func TestCheckVersionAndName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if got := h.b.Name(); got != "k3s" {
		t.Errorf("Name() = %q, want k3s", got)
	}
	if err := h.b.CheckVersion("1.36"); err != nil {
		t.Errorf("CheckVersion(1.36) = %v, want nil", err)
	}
	err := h.b.CheckVersion("1.99")
	if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MK8S_K3S_IMAGES") {
		t.Errorf("CheckVersion(1.99) = %v, want a BadRequest naming NEBIUS_MOCKER_MK8S_K3S_IMAGES", err)
	}
}

func TestDeleteClusterRemovesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	const other = "mk8scluster-other"
	for _, id := range []string{testCluster, other} {
		h.ensureCluster(t, id)
		h.ensureNodes(t, testGroupSpec(id, "n1"))
	}
	if err := h.b.DeleteCluster(t.Context(), testCluster); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := h.b.DeleteCluster(t.Context(), testCluster); err != nil {
		t.Fatalf("second DeleteCluster: %v", err)
	}
	if err := h.b.DeleteCluster(t.Context(), "mk8scluster-unknown"); err != nil {
		t.Fatalf("DeleteCluster of an unknown cluster: %v", err)
	}
	var names []string
	for _, c := range h.docker.Containers() {
		names = append(names, c.Name)
	}
	if want := []string{h.b.agentName(other, "n1"), h.b.serverName(other)}; !slices.Equal(names, want) {
		t.Errorf("containers = %v, want only %v", names, want)
	}
	if got, want := h.docker.Volumes(), []string{h.b.volumeName(other)}; !slices.Equal(got, want) {
		t.Errorf("volumes = %v, want %v", got, want)
	}
	if nets := h.docker.Networks(); len(nets) != 1 || nets[0].Name != h.b.networkName(other) {
		t.Errorf("networks = %v, want only %s", nets, h.b.networkName(other))
	}
	// The deleted cluster's state is gone: its nodes need an ensured cluster.
	if _, err := h.b.EnsureNodes(t.Context(), testGroupSpec(testCluster, "n1")); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("EnsureNodes on a deleted cluster = %v, want BadState", err)
	}
}

func TestCloseRemovesEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for _, id := range []string{testCluster, "mk8scluster-other"} {
		h.ensureCluster(t, id)
		h.ensureNodes(t, testGroupSpec(id, "n1", "n2"))
	}
	if err := h.b.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cs := h.docker.Containers(); len(cs) != 0 {
		t.Errorf("containers after Close = %v, want none", cs)
	}
	if vs := h.docker.Volumes(); len(vs) != 0 {
		t.Errorf("volumes after Close = %v, want none", vs)
	}
	if ns := h.docker.Networks(); len(ns) != 0 {
		t.Errorf("networks after Close = %v, want none", ns)
	}
}
