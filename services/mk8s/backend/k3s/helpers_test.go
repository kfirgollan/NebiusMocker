package k3s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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
	// events is the ordered log of agent creations and removals ("create:n",
	// "remove:n") and of what the fake kube sees ("ready:n", "cordon:n",
	// "evict:n:ns/pod").
	events []string
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
	if spec.Labels[LabelRole] == roleAgent {
		d.events = append(d.events, "create:"+spec.Labels[LabelNode])
	}
	d.mu.Unlock()
	return id, nil
}

func (d *recDocker) RemoveContainer(ctx context.Context, id string) error {
	d.mu.Lock()
	spec := d.byID[id]
	if spec.Labels[LabelRole] == roleAgent {
		d.events = append(d.events, "remove:"+spec.Labels[LabelNode])
	}
	d.mu.Unlock()
	return d.Fake.RemoveContainer(ctx, id)
}

func (d *recDocker) record(event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}

// eventIndex is the position of event in the ordered log, -1 when absent.
func (d *recDocker) eventIndex(event string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Index(d.events, event)
}

// eventsWithPrefix lists the logged events starting with prefix, in order.
func (d *recDocker) eventsWithPrefix(prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, e := range d.events {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
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

type updateCall struct {
	Node  string
	Patch nodePatch
}

// fakeNode is a Node's mutable metadata in the fake API server.
type fakeNode struct {
	Labels      map[string]string
	Annotations map[string]string
	Taints      []kubeTaint
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

	mu        sync.Mutex
	version   string
	readyErr  error
	hold      map[string]bool
	nodes     map[string]*fakeNode // registered nodes
	capacity  map[string]map[string]string
	updates   []updateCall
	gpuCalls  []gpuCall
	deleted   []string
	pods      map[string][]kubePod // by node
	blocked   map[string]int       // "ns/name" -> evictions a PDB still refuses; negative: forever
	seenReady map[string]bool
}

func newFakeKube(d *recDocker) *fakeKube {
	return &fakeKube{
		docker: d, version: "1.35.10", hold: map[string]bool{}, nodes: map[string]*fakeNode{},
		capacity: map[string]map[string]string{}, pods: map[string][]kubePod{}, blocked: map[string]int{}, seenReady: map[string]bool{},
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
		n := k.register(name, k.docker.spec(c.Name).Cmd)
		if !k.seenReady[name] {
			k.seenReady[name] = true
			k.docker.record("ready:" + name)
		}
		out[name] = kubeNode{
			Ready: true, Labels: maps.Clone(n.Labels), Annotations: maps.Clone(n.Annotations), Taints: slices.Clone(n.Taints),
			Capacity: k.capacity[name],
		}
	}
	return out, nil
}

// register returns the node of an agent, creating it from the agent's
// --node-label and --node-taint flags the first time, like a kubelet
// registering. Callers hold k.mu.
func (k *fakeKube) register(name string, cmd []string) *fakeNode {
	if n, ok := k.nodes[name]; ok {
		return n
	}
	n := &fakeNode{Labels: map[string]string{"kubernetes.io/hostname": name}, Annotations: map[string]string{}}
	for _, a := range cmd {
		if kv, ok := strings.CutPrefix(a, "--node-label="); ok {
			key, v, _ := strings.Cut(kv, "=")
			n.Labels[key] = v
		}
		if kv, ok := strings.CutPrefix(a, "--node-taint="); ok {
			kvPart, effect, _ := strings.Cut(kv, ":")
			key, v, _ := strings.Cut(kvPart, "=")
			n.Taints = append(n.Taints, kubeTaint{Key: key, Value: v, Effect: effect})
		}
	}
	k.nodes[name] = n
	return n
}

// UpdateNode applies p with JSON merge-patch semantics: a nil label removes
// it, annotations merge, taints (when non-nil) replace the list.
func (k *fakeKube) UpdateNode(_ context.Context, name string, p nodePatch) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.updates = append(k.updates, updateCall{name, p})
	n, ok := k.nodes[name]
	if !ok {
		return fmt.Errorf("node %s not found", name)
	}
	for key, v := range p.Labels {
		if v == nil {
			delete(n.Labels, key)
			continue
		}
		n.Labels[key] = *v
	}
	maps.Copy(n.Annotations, p.Annotations)
	if p.Taints != nil {
		n.Taints = slices.Clone(p.Taints)
	}
	return nil
}

// n1 returns a copy of the metadata of node n1, the one the metadata tests use.
func (k *fakeKube) n1() fakeNode {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := k.nodes["n1"]
	if n == nil {
		return fakeNode{}
	}
	return fakeNode{Labels: maps.Clone(n.Labels), Annotations: maps.Clone(n.Annotations), Taints: slices.Clone(n.Taints)}
}

// addForeign adds a label and a taint someone else (kubectl, a controller) set.
func (k *fakeKube) addForeign(name, label, value string, taint kubeTaint) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := k.nodes[name]
	n.Labels[label] = value
	n.Taints = append(n.Taints, taint)
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
	delete(k.nodes, name)
	delete(k.capacity, name)
	delete(k.seenReady, name)
	return nil
}

func (k *fakeKube) Cordon(_ context.Context, name string) error {
	k.docker.record("cordon:" + name)
	return nil
}

func (k *fakeKube) PodsOn(_ context.Context, node string) ([]kubePod, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.pods[node]), nil
}

func (k *fakeKube) Evict(_ context.Context, namespace, name string) error {
	key := namespace + "/" + name
	k.mu.Lock()
	defer k.mu.Unlock()
	var node string
	for n, ps := range k.pods {
		if slices.ContainsFunc(ps, func(p kubePod) bool { return p.Namespace == namespace && p.Name == name }) {
			node = n
		}
	}
	k.docker.record("evict:" + node + ":" + key)
	if k.blocked[key] != 0 {
		if k.blocked[key] > 0 {
			k.blocked[key]--
		}
		return errEvictionBlocked
	}
	k.pods[node] = slices.DeleteFunc(k.pods[node], func(p kubePod) bool { return p.Namespace == namespace && p.Name == name })
	return nil
}

func (k *fakeKube) DeletePod(_ context.Context, namespace, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for node, ps := range k.pods {
		if slices.ContainsFunc(ps, func(p kubePod) bool { return p.Namespace == namespace && p.Name == name }) {
			k.docker.record("delete:" + node + ":" + namespace + "/" + name)
			k.pods[node] = slices.DeleteFunc(ps, func(p kubePod) bool { return p.Namespace == namespace && p.Name == name })
		}
	}
	return nil
}

// addPods schedules pods on a node.
func (k *fakeKube) addPods(node string, pods ...kubePod) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pods[node] = append(k.pods[node], pods...)
}

// blockEvictions makes a PodDisruptionBudget refuse the next n evictions of
// the pod (n < 0: all of them).
func (k *fakeKube) blockEvictions(namespace, name string, n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.blocked[namespace+"/"+name] = n
}

func (k *fakeKube) podNames(node string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, p := range k.pods[node] {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	return out
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

func (k *fakeKube) snapshot() (updates []updateCall, gpus []gpuCall, deleted []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]updateCall(nil), k.updates...), append([]gpuCall(nil), k.gpuCalls...), append([]string(nil), k.deleted...)
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
