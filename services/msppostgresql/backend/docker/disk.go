package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// readOnlySetting is the per-database setting that makes a full disk refuse
// writes. A database-level setting wins over the -c server default, so a
// user's own default_transaction_read_only parameter cannot undo it; a
// session can still SET it off, which is also how a user frees space.
const readOnlySetting = "default_transaction_read_only"

// diskScript prints the size of the data directory in KiB (WAL included;
// archived WAL and backups live elsewhere and do not count).
const diskScript = `du -sk "$PGDATA" | cut -f1`

// CheckDisk implements domain.Backend. The "full" state is read back from the
// databases themselves (pg_db_role_setting), so the backend keeps none: a
// cluster is full as long as its databases carry the setting.
func (b *Backend) CheckDisk(ctx context.Context, clusterID string, limitBytes int64) (domain.DiskUsage, error) {
	usage := domain.DiskUsage{LimitBytes: limitBytes}
	ctr, err := b.docker.InspectContainer(ctx, b.hostName(clusterID, 0))
	if errors.Is(err, dockerhost.ErrNotFound) || (err == nil && !ctr.Running) {
		return usage, nil
	}
	if err != nil {
		return usage, internal("inspect primary of "+clusterID, err)
	}
	out, err := b.sh(ctx, ctr.ID, diskScript)
	if err != nil {
		return usage, internal("measure the data directory", err)
	}
	kib, err := strconv.ParseInt(lastLine(out), 10, 64)
	if err != nil {
		return usage, errs.Internal(fmt.Sprintf("data directory size %q is not a number", out))
	}
	usage.UsedBytes = kib << 10
	user := ctr.Labels[labelUser]
	dbs, err := b.databases(ctx, ctr.ID, user)
	if err != nil {
		return usage, err
	}
	full := usage.UsedBytes >= limitBytes
	if sql := enforceSQL(dbs, full); sql != "" {
		if _, err := b.psql(ctx, ctr.ID, user, sql); err != nil {
			return usage, internal("enforce the disk limit", err)
		}
		b.log.InfoContext(ctx, "msp postgresql: disk limit enforced", "cluster", clusterID, "full", full,
			"used_bytes", usage.UsedBytes, "limit_bytes", limitBytes)
	}
	usage.Full = full
	return usage, nil
}

// database is a connectable, non-template database and whether it is marked
// read-only.
type database struct {
	name     string
	readOnly bool
}

// databasesSQL lists the databases with their read-only mark.
const databasesSQL = `select d.datname || '|' || coalesce((select true from pg_db_role_setting s where s.setdatabase = d.oid and s.setrole = 0 ` +
	`and '` + readOnlySetting + `=on' = any(s.setconfig)), false) from pg_database d where not d.datistemplate and d.datallowconn order by 1`

func (b *Backend) databases(ctx context.Context, id, user string) ([]database, error) {
	out, err := b.psql(ctx, id, user, databasesSQL)
	if err != nil {
		return nil, internal("list databases", err)
	}
	return parseDatabases(out), nil
}

func parseDatabases(out string) []database {
	var dbs []database
	for line := range strings.SplitSeq(out, "\n") {
		name, mark, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		dbs = append(dbs, database{name: name, readOnly: mark == "true" || mark == "t"})
	}
	return dbs
}

// terminateSQL ends the sessions of clients (not the replication and pooler
// logins, nor the caller), so existing and pooled connections pick up the new
// default.
const terminateSQL = `select pg_terminate_backend(pid) from pg_stat_activity where backend_type = 'client backend' ` +
	`and pid <> pg_backend_pid() and usename not in ('` + replicationRole + `', '` + poolerRole + `')`

// enforceSQL is what moves the databases to the wanted state, or "" when they
// already are. Sessions are ended only when something changed.
func enforceSQL(dbs []database, full bool) string {
	var sb strings.Builder
	for _, d := range dbs {
		switch {
		case full && !d.readOnly:
			fmt.Fprintf(&sb, "ALTER DATABASE %s SET %s = on;\n", sqlIdent(d.name), readOnlySetting)
		case !full && d.readOnly:
			fmt.Fprintf(&sb, "ALTER DATABASE %s RESET %s;\n", sqlIdent(d.name), readOnlySetting)
		}
	}
	if sb.Len() == 0 {
		return ""
	}
	return sb.String() + terminateSQL
}
