package docker

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/dockerfake"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const testOwner = ":4566"

// harness is a backend on a fake Docker whose PostgreSQL always answers.
type harness struct {
	t *testing.T
	b *Backend
	f *dockerfake.Fake
}

func testConfig() config.MSPConfig {
	return config.MSPConfig{
		BindIP: "127.0.0.1", EndpointHost: "pg.example.test", StartTimeout: 200 * time.Millisecond,
		PostgresImages: config.DefaultPostgresImages(), PgBouncerImage: "edoburu/pgbouncer:v1.25.2-p0", PostgresMaxHosts: 3,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := dockerfake.New()
	f.SetAutoExit("nmock-pg-helper-", 0, "")
	f.SetExecHandler(func(_ string, e dockerhost.ExecSpec) dockerhost.ExecResult {
		last := e.Cmd[len(e.Cmd)-1]
		switch {
		case last == "select pg_is_in_recovery()":
			return dockerhost.ExecResult{Stdout: "f\n"}
		case strings.Contains(strings.Join(e.Cmd, " "), "pg_basebackup"):
			return dockerhost.ExecResult{Stdout: "12345\n"}
		}
		return dockerhost.ExecResult{}
	})
	ca, err := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), Options{
		Config: testConfig(), EdgeListen: testOwner, Docker: f, Clock: clock.Real{}, CA: ca,
		Log: slog.New(slog.DiscardHandler), pollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return &harness{t: t, b: b, f: f}
}

func testCluster(id string) domain.BackendCluster {
	return domain.BackendCluster{
		ID: id, ProjectID: "project-1", Name: "db", Version: "16", Hosts: 2, CPUs: 2, MemoryBytes: 8 << 30, DiskGiB: 10,
		Parameters: map[string]string{"statement_timeout": "30000ms", "max_connections": "200"},
		Pooler:     domain.Pooler{Mode: "transaction", MaxPoolSize: 20},
		UserName:   "app", DBName: "appdb", Password: "s3cret'pw",
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

func (h *harness) container(name string) dockerhost.Container {
	h.t.Helper()
	c, err := h.f.InspectContainer(h.t.Context(), name)
	if err != nil {
		h.t.Fatalf("container %s: %v", name, err)
	}
	return c
}

func (h *harness) names() []string {
	var out []string
	for _, c := range h.f.Containers() {
		out = append(out, c.Name)
	}
	return out
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

func TestNewPingFailureNamesTheSettings(t *testing.T) {
	t.Parallel()
	f := dockerfake.New()
	f.FailNext("Ping", dockerhost.ErrUnavailable)
	ca, _ := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	_, err := New(t.Context(), Options{Config: testConfig(), Docker: f, Clock: clock.Real{}, CA: ca, Log: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MSP_DOCKER_HOST") || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MSP_POSTGRESQL_BACKEND=mock") {
		t.Fatalf("New err = %v, want the settings named", err)
	}
}

func TestReapRemovesOnlyOwnLeftovers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	mine := map[string]string{LabelBackend: backendLabel, LabelOwner: testOwner}
	theirs := map[string]string{LabelBackend: backendLabel, LabelOwner: ":9999"}
	other := map[string]string{LabelBackend: "k3s", LabelOwner: testOwner}
	for name, l := range map[string]map[string]string{"mine": mine, "theirs": theirs, "other": other} {
		if _, err := f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: name, Labels: l}); err != nil {
			t.Fatal(err)
		}
		_ = f.EnsureVolume(ctx, "vol-"+name, l)
		_, _ = f.EnsureNetwork(ctx, "net-"+name, l)
	}
	ca, _ := localca.LoadOrCreate(t.TempDir(), clock.Real{})
	if _, err := New(ctx, Options{Config: testConfig(), EdgeListen: testOwner, Docker: f, Clock: clock.Real{}, CA: ca, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range f.Containers() {
		got = append(got, c.Name)
	}
	if want := []string{"other", "theirs"}; !slices.Equal(got, want) {
		t.Errorf("containers after reap = %v, want %v", got, want)
	}
	if vols := f.Volumes(); !slices.Equal(vols, []string{"vol-other", "vol-theirs"}) {
		t.Errorf("volumes after reap = %v", vols)
	}
	if len(f.Networks()) != 2 {
		t.Errorf("networks after reap = %v", f.Networks())
	}
}

func TestEnsureClusterCreatesTheCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ep := h.ensure(testCluster("pg1"))

	wantNames := []string{"nmock-pg-pg1-0", "nmock-pg-pg1-1", "nmock-pg-pg1-pooler"}
	if got := h.names(); !slices.Equal(got, wantNames) {
		t.Fatalf("containers = %v, want %v", got, wantNames)
	}
	if got := h.f.Volumes(); !slices.Equal(got, []string{"nmock-pg-backups-4566", "nmock-pg-pg1-0", "nmock-pg-pg1-1"}) {
		t.Errorf("volumes = %v", got)
	}
	if nets := h.f.Networks(); len(nets) != 1 || nets[0].Name != "nmock-pg-pg1" {
		t.Errorf("networks = %v", nets)
	}
	for _, name := range wantNames {
		c := h.container(name)
		if !c.Running {
			t.Errorf("%s is not running", name)
		}
		if c.Labels[LabelBackend] != "msp-postgresql" || c.Labels[LabelOwner] != testOwner || c.Labels[LabelCluster] != "pg1" || c.Labels[labelSpec] == "" {
			t.Errorf("%s labels = %v", name, c.Labels)
		}
	}

	primary := h.spec("nmock-pg-pg1-0")
	if primary.Image != "postgres:16.11-alpine" || primary.Memory != 8<<30 || primary.NanoCPUs != 2_000_000_000 {
		t.Errorf("primary image/limits = %q %d %d", primary.Image, primary.Memory, primary.NanoCPUs)
	}
	if !slices.Equal(primary.Aliases, []string{"pg-0"}) || primary.Network != "nmock-pg-pg1" {
		t.Errorf("primary network = %q aliases %v", primary.Network, primary.Aliases)
	}
	wantBinds := []string{"nmock-pg-pg1-0:/var/lib/postgresql/data", "nmock-pg-backups-4566:/backups"}
	if !slices.Equal(primary.Binds, wantBinds) {
		t.Errorf("primary binds = %v, want %v", primary.Binds, wantBinds)
	}
	for _, want := range []string{"POSTGRES_USER=app", "POSTGRES_DB=appdb", "POSTGRES_PASSWORD=s3cret'pw"} {
		if !slices.Contains(primary.Env, want) {
			t.Errorf("primary env %v lacks %s", primary.Env, want)
		}
	}
	if primary.Ports[0].HostIP != "127.0.0.1" || primary.Ports[0].ContainerPort != "5432/tcp" || primary.Ports[0].HostPort == "" {
		t.Errorf("primary ports = %+v", primary.Ports)
	}

	key := fileOf(t, primary, tlsKey)
	if key.Mode != 0o600 || key.UID != 70 || !strings.Contains(string(key.Content), "PRIVATE KEY") {
		t.Errorf("server key = mode %o uid %d", key.Mode, key.UID)
	}
	if crt := fileOf(t, primary, tlsCert); !strings.Contains(string(crt.Content), "BEGIN CERTIFICATE") {
		t.Errorf("server cert = %q", crt.Content)
	}

	rw, ro := port(t, h.container("nmock-pg-pg1-pooler")), port(t, h.container("nmock-pg-pg1-1"))
	if ep.PrivateReadWrite != "pg.example.test:"+rw || ep.PrivateReadOnly != "pg.example.test:"+ro {
		t.Errorf("endpoints = %+v, want rw port %s ro port %s (first replica)", ep, rw, ro)
	}
	if ep.PublicReadWrite != "" || ep.PublicReadOnly != "" {
		t.Errorf("public endpoints without public access: %+v", ep)
	}
}

func fileOf(t *testing.T, s dockerhost.ContainerSpec, path string) dockerhost.File {
	t.Helper()
	for _, f := range s.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("container %s has no file %s", s.Name, path)
	return dockerhost.File{}
}

func port(t *testing.T, c dockerhost.Container) string {
	t.Helper()
	b := c.Ports["5432/tcp"]
	if len(b) != 1 {
		t.Fatalf("%s ports = %v", c.Name, c.Ports)
	}
	return b[0].HostPort
}

func TestParametersRenderIntoFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	for _, name := range []string{"nmock-pg-pg1-0", "nmock-pg-pg1-1"} {
		cmd := h.spec(name).Cmd
		joined := strings.Join(cmd, " ")
		for _, want := range []string{
			"-c max_connections=200", "-c statement_timeout=30000ms", "-c ssl=on", "-c ssl_cert_file=/etc/nmock/tls/server.crt",
			"-c ssl_key_file=/etc/nmock/tls/server.key", "-c listen_addresses=*", "-c wal_level=replica", "-c max_wal_senders=10",
			"-c hot_standby=on", "-c archive_mode=on",
			"-c archive_command=test ! -f /backups/pg1/wal/%f && cp %p /backups/pg1/wal/%f",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s args %q lack %q", name, joined, want)
			}
		}
		if strings.Index(joined, "max_connections") > strings.Index(joined, "statement_timeout") {
			t.Errorf("%s: parameters are not sorted: %q", name, joined)
		}
	}
}

func TestPostgresFlagsDefaultsComeFirstAndMandatoryLast(t *testing.T) {
	t.Parallel()
	flags := strings.Join(postgresFlags("c", map[string]string{"archive_timeout": "10", "ssl": "off"}), " ")
	if strings.Index(flags, "archive_timeout=60") > strings.Index(flags, "archive_timeout=10") {
		t.Errorf("a parameter must override the default archive_timeout: %q", flags)
	}
	if strings.Index(flags, "ssl=off") > strings.Index(flags, "ssl=on") {
		t.Errorf("ssl=on must come after parameters so it wins: %q", flags)
	}
}

func TestReplicaSeedsFromThePrimary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	replica, primary := h.spec("nmock-pg-pg1-1"), h.spec("nmock-pg-pg1-0")
	script := replica.Entrypoint[2]
	if !strings.Contains(script, "pg_basebackup -h pg-0 -U nmock_replicator") || !strings.Contains(script, "-R -X stream") {
		t.Errorf("replica script = %s", script)
	}
	if strings.Contains(primary.Entrypoint[2], "pg_basebackup") {
		t.Errorf("primary script seeds from a primary: %s", primary.Entrypoint[2])
	}
	if !strings.HasSuffix(strings.TrimSpace(script), `exec docker-entrypoint.sh postgres "$@"`) {
		t.Errorf("replica script does not hand over to the image entrypoint: %s", script)
	}
	if !slices.ContainsFunc(replica.Env, func(e string) bool { return strings.HasPrefix(e, "PGPASSWORD=") }) {
		t.Errorf("replica env = %v", replica.Env)
	}
	if !slices.Equal(replica.Cmd, primary.Cmd) {
		t.Errorf("hosts run with different flags:\n%v\n%v", primary.Cmd, replica.Cmd)
	}
	var sawHBA, sawRoles bool
	for _, e := range h.f.Execs() {
		joined := strings.Join(e.Spec.Cmd, " ")
		sawHBA = sawHBA || (strings.Contains(joined, "host replication") && strings.HasSuffix(joined, "nmock nmock_replicator"))
		sawRoles = sawRoles || strings.Contains(joined, "CREATE ROLE nmock_replicator WITH LOGIN REPLICATION")
	}
	if !sawHBA || !sawRoles {
		t.Errorf("replication setup missing: pg_hba %v, roles %v", sawHBA, sawRoles)
	}
}

