package k3s

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.yaml.in/yaml/v3"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestResolveServiceCIDR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: "10.43.0.0/16"},
		{in: "/16", want: "10.43.0.0/16"},
		{in: "/20", want: "10.43.0.0/20"},
		{in: "/24", want: "10.43.0.0/24"},
		{in: "10.96.5.7/16", want: "10.96.0.0/16"},
		{in: "/8", wantErr: true},
		{in: "/25", wantErr: true},
		{in: "/x", wantErr: true},
		{in: "garbage", wantErr: true},
		{in: "fd00::/64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := resolveServiceCIDR(tt.in)
			if tt.wantErr {
				if !errs.IsKind(err, errs.KindBadRequest) {
					t.Fatalf("resolveServiceCIDR(%q) = %q, %v; want a BadRequest", tt.in, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveServiceCIDR(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func fileContent(t *testing.T, spec dockerhost.ContainerSpec, path string) string {
	t.Helper()
	for _, f := range spec.Files {
		if f.Path == path {
			return string(f.Content)
		}
	}
	t.Fatalf("container %s has no file %s", spec.Name, path)
	return ""
}

func TestEnsureClusterServerSpec(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.Config.BindIP = "10.1.2.3" })
	c := testClusterSpec(testCluster)
	c.ServiceCIDR = "/20"
	ep, err := h.b.EnsureCluster(t.Context(), c)
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	spec := h.docker.spec(serverName(testCluster))
	if spec.Image != "rancher/k3s:v1.35.10-k3s1" || !spec.Privileged || spec.Network != networkName(testCluster) {
		t.Errorf("server spec = image %q privileged %v network %q", spec.Image, spec.Privileged, spec.Network)
	}
	wantLabels := map[string]string{
		LabelBackend: "k3s", LabelOwner: h.b.webhookURL, LabelCluster: testCluster, LabelRole: roleServer, labelSpec: "rancher/k3s:v1.35.10-k3s1",
	}
	if diff := cmp.Diff(wantLabels, spec.Labels); diff != "" {
		t.Errorf("server labels mismatch (-want +got):\n%s", diff)
	}
	if want := []dockerhost.PortBinding{{ContainerPort: "6443/tcp", HostIP: "10.1.2.3"}}; !slices.Equal(spec.Ports, want) {
		t.Errorf("server ports = %v, want %v", spec.Ports, want)
	}
	for _, want := range []string{
		"server", "--disable-agent", "--service-cidr=10.43.0.0/20", "--tls-san=k8s.test", "--tls-san=127.0.0.1",
		"--kube-apiserver-arg=authentication-token-webhook-config-file=" + webhookConfigPath,
		"--kube-apiserver-arg=authentication-token-webhook-version=v1",
	} {
		if !slices.Contains(spec.Cmd, want) {
			t.Errorf("server command %v lacks %q", spec.Cmd, want)
		}
	}
	if !slices.Contains(spec.Env, "K3S_TOKEN="+joinToken(h.b.secret, testCluster)) {
		t.Errorf("server env = %v, want the cluster join token", spec.Env)
	}
	if got := h.docker.containerNamed(t, serverName(testCluster)); !got.Running {
		t.Errorf("server is not running: %+v", got)
	}

	// The webhook kubeconfig points at the mocker's TokenReview endpoint.
	hook := fileContent(t, spec, webhookConfigPath)
	wantServer := "server: http://host.docker.internal:4566/mk8s/v1/clusters/" + testCluster + "/tokenreview"
	if !strings.Contains(hook, wantServer) || strings.Contains(hook, "insecure-skip-tls-verify") {
		t.Errorf("webhook config = %q, want %q and no insecure-skip-tls-verify", hook, wantServer)
	}
	assertRBAC(t, fileContent(t, spec, rbacManifestPath))

	// The Kubernetes client is built for the published port, with the admin credentials.
	build := h.lastBuild(t)
	port := h.docker.containerNamed(t, serverName(testCluster)).Ports[apiPort][0].HostPort
	if build.base != "https://127.0.0.1:"+port && build.base != "https://10.1.2.3:"+port {
		t.Errorf("Kubernetes client base = %q, want the published port %s", build.base, port)
	}
	if string(build.kc.CA) != testCA || string(build.kc.Cert) != "CERT" || string(build.kc.Key) != "KEY" {
		t.Errorf("Kubernetes client credentials = %+v, want the ones of k3s.yaml", build.kc)
	}
	wantURL := "https://k8s.test:" + port
	want := ep
	want.PublicEndpoint, want.PrivateEndpoint, want.CACertificate, want.KubernetesVersion = wantURL, wantURL, testCA, "1.35.10"
	if diff := cmp.Diff(want, ep); diff != "" {
		t.Errorf("endpoint mismatch (-want +got):\n%s", diff)
	}
}

func assertRBAC(t *testing.T, manifest string) {
	t.Helper()
	got := map[string][]string{} // binding name -> its role, then its groups
	roles := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			RoleRef struct {
				Kind, Name string
			} `yaml:"roleRef"`
			Subjects []struct {
				Kind, Name string
			} `yaml:"subjects"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("RBAC manifest does not parse: %v", err)
		}
		if doc.Kind == "ClusterRole" {
			roles[doc.Metadata.Name] = true
			continue
		}
		if doc.Kind != "ClusterRoleBinding" || doc.RoleRef.Kind != "ClusterRole" {
			t.Fatalf("unexpected RBAC document %+v", doc)
		}
		entry := []string{doc.RoleRef.Name}
		for _, s := range doc.Subjects {
			if s.Kind != "Group" {
				t.Fatalf("binding %s has a %s subject, want groups only", doc.Metadata.Name, s.Kind)
			}
			entry = append(entry, s.Name)
		}
		got[doc.Metadata.Name] = entry
	}
	want := map[string][]string{
		"nebius:viewer":      {"view", "nebius:viewer"},
		"nebius:editor":      {"edit", "nebius:editor"},
		"nebius:admin":       {"cluster-admin", "nebius:admin"},
		"nebius:node-reader": {"nebius:node-reader", "nebius:viewer", "nebius:editor"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RBAC bindings mismatch (-want +got):\n%s", diff)
	}
	if !roles["nebius:node-reader"] {
		t.Error("RBAC manifest lacks the nebius:node-reader ClusterRole")
	}
}

func TestEnsureClusterTLSEdgeWebhook(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.EdgeTLS = true })
	h.ensureCluster(t, testCluster)
	hook := fileContent(t, h.docker.spec(serverName(testCluster)), webhookConfigPath)
	for _, want := range []string{
		"server: https://host.docker.internal:4566/mk8s/v1/clusters/" + testCluster + "/tokenreview",
		"insecure-skip-tls-verify: true",
	} {
		if !strings.Contains(hook, want) {
			t.Errorf("webhook config = %q, want it to contain %q", hook, want)
		}
	}
}

func TestEnsureClusterDefaultServiceCIDR(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.ensureCluster(t, testCluster)
	if cmd := h.docker.spec(serverName(testCluster)).Cmd; !slices.Contains(cmd, "--service-cidr=10.43.0.0/16") {
		t.Errorf("server command %v lacks the default --service-cidr=10.43.0.0/16", cmd)
	}
}

func TestEnsureClusterPrivateOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	c := testClusterSpec(testCluster)
	c.PublicEndpoint = false
	ep, err := h.b.EnsureCluster(t.Context(), c)
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	if ep.PublicEndpoint != "" || !strings.HasPrefix(ep.PrivateEndpoint, "https://k8s.test:") {
		t.Fatalf("endpoint = %+v, want no public endpoint and a private https://k8s.test:<port>", ep)
	}
	// Asking again, now with a public endpoint, returns the same address as public.
	c.PublicEndpoint = true
	again, err := h.b.EnsureCluster(t.Context(), c)
	if err != nil {
		t.Fatalf("second EnsureCluster: %v", err)
	}
	if again.PublicEndpoint != ep.PrivateEndpoint {
		t.Fatalf("public endpoint after enabling = %q, want %q", again.PublicEndpoint, ep.PrivateEndpoint)
	}
}

func TestEnsureClusterIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	first := h.ensureCluster(t, testCluster)
	creates := h.docker.createCount()
	second := h.ensureCluster(t, testCluster)
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("second EnsureCluster endpoint differs (-first +second):\n%s", diff)
	}
	if got := h.docker.createCount(); got != creates {
		t.Errorf("second EnsureCluster created %d containers, want 0", got-creates)
	}

	// Without the backend's memory of the cluster (cleared here), EnsureCluster
	// attaches to the running server of the right version instead of replacing it.
	h.b.mu.Lock()
	h.b.clusters = map[string]*clusterState{}
	h.b.mu.Unlock()
	third := h.ensureCluster(t, testCluster)
	if third.PublicEndpoint != first.PublicEndpoint || h.docker.createCount() != creates {
		t.Errorf("EnsureCluster without state = %+v after %d creates, want the same server (%+v, %d creates)",
			third, h.docker.createCount(), first, creates)
	}
}

func TestEnsureClusterVersionChangeKeepsPort(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	first := h.ensureCluster(t, testCluster)
	oldID := h.docker.containerNamed(t, serverName(testCluster)).ID
	c := testClusterSpec(testCluster)
	c.Version = "1.36"
	h.kube.mu.Lock()
	h.kube.version = "1.36.6"
	h.kube.mu.Unlock()
	second, err := h.b.EnsureCluster(t.Context(), c)
	if err != nil {
		t.Fatalf("EnsureCluster(1.36): %v", err)
	}
	got := h.docker.containerNamed(t, serverName(testCluster))
	if got.ID == oldID || got.Image != "rancher/k3s:v1.36.6-k3s1" {
		t.Errorf("server after the upgrade = %+v, want a new container on the 1.36 image", got)
	}
	if second.PublicEndpoint != first.PublicEndpoint {
		t.Errorf("endpoint after the upgrade = %q, want the same host port as %q", second.PublicEndpoint, first.PublicEndpoint)
	}
	if second.KubernetesVersion != "1.36.6" {
		t.Errorf("Kubernetes version = %q, want 1.36.6", second.KubernetesVersion)
	}
	if n := len(h.docker.Volumes()); n != 1 {
		t.Errorf("volumes = %d, want the data volume kept (1)", n)
	}
}

func TestEnsureClusterServerExits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.docker.exitServer = &exitScript{code: 1, logs: "line one\nFATA[0001] etcd: no space left on device\n"}
	_, err := h.b.EnsureCluster(t.Context(), testClusterSpec(testCluster))
	if !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("EnsureCluster = %v, want BadState", err)
	}
	for _, want := range []string{"exited (code 1)", "no space left on device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestEnsureClusterNotReadyInTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.kube.setReadyErr(errBoom)
	start := h.clk.Now()
	_, err := h.b.EnsureCluster(t.Context(), testClusterSpec(testCluster))
	if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT") {
		t.Fatalf("EnsureCluster = %v, want BadState naming NEBIUS_MOCKER_MK8S_K3S_START_TIMEOUT", err)
	}
	if elapsed := h.clk.Now().Sub(start); elapsed < 5*time.Second {
		t.Errorf("gave up after %s, want at least the 5s StartTimeout", elapsed)
	}
}

func TestEnsureClusterRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.BackendCluster)
	}{
		{name: "allowed cidrs", mutate: func(c *domain.BackendCluster) { c.AllowedCIDRs = []string{"10.0.0.0/8"} }},
		{name: "unknown version", mutate: func(c *domain.BackendCluster) { c.Version = "1.99" }},
		{name: "bad service cidr", mutate: func(c *domain.BackendCluster) { c.ServiceCIDR = "nope" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			c := testClusterSpec(testCluster)
			tt.mutate(&c)
			if _, err := h.b.EnsureCluster(t.Context(), c); !errs.IsKind(err, errs.KindBadRequest) {
				t.Fatalf("EnsureCluster = %v, want BadRequest", err)
			}
			if n := len(h.docker.Containers()); n != 0 {
				t.Errorf("a rejected cluster created %d containers", n)
			}
		})
	}
}

func TestEnsureClusterCreateFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.docker.FailNext("CreateContainer", errBoom)
	if _, err := h.b.EnsureCluster(t.Context(), testClusterSpec(testCluster)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("EnsureCluster = %v, want the Docker error", err)
	}
}

func TestWebhookConfigIsKubeconfig(t *testing.T) {
	t.Parallel()
	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Server   string `yaml:"server"`
				Insecure bool   `yaml:"insecure-skip-tls-verify"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.NewDecoder(bytes.NewReader([]byte(webhookConfig("https://h:1", "c-1")))).Decode(&cfg); err != nil {
		t.Fatalf("webhook config does not parse: %v", err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != "https://h:1/mk8s/v1/clusters/c-1/tokenreview" || !cfg.Clusters[0].Cluster.Insecure {
		t.Fatalf("webhook config = %+v", cfg)
	}
}
