package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Roles the backend adds to the cluster, next to the bootstrap user.
const (
	replicationRole = "nmock_replicator"
	poolerRole      = "nmock_pooler"
)

// In-container paths.
const (
	pgData     = "/var/lib/postgresql/data"
	tlsDir     = "/etc/nmock/tls"
	tlsCert    = tlsDir + "/server.crt"
	tlsKey     = tlsDir + "/server.key"
	backupsDir = "/backups"
	// superuserFile in the data directory of a restored cluster names the
	// superuser of the backup (the bootstrap user of the source cluster).
	superuserFile = pgData + "/nmock.superuser"
)

// Every Docker object name carries the owner tag, so mockers that share a
// Docker host (and sequential cluster IDs) never collide.
func (b *Backend) base(id string) string        { return "nmock-pg-" + b.tag + "-" + id }
func (b *Backend) networkName(id string) string { return b.base(id) }
func (b *Backend) hostName(id string, n int) string {
	return b.base(id) + "-" + strconv.Itoa(n)
}
func (b *Backend) volumeName(id string, n int) string { return b.hostName(id, n) }
func hostAlias(n int) string                          { return "pg-" + strconv.Itoa(n) }
func (b *Backend) poolerName(id string) string        { return b.base(id) + "-pooler" }

func walDir(id string) string              { return backupsDir + "/" + id + "/wal" }
func baseDir(id string) string             { return backupsDir + "/" + id + "/base" }
func backupDir(id, backupID string) string { return baseDir(id) + "/" + backupID }

// hash is a short digest of v's JSON, for the nebius-mocker.spec label.
func hash(v any) string {
	raw, _ := json.Marshal(v) //nolint:errchkjson // plain strings, ints and string slices always marshal
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// postgresFlags are the -c settings every host of the cluster runs with:
// overridable defaults, then the cluster's parameters (sorted), then the
// settings the backend depends on, which parameters cannot change.
func postgresFlags(clusterID string, params map[string]string) []string {
	var flags []string
	add := func(name, value string) { flags = append(flags, "-c", name+"="+value) }
	// A forced segment switch every minute bounds how much recent WAL a
	// point-in-time restore of a deleted cluster can miss.
	add("archive_timeout", "60")
	for _, name := range slices.Sorted(maps.Keys(params)) {
		add(name, params[name])
	}
	add("listen_addresses", "*")
	add("ssl", "on")
	add("ssl_cert_file", tlsCert)
	add("ssl_key_file", tlsKey)
	add("wal_level", "replica")
	add("max_wal_senders", "10")
	add("hot_standby", "on")
	add("archive_mode", "on")
	add("archive_command", fmt.Sprintf("test ! -f %[1]s/%%f && cp %%p %[1]s/%%f", walDir(clusterID)))
	return flags
}

// hostScript is the entrypoint of a host container (arguments: the -c
// flags). It runs as root: it prepares the archive directories, which belong
// to postgres (uid 70), seeds a replica from the primary on first start, and
// hands over to the image's entrypoint, which drops to postgres.
func hostScript(clusterID string, n int) string {
	replica := n > 0
	var sb strings.Builder
	sb.WriteString("set -e\n")
	fmt.Fprintf(&sb, "mkdir -p %[1]s/%[2]s/wal %[1]s/%[2]s/base\n", backupsDir, clusterID)
	fmt.Fprintf(&sb, "chown %[1]d:%[1]d %[2]s %[2]s/%[3]s %[2]s/%[3]s/wal %[2]s/%[3]s/base\n", pgUID, backupsDir, clusterID)
	if replica {
		// A failed attempt leaves a partial directory behind; empty it
		// before the next one. The primary is already ready, so a retry is
		// only for a slow start of its WAL senders.
		fmt.Fprintf(&sb, `if [ ! -s %[1]s/PG_VERSION ]; then
  mkdir -p %[1]s
  n=0
  until [ "$n" -ge 30 ]; do
    find %[1]s -mindepth 1 -delete
    chown postgres:postgres %[1]s
    chmod 700 %[1]s
    gosu postgres pg_basebackup -h pg-0 -U %[2]s -D %[1]s -R -X stream -w && break
    n=$((n+1))
    sleep 1
  done
  [ -s %[1]s/PG_VERSION ]
fi
`, pgData, replicationRole)
	}
	sb.WriteString(`exec docker-entrypoint.sh postgres "$@"` + "\n")
	return sb.String()
}

// sqlString quotes s as an SQL string literal.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sqlIdent quotes s as an SQL identifier.
func sqlIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
