package docker

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// labelUser names the cluster's superuser on its host containers, so that
// stop, delete and backup, which are not given the cluster, can connect.
const labelUser = "nebius-mocker.user"

// EnsureCluster implements domain.Backend.
func (b *Backend) EnsureCluster(ctx context.Context, c domain.BackendCluster) (domain.Endpoints, error) {
	image, ok := b.cfg.PostgresImages[c.Version]
	if !ok {
		return domain.Endpoints{}, errs.BadRequest(errs.Violation{
			Field:   "spec.config.version",
			Message: fmt.Sprintf("the docker backend has no PostgreSQL image for version %s (NEBIUS_MOCKER_MSP_POSTGRESQL_IMAGES)", c.Version),
		})
	}
	hosts := max(c.Hosts, 1)
	for _, ref := range []string{image, b.cfg.PgBouncerImage} {
		if err := b.docker.EnsureImage(ctx, ref); err != nil {
			return domain.Endpoints{}, internal("pull image "+ref, err)
		}
	}
	if _, err := b.docker.EnsureNetwork(ctx, networkName(c.ID), b.clusterLabels(c.ID)); err != nil {
		return domain.Endpoints{}, internal("create network", err)
	}
	if err := b.docker.EnsureVolume(ctx, b.backups, b.baseLabels()); err != nil {
		return domain.Endpoints{}, internal("create backups volume", err)
	}
	if c.Restore != nil {
		if err := b.restoreData(ctx, c, image); err != nil {
			return domain.Endpoints{}, err
		}
	}
	pki := &certs{b: b, clusterID: c.ID}

	primary, err := b.ensureHost(ctx, c, 0, image, pki)
	if err != nil {
		return domain.Endpoints{}, err
	}
	if err := b.setupPrimary(ctx, c, primary); err != nil {
		return domain.Endpoints{}, err
	}
	for n := 1; n < hosts; n++ {
		if _, err := b.ensureHost(ctx, c, n, image, pki); err != nil {
			return domain.Endpoints{}, err
		}
	}
	if err := b.removeHostsFrom(ctx, c.ID, hosts); err != nil {
		return domain.Endpoints{}, err
	}
	pooler, err := b.ensurePooler(ctx, c, pki)
	if err != nil {
		return domain.Endpoints{}, err
	}
	return b.endpoints(ctx, c, hosts, pooler)
}

// certs issues the cluster's server certificate at most once per call.
type certs struct {
	b         *Backend
	clusterID string
	files     []dockerhost.File
}

// get returns the certificate and key as files of a container.
func (p *certs) get() ([]dockerhost.File, error) {
	if p.files != nil {
		return p.files, nil
	}
	hosts := []string{p.b.cfg.EndpointHost, "localhost", "127.0.0.1", "host.docker.internal", poolerName(p.clusterID)}
	for n := range max(p.b.cfg.PostgresMaxHosts, 1) {
		hosts = append(hosts, hostAlias(n))
	}
	cert, err := p.b.ca.Issue(hosts...)
	if err != nil {
		return nil, internal("issue server certificate", err)
	}
	var chain []byte
	for _, der := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, internal("encode server key", err)
	}
	// postgres (uid 70) owns the key: PostgreSQL insists on mode 0600 and an
	// owner it runs as.
	p.files = []dockerhost.File{
		{Path: tlsCert, Mode: 0o644, UID: pgUID, GID: pgUID, Content: chain},
		{Path: tlsKey, Mode: 0o600, UID: pgUID, GID: pgUID, Content: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
	}
	return p.files, nil
}

// superuser is the role that administers the cluster: the bootstrap user, or
// for a cluster restored from a backup the superuser the backup was taken
// with (the bootstrap user is created in it after the restore).
func (b *Backend) superuser(ctx context.Context, c domain.BackendCluster, primaryID string) string {
	if c.Restore == nil {
		return c.UserName
	}
	out, err := b.run(ctx, primaryID, "cat", superuserFile)
	if err != nil || out == "" {
		return c.UserName
	}
	return out
}

// hostSpec is the container a host runs in.
type hostSpec struct {
	spec dockerhost.ContainerSpec
	hash string
}

