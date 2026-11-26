package domain_test

import (
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// onDemand takes an on-demand backup and returns its record.
func (e *env) onDemand(t *testing.T, clusterID string) domain.BackupRecord {
	t.Helper()
	before := map[string]bool{}
	for _, b := range e.backupsOf(t, clusterID) {
		before[b.Backup.GetId()] = true
	}
	done(t)(e.svc.CreateBackup(t.Context(), clusterID, nil))
	for _, b := range e.backupsOf(t, clusterID) {
		if !before[b.Backup.GetId()] {
			return b
		}
	}
	t.Fatal("no new backup after CreateBackup")
	return domain.BackupRecord{}
}

func TestOnDemandBackupLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	auto := e.backupsOf(t, id)[0]

	e.clk.Advance(time.Hour)
	b := e.onDemand(t, id)
	if !b.Backup.GetOnDemand() || !b.Done || b.RetentionDays != 0 || b.Backup.GetSourceClusterName() != "main" || b.Backup.GetSourceClusterId() != id {
		t.Fatalf("backup = %+v", b)
	}
	if !b.Backup.GetCreationFinish().AsTime().Equal(t0.Add(time.Hour)) || b.Backup.GetSizeBytes() != 0 {
		t.Errorf("backup times/size = %v", b.Backup)
	}
	got, err := e.svc.GetBackup(t.Context(), id, b.Backup.GetId())
	if err != nil || got.Backup.GetId() != b.Backup.GetId() {
		t.Errorf("GetBackup = %v, %v", got, err)
	}
	if _, err := e.svc.GetBackup(t.Context(), "postgresql-e00other", b.Backup.GetId()); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetBackup under another cluster: %v, want NotFound", err)
	}
	if p, err := e.svc.ListBackups(t.Context(), projectID, 0, ""); err != nil || len(p.Items) != 2 {
		t.Errorf("ListBackups = %v, %v, want 2", p.Items, err)
	}

	// The WAL needed by the oldest remaining backup is kept.
	done(t)(e.svc.DeleteBackup(t.Context(), id, b.Backup.GetId(), nil))
	if keep := e.backend.keepWAL[b.Backup.GetId()]; !keep.Equal(auto.Backup.GetCreationStart().AsTime()) {
		t.Errorf("keepWALFrom = %v, want the start of the remaining backup %v", keep, auto.Backup.GetCreationStart().AsTime())
	}
	if len(e.backupsOf(t, id)) != 1 {
		t.Error("the on-demand backup was not deleted")
	}
	wantKind(t, "delete automatic", errOf(e.svc.DeleteBackup(t.Context(), id, auto.Backup.GetId(), nil)), errs.KindBadRequest, "only on-demand backups can be deleted")
	wantKind(t, "delete missing", errOf(e.svc.DeleteBackup(t.Context(), id, "postgresqlbackup-e00x", nil)), errs.KindNotFound)
	wantKind(t, "backup of a missing cluster", errOf(e.svc.CreateBackup(t.Context(), "postgresql-e00missing", nil)), errs.KindNotFound)
}

func TestBackupFailureLeavesNoRecord(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	e.backend.backupErr = errs.Internal("disk full")
	op, err := e.svc.CreateBackup(t.Context(), id, nil)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	wantKind(t, "operation", op.Err, errs.KindInternal, "disk full")
	if n := len(e.backupsOf(t, id)); n != 1 {
		t.Errorf("backups = %d, want only the first automatic one", n)
	}
}

func TestFirstBackupFailureDoesNotFailCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.backupErr = errs.Internal("no storage")
	c := e.create(t, "main", nil)
	if c.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || len(e.backupsOf(t, c.GetMetadata().GetId())) != 0 {
		t.Errorf("status = %v, backups = %d", c.GetStatus(), len(e.backupsOf(t, c.GetMetadata().GetId())))
	}
	// The daily schedule retries once the window passes.
	e.backend.backupErr = nil
	e.clk.Advance(13 * time.Hour)
	if err := e.svc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n := len(e.backupsOf(t, c.GetMetadata().GetId())); n != 1 {
		t.Errorf("backups after the sweep = %d, want 1", n)
	}
}

