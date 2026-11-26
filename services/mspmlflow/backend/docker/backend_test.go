package docker

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/dockerfake"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const testOwner = ":4566"

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

type harness struct {
	t *testing.T
	b *Backend
	f *dockerfake.Fake
	// handler answers the tracking server's HTTP API.
	handler atomic.Pointer[func(*http.Request) *http.Response]
}

func testConfig() config.MSPConfig {
	return config.MSPConfig{
		BindIP: "127.0.0.1", EndpointHost: "mlflow.example.test", StartTimeout: 200 * time.Millisecond,
		MLflowImage: "ghcr.io/mlflow/mlflow:v2.22.0", MLflowVersion: "2.22.0",
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, f: dockerfake.New()}
	ok := func(*http.Request) *http.Response { return reply(200, `{"experiments":[{"experiment_id":"0"}]}`) }
	h.handler.Store(&ok)
	ca, err := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	h.b, err = New(t.Context(), Options{
		Config: testConfig(), EdgeListen: testOwner, S3Listen: ":4580", Docker: h.f, Clock: clock.Real{}, CA: ca,
		Log: slog.New(slog.DiscardHandler), pollInterval: time.Millisecond,
		httpClient: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			return (*h.handler.Load())(r), nil
		})},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return h
}

func testCluster(id string) domain.BackendCluster {
	return domain.BackendCluster{
		ID: id, ProjectID: "project-1", Name: "ml", AdminUsername: "admin", AdminPassword: "pw-1",
		CPUs: 8, MemoryBytes: 32 << 30,
		Artifacts: domain.ArtifactStore{Bucket: "bkt", Region: "eu-north1", AccessKeyID: "AK", Secret: "SK"},
	}
}

func (h *harness) ensure(c domain.BackendCluster) domain.Endpoints {
	h.t.Helper()
	ep, err := h.b.EnsureCluster(h.t.Context(), c)
	if err != nil {
		h.t.Fatalf("EnsureCluster = %v", err)
	}
	return ep
}

func (h *harness) server() dockerhost.Container {
	h.t.Helper()
	c, err := h.f.InspectContainer(h.t.Context(), h.b.containerName("ml1"))
	if err != nil {
		h.t.Fatalf("server container: %v", err)
	}
	return c
}

func (h *harness) spec(name string) dockerhost.ContainerSpec {
	h.t.Helper()
	specs := h.f.Created()
	for i := len(specs) - 1; i >= 0; i-- {
		if specs[i].Name == name {
			return specs[i]
		}
	}
	h.t.Fatalf("no container %s was created", name)
	return dockerhost.ContainerSpec{}
}

func fileContent(t *testing.T, s dockerhost.ContainerSpec, path string) dockerhost.File {
	t.Helper()
	for _, f := range s.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %s", path)
	return dockerhost.File{}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()
	ca, _ := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	f := dockerfake.New()
	f.FailNext("Ping", dockerhost.ErrUnavailable)
	o := Options{Config: testConfig(), S3Listen: ":4580", Docker: f, Clock: clock.Real{}, CA: ca, Log: slog.New(slog.DiscardHandler)}
	if _, err := New(t.Context(), o); err == nil || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MSP_DOCKER_HOST") {
		t.Errorf("New with Docker down err = %v", err)
	}
	o.S3Listen = "off"
	if _, err := New(t.Context(), o); err == nil || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MSP_S3_ENDPOINT") {
		t.Errorf("New without an S3 address err = %v", err)
	}
}

func TestReapRemovesOnlyOwnLeftovers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	for name, l := range map[string]map[string]string{
		"mine":   {LabelBackend: backendLabel, LabelOwner: testOwner},
		"theirs": {LabelBackend: backendLabel, LabelOwner: ":9999"},
		"pg":     {LabelBackend: "msp-postgresql", LabelOwner: testOwner},
	} {
		_, _ = f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: name, Labels: l})
		_ = f.EnsureVolume(ctx, "vol-"+name, l)
	}
	ca, _ := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	if _, err := New(ctx, Options{Config: testConfig(), EdgeListen: testOwner, S3Listen: ":4580", Docker: f, Clock: clock.Real{}, CA: ca, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range f.Containers() {
		got = append(got, c.Name)
	}
	if !slices.Equal(got, []string{"pg", "theirs"}) || !slices.Equal(f.Volumes(), []string{"vol-pg", "vol-theirs"}) {
		t.Errorf("after reap: containers %v volumes %v", got, f.Volumes())
	}
}

func TestEnsureClusterBuildsTheImageOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("ml1"))
	const tag = "ghcr.io/mlflow/mlflow:v2.22.0-nebius-mocker"
	if !slices.Contains(h.f.Images(), tag) || !slices.Contains(h.f.Images(), "ghcr.io/mlflow/mlflow:v2.22.0") {
		t.Errorf("images = %v", h.f.Images())
	}
	h.f.FailNext("BuildImage", errors.New("must not rebuild"))
	h.ensure(testCluster("ml2"))
	if got := h.spec(h.b.containerName("ml2")).Image; got != tag {
		t.Errorf("image = %q", got)
	}
}

