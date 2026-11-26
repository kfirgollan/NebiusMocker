package domain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// GetBackup returns a backup of a cluster (also of a deleted one).
func (s *Service) GetBackup(ctx context.Context, clusterID, backupID string) (BackupRecord, error) {
	return s.d.Backups.Get(ctx, clusterID, backupID)
}

// ListBackups returns one page of a project's backups, newest first.
func (s *Service) ListBackups(ctx context.Context, projectID string, size int64, pageToken string) (Page[BackupRecord], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[BackupRecord]{}, err
	}
	return s.d.Backups.ListByProject(ctx, projectID, n, pageToken)
}

// ListBackupsByCluster returns one page of a cluster's backups, newest first.
func (s *Service) ListBackupsByCluster(ctx context.Context, clusterID string, size int64, pageToken string) (Page[BackupRecord], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[BackupRecord]{}, err
	}
	return s.d.Backups.ListByCluster(ctx, clusterID, n, pageToken)
}

// GetClusterForBackup returns the cluster as it was when the backup was
// taken. It works after the cluster is deleted.
func (s *Service) GetClusterForBackup(ctx context.Context, clusterID, backupID string) (*pgpb.Cluster, error) {
	rec, err := s.d.Backups.Get(ctx, clusterID, backupID)
	if err != nil {
		return nil, err
	}
	return proto.CloneOf(rec.Cluster), nil
}

// backupKind says who asked for a backup.
type backupKind int

const (
	// automaticBackup is taken by the schedule and kept for the retention.
	automaticBackup backupKind = iota
	// onDemandBackup is taken on request and kept until it is deleted.
	onDemandBackup
)

// CreateBackup takes an on-demand backup of a running cluster.
func (s *Service) CreateBackup(ctx context.Context, clusterID string, request proto.Message) (*operations.Operation, error) {
	return s.submitBackup(ctx, clusterID, onDemandBackup, request)
}

// submitBackup starts a backup operation. Its resource_id is the new
// backup (common.v1alpha1.Operation: "ID of the resource that this operation
// creates"), and it holds the cluster's resource lock so that it serializes
// with the cluster's own operations.
func (s *Service) submitBackup(ctx context.Context, clusterID string, kind backupKind, request proto.Message) (*operations.Operation, error) {
	backupID, err := s.d.IDs.New(KindBackup)
	if err != nil {
		return nil, fmt.Errorf("new backup id: %w", err)
	}
	var rec BackupRecord
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: backupID, LockID: clusterID, Description: "Create PostgreSQL backup", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			var err error
			rec, err = s.prepareBackup(ctx, clusterID, backupID, kind)
			return err
		},
		Work: func(ctx context.Context) error {
			unlock := s.clusterLock(clusterID)
			defer unlock()
			return s.takeBackup(ctx, rec)
		},
	})
}

// prepareBackup stores the record of a backup that is being taken.
func (s *Service) prepareBackup(ctx context.Context, clusterID, backupID string, kind backupKind) (BackupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.d.Clusters.Get(ctx, clusterID)
	if err != nil {
		return BackupRecord{}, err
	}
	if isStopped(c) {
		return BackupRecord{}, errs.BadState(clusterID, fmt.Sprintf("cluster %s is stopped: start it before taking a backup", c.GetMetadata().GetName()))
	}
	if !isRunning(c) {
		return BackupRecord{}, errs.BadState(clusterID, fmt.Sprintf("cluster %s is not running (phase %s, state %s)",
			c.GetMetadata().GetName(), c.GetStatus().GetPhase(), c.GetStatus().GetState()))
	}
	rec, err := s.newBackupRecord(c, backupID)
	if err != nil {
		return BackupRecord{}, err
	}
	if kind == onDemandBackup {
		rec.Backup.OnDemand, rec.RetentionDays = true, 0
	}
	return rec, s.d.Backups.Put(ctx, rec)
}

// newBackupRecord is the record of an automatic backup of c that starts now;
// an empty id allocates one.
func (s *Service) newBackupRecord(c *pgpb.Cluster, id string) (BackupRecord, error) {
	if id == "" {
		var err error
		if id, err = s.d.IDs.New(KindBackup); err != nil {
			return BackupRecord{}, fmt.Errorf("new backup id: %w", err)
		}
	}
	rec := BackupRecord{
		Backup: &pgpb.Backup{
			Id: id, SourceClusterId: c.GetMetadata().GetId(), SourceClusterName: c.GetMetadata().GetName(),
			SourceClusterVisible: true, CreationStart: timestamppb.New(s.d.Clock.Now()),
		},
		ProjectID: c.GetMetadata().GetParentId(),
		Cluster:   proto.CloneOf(c),
	}
	_, rec.RetentionDays, _ = backupSettings(c.GetSpec().GetBackup())
	return rec, nil
}

