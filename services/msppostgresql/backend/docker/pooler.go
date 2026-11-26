package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

const (
	poolerINIPath      = "/etc/pgbouncer/pgbouncer.ini"
	poolerUserlistPath = "/etc/pgbouncer/userlist.txt"
	poolerMaxClients   = 1000
)

// poolerINI is pgbouncer.ini: every database goes to the primary (pg-0);
// clients authenticate by auth_query, with auth_user's own login from the
// userlist. Client TLS is "allow": a client that asks for TLS (sslmode=
// verify-full) gets it, one that does not (sslmode=prefer falling back) still
// connects. The pooler requires TLS to PostgreSQL.
func poolerINI(c domain.BackendCluster) string {
	var sb strings.Builder
	sb.WriteString("[databases]\n* = host=pg-0 port=5432\n\n[pgbouncer]\n")
	fmt.Fprintf(&sb, `listen_addr = 0.0.0.0
listen_port = 5432
auth_type = scram-sha-256
auth_file = %s
auth_user = %s
auth_dbname = postgres
auth_query = SELECT usename, passwd FROM %s.user_search($1)
pool_mode = %s
default_pool_size = %d
max_client_conn = %d
ignore_startup_parameters = extra_float_digits,options
client_tls_sslmode = allow
client_tls_key_file = %s
client_tls_cert_file = %s
server_tls_sslmode = require
`, poolerUserlistPath, poolerRole, poolerRole, c.Pooler.Mode, c.Pooler.MaxPoolSize, poolerMaxClients, tlsKey, tlsCert)
	return sb.String()
}

// ensurePooler creates the pooler, or recreates it when its configuration
// changed, and waits until it accepts connections. It returns the container ID.
func (b *Backend) ensurePooler(ctx context.Context, c domain.BackendCluster, pki *certs) (string, error) {
	ini := poolerINI(c)
	userlist := fmt.Sprintf("%q %q\n", poolerRole, b.poolerPassword(c.ID))
	labels := b.clusterLabels(c.ID)
	labels[LabelRole] = rolePooler
	hs := hostSpec{
		spec: dockerhost.ContainerSpec{
			Name: poolerName(c.ID), Image: b.cfg.PgBouncerImage, Labels: labels,
			Network: networkName(c.ID), Aliases: []string{"pooler"},
			Ports: []dockerhost.PortBinding{{ContainerPort: pgPort, HostIP: b.cfg.BindIP}},
			Files: []dockerhost.File{
				{Path: poolerINIPath, Mode: 0o644, UID: pgUID, GID: pgUID, Content: []byte(ini)},
				{Path: poolerUserlistPath, Mode: 0o600, UID: pgUID, GID: pgUID, Content: []byte(userlist)},
			},
		},
		hash: hash(map[string]any{"image": b.cfg.PgBouncerImage, "ini": ini, "userlist": userlist, "endpoint": b.cfg.EndpointHost}),
	}
	id, err := b.converge(ctx, hs, func(spec *dockerhost.ContainerSpec) error {
		files, err := pki.get()
		if err != nil {
			return err
		}
		spec.Files = append(spec.Files, files...)
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := b.waitPooler(ctx, id, hs.spec.Name); err != nil {
		return "", err
	}
	return id, nil
}