func TestPoolerConfiguration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	s := h.spec("nmock-pg-pg1-pooler")
	if s.Image != "edoburu/pgbouncer:v1.25.2-p0" || s.Network != "nmock-pg-pg1" {
		t.Errorf("pooler = %q on %q", s.Image, s.Network)
	}
	ini := string(fileOf(t, s, poolerINIPath).Content)
	for _, want := range []string{
		"* = host=pg-0 port=5432", "pool_mode = transaction", "default_pool_size = 20", "listen_port = 5432", "auth_type = scram-sha-256",
		"auth_user = nmock_pooler", "auth_query = SELECT usename, passwd FROM nmock_pooler.user_search($1)", "auth_dbname = postgres",
		"client_tls_sslmode = allow", "server_tls_sslmode = require", "client_tls_key_file = /etc/nmock/tls/server.key",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("pgbouncer.ini lacks %q:\n%s", want, ini)
		}
	}
	users := fileOf(t, s, poolerUserlistPath)
	if !strings.HasPrefix(string(users.Content), `"nmock_pooler" "`) || users.Mode != 0o600 || users.UID != 70 {
		t.Errorf("userlist = %q mode %o uid %d", users.Content, users.Mode, users.UID)
	}
	if key := fileOf(t, s, tlsKey); key.UID != 70 || key.Mode != 0o600 {
		t.Errorf("pooler TLS key = mode %o uid %d", key.Mode, key.UID)
	}
}