// takeBackup runs the backend backup of a stored record and finishes it. The
// caller holds the cluster lock. A failed backup leaves no record.
func (s *Service) takeBackup(ctx context.Context, rec BackupRecord) error {
	info, err := s.d.Backend.CreateBackup(ctx, rec.Backup.GetSourceClusterId(), rec.Backup.GetId())
	if err != nil {
		return errors.Join(err, s.d.Backups.Delete(ctx, rec.Backup.GetSourceClusterId(), rec.Backup.GetId()))
	}
	rec.Backup.CreationFinish = timestamppb.New(s.d.Clock.Now())
	rec.Backup.SizeBytes = info.SizeBytes
	rec.Done = true
	return s.d.Backups.Put(ctx, rec)
}

// firstBackup takes the automatic backup of a cluster that has just become
// RUNNING. The caller holds the cluster lock. Its failure does not fail the
// operation that created the cluster; the daily schedule retries.
func (s *Service) firstBackup(ctx context.Context, c *pgpb.Cluster) {
	rec, err := s.newBackupRecord(c, "")
	if err == nil {
		err = s.d.Backups.Put(ctx, rec)
	}
	if err == nil {
		err = s.takeBackup(ctx, rec)
	}
	if err != nil {
		s.log.WarnContext(ctx, "first automatic backup failed", "cluster_id", c.GetMetadata().GetId(), "error", err)
	}
}

// DeleteBackup deletes an on-demand backup. Automatic backups expire with
// their retention.
func (s *Service) DeleteBackup(ctx context.Context, clusterID, backupID string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: backupID, LockID: clusterID, Description: "Delete PostgreSQL backup", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			rec, err := s.d.Backups.Get(ctx, clusterID, backupID)
			if err != nil {
				return err
			}
			if !rec.Backup.GetOnDemand() {
				return errs.BadRequest(errs.Violation{Field: "backup_id", Message: "only on-demand backups can be deleted: automatic backups expire with the retention policy"})
			}
			if !rec.Done {
				return errs.BadState(backupID, "the backup is still being taken")
			}
			return nil
		},
		Work: func(ctx context.Context) error { return s.removeBackup(ctx, clusterID, backupID) },
	})
}

// removeBackup deletes a backup's data and record. The WAL needed by the
// cluster's oldest remaining backup stays.
func (s *Service) removeBackup(ctx context.Context, clusterID, backupID string) error {
	all, err := s.d.Backups.All(ctx)
	if err != nil {
		return err
	}
	var keepFrom time.Time
	for _, r := range all {
		if r.Backup.GetSourceClusterId() != clusterID || r.Backup.GetId() == backupID {
			continue
		}
		if t := r.Backup.GetCreationStart().AsTime(); keepFrom.IsZero() || t.Before(keepFrom) {
			keepFrom = t
		}
	}
	if err := s.d.Backend.DeleteBackup(ctx, clusterID, backupID, keepFrom); err != nil {
		return err
	}
	return s.d.Backups.Delete(ctx, clusterID, backupID)
}