func TestBackupScheduleRunsDailyAtTheWindow(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // created 2026-01-01 12:00 UTC; the default window is 00:02
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	sweep := func(d time.Duration) int {
		t.Helper()
		e.clk.Advance(d)
		if err := e.svc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		return len(e.backupsOf(t, id))
	}
	if n := sweep(0); n != 1 {
		t.Fatalf("backups right after create = %d, want the first one only", n)
	}
	if n := sweep(11*time.Hour + 59*time.Minute); n != 1 { // 2026-01-02 00:00
		t.Errorf("backups before the window = %d, want 1", n)
	}
	if n := sweep(3 * time.Minute); n != 2 { // 00:03
		t.Errorf("backups after the window = %d, want 2", n)
	}
	if n := sweep(time.Hour); n != 2 {
		t.Errorf("backups later the same day = %d, want 2 (one per day)", n)
	}
	for _, b := range e.backupsOf(t, id) {
		if b.Backup.GetOnDemand() {
			t.Errorf("scheduled backup %s is on-demand", b.Backup.GetId())
		}
	}
	// A stopped cluster is not backed up.
	done(t)(e.svc.StopCluster(t.Context(), id, nil))
	if n := sweep(24 * time.Hour); n != 2 {
		t.Errorf("backups of a stopped cluster = %d, want 2", n)
	}
}

func TestBackupScheduleUsesTheConfiguredWindow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", func(s *pgpb.ClusterSpec) {
		s.Backup = &pgpb.BackupSpec{BackupWindowStart: "13:30:00", RetentionPolicy: "10d"}
	})
	id := c.GetMetadata().GetId()
	e.clk.Advance(time.Hour) // 13:00
	if err := e.svc.Sweep(t.Context()); err != nil || len(e.backupsOf(t, id)) != 1 {
		t.Fatalf("Sweep before the window: %v, %d backups", err, len(e.backupsOf(t, id)))
	}
	e.clk.Advance(31 * time.Minute) // 13:31
	if err := e.svc.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	bk := e.backupsOf(t, id)
	if len(bk) != 2 || bk[0].RetentionDays != 10 {
		t.Errorf("backups = %d (retention %d), want 2 kept 10 days", len(bk), bk[0].RetentionDays)
	}
}

func TestBackupRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	e.clk.Advance(time.Minute)
	manual := e.onDemand(t, id)
	first := e.backupsOf(t, id)[1].Backup.GetId() // the automatic one (oldest, sorted last)
	for range 9 {
		e.clk.Advance(24 * time.Hour)
		if err := e.svc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	now := e.clk.Now()
	for _, b := range e.backupsOf(t, id) {
		if b.Backup.GetOnDemand() {
			continue
		}
		if age := now.Sub(b.Backup.GetCreationStart().AsTime()); age > 7*24*time.Hour {
			t.Errorf("automatic backup %s is %v old, past the 7-day retention", b.Backup.GetId(), age)
		}
	}
	if _, err := e.svc.GetBackup(t.Context(), id, first); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("the first automatic backup survived retention: %v", err)
	}
	if _, err := e.svc.GetBackup(t.Context(), id, manual.Backup.GetId()); err != nil {
		t.Errorf("the on-demand backup expired: %v", err)
	}
	if keep, ok := e.backend.keepWAL[first]; !ok || keep.IsZero() {
		t.Errorf("keepWALFrom of the expired backup = %v (called: %v), want the start of the oldest remaining one", keep, ok)
	}

	// Backups outlive their cluster until their retention expires.
	done(t)(e.svc.DeleteCluster(t.Context(), id, nil))
	for range 9 {
		e.clk.Advance(24 * time.Hour)
		if err := e.svc.Sweep(t.Context()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	rest := e.backupsOf(t, id)
	if len(rest) != 1 || !rest[0].Backup.GetOnDemand() || rest[0].Backup.GetSourceClusterVisible() {
		t.Errorf("backups of the deleted cluster = %+v, want only the on-demand one, source not visible", rest)
	}
}

func TestRestoreClusterFromBackup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.create(t, "main", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 30 })
	srcID := src.GetMetadata().GetId()
	e.clk.Advance(2 * time.Hour)
	manual := e.onDemand(t, srcID)

	restore := func(mod func(*pgpb.ClusterSpec), backupID string, rt *time.Time, parent string) (string, error) {
		spec := validSpec()
		spec.Config.Template.Disk.SizeGibibytes = 30
		if mod != nil {
			mod(spec)
		}
		op, err := e.svc.RestoreCluster(t.Context(), domain.RestoreClusterCmd{
			Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: "restored"}, Spec: spec,
			SourceClusterID: srcID, BackupID: backupID, RecoveryTime: rt,
		})
		if err != nil {
			return "", err
		}
		return op.ResourceID, op.Err
	}

	id, err := restore(func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 40 }, manual.Backup.GetId(), nil, projectID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	bc := e.backend.last()
	if bc.ID != id || bc.Restore == nil || bc.Restore.SourceClusterID != srcID || bc.Restore.BackupID != manual.Backup.GetId() || !bc.Restore.RecoveryTime.IsZero() {
		t.Errorf("backend cluster = %+v, restore %+v", bc, bc.Restore)
	}
	if bc.Password != "s3cret-pass" || bc.DiskGiB != 40 {
		t.Errorf("backend password/disk = %q/%d", bc.Password, bc.DiskGiB)
	}
	got := e.get(t, id)
	if got.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || got.GetSpec().GetBootstrap().GetUserPassword() != "" {
		t.Errorf("restored cluster = %v", got)
	}
	if n := len(e.backupsOf(t, id)); n != 1 {
		t.Errorf("restored cluster backups = %d, want its own first backup", n)
	}

	auto := e.backupsOf(t, srcID)[1]
	if auto.Backup.GetOnDemand() {
		t.Fatal("test setup: expected the automatic backup")
	}
	tests := []struct {
		name   string
		mod    func(*pgpb.ClusterSpec)
		backup string
		rt     *time.Time
		parent string
		kind   errs.Kind
		frag   string
	}{
		{"unknown backup", nil, "postgresqlbackup-e00x", nil, projectID, errs.KindNotFound, "postgresqlbackup-e00x"},
		{"version", func(s *pgpb.ClusterSpec) { s.Config.Version = "17" }, auto.Backup.GetId(), nil, projectID, errs.KindBadRequest, "spec.config.version"},
		{"disk smaller", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 20 }, auto.Backup.GetId(), nil, projectID, errs.KindBadRequest, "disk.size_gibibytes"},
		{"no password", func(s *pgpb.ClusterSpec) { s.Bootstrap.UserPassword = "" }, auto.Backup.GetId(), nil, projectID, errs.KindBadRequest, "spec.bootstrap.user_password is required"},
		{"recovery of an on-demand backup", nil, manual.Backup.GetId(), ptr(t0.Add(3 * time.Hour)), projectID, errs.KindBadRequest, "recovery_time"},
		{"recovery before the backup", nil, auto.Backup.GetId(), ptr(t0.Add(-time.Hour)), projectID, errs.KindBadRequest, "recovery_time"},
		{"recovery in the future", nil, auto.Backup.GetId(), ptr(e.clk.Now().Add(time.Minute)), projectID, errs.KindBadRequest, "future"},
		{"other project", nil, auto.Backup.GetId(), nil, otherProjectID, errs.KindBadRequest, "metadata.parent_id"},
		{"name taken", nil, auto.Backup.GetId(), nil, projectID, errs.KindAlreadyExists, ""},
	}
	for _, tc := range tests {
		_, err := restore(tc.mod, tc.backup, tc.rt, tc.parent)
		wantKind(t, tc.name, err, tc.kind, tc.frag)
	}
}

func TestPointInTimeRecovery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.create(t, "main", nil)
	srcID := src.GetMetadata().GetId()
	auto := e.backupsOf(t, srcID)[0]
	e.clk.Advance(3 * time.Hour)
	rt := t0.Add(90 * time.Minute)
	op, err := e.svc.RestoreCluster(t.Context(), domain.RestoreClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "pitr"}, Spec: validSpec(),
		SourceClusterID: srcID, BackupID: auto.Backup.GetId(), RecoveryTime: &rt,
	})
	if err != nil || op.Err != nil {
		t.Fatalf("RestoreCluster: %v, %v", err, op)
	}
	if got := e.backend.last().Restore; got == nil || !got.RecoveryTime.Equal(rt) {
		t.Errorf("restore source = %+v, want recovery time %v", got, rt)
	}
	// The source cluster may be gone.
	done(t)(e.svc.DeleteCluster(t.Context(), srcID, nil))
	op, err = e.svc.RestoreCluster(t.Context(), domain.RestoreClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "from-deleted"}, Spec: validSpec(),
		SourceClusterID: srcID, BackupID: auto.Backup.GetId(),
	})
	if err != nil || op.Err != nil {
		t.Fatalf("restore from a deleted cluster: %v, %v", err, op)
	}
}