func TestEnsureIsIdempotentAndRecreatesOnSpecChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	h.ensure(c)
	ids := map[string]string{}
	for _, n := range h.names() {
		ids[n] = h.container(n).ID
	}

	h.ensure(c) // nothing changed
	for _, n := range h.names() {
		if h.container(n).ID != ids[n] {
			t.Errorf("%s was recreated without a change", n)
		}
	}

	c.Password = "new" // a password change is an ALTER ROLE, not a recreation
	h.ensure(c)
	for _, n := range h.names() {
		if h.container(n).ID != ids[n] {
			t.Errorf("%s was recreated by a password change", n)
		}
	}
	var alter bool
	for _, e := range h.f.Execs() {
		alter = alter || strings.Contains(strings.Join(e.Spec.Cmd, " "), `ALTER ROLE "app" PASSWORD 'new'`)
	}
	if !alter {
		t.Error("the password change ran no ALTER ROLE")
	}

	c.Parameters["statement_timeout"] = "1000ms"
	h.ensure(c)
	for _, n := range []string{"nmock-pg-pg1-0", "nmock-pg-pg1-1"} {
		if h.container(n).ID == ids[n] {
			t.Errorf("%s was not recreated after a parameter change", n)
		}
		if !strings.Contains(strings.Join(h.spec(n).Cmd, " "), "statement_timeout=1000ms") {
			t.Errorf("%s: new flags missing", n)
		}
	}
	if h.container("nmock-pg-pg1-pooler").ID != ids["nmock-pg-pg1-pooler"] {
		t.Error("the pooler was recreated by a PostgreSQL parameter change")
	}
	if got := h.f.Volumes(); !slices.Contains(got, "nmock-pg-pg1-0") || !slices.Contains(got, "nmock-pg-pg1-1") {
		t.Errorf("data volumes were lost: %v", got)
	}

	c.MemoryBytes = 16 << 30
	h.ensure(c)
	if h.container("nmock-pg-pg1-0").ID == ids["nmock-pg-pg1-0"] || h.spec("nmock-pg-pg1-0").Memory != 16<<30 {
		t.Error("a resources change did not recreate the host")
	}

	poolerID := h.container("nmock-pg-pg1-pooler").ID
	c.Pooler.MaxPoolSize = 50
	h.ensure(c)
	if h.container("nmock-pg-pg1-pooler").ID == poolerID {
		t.Error("the pooler was not recreated after its configuration changed")
	}
}