// newHostSpec builds host n's container spec without its certificate files
// (added when the container is created), and the hash that says when the
// container must be recreated.
func (b *Backend) newHostSpec(ctx context.Context, c domain.BackendCluster, n int, image string) hostSpec {
	flags := postgresFlags(c.ID, c.Parameters)
	script := hostScript(c.ID, n)
	nano := c.CPUs * 1_000_000_000
	if info, err := b.docker.Info(ctx); err == nil && info.NCPU > 0 {
		nano = min(nano, int64(info.NCPU)*1_000_000_000)
	}
	labels := b.clusterLabels(c.ID)
	labels[LabelRole], labels[LabelHost], labels[labelUser] = roleHost, strconv.Itoa(n), c.UserName
	env := []string{"POSTGRES_USER=" + c.UserName, "POSTGRES_DB=" + c.DBName, "POSTGRES_PASSWORD=" + b.bootPassword(c)}
	if n > 0 {
		env = []string{"PGPASSWORD=" + b.replicationPassword(c.ID)}
	}
	return hostSpec{
		spec: dockerhost.ContainerSpec{
			Name: hostName(c.ID, n), Image: image, Labels: labels, Env: env,
			Entrypoint: []string{"sh", "-c", script, "nmock-pg"}, Cmd: flags,
			Network: networkName(c.ID), Aliases: []string{hostAlias(n)},
			Ports:  []dockerhost.PortBinding{{ContainerPort: pgPort, HostIP: b.cfg.BindIP}},
			Memory: c.MemoryBytes, NanoCPUs: nano,
			Binds: []string{volumeName(c.ID, n) + ":" + pgData, b.backups + ":" + backupsDir},
		},
		hash: hash(map[string]any{
			"image": image, "flags": flags, "script": script, "memory": c.MemoryBytes, "nano": nano, "endpoint": b.cfg.EndpointHost,
		}),
	}
}

// bootPassword is the password of the bootstrap user when the data directory
// is initialized: the requested one, else one nobody knows (the owner sets
// one by updating spec.bootstrap.user_password).
func (b *Backend) bootPassword(c domain.BackendCluster) string {
	if c.Password != "" {
		return c.Password
	}
	return b.mac("bootstrap/" + c.ID)
}

func (b *Backend) replicationPassword(clusterID string) string {
	return b.mac("replication/" + clusterID)
}
func (b *Backend) poolerPassword(clusterID string) string { return b.mac("pooler/" + clusterID) }

// ensureHost creates host n, or brings the existing one to the spec
// (recreating it, with its volume, if the spec changed), and waits until
// PostgreSQL in it is ready. It returns the container ID.
func (b *Backend) ensureHost(ctx context.Context, c domain.BackendCluster, n int, image string, pki *certs) (string, error) {
	hs := b.newHostSpec(ctx, c, n, image)
	id, err := b.converge(ctx, hs, func(spec *dockerhost.ContainerSpec) error {
		files, err := pki.get()
		if err != nil {
			return err
		}
		spec.Files = files
		return b.docker.EnsureVolume(ctx, volumeName(c.ID, n), spec.Labels)
	})
	if err != nil {
		return "", err
	}
	user, want := c.UserName, accepting
	if n == 0 {
		user = b.superuser(ctx, c, id)
		if c.Restore != nil {
			want = promoted
		}
	}
	if err := b.waitPostgres(ctx, id, hs.spec.Name, user, want); err != nil {
		return "", err
	}
	return id, nil
}

// converge makes the container of hs exist, run and carry hs's hash:
// created if missing, started if stopped, recreated if its hash differs.
// prepare runs before a container is created.
func (b *Backend) converge(ctx context.Context, hs hostSpec, prepare func(*dockerhost.ContainerSpec) error) (string, error) {
	cur, err := b.docker.InspectContainer(ctx, hs.spec.Name)
	pinned := ""
	switch {
	case err == nil && cur.Labels[labelSpec] == hs.hash:
		if !cur.Running {
			if err := b.docker.StartContainer(ctx, cur.ID); err != nil {
				return "", internal("start "+hs.spec.Name, err)
			}
		}
		return cur.ID, nil
	case err == nil:
		pinned = cur.Labels[labelPort]
		b.log.InfoContext(ctx, "msp postgresql: recreating container, its spec changed", "container", hs.spec.Name)
		if err := b.removeContainer(ctx, cur); err != nil {
			return "", err
		}
	case !errors.Is(err, dockerhost.ErrNotFound):
		return "", internal("inspect "+hs.spec.Name, err)
	}
	spec := hs.spec
	spec.Labels = map[string]string{labelSpec: hs.hash}
	for k, v := range hs.spec.Labels {
		spec.Labels[k] = v
	}
	if len(spec.Ports) > 0 {
		// Pin the published port for the container's life, and across its
		// recreation, so connection strings stay valid.
		if pinned == "" {
			if pinned, err = dockerhost.FreePort(ctx, b.cfg.BindIP); err != nil {
				return "", internal("pick a host port", err)
			}
		}
		spec.Ports = slices.Clone(spec.Ports)
		spec.Ports[0].HostPort = pinned
		spec.Labels[labelPort] = pinned
	}
	if err := prepare(&spec); err != nil {
		return "", err
	}
	id, err := b.docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", internal("create "+spec.Name, err)
	}
	if err := b.docker.StartContainer(ctx, id); err != nil {
		return "", internal("start "+spec.Name, err)
	}
	return id, nil
}

