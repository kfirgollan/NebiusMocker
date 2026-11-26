package k3s

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/dockerfake"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const (
	testCluster = "mk8scluster-test1"
	testGroup   = "mk8snodegroup-test1"
)

var errBoom = errors.New("boom")

// stepClock is a clock whose After advances it by the duration at once, so
// polling loops and timeouts run without sleeping.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock() *stepClock { return &stepClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// recDocker is the Docker fake plus what the fake cannot keep: the specs
// containers were created with, and scripted behavior at start.
type recDocker struct {
	*dockerfake.Fake
	mu      sync.Mutex
	specs   map[string]dockerhost.ContainerSpec // by container name, last created
	byID    map[string]dockerhost.ContainerSpec
	creates int
	// exitServer, when set, exits the server right after it starts.
	exitServer *exitScript
	// exitAgents maps a node name to the logs of its agent, which exits right after it starts.
	exitAgents map[string]string
}

type exitScript struct {
	code int
	logs string
}

func newRecDocker() *recDocker {
	return &recDocker{
		Fake: dockerfake.New(), specs: map[string]dockerhost.ContainerSpec{}, byID: map[string]dockerhost.ContainerSpec{},
		exitAgents: map[string]string{},
	}
}

func (d *recDocker) CreateContainer(ctx context.Context, spec dockerhost.ContainerSpec) (string, error) {
	id, err := d.Fake.CreateContainer(ctx, spec)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.specs[spec.Name], d.byID[id] = spec, spec
	d.creates++
	d.mu.Unlock()
	return id, nil
}

func (d *recDocker) StartContainer(ctx context.Context, id string) error {
	if err := d.Fake.StartContainer(ctx, id); err != nil {
		return err
	}
	d.mu.Lock()
	spec := d.byID[id]
	exitServer, exitLogs := d.exitServer, d.exitAgents[spec.Labels[LabelNode]]
	d.mu.Unlock()
	switch spec.Labels[LabelRole] {
	case roleServer:
		if exitServer != nil {
			d.SetExited(id, exitServer.code, exitServer.logs)
			return nil
		}
		d.SetFile(id, adminKubeconfigPath, testKubeconfig())
	case roleAgent:
		if exitLogs != "" {
			d.SetExited(id, 3, exitLogs)
		}
	}
	return nil
}

func (d *recDocker) spec(name string) dockerhost.ContainerSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[name]
}

func (d *recDocker) createCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.creates
}

// containerNamed returns the fake's container by name.
func (d *recDocker) containerNamed(t *testing.T, name string) dockerhost.Container {
	t.Helper()
	c, err := d.InspectContainer(t.Context(), name)
	if err != nil {
		t.Fatalf("InspectContainer(%s) = %v", name, err)
	}
	return c
}

const testCA = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"

func testKubeconfig() []byte {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: default\n  cluster:\n    server: https://127.0.0.1:6443\n" +
		"    certificate-authority-data: " + b64(testCA) + "\nusers:\n- name: default\n  user:\n" +
		"    client-certificate-data: " + b64("CERT") + "\n    client-key-data: " + b64("KEY") + "\n")
}

type labelCall struct {
	Node   string
	Labels map[string]string
}

type gpuCall struct {
	Node, Resource string
	Count          int
}

// fakeKube is a kubeAPI whose nodes are the agent containers of the Docker
// fake: a running agent registers as a Ready node (unless held back), with
// the labels of its --node-label flags, like a kubelet.
type fakeKube struct {
	docker *recDocker

	mu         sync.Mutex
	version    string
	readyErr   error
	hold       map[string]bool
	extra      map[string]map[string]string // labels applied through LabelNode
	capacity   map[string]map[string]string
	labelCalls []labelCall
	gpuCalls   []gpuCall
	deleted    []string
}

func newFakeKube(d *recDocker) *fakeKube {
	return &fakeKube{
		docker: d, version: "1.35.10", hold: map[string]bool{}, extra: map[string]map[string]string{},
		capacity: map[string]map[string]string{},
	}
}

func (k *fakeKube) Ready(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.readyErr
}

func (k *fakeKube) Version(context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.version, nil
}

func (k *fakeKube) Nodes(ctx context.Context) (map[string]kubeNode, error) {
	cs, err := k.docker.ListContainers(ctx, map[string]string{LabelBackend: backendName, LabelRole: roleAgent})
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]kubeNode{}
	for _, c := range cs {
		name := c.Labels[LabelNode]
		if !c.Running || k.hold[name] {
			continue
		}
		labels := map[string]string{"kubernetes.io/hostname": name}
		for _, a := range k.docker.spec(c.Name).Cmd {
			if kv, ok := strings.CutPrefix(a, "--node-label="); ok {
				key, v, _ := strings.Cut(kv, "=")
				labels[key] = v
			}
		}
		for key, v := range k.extra[name] {
			labels[key] = v
		}
		out[name] = kubeNode{Ready: true, Labels: labels, Capacity: k.capacity[name]}
	}
	return out, nil
}