func TestStopAndStartKeepTheContainers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	h.ensure(c)
	ids := map[string]string{}
	for _, n := range h.names() {
		ids[n] = h.container(n).ID
	}
	if err := h.b.StopCluster(t.Context(), "pg1"); err != nil {
		t.Fatalf("StopCluster = %v", err)
	}
	for _, n := range h.names() {
		if h.container(n).Running {
			t.Errorf("%s still running after StopCluster", n)
		}
	}
	if err := h.b.StopCluster(t.Context(), "unknown"); err != nil {
		t.Errorf("StopCluster of an unknown cluster = %v", err)
	}
	h.ensure(c)
	for _, n := range h.names() {
		if got := h.container(n); !got.Running || got.ID != ids[n] {
			t.Errorf("%s after start: running %v, same container %v", n, got.Running, got.ID == ids[n])
		}
	}
}

func TestEndpointsStayStable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	first := h.ensure(c)
	if first.PrivateReadWrite == "" || first.PrivateReadOnly == "" {
		t.Fatalf("endpoints = %+v", first)
	}
	if err := h.b.StopCluster(t.Context(), "pg1"); err != nil {
		t.Fatal(err)
	}
	if got := h.ensure(c); got != first {
		t.Errorf("endpoints after stop/start = %+v, want %+v", got, first)
	}
	c.Parameters["statement_timeout"] = "5000ms"
	c.Pooler.MaxPoolSize = 7 // recreates hosts and the pooler
	if got := h.ensure(c); got != first {
		t.Errorf("endpoints after recreation = %+v, want %+v", got, first)
	}
	if h.container("nmock-pg-pg1-0").Labels[labelPort] == "" {
		t.Error("the pinned port is not recorded in a label")
	}
}