func TestEnsureClusterCreatesTheServer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ep := h.ensure(testCluster("ml1"))
	s := h.spec(h.b.containerName("ml1"))
	c := h.server()
	if !c.Running || c.Labels[LabelBackend] != "msp-mlflow" || c.Labels[LabelOwner] != testOwner || c.Labels[LabelCluster] != "ml1" || c.Labels[labelSpec] == "" {
		t.Errorf("container = running %v labels %v", c.Running, c.Labels)
	}
	if !slices.Equal(h.f.Volumes(), []string{h.b.containerName("ml1")}) || !slices.Equal(s.Binds, []string{h.b.volumeName("ml1") + ":/mlflow"}) {
		t.Errorf("volumes %v binds %v", h.f.Volumes(), s.Binds)
	}
	if s.Memory != 32<<30 || s.NanoCPUs != 8_000_000_000 {
		t.Errorf("limits = %d %d", s.Memory, s.NanoCPUs)
	}
	for _, want := range []string{
		"MLFLOW_AUTH_CONFIG_PATH=/etc/nmock/auth.ini", "MLFLOW_S3_ENDPOINT_URL=http://host.docker.internal:4580", "AWS_ACCESS_KEY_ID=AK",
		"AWS_SECRET_ACCESS_KEY=SK", "AWS_DEFAULT_REGION=eu-north1",
	} {
		if !slices.Contains(s.Env, want) {
			t.Errorf("env %v lacks %s", s.Env, want)
		}
	}
	if !slices.ContainsFunc(s.Env, func(e string) bool { return strings.HasPrefix(e, "MLFLOW_FLASK_SERVER_SECRET_KEY=") && len(e) > 40 }) {
		t.Errorf("env %v lacks a flask secret", s.Env)
	}
	script := s.Cmd[2]
	for _, want := range []string{
		"mlflow server --app-name basic-auth --host 0.0.0.0 --port 5000", "--backend-store-uri sqlite:////mlflow/mlflow.db",
		`--artifacts-destination "s3://$1/" --serve-artifacts`, "--certfile=/etc/nmock/tls/tls.crt --keyfile=/etc/nmock/tls/tls.key",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("command lacks %q: %s", want, script)
		}
	}
	// The auth database is migrated once, before gunicorn forks its workers.
	if !strings.HasPrefix(script, "python /etc/nmock/init_auth.py && exec mlflow server ") {
		t.Errorf("command does not initialize the auth database first: %s", script)
	}
	if init := string(fileContent(t, s, initAuthPath).Content); !strings.Contains(init, "auth.store.init_db(auth.auth_config.database_uri)") ||
		!strings.Contains(init, "auth.create_admin_user(") {
		t.Errorf("init_auth.py = %s", init)
	}
	if s.Cmd[len(s.Cmd)-1] != "bkt" {
		t.Errorf("bucket argument = %v", s.Cmd)
	}
	ini := string(fileContent(t, s, authPath).Content)
	for _, want := range []string{
		"default_permission = READ", "database_uri = sqlite:////mlflow/basic_auth.db", "admin_username = admin", "admin_password = pw-1",
		"authorization_function = mlflow.server.auth:authenticate_request_basic_auth",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("auth.ini lacks %q:\n%s", want, ini)
		}
	}
	if key := fileContent(t, s, tlsKeyPath); key.Mode != 0o600 || !strings.Contains(string(key.Content), "PRIVATE KEY") {
		t.Errorf("tls key = mode %o", key.Mode)
	}
	port := c.Ports[serverPort][0].HostPort
	if ep.Private != "mlflow.example.test:"+port || ep.Public != "" {
		t.Errorf("endpoints = %+v, published port %s", ep, port)
	}
	pc := testCluster("ml3")
	pc.PublicAccess = true
	if ep := h.ensure(pc); ep.Public != ep.Private || ep.Public == "" {
		t.Errorf("public endpoint = %+v", ep)
	}
}