func (k *fakeKube) LabelNode(_ context.Context, name string, labels map[string]string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.labelCalls = append(k.labelCalls, labelCall{name, labels})
	if k.extra[name] == nil {
		k.extra[name] = map[string]string{}
	}
	for key, v := range labels {
		k.extra[name][key] = v
	}
	return nil
}

func (k *fakeKube) SetExtendedResource(_ context.Context, node, resource string, count int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gpuCalls = append(k.gpuCalls, gpuCall{node, resource, count})
	if k.capacity[node] == nil {
		k.capacity[node] = map[string]string{}
	}
	k.capacity[node][resource] = strconv.Itoa(count)
	return nil
}

func (k *fakeKube) DeleteNode(_ context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deleted = append(k.deleted, name)
	return nil
}

func (k *fakeKube) setHold(node string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.hold[node] = true
}

func (k *fakeKube) setReadyErr(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.readyErr = err
}

func (k *fakeKube) snapshot() (labels []labelCall, gpus []gpuCall, deleted []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]labelCall(nil), k.labelCalls...), append([]gpuCall(nil), k.gpuCalls...), append([]string(nil), k.deleted...)
}

type kubeBuild struct {
	base string
	kc   adminKubeconfig
}

// harness is a Backend over the fakes.
type harness struct {
	b      *Backend
	docker *recDocker
	kube   *fakeKube
	clk    *stepClock

	mu     sync.Mutex
	builds []kubeBuild
}

func testOptions(d *recDocker) Options {
	cfg := config.Default().MK8sK3s
	cfg.Images = map[string]string{"1.35": "rancher/k3s:v1.35.10-k3s1", "1.36": "rancher/k3s:v1.36.6-k3s1"}
	cfg.StartTimeout = 5 * time.Second
	cfg.EndpointHost = "k8s.test"
	return Options{
		Config: cfg, Versions: []string{"1.35", "1.36"}, EdgeListen: ":4566",
		Docker: d, Clock: newStepClock(), Log: slog.New(slog.DiscardHandler), pollInterval: time.Second,
	}
}

// newHarness builds a Backend over the fakes; mutate adjusts the options
// before New.
func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	d := newRecDocker()
	h := &harness{docker: d, kube: newFakeKube(d)}
	o := testOptions(d)
	h.clk, _ = o.Clock.(*stepClock)
	o.newKube = func(base string, kc adminKubeconfig) (kubeAPI, error) {
		h.mu.Lock()
		h.builds = append(h.builds, kubeBuild{base, kc})
		h.mu.Unlock()
		return h.kube, nil
	}
	if mutate != nil {
		mutate(&o)
	}
	b, err := New(t.Context(), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.b = b
	return h
}

func (h *harness) lastBuild(t *testing.T) kubeBuild {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.builds) == 0 {
		t.Fatal("no Kubernetes client was built")
	}
	return h.builds[len(h.builds)-1]
}

func testClusterSpec(id string) domain.BackendCluster {
	return domain.BackendCluster{ID: id, ProjectID: "project-1", Name: "c", Version: "1.35", PublicEndpoint: true, EtcdClusterSize: 1}
}

func (h *harness) ensureCluster(t *testing.T, id string) domain.ClusterEndpoint {
	t.Helper()
	ep, err := h.b.EnsureCluster(t.Context(), testClusterSpec(id))
	if err != nil {
		t.Fatalf("EnsureCluster(%s): %v", id, err)
	}
	return ep
}

func testGroupSpec(clusterID string, nodes ...string) domain.BackendNodeGroup {
	g := domain.BackendNodeGroup{ClusterID: clusterID, NodeGroupID: testGroup, Name: "g", Version: "1.35"}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, domain.BackendNode{Name: n, InstanceID: "computeinstance-" + n})
	}
	return g
}

func (h *harness) ensureNodes(t *testing.T, g domain.BackendNodeGroup) []domain.NodeReadiness {
	t.Helper()
	out, err := h.b.EnsureNodes(t.Context(), g)
	if err != nil {
		t.Fatalf("EnsureNodes: %v", err)
	}
	return out
}

// agents lists the agent containers of testCluster, by node name.
func (h *harness) agents(t *testing.T) map[string]dockerhost.Container {
	t.Helper()
	cs, err := h.docker.ListContainers(t.Context(), map[string]string{LabelBackend: backendName, LabelCluster: testCluster, LabelRole: roleAgent})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	out := map[string]dockerhost.Container{}
	for _, c := range cs {
		out[c.Labels[LabelNode]] = c
	}
	return out
}