func TestScaleDownRemovesReplicas(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	c.Hosts = 3
	h.ensure(c)
	if len(h.names()) != 4 {
		t.Fatalf("containers = %v", h.names())
	}
	c.Hosts = 1
	ep := h.ensure(c)
	if want := []string{"nmock-pg-pg1-0", "nmock-pg-pg1-pooler"}; !slices.Equal(h.names(), want) {
		t.Errorf("containers = %v, want %v", h.names(), want)
	}
	if vols := h.f.Volumes(); slices.Contains(vols, "nmock-pg-pg1-1") || slices.Contains(vols, "nmock-pg-pg1-2") || !slices.Contains(vols, "nmock-pg-pg1-0") {
		t.Errorf("volumes = %v", vols)
	}
	if want := "pg.example.test:" + port(t, h.container("nmock-pg-pg1-0")); ep.PrivateReadOnly != want {
		t.Errorf("one host: read-only endpoint = %q, want the primary %q", ep.PrivateReadOnly, want)
	}
}

func TestPublicAccessRepeatsThePrivateEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	c.PublicAccess = true
	ep := h.ensure(c)
	if ep.PublicReadWrite != ep.PrivateReadWrite || ep.PublicReadOnly != ep.PrivateReadOnly || ep.PublicReadWrite == "" {
		t.Errorf("endpoints = %+v", ep)
	}
}

func TestDeleteClusterKeepsBackups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	h.ensure(testCluster("pg2"))
	if err := h.b.DeleteCluster(t.Context(), "pg1"); err != nil {
		t.Fatalf("DeleteCluster = %v", err)
	}
	if err := h.b.DeleteCluster(t.Context(), "pg1"); err != nil {
		t.Errorf("second DeleteCluster = %v", err)
	}
	for _, n := range h.names() {
		if strings.Contains(n, "pg1") {
			t.Errorf("%s survived DeleteCluster", n)
		}
	}
	for _, v := range h.f.Volumes() {
		if strings.Contains(v, "pg1") {
			t.Errorf("volume %s survived DeleteCluster", v)
		}
	}
	if !slices.Contains(h.f.Volumes(), "nmock-pg-backups-4566") {
		t.Error("DeleteCluster removed the backups volume")
	}
	if len(h.f.Networks()) != 1 {
		t.Errorf("networks = %v, want only pg2's", h.f.Networks())
	}
	if err := h.b.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if len(h.f.Containers())+len(h.f.Volumes())+len(h.f.Networks()) != 0 {
		t.Errorf("Close left %v %v %v", h.f.Containers(), h.f.Volumes(), h.f.Networks())
	}
}

func TestUnknownVersionIsBadRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg1")
	c.Version = "9"
	_, err := h.b.EnsureCluster(t.Context(), c)
	if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES") {
		t.Fatalf("EnsureCluster err = %v, want BadRequest naming the setting", err)
	}
}

func TestReadinessTimeoutIsInternalWithLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.SetExecHandler(func(_ string, e dockerhost.ExecSpec) dockerhost.ExecResult {
		if e.Cmd[0] == "pg_isready" {
			return dockerhost.ExecResult{ExitCode: 2, Stderr: "no response"}
		}
		return dockerhost.ExecResult{}
	})
	_, err := h.b.EnsureCluster(t.Context(), testCluster("pg1"))
	if !errs.IsKind(err, errs.KindInternal) || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("EnsureCluster err = %v, want Internal not ready", err)
	}
}

func TestStartFailureCarriesTheLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.SetAutoExit("nmock-pg-pg1-0", 1, "FATAL: could not open certificate file\n")
	_, err := h.b.EnsureCluster(t.Context(), testCluster("pg1"))
	if !errs.IsKind(err, errs.KindInternal) || !strings.Contains(err.Error(), "could not open certificate file") {
		t.Fatalf("EnsureCluster err = %v, want Internal with the log", err)
	}
}

func TestCreateBackupCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	info, err := h.b.CreateBackup(t.Context(), "pg1", "bk1")
	if err != nil {
		t.Fatalf("CreateBackup = %v", err)
	}
	if info.SizeBytes != 12345 {
		t.Errorf("size = %d, want 12345", info.SizeBytes)
	}
	calls := h.f.Execs()
	last := calls[len(calls)-1]
	if last.Container != "nmock-pg-pg1-0" || last.Spec.User != "postgres" {
		t.Errorf("backup ran in %s as %q, want the primary as postgres", last.Container, last.Spec.User)
	}
	args := strings.Join(last.Spec.Cmd, " ")
	if !strings.HasSuffix(args, "nmock /backups/pg1/base/bk1 app") || !strings.Contains(args, "pg_basebackup -U \"$2\" -D \"$1\" -Ft -z -X fetch") {
		t.Errorf("backup command = %q", args)
	}

	if err := h.b.StopCluster(t.Context(), "pg1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.CreateBackup(t.Context(), "pg1", "bk2"); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("CreateBackup of a stopped cluster err = %v, want BadState", err)
	}
}

func TestRestoreSeedsTheVolumeBeforeStarting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := testCluster("pg2")
	c.Hosts = 1
	at := time.Date(2026, 11, 26, 10, 30, 15, 123456000, time.UTC)
	c.Restore = &domain.RestoreSource{SourceClusterID: "pg1", BackupID: "bk1", RecoveryTime: at}
	h.ensure(c)

	helper := findHelper(t, h.f)
	script := helper.Entrypoint[2]
	for _, want := range []string{
		"S=/backups/pg1/base/bk1", "tar xzf \"$S/base.tar.gz\"", "touch recovery.signal", "cat /restore.conf >> postgresql.auto.conf", "chown -R 70:70",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("restore script lacks %q:\n%s", want, script)
		}
	}
	if !slices.Equal(helper.Binds, []string{"nmock-pg-pg2-0:/var/lib/postgresql/data", "nmock-pg-backups-4566:/backups"}) {
		t.Errorf("helper binds = %v", helper.Binds)
	}
	conf := string(fileOf(t, helper, "/restore.conf").Content)
	for _, want := range []string{
		"restore_command = 'cp /backups/pg1/wal/%f %p'", "recovery_target_time = '2026-11-26 10:30:15.123456+00'", "recovery_target_action = 'promote'",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("recovery settings lack %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "recovery_target = ") {
		t.Errorf("a time target must not also say immediate:\n%s", conf)
	}
	// The helper is gone and the new cluster archives to its own directory.
	for _, n := range h.names() {
		if strings.Contains(n, "helper") {
			t.Errorf("helper container %s was left behind", n)
		}
	}
	if !strings.Contains(strings.Join(h.spec("nmock-pg-pg2-0").Cmd, " "), "cp %p /backups/pg2/wal/%f") {
		t.Error("the restored cluster does not archive to its own WAL directory")
	}
	// The bootstrap role and database are ensured after the promotion.
	var role, db bool
	for _, e := range h.f.Execs() {
		joined := strings.Join(e.Spec.Cmd, " ")
		role = role || strings.Contains(joined, "CREATE ROLE \"app\" SUPERUSER LOGIN")
		db = db || strings.Contains(joined, "pg_database WHERE datname = 'appdb'")
	}
	if !role || !db {
		t.Errorf("bootstrap role ensured %v, database ensured %v", role, db)
	}

	// A second call finds the data volume and restores nothing again.
	before := len(h.f.Created())
	h.ensure(c)
	for _, s := range h.f.Created()[before:] {
		if strings.Contains(s.Name, "helper") {
			t.Errorf("the second EnsureCluster ran helper %s", s.Name)
		}
	}
}

func findHelper(t *testing.T, f *dockerfake.Fake) dockerhost.ContainerSpec {
	t.Helper()
	specs := f.Created()
	for i := range specs {
		if strings.HasPrefix(specs[i].Name, "nmock-pg-helper-") {
			return specs[i]
		}
	}
	t.Fatal("no helper container was created")
	return dockerhost.ContainerSpec{}
}