func TestStopStartKeepsContainerAndPort(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("ml1")
	first := h.ensure(c)
	id := h.server().ID
	if err := h.b.StopCluster(t.Context(), "ml1"); err != nil {
		t.Fatal(err)
	}
	if h.server().Running {
		t.Error("still running after StopCluster")
	}
	if err := h.b.StopCluster(t.Context(), "nope"); err != nil {
		t.Errorf("StopCluster of an unknown cluster = %v", err)
	}
	c.AdminPassword = "" // only the first call carries it
	if got := h.ensure(c); got != first {
		t.Errorf("endpoint after start = %+v, want %+v", got, first)
	}
	if got := h.server(); !got.Running || got.ID != id {
		t.Errorf("after start: running %v same container %v", got.Running, got.ID == id)
	}

	// A changed artifact store recreates the server on the same port.
	c.Artifacts.Secret = "SK2"
	if got := h.ensure(c); got != first {
		t.Errorf("endpoint after recreation = %+v, want %+v", got, first)
	}
	if h.server().ID == id {
		t.Error("the container was not recreated")
	}
	if !strings.Contains(string(fileContent(t, h.spec(h.b.containerName("ml1")), authPath).Content), "admin_password = pw-1") {
		t.Error("the recreated server lost the remembered admin password")
	}
}

func TestDeleteRemovesServerAndVolume(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("ml1"))
	h.ensure(testCluster("ml2"))
	for range 2 {
		if err := h.b.DeleteCluster(t.Context(), "ml1"); err != nil {
			t.Fatalf("DeleteCluster = %v", err)
		}
	}
	if len(h.f.Containers()) != 1 || !slices.Equal(h.f.Volumes(), []string{h.b.containerName("ml2")}) {
		t.Errorf("containers %v volumes %v", h.f.Containers(), h.f.Volumes())
	}
	if err := h.b.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.f.Containers())+len(h.f.Volumes()) != 0 {
		t.Errorf("Close left %v %v", h.f.Containers(), h.f.Volumes())
	}
}

func TestExperimentsCountPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("ml1"))
	var users []string
	handler := func(r *http.Request) *http.Response {
		u, p, _ := r.BasicAuth()
		users = append(users, u+":"+p)
		if r.URL.Query().Get("view_type") != "ACTIVE_ONLY" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("page_token") == "" {
			return reply(200, `{"experiments":[{},{}],"next_page_token":"p2"}`)
		}
		return reply(200, `{"experiments":[{}]}`)
	}
	h.handler.Store(&handler)
	n, err := h.b.ExperimentsCount(t.Context(), "ml1")
	if err != nil || n != 3 {
		t.Fatalf("ExperimentsCount = %d, %v; want 3", n, err)
	}
	if users[0] != "admin:pw-1" {
		t.Errorf("basic auth = %v", users)
	}
	if err := h.b.StopCluster(t.Context(), "ml1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.ExperimentsCount(t.Context(), "ml1"); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("ExperimentsCount of a stopped server err = %v, want BadState", err)
	}
}

func TestReadinessTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	down := func(*http.Request) *http.Response { return reply(503, "starting") }
	h.handler.Store(&down)
	_, err := h.b.EnsureCluster(t.Context(), testCluster("ml1"))
	if !errs.IsKind(err, errs.KindInternal) || !strings.Contains(err.Error(), "not ready within") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("EnsureCluster err = %v", err)
	}
	// Bad credentials are not ready either.
	unauth := func(*http.Request) *http.Response { return reply(401, "no") }
	h.handler.Store(&unauth)
	if _, err := h.b.EnsureCluster(t.Context(), testCluster("ml1")); !errs.IsKind(err, errs.KindInternal) {
		t.Errorf("EnsureCluster with 401 and known credentials err = %v, want Internal", err)
	}
}

func TestExitedServerReportsItsLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.SetAutoExit("nmock-mlflow-", 1, "ModuleNotFoundError: boto3\n")
	_, err := h.b.EnsureCluster(t.Context(), testCluster("ml1"))
	if !errs.IsKind(err, errs.KindInternal) || !strings.Contains(err.Error(), "ModuleNotFoundError") {
		t.Fatalf("EnsureCluster err = %v", err)
	}
}

func TestS3Endpoint(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	for listen, want := range map[string]string{":4580": "http://host.docker.internal:4580", "127.0.0.1:4581": "http://host.docker.internal:4581"} {
		if got, err := s3Endpoint(cfg, listen); err != nil || got != want {
			t.Errorf("s3Endpoint(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	cfg.S3Endpoint = "https://s3.example.test"
	if got, _ := s3Endpoint(cfg, "off"); got != "https://s3.example.test" {
		t.Errorf("explicit endpoint = %q", got)
	}
}