// labelPort holds the host port a container's port is pinned to.
const labelPort = "nebius-mocker.port"

// removeContainer stops a container gracefully (PostgreSQL gets to shut
// down) and removes it; its volumes stay.
func (b *Backend) removeContainer(ctx context.Context, c dockerhost.Container) error {
	if c.Running {
		if err := b.docker.StopContainer(ctx, c.ID, stopTimeout); err != nil && !errors.Is(err, dockerhost.ErrNotFound) {
			return internal("stop "+c.Name, err)
		}
	}
	if err := b.docker.RemoveContainer(ctx, c.ID); err != nil {
		return internal("remove "+c.Name, err)
	}
	return nil
}

// removeHostsFrom removes the hosts numbered first and above, with their
// data volumes (scale down).
func (b *Backend) removeHostsFrom(ctx context.Context, clusterID string, first int) error {
	cs, err := b.docker.ListContainers(ctx, map[string]string{LabelBackend: backendLabel, LabelOwner: b.owner, LabelCluster: clusterID, LabelRole: roleHost})
	if err != nil {
		return internal("list hosts", err)
	}
	for _, c := range cs {
		n, err := strconv.Atoi(c.Labels[LabelHost])
		if err != nil || n < first {
			continue
		}
		if err := b.removeContainer(ctx, c); err != nil {
			return err
		}
		if err := b.docker.RemoveVolume(ctx, volumeName(clusterID, n)); err != nil {
			return internal("remove volume of "+c.Name, err)
		}
	}
	return nil
}

// setupPrimary applies what lives in the database: the pooler's and the
// replication roles, the pooler's lookup function, the replication entry in
// pg_hba.conf, and, for a restored cluster, the bootstrap role and database.
// Every step is idempotent.
func (b *Backend) setupPrimary(ctx context.Context, c domain.BackendCluster, id string) error {
	admin := b.superuser(ctx, c, id)
	if c.Restore != nil {
		if err := b.ensureBootstrap(ctx, c, id, admin); err != nil {
			return err
		}
		admin = c.UserName
	}
	if _, err := b.psql(ctx, id, admin, rolesSQL(b.replicationPassword(c.ID), b.poolerPassword(c.ID))); err != nil {
		return internal("create roles", err)
	}
	if _, err := b.sh(ctx, id, hbaScript, replicationRole); err != nil {
		return internal("allow replication in pg_hba.conf", err)
	}
	if c.Password != "" {
		if _, err := b.psql(ctx, id, admin, "ALTER ROLE "+sqlIdent(c.UserName)+" PASSWORD "+sqlString(c.Password)); err != nil {
			return internal("set the password of "+c.UserName, err)
		}
	}
	return nil
}

// ensureBootstrap creates the bootstrap role and database in a restored
// cluster if the backup lacks them.
func (b *Backend) ensureBootstrap(ctx context.Context, c domain.BackendCluster, id, admin string) error {
	role := fmt.Sprintf(`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s SUPERUSER LOGIN; END IF; END $$`,
		sqlString(c.UserName), sqlIdent(c.UserName))
	if _, err := b.psql(ctx, id, admin, role); err != nil {
		return internal("create bootstrap role", err)
	}
	have, err := b.psql(ctx, id, admin, "SELECT 1 FROM pg_database WHERE datname = "+sqlString(c.DBName))
	if err != nil {
		return internal("look up bootstrap database", err)
	}
	if have == "" {
		if _, err := b.psql(ctx, id, admin, "CREATE DATABASE "+sqlIdent(c.DBName)+" OWNER "+sqlIdent(c.UserName)); err != nil {
			return internal("create bootstrap database", err)
		}
	}
	return nil
}