func TestRestoreWithoutTimeRecoversToTheBackupEnd(t *testing.T) {
	t.Parallel()
	conf := recoverySettings(&domain.RestoreSource{SourceClusterID: "a", BackupID: "b"})
	if !strings.Contains(conf, "recovery_target = 'immediate'") || strings.Contains(conf, "recovery_target_time") {
		t.Errorf("recovery settings = %q", conf)
	}
}

func TestFailedRestoreLeavesNoVolume(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.f.SetAutoExit("nmock-pg-helper-", 3, "backup /backups/pg1/base/bk1 does not exist\n")
	c := testCluster("pg2")
	c.Restore = &domain.RestoreSource{SourceClusterID: "pg1", BackupID: "bk1"}
	_, err := h.b.EnsureCluster(t.Context(), c)
	if !errs.IsKind(err, errs.KindInternal) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("EnsureCluster err = %v, want the helper's log", err)
	}
	if slices.Contains(h.f.Volumes(), "nmock-pg-pg2-0") {
		t.Error("a half-restored data volume was kept")
	}
}

func TestDeleteBackup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.b.DeleteBackup(t.Context(), "pg1", "bk1", time.Time{}); err != nil {
		t.Fatalf("DeleteBackup without any backups volume = %v", err)
	}
	if len(h.f.Created()) != 0 {
		t.Error("DeleteBackup started a container although no backup was ever taken")
	}
	h.ensure(testCluster("pg1"))
	before := len(h.f.Created())

	if err := h.b.DeleteBackup(t.Context(), "pg1", "bk1", time.Time{}); err != nil {
		t.Fatalf("DeleteBackup = %v", err)
	}
	s := h.f.Created()[before]
	if !strings.HasPrefix(s.Entrypoint[2], "set -- /backups/pg1/base/bk1 /backups/pg1/wal all\n") || !slices.Equal(s.Binds, []string{"nmock-pg-backups-4566:/backups"}) {
		t.Errorf("delete helper = %q binds %v", s.Entrypoint[2], s.Binds)
	}

	keep := clock.Real{}.Now().Add(-90 * time.Minute)
	if err := h.b.DeleteBackup(t.Context(), "pg1", "bk2", keep); err != nil {
		t.Fatalf("DeleteBackup = %v", err)
	}
	s = h.f.Created()[before+1]
	if !strings.HasPrefix(s.Entrypoint[2], "set -- /backups/pg1/base/bk2 /backups/pg1/wal 9") {
		t.Errorf("delete helper = %q, want ~90 minutes", s.Entrypoint[2])
	}
}

func TestArchiveAgeRoundsUp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		keep time.Time
		want int
	}{
		{now.Add(-90 * time.Minute), 90},
		{now.Add(-90*time.Minute - time.Second), 91},
		{now.Add(-time.Second), 1},
		{now.Add(time.Hour), 1},
	}
	for _, tt := range tests {
		if got := archiveAgeMinutes(now, tt.keep); got != tt.want {
			t.Errorf("archiveAgeMinutes(keep %v) = %d, want %d", tt.keep.Sub(now), got, tt.want)
		}
	}
}

func TestBackupsVolumeName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		":4566":          "nmock-pg-backups-4566",
		"0.0.0.0:4566":   "nmock-pg-backups-0-0-0-0-4566",
		"Host_A:4566":    "nmock-pg-backups-host-a-4566",
		"":               "nmock-pg-backups-default",
		"e2e-msp-17":     "nmock-pg-backups-e2e-msp-17",
		"[::1]:4566":     "nmock-pg-backups-1-4566",
		"/tmp/edge.sock": "nmock-pg-backups-tmp-edge-sock",
	}
	for owner, want := range tests {
		if got := backupsVolumeName(owner); got != want {
			t.Errorf("backupsVolumeName(%q) = %q, want %q", owner, got, want)
		}
	}
}

func TestSQLQuoting(t *testing.T) {
	t.Parallel()
	if got := sqlString(`it's`); got != `'it''s'` {
		t.Errorf("sqlString = %s", got)
	}
	if got := sqlIdent(`we"ird`); got != `"we""ird"` {
		t.Errorf("sqlIdent = %s", got)
	}
}
