package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// backupScript ($1: directory, $2: role) takes a base backup over the local
// socket (the image's pg_hba.conf trusts local replication connections),
// notes the superuser it was taken with, and prints the size in bytes. A
// fast checkpoint starts it at once instead of within checkpoint_timeout.
const backupScript = `set -e
rm -rf "$1"
pg_basebackup -U "$2" -D "$1" -Ft -z -X fetch -c fast
echo "$POSTGRES_USER" > "$1/nmock.user"
du -sb "$1" | cut -f1
`

// CreateBackup implements domain.Backend.
func (b *Backend) CreateBackup(ctx context.Context, clusterID, backupID string) (domain.BackupInfo, error) {
	ctr, err := b.docker.InspectContainer(ctx, hostName(clusterID, 0))
	if err != nil {
		return domain.BackupInfo{}, internal("inspect primary of "+clusterID, err)
	}
	if !ctr.Running {
		return domain.BackupInfo{}, errs.BadState(clusterID, "the cluster is not running")
	}
	out, err := b.sh(ctx, ctr.ID, backupScript, backupDir(clusterID, backupID), ctr.Labels[labelUser])
	if err != nil {
		return domain.BackupInfo{}, internal("take base backup", err)
	}
	size, err := strconv.ParseInt(lastLine(out), 10, 64)
	if err != nil {
		return domain.BackupInfo{}, errs.Internal(fmt.Sprintf("backup size %q is not a number", out))
	}
	return domain.BackupInfo{SizeBytes: size}, nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return strings.TrimSpace(s)
}

// flushWAL switches the primary to a new WAL segment and waits until the
// finished one is in the archive, so what was committed so far can be
// recovered from the archive alone. It is best-effort: a cluster that is
// stopped, or a failure, only costs the newest transactions in a restore.
func (b *Backend) flushWAL(ctx context.Context, clusterID string) {
	ctr, err := b.docker.InspectContainer(ctx, hostName(clusterID, 0))
	if err != nil || !ctr.Running {
		return
	}
	segment, err := b.psql(ctx, ctr.ID, ctr.Labels[labelUser], "select pg_walfile_name(pg_switch_wal() - 1)")
	if err != nil {
		b.log.WarnContext(ctx, "msp postgresql: switch WAL segment", "cluster", clusterID, "error", err)
		return
	}
	deadline := b.clk.Now().Add(pollTimeout)
	for {
		if _, err := b.sh(ctx, ctr.ID, `test -f "$1"`, walDir(clusterID)+"/"+segment); err == nil {
			return
		}
		if !b.clk.Now().Before(deadline) || b.sleep(ctx, b.poll) != nil {
			b.log.WarnContext(ctx, "msp postgresql: WAL segment was not archived in time", "cluster", clusterID, "segment", segment)
			return
		}
	}
}

// restoreData seeds the data volume of a restored cluster's primary from a
// backup, once: a cluster whose volume exists is left alone.
func (b *Backend) restoreData(ctx context.Context, c domain.BackendCluster, image string) error {
	vols, err := b.docker.ListVolumes(ctx, b.clusterLabels(c.ID))
	if err != nil {
		return internal("list volumes", err)
	}
	vol := volumeName(c.ID, 0)
	if slices.Contains(vols, vol) {
		return nil
	}
	labels := b.clusterLabels(c.ID)
	labels[LabelRole], labels[LabelHost] = roleHost, "0"
	if err := b.docker.EnsureVolume(ctx, vol, labels); err != nil {
		return internal("create data volume", err)
	}
	b.flushWAL(ctx, c.Restore.SourceClusterID)
	script, files := restoreHelper(c)
	err = b.runHelper(ctx, image, script, []string{vol + ":" + pgData, b.backups + ":" + backupsDir}, files)
	if err != nil {
		// Do not leave a half-restored volume that the next call would trust.
		_ = b.docker.RemoveVolume(context.WithoutCancel(ctx), vol) // best-effort: reaped by label
		return internal("restore backup "+c.Restore.BackupID+" of "+c.Restore.SourceClusterID, err)
	}
	return nil
}