// hbaScript ($1: replication role) lets the role replicate from any host and
// reloads the configuration, once.
const hbaScript = `grep -q "$1" "$PGDATA/pg_hba.conf" && exit 0
echo "host replication $1 all scram-sha-256" >> "$PGDATA/pg_hba.conf"
psql -X -q -U "$POSTGRES_USER" -d postgres -c 'select pg_reload_conf()'
`

// rolesSQL creates the replication role, the pooler's role and its lookup
// function (CNPG style: PgBouncer calls it with auth_query, so it learns
// users and password changes without a restart).
func rolesSQL(replicationPassword, poolerPassword string) string {
	return fmt.Sprintf(`
DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s WITH LOGIN REPLICATION; END IF; END $$;
ALTER ROLE %[1]s PASSWORD '%[2]s';
DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[3]s') THEN CREATE ROLE %[3]s WITH LOGIN; END IF; END $$;
ALTER ROLE %[3]s PASSWORD '%[4]s';
CREATE SCHEMA IF NOT EXISTS %[3]s;
CREATE OR REPLACE FUNCTION %[3]s.user_search(uname text) RETURNS TABLE(usename name, passwd text)
  LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog
  AS $f$ SELECT usename, passwd FROM pg_catalog.pg_shadow WHERE usename = uname $f$;
REVOKE ALL ON FUNCTION %[3]s.user_search(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA %[3]s TO %[3]s;
GRANT EXECUTE ON FUNCTION %[3]s.user_search(text) TO %[3]s;
`, replicationRole, replicationPassword, poolerRole, poolerPassword)
}

// endpoints reads the published ports back from Docker.
func (b *Backend) endpoints(ctx context.Context, c domain.BackendCluster, hosts int, poolerID string) (domain.Endpoints, error) {
	rw, err := b.hostPort(ctx, poolerID)
	if err != nil {
		return domain.Endpoints{}, err
	}
	target := hostName(c.ID, 0)
	if hosts > 1 {
		target = hostName(c.ID, 1)
	}
	ro, err := b.hostPort(ctx, target)
	if err != nil {
		return domain.Endpoints{}, err
	}
	e := domain.Endpoints{
		PrivateReadWrite: net.JoinHostPort(b.cfg.EndpointHost, rw),
		PrivateReadOnly:  net.JoinHostPort(b.cfg.EndpointHost, ro),
	}
	if c.PublicAccess {
		e.PublicReadWrite, e.PublicReadOnly = e.PrivateReadWrite, e.PrivateReadOnly
	}
	return e, nil
}

// hostPort is the host port Docker published for the container's PostgreSQL
// port.
func (b *Backend) hostPort(ctx context.Context, idOrName string) (string, error) {
	ctr, err := b.docker.InspectContainer(ctx, idOrName)
	if err != nil {
		return "", internal("inspect "+idOrName, err)
	}
	for _, pb := range ctr.Ports[pgPort] {
		if _, err := strconv.Atoi(pb.HostPort); err == nil {
			return pb.HostPort, nil
		}
	}
	return "", errs.Internal(fmt.Sprintf("container %s publishes no %s port", ctr.Name, pgPort))
}

// StopCluster implements domain.Backend.
func (b *Backend) StopCluster(ctx context.Context, clusterID string) error {
	cs, err := b.docker.ListContainers(ctx, b.clusterLabels(clusterID))
	if err != nil {
		return internal("list containers", err)
	}
	b.flushWAL(ctx, clusterID)
	// Pooler and replicas first, the primary last.
	for _, role := range []string{rolePooler, roleHost} {
		for i := len(cs) - 1; i >= 0; i-- {
			c := cs[i]
			if c.Labels[LabelRole] != role || !c.Running {
				continue
			}
			if err := b.docker.StopContainer(ctx, c.ID, stopTimeout); err != nil && !errors.Is(err, dockerhost.ErrNotFound) {
				return internal("stop "+c.Name, err)
			}
		}
	}
	return nil
}

// DeleteCluster implements domain.Backend.
func (b *Backend) DeleteCluster(ctx context.Context, clusterID string) error {
	b.flushWAL(ctx, clusterID)
	return b.removeLabeled(ctx, b.clusterLabels(clusterID))
}
