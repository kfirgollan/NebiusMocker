//go:build e2e_msp

package docker_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
	mspdocker "github.com/kfirgollan/NebiusMocker/services/msppostgresql/backend/docker"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

const (
	e2eUser   = "app"
	e2eDB     = "appdb"
	clientImg = "postgres:16.11-alpine"
)

// e2eEnv is the real daemon, the backend under test and a client container
// that connects to the clusters the way a user's machine does: through the
// published ports, by host.docker.internal, verifying the mocker CA.
type e2eEnv struct {
	t      *testing.T
	clk    clock.Real
	docker *dockerhost.Client
	b      *mspdocker.Backend
	owner  string
	client string
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	ctx := t.Context()
	docker, err := dockerhost.New("")
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	clk := clock.Real{}
	dir := t.TempDir()
	ca, err := localca.LoadOrCreate(dir, clk)
	if err != nil {
		t.Fatalf("local CA: %v", err)
	}
	cfg := config.MSPConfig{
		BindIP: "127.0.0.1", EndpointHost: "host.docker.internal", TLSDir: dir, StartTimeout: 3 * time.Minute,
		PostgresImages: config.DefaultPostgresImages(), PgBouncerImage: "edoburu/pgbouncer:v1.25.2-p0", PostgresMaxHosts: 3,
	}
	owner := liveOwner(t)
	b, err := mspdocker.New(ctx, mspdocker.Options{
		Config: cfg, EdgeListen: owner, Docker: docker, Clock: clk, CA: ca,
		Log: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := &e2eEnv{t: t, clk: clk, docker: docker, b: b, owner: b.Owner()}
	t.Cleanup(func() {
		cctx := context.WithoutCancel(ctx)
		if err := b.Close(cctx); err != nil {
			t.Errorf("Close: %v", err)
		}
		if e.client != "" {
			_ = docker.RemoveContainer(cctx, e.client)
		}
		e.assertNoLeftovers(cctx)
	})
	if err := docker.EnsureImage(ctx, clientImg); err != nil {
		t.Fatal(err)
	}
	e.client, err = docker.CreateContainer(ctx, dockerhost.ContainerSpec{
		Name: "nmock-pg-e2e-client-" + strconv.FormatInt(clk.Now().UnixNano(), 10), Image: clientImg,
		Entrypoint: []string{"sleep", "3600"}, ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Labels: map[string]string{"nebius-mocker.e2e": owner},
		Files:  []dockerhost.File{{Path: "/ca.pem", Content: ca.CertPEM()}},
	})
	if err != nil {
		t.Fatalf("client container: %v", err)
	}
	if err := docker.StartContainer(ctx, e.client); err != nil {
		t.Fatal(err)
	}
	return e
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (e *e2eEnv) assertNoLeftovers(ctx context.Context) {
	e.t.Helper()
	labels := map[string]string{mspdocker.LabelBackend: "msp-postgresql", mspdocker.LabelOwner: e.owner}
	cs, _ := e.docker.ListContainers(ctx, labels)
	vs, _ := e.docker.ListVolumes(ctx, labels)
	ns, _ := e.docker.ListNetworks(ctx, labels)
	if len(cs)+len(vs)+len(ns) != 0 {
		e.t.Errorf("leftovers: %d containers %v, volumes %v, %d networks", len(cs), cs, vs, len(ns))
	}
}

// sql runs query through psql in the client container against endpoint.
func (e *e2eEnv) sql(endpoint, password, query string) (string, error) {
	e.t.Helper()
	host, port, _ := strings.Cut(endpoint, ":")
	conn := fmt.Sprintf("host=%s port=%s sslmode=verify-full sslrootcert=/ca.pem user=%s dbname=%s connect_timeout=10", host, port, e2eUser, e2eDB)
	res, err := e.docker.Exec(e.t.Context(), e.client, dockerhost.ExecSpec{
		Cmd: []string{"psql", "-X", "-tA", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=verbose", conn, "-c", query}, Env: []string{"PGPASSWORD=" + password},
	})
	if err != nil {
		e.t.Fatalf("exec psql: %v", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("psql exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// mustSQL is sql that must succeed, retrying while the cluster settles.
func (e *e2eEnv) mustSQL(endpoint, password, query string) string {
	e.t.Helper()
	out, err := e.sql(endpoint, password, query)
	if err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return out
}

// eventually polls f until it returns want.
func (e *e2eEnv) eventually(what, want string, f func() (string, error)) {
	e.t.Helper()
	deadline := e.clk.Now().Add(60 * time.Second)
	var got string
	var err error
	for e.clk.Now().Before(deadline) {
		if got, err = f(); err == nil && got == want {
			return
		}
		<-e.clk.After(time.Second)
	}
	e.t.Fatalf("%s: got %q (err %v), want %q", what, got, err, want)
}

func (e *e2eEnv) ensure(c domain.BackendCluster) domain.Endpoints {
	e.t.Helper()
	ep, err := e.b.EnsureCluster(e.t.Context(), c)
	if err != nil {
		e.t.Fatalf("EnsureCluster(%s): %v", c.ID, err)
	}
	return ep
}

func TestE2EPostgresLifecycle(t *testing.T) {
	e := newE2EEnv(t)
	ctx := t.Context()
	a := domain.BackendCluster{
		ID: "postgresql-e2ea", ProjectID: "project-x", Name: "a", Version: "16", Hosts: 2,
		CPUs: 2, MemoryBytes: 2 << 30, DiskGiB: 10,
		Parameters: map[string]string{"statement_timeout": "30000ms", "max_connections": "150"},
		Pooler:     domain.Pooler{Mode: "transaction", MaxPoolSize: 10},
		UserName:   e2eUser, DBName: e2eDB, Password: "pw-one",
	}
	ep := e.ensure(a)
	a.Password = ""
	t.Logf("endpoints: %+v", ep)
	if ep.PublicReadWrite != "" {
		t.Errorf("PublicReadWrite = %q without public access", ep.PublicReadWrite)
	}

	// Through the pooler, with certificate verification.
	e.mustSQL(ep.PrivateReadWrite, "pw-one", "create table t (id int primary key)")
	e.mustSQL(ep.PrivateReadWrite, "pw-one", "insert into t select generate_series(1, 3)")
	if got := e.mustSQL(ep.PrivateReadWrite, "pw-one", "show statement_timeout"); got != "30s" {
		t.Errorf("statement_timeout = %q, want 30s", got)
	}
	if got := e.mustSQL(ep.PrivateReadWrite, "pw-one", "show max_connections"); got != "150" {
		t.Errorf("max_connections = %q, want 150", got)
	}

	// A client that does not verify (or even use) TLS connects as well.
	host, p, _ := strings.Cut(ep.PrivateReadWrite, ":")
	res, err := e.docker.Exec(ctx, e.client, dockerhost.ExecSpec{
		Cmd: []string{"psql", "-X", "-tA", fmt.Sprintf("host=%s port=%s sslmode=prefer user=%s dbname=%s", host, p, e2eUser, e2eDB), "-c", "select 1"},
		Env: []string{"PGPASSWORD=pw-one"},
	})
	if err != nil || res.ExitCode != 0 {
		t.Errorf("sslmode=prefer through the pooler: %v %+v", err, res)
	}

	// The read-only endpoint is a streaming replica.
	e.eventually("replica rows", "3", func() (string, error) {
		return e.sql(ep.PrivateReadOnly, "pw-one", "select count(*) from t")
	})
	if got := e.mustSQL(ep.PrivateReadOnly, "pw-one", "select pg_is_in_recovery()"); got != "t" {
		t.Errorf("read-only endpoint pg_is_in_recovery = %q, want t", got)
	}
	if _, err := e.sql(ep.PrivateReadOnly, "pw-one", "insert into t values (100)"); err == nil {
		t.Error("insert on the replica succeeded")
	}

	// Password change.
	a.Password = "pw-two"
	ep = e.ensure(a)
	a.Password = ""
	e.mustSQL(ep.PrivateReadWrite, "pw-two", "select 1")
	if _, err := e.sql(ep.PrivateReadWrite, "pw-one", "select 1"); err == nil {
		t.Error("the old password still works through the pooler")
	}

	// Stop and start keep the data.
	if err := e.b.StopCluster(ctx, a.ID); err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	if _, err := e.sql(ep.PrivateReadWrite, "pw-two", "select 1"); err == nil {
		t.Error("a stopped cluster accepts connections")
	}
	before := ep
	ep = e.ensure(a)
	if ep != before {
		t.Errorf("endpoints changed across stop/start: %+v -> %+v", before, ep)
	}
	if got := e.mustSQL(ep.PrivateReadWrite, "pw-two", "select count(*) from t"); got != "3" {
		t.Errorf("rows after restart = %q, want 3", got)
	}

	// Changing a parameter recreates the hosts and keeps the data.
	a.Parameters["statement_timeout"] = "45000ms"
	before = ep
	ep = e.ensure(a)
	if ep != before {
		t.Errorf("endpoints changed after a parameter change: %+v -> %+v", before, ep)
	}
	if got := e.mustSQL(ep.PrivateReadWrite, "pw-two", "show statement_timeout"); got != "45s" {
		t.Errorf("statement_timeout after change = %q, want 45s", got)
	}
	if got := e.mustSQL(ep.PrivateReadWrite, "pw-two", "select count(*) from t"); got != "3" {
		t.Errorf("rows after recreate = %q, want 3", got)
	}

	// Backup, more data, restore to a point in between.
	info, err := e.b.CreateBackup(ctx, a.ID, "bk1")
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if info.SizeBytes <= 0 {
		t.Errorf("backup size = %d", info.SizeBytes)
	}
	rw := ep.PrivateReadWrite
	e.mustSQL(rw, "pw-two", "insert into t select generate_series(4, 6)")
	<-e.clk.After(2 * time.Second)
	target := e.mustSQL(rw, "pw-two", `select to_char(clock_timestamp() at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`)
	recoveryTime, err := time.Parse(time.RFC3339Nano, target)
	if err != nil {
		t.Fatalf("parse recovery time %q: %v", target, err)
	}
	<-e.clk.After(2 * time.Second)
	e.mustSQL(rw, "pw-two", "insert into t select generate_series(7, 9)")

	b := a
	b.ID, b.Name, b.Hosts, b.Password = "postgresql-e2eb", "b", 1, "pw-three"
	b.Restore = &domain.RestoreSource{SourceClusterID: a.ID, BackupID: "bk1", RecoveryTime: recoveryTime}
	epB := e.ensure(b)
	b.Password = ""
	if got := e.mustSQL(epB.PrivateReadWrite, "pw-three", "select count(*) || '/' || max(id) from t"); got != "6/6" {
		t.Errorf("restored rows count/max = %q, want 6/6 (point in time between the inserts)", got)
	}
	e.mustSQL(epB.PrivateReadWrite, "pw-three", "insert into t values (1000)")
	if got := e.mustSQL(epB.PrivateReadOnly, "pw-three", "select pg_is_in_recovery()"); got != "f" {
		t.Errorf("restored single host read-only endpoint is the primary, pg_is_in_recovery = %q", got)
	}
	// The source is untouched.
	if got := e.mustSQL(rw, "pw-two", "select count(*) from t"); got != "9" {
		t.Errorf("source rows = %q, want 9", got)
	}

	// Scale the source down to one host.
	a.Hosts = 1
	ep = e.ensure(a)
	if got := e.mustSQL(ep.PrivateReadOnly, "pw-two", "select pg_is_in_recovery()"); got != "f" {
		t.Errorf("one host: read-only endpoint pg_is_in_recovery = %q, want f (the primary)", got)
	}

	// Delete everything.
	if err := e.b.DeleteCluster(ctx, b.ID); err != nil {
		t.Fatalf("DeleteCluster(b): %v", err)
	}
	if err := e.b.DeleteCluster(ctx, a.ID); err != nil {
		t.Fatalf("DeleteCluster(a): %v", err)
	}
	if err := e.b.DeleteBackup(ctx, a.ID, "bk1", time.Time{}); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if err := e.b.DeleteCluster(ctx, a.ID); err != nil {
		t.Errorf("second DeleteCluster: %v", err)
	}
	cs, _ := e.docker.ListContainers(ctx, map[string]string{mspdocker.LabelBackend: "msp-postgresql", mspdocker.LabelOwner: e.owner})
	if len(cs) != 0 {
		t.Errorf("containers left after deleting the clusters: %v", cs)
	}
}

// TestE2EDiskFull: a cluster over its disk limit refuses writes (SQLSTATE
// 25006) through the pooler and directly, keeps serving reads, and accepts
// writes again once the limit is raised.
func TestE2EDiskFull(t *testing.T) {
	e := newE2EEnv(t)
	ctx := t.Context()
	c := domain.BackendCluster{
		ID: "postgresql-e2ed", ProjectID: "project-x", Name: "d", Version: "16", Hosts: 1,
		CPUs: 2, MemoryBytes: 2 << 30, DiskGiB: 10,
		Pooler:   domain.Pooler{Mode: "transaction", MaxPoolSize: 10},
		UserName: e2eUser, DBName: e2eDB, Password: "pw-disk",
	}
	ep := e.ensure(c)
	c.Password = ""
	rw, direct := ep.PrivateReadWrite, ep.PrivateReadOnly // one host: the read-only endpoint is the primary itself
	e.mustSQL(rw, "pw-disk", "create table t (id int, filler text)")

	base, err := e.b.CheckDisk(ctx, c.ID, 1<<40)
	if err != nil || base.Full || base.UsedBytes <= 0 {
		t.Fatalf("CheckDisk with a huge limit = %+v, %v", base, err)
	}
	limit := base.UsedBytes + 30<<20
	if u, err := e.b.CheckDisk(ctx, c.ID, limit); err != nil || u.Full {
		t.Fatalf("CheckDisk below the limit = %+v, %v", u, err)
	}
	e.mustSQL(rw, "pw-disk", "insert into t select g, repeat('x', 200) from generate_series(1, 400000) g")

	u, err := e.b.CheckDisk(ctx, c.ID, limit)
	if err != nil || !u.Full || u.UsedBytes < limit {
		t.Fatalf("CheckDisk over the limit = %+v, %v; want full", u, err)
	}
	for name, endpoint := range map[string]string{"through the pooler": rw, "directly": direct} {
		_, err := e.sql(endpoint, "pw-disk", "insert into t values (-1, 'refused')")
		if err == nil || !strings.Contains(err.Error(), "25006") || !strings.Contains(err.Error(), "read-only transaction") {
			t.Errorf("write %s on a full disk: err = %v, want SQLSTATE 25006 read-only transaction", name, err)
		}
		if got := e.mustSQL(endpoint, "pw-disk", "select count(*) from t"); got != "400000" {
			t.Errorf("read %s on a full disk = %q, want 400000", name, got)
		}
	}
	// Checking again changes nothing, and the control plane's own statements
	// (a password change) still work on a full cluster.
	if u2, err := e.b.CheckDisk(ctx, c.ID, limit); err != nil || !u2.Full {
		t.Errorf("second CheckDisk = %+v, %v", u2, err)
	}
	c.Password = "pw-disk-2"
	e.ensure(c)
	c.Password = ""
	if _, err := e.sql(rw, "pw-disk-2", "insert into t values (-2, 'still refused')"); err == nil {
		t.Error("a write succeeded after EnsureCluster on a full disk")
	}

	// A bigger disk: writes work again.
	if u, err := e.b.CheckDisk(ctx, c.ID, limit*20); err != nil || u.Full {
		t.Fatalf("CheckDisk with a bigger limit = %+v, %v", u, err)
	}
	for _, endpoint := range []string{rw, direct} {
		e.mustSQL(endpoint, "pw-disk-2", "insert into t values (1000000, 'accepted')")
	}
}

// liveOwner plays the mocker's edge for the owner label: a listener that
// stays open for the test, so reapers (deploy/docker/reap.sh) see the owner
// alive and leave a concurrently running test's objects alone.
func liveOwner(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}