// restoreHelper returns the helper script that unpacks the backup into the
// data directory and prepares archive recovery, and the file with the
// recovery settings it appends.
func restoreHelper(c domain.BackendCluster) (script string, files []dockerhost.File) {
	src := backupDir(c.Restore.SourceClusterID, c.Restore.BackupID)
	script = fmt.Sprintf(`set -e
S=%[1]s
D=%[2]s
[ -f "$S/base.tar.gz" ] || { echo "backup $S does not exist" >&2; exit 3; }
cd "$D"
tar xzf "$S/base.tar.gz"
if [ -f "$S/pg_wal.tar.gz" ]; then
  mkdir -p pg_wal
  tar xzf "$S/pg_wal.tar.gz" -C pg_wal
fi
cp "$S/nmock.user" %[3]s
touch recovery.signal
cat /restore.conf >> postgresql.auto.conf
chown -R %[4]d:%[4]d "$D"
chmod 700 "$D"
`, src, pgData, superuserFile, pgUID)
	return script, []dockerhost.File{{Path: "/restore.conf", Content: []byte(recoverySettings(c.Restore))}}
}

// recoverySettings are the postgresql.auto.conf lines of an archive
// recovery: read WAL from the source cluster's archive, stop at the target
// time (or as soon as the backup is consistent) and promote.
func recoverySettings(r *domain.RestoreSource) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "restore_command = 'cp %s/%%f %%p'\n", walDir(r.SourceClusterID))
	if r.RecoveryTime.IsZero() {
		sb.WriteString("recovery_target = 'immediate'\n")
	} else {
		fmt.Fprintf(&sb, "recovery_target_time = '%s'\n", r.RecoveryTime.UTC().Format("2006-01-02 15:04:05.999999-07"))
	}
	sb.WriteString("recovery_target_action = 'promote'\n")
	return sb.String()
}

// deleteScript ($1: backup directory, $2: WAL directory, $3: minutes) removes
// a backup and the archived WAL that no remaining backup needs: segments
// archived more than $3 minutes ago ($3 "all": every one). Timeline history
// files stay; they are tiny and a recovery asks for them.
const deleteScript = `set -e
rm -rf "$1"
[ -d "$2" ] || exit 0
if [ "$3" = all ]; then
  find "$2" -mindepth 1 -type f ! -name '*.history' -delete
else
  find "$2" -type f ! -name '*.history' -mmin +"$3" -delete
fi
`

// DeleteBackup implements domain.Backend. It runs in a short-lived helper
// container, as the cluster may be gone.
func (b *Backend) DeleteBackup(ctx context.Context, clusterID, backupID string, keepWALFrom time.Time) error {
	vols, err := b.docker.ListVolumes(ctx, b.baseLabels())
	if err != nil {
		return internal("list volumes", err)
	}
	if !slices.Contains(vols, b.backups) {
		return nil // no backup was ever taken
	}
	image := b.cfg.PostgresImages[slices.Sorted(maps.Keys(b.cfg.PostgresImages))[0]]
	if err := b.docker.EnsureImage(ctx, image); err != nil {
		return internal("pull image "+image, err)
	}
	age := "all"
	if !keepWALFrom.IsZero() {
		age = strconv.Itoa(archiveAgeMinutes(b.clk.Now(), keepWALFrom))
	}
	script := fmt.Sprintf("set -- %s %s %s\n%s", backupDir(clusterID, backupID), walDir(clusterID), age, deleteScript)
	if err := b.runHelper(ctx, image, script, []string{b.backups + ":" + backupsDir}, nil); err != nil {
		return internal("delete backup "+backupID, err)
	}
	return nil
}

// archiveAgeMinutes is how many whole minutes before now a WAL segment must
// have been archived to be older than keepFrom for certain: rounded up, so a
// segment archived within the same minute as keepFrom is kept.
func archiveAgeMinutes(now, keepFrom time.Time) int {
	d := now.Sub(keepFrom)
	return max(1, int((d+time.Minute-1)/time.Minute))
}