// hideBackups marks the backups of a deleted cluster: their source cluster is
// no longer visible.
func (s *Service) hideBackups(ctx context.Context, clusterID string) error {
	all, err := s.d.Backups.All(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Backup.GetSourceClusterId() != clusterID || !r.Backup.GetSourceClusterVisible() {
			continue
		}
		r.Backup.SourceClusterVisible = false
		if err := s.d.Backups.Put(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// Sweep applies the backup schedule once: it deletes the automatic backups
// that outlived their retention (also of deleted clusters), then starts the
// daily automatic backup of every running cluster whose backup window has
// passed without one. The module runs it every minute.
func (s *Service) Sweep(ctx context.Context) error {
	all, err := s.d.Backups.All(ctx)
	if err != nil {
		return err
	}
	now := s.d.Clock.Now()
	var errList []error
	kept := all[:0:0]
	for _, r := range all {
		if !expired(r, now) {
			kept = append(kept, r)
			continue
		}
		if err := s.removeBackup(ctx, r.Backup.GetSourceClusterId(), r.Backup.GetId()); err != nil {
			errList = append(errList, fmt.Errorf("expire backup %s: %w", r.Backup.GetId(), err))
			kept = append(kept, r)
		}
	}
	clusters, err := s.d.Clusters.All(ctx)
	if err != nil {
		return errors.Join(append(errList, err)...)
	}
	for _, c := range clusters {
		if err := s.scheduleBackup(ctx, c, kept, now); err != nil {
			errList = append(errList, err)
		}
	}
	return errors.Join(errList...)
}

// expired reports whether an automatic backup is older than its retention.
func expired(r BackupRecord, now time.Time) bool {
	if r.Backup.GetOnDemand() || !r.Done || r.RetentionDays <= 0 {
		return false
	}
	return now.Sub(r.Backup.GetCreationStart().AsTime()) > time.Duration(r.RetentionDays)*day
}

// scheduleBackup starts the daily automatic backup of c when it is due: the
// cluster is running and has no automatic backup since its last window.
func (s *Service) scheduleBackup(ctx context.Context, c *pgpb.Cluster, backups []BackupRecord, now time.Time) error {
	if !isRunning(c) {
		return nil
	}
	id := c.GetMetadata().GetId()
	window, _, _ := backupSettings(c.GetSpec().GetBackup())
	due := lastWindow(now, window)
	for _, r := range backups {
		if r.Backup.GetSourceClusterId() == id && !r.Backup.GetOnDemand() && !r.Backup.GetCreationStart().AsTime().Before(due) {
			return nil
		}
	}
	_, err := s.submitBackup(ctx, id, automaticBackup, &pgpb.CreateBackupRequest{ClusterId: id})
	if errs.IsKind(err, errs.KindOperationConflict) || errs.IsKind(err, errs.KindBadState) {
		return nil // busy or changing: the next sweep tries again
	}
	return err
}

// lastWindow is the latest backup window start (UTC) that is not after now.
func lastWindow(now time.Time, window time.Duration) time.Time {
	t := now.UTC().Truncate(day).Add(window)
	if t.After(now) {
		t = t.Add(-day)
	}
	return t
}

// RestoreCluster creates a new cluster from a backup, or from a point in time
// after an automatic backup. The source cluster may be deleted.
func (s *Service) RestoreCluster(ctx context.Context, cmd RestoreClusterCmd) (*operations.Operation, error) {
	id, err := s.d.IDs.New(KindCluster)
	if err != nil {
		return nil, fmt.Errorf("new cluster id: %w", err)
	}
	password := cmd.Spec.GetBootstrap().GetUserPassword()
	var source *RestoreSource
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: "Restore PostgreSQL cluster", CreatedBy: reqctx.PrincipalFrom(ctx).ID,
		Request: cmd.Request, Detached: s.d.Config.Detached,
		Prepare: func(ctx context.Context) error {
			var err error
			if source, err = s.checkRestoreSource(ctx, cmd); err != nil {
				return err
			}
			return s.prepareNewCluster(ctx, id, cmd.Metadata, cmd.Spec)
		},
		Work: func(ctx context.Context) error {
			return s.ensureCluster(ctx, id, run{password: password, restore: source, firstBackup: true})
		},
	})
}

// checkRestoreSource validates the backup, the target spec against the
// source cluster, and the recovery time.
func (s *Service) checkRestoreSource(ctx context.Context, cmd RestoreClusterCmd) (*RestoreSource, error) {
	rec, err := s.d.Backups.Get(ctx, cmd.SourceClusterID, cmd.BackupID)
	if err != nil {
		return nil, err
	}
	if !rec.Done {
		return nil, errs.BadState(cmd.BackupID, "the backup is still being taken")
	}
	if p := cmd.Metadata.GetParentId(); p != rec.ProjectID {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: fmt.Sprintf(
			"restoring into another project (%s) than the backup's (%s) is not emulated by NebiusMocker", p, rec.ProjectID)})
	}
	if err := checkRestore(rec.Cluster.GetSpec(), cmd.Spec); err != nil {
		return nil, err
	}
	src := &RestoreSource{SourceClusterID: cmd.SourceClusterID, BackupID: cmd.BackupID}
	if cmd.RecoveryTime == nil {
		return src, nil
	}
	const field = "recovery_time"
	rt := *cmd.RecoveryTime
	switch {
	case rec.Backup.GetOnDemand():
		return nil, errs.BadRequest(errs.Violation{Field: field, Message: "point-in-time recovery needs an automatic backup: on-demand backups restore as taken"})
	case rt.Before(rec.Backup.GetCreationFinish().AsTime()):
		return nil, errs.BadRequest(errs.Violation{Field: field, Message: "must not be before the end of the backup (" +
			rec.Backup.GetCreationFinish().AsTime().Format(time.RFC3339) + ")"})
	case rt.After(s.d.Clock.Now()):
		return nil, errs.BadRequest(errs.Violation{Field: field, Message: "must not be in the future"})
	}
	src.RecoveryTime = rt
	return src, nil
}
