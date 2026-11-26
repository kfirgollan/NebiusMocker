package contract

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

func TestMSPPostgresBackupRPCs(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()
	id := e.newCluster(t, "main", nil)

	// The cluster has the automatic backup that came with its first start.
	first := e.listBackups(t, id)
	if len(first) != 1 || first[0].GetOnDemand() || first[0].GetSourceClusterId() != id || first[0].GetSourceClusterName() != "main" ||
		!first[0].GetSourceClusterVisible() || first[0].GetCreationStart() == nil || first[0].GetCreationFinish() == nil {
		t.Fatalf("backups of a new cluster = %v", first)
	}

	b := e.onDemandBackup(t, id)
	if !b.GetOnDemand() || !strings.HasPrefix(b.GetId(), "postgresqlbackup-") {
		t.Fatalf("on-demand backup = %v", b)
	}
	got, err := e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id, BackupId: b.GetId()})
	if err != nil || !proto.Equal(got, b) {
		t.Errorf("Get = %v, %v, want %v", got, err, b)
	}
	byProject, err := e.backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project})
	if err != nil || len(byProject.GetBackups()) != 2 {
		t.Fatalf("List = %v, %v, want 2 backups", byProject, err)
	}
	page, err := e.backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project, PageSize: 1})
	if err != nil || len(page.GetBackups()) != 1 || page.GetNextPageToken() == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	page2, err := e.backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project, PageSize: 1, PageToken: page.GetNextPageToken()})
	if err != nil || len(page2.GetBackups()) != 1 || page2.GetNextPageToken() != "" || page2.GetBackups()[0].GetId() == page.GetBackups()[0].GetId() {
		t.Errorf("List page 2 = %v, %v", page2, err)
	}
	var seen int
	for _, err := range e.backups.Filter(ctx, &pgpb.ListBackupsRequest{ParentId: e.project, PageSize: 1}) {
		if err != nil {
			t.Fatalf("Filter: %v", err)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("Filter saw %d backups, want 2", seen)
	}
	other := e.newCluster(t, "other", nil)
	if n := len(e.listBackups(t, other)); n != 1 {
		t.Errorf("backups of the other cluster = %d, want only its own", n)
	}

	// Delete: on-demand backups only.
	alphaWait(t)(e.backups.Delete(ctx, &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: b.GetId()}))
	_, err = e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id, BackupId: b.GetId()})
	wantFailure(t, "get deleted backup", err, codes.NotFound)
	_, err = e.backups.Delete(ctx, &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: first[0].GetId()})
	wantFailure(t, "delete automatic backup", err, codes.InvalidArgument, "only on-demand backups can be deleted")
	_, err = e.backups.Delete(ctx, &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: "postgresqlbackup-e00missing"})
	wantFailure(t, "delete missing backup", err, codes.NotFound)
	// A backup is addressed through its own cluster.
	_, err = e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: other, BackupId: first[0].GetId()})
	wantFailure(t, "get under the wrong cluster", err, codes.NotFound)
	_, err = e.backups.Create(ctx, &pgpb.CreateBackupRequest{ClusterId: "postgresql-e00missing"})
	wantFailure(t, "backup of a missing cluster", err, codes.NotFound)
	_, err = e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id})
	wantFailure(t, "get without backup id", err, codes.InvalidArgument)
	_, err = e.backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project, PageSize: 5000})
	wantFailure(t, "page size", err, codes.InvalidArgument, "page_size")
}

func TestMSPPostgresRestore(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()
	srcID := e.newCluster(t, "source", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 30 })
	b := e.onDemandBackup(t, srcID)
	auto := e.listBackups(t, srcID)
	var automatic *pgpb.Backup
	for _, x := range auto {
		if !x.GetOnDemand() {
			automatic = x
		}
	}

	restoreReq := func(name string, mod func(*pgpb.ClusterSpec)) *pgpb.RestoreClusterRequest {
		spec := pgSpec()
		spec.Config.Template.Disk.SizeGibibytes = 40
		if mod != nil {
			mod(spec)
		}
		return &pgpb.RestoreClusterRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: name}, Spec: spec,
			SourceClusterId: srcID, BackupId: b.GetId(),
		}
	}
	id := alphaWait(t)(e.clusters.Restore(ctx, restoreReq("restored", nil)))
	c := e.getCluster(t, id)
	if phase, state := pgPhase(c); phase != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || state != mspv1alpha1pb.ClusterStatus_STATE_FINISHED || c.GetSpec().GetConfig().GetTemplate().GetDisk().GetSizeGibibytes() != 40 {
		t.Errorf("restored cluster = %v", c)
	}
	checkNoPassword(t, c)
	if n := len(e.listBackups(t, id)); n != 1 {
		t.Errorf("restored cluster backups = %d, want its own first backup", n)
	}

	rejections := []struct {
		name string
		req  *pgpb.RestoreClusterRequest
		code codes.Code
		frag string
	}{
		{"unknown backup", func() *pgpb.RestoreClusterRequest {
			r := restoreReq("r1", nil)
			r.BackupId = "postgresqlbackup-e00missing"
			return r
		}(), codes.NotFound, "postgresqlbackup-e00missing"},
		{"smaller disk", restoreReq("r2", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 20 }), codes.InvalidArgument, "disk"},
		{"no password", restoreReq("r3", func(s *pgpb.ClusterSpec) { s.Bootstrap.UserPassword = "" }), codes.InvalidArgument, "spec.bootstrap.user_password is required"},
		{"duplicate name", restoreReq("restored", nil), codes.AlreadyExists, ""},
		{"point in time of an on-demand backup", func() *pgpb.RestoreClusterRequest {
			r := restoreReq("r4", nil)
			r.RecoveryTime = timestamppb.New(b.GetCreationFinish().AsTime())
			return r
		}(), codes.InvalidArgument, "recovery_time"},
		{"recovery before the backup", func() *pgpb.RestoreClusterRequest {
			r := restoreReq("r5", nil)
			r.BackupId = automatic.GetId()
			r.RecoveryTime = timestamppb.New(automatic.GetCreationFinish().AsTime().Add(-time.Hour))
			return r
		}(), codes.InvalidArgument, "recovery_time"},
		{"recovery in the future", func() *pgpb.RestoreClusterRequest {
			r := restoreReq("r6", nil)
			r.BackupId = automatic.GetId()
			r.RecoveryTime = timestamppb.New(clock.Real{}.Now().Add(time.Hour))
			return r
		}(), codes.InvalidArgument, "future"},
	}
	for _, tc := range rejections {
		_, err := e.clusters.Restore(ctx, tc.req)
		wantFailure(t, tc.name, err, tc.code, tc.frag)
	}

	// Point-in-time recovery of the automatic backup, then restoring from a
	// deleted source cluster.
	pit := restoreReq("pitr", nil)
	pit.BackupId = automatic.GetId()
	pit.RecoveryTime = timestamppb.New(automatic.GetCreationFinish().AsTime())
	alphaWait(t)(e.clusters.Restore(ctx, pit))

	snap, err := e.clusters.GetForBackup(ctx, &pgpb.GetClusterForBackupRequest{Id: srcID, BackupId: b.GetId()})
	if err != nil || snap.GetMetadata().GetName() != "source" || snap.GetSpec().GetConfig().GetTemplate().GetDisk().GetSizeGibibytes() != 30 {
		t.Errorf("GetForBackup = %v, %v", snap, err)
	}
	checkNoPassword(t, snap)
	alphaWait(t)(e.clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: srcID}))
	if _, err := e.clusters.Get(ctx, &pgpb.GetClusterRequest{Id: srcID}); err == nil {
		t.Fatal("the source cluster survived Delete")
	}
	snap, err = e.clusters.GetForBackup(ctx, &pgpb.GetClusterForBackupRequest{Id: srcID, BackupId: b.GetId()})
	if err != nil || snap.GetMetadata().GetName() != "source" {
		t.Errorf("GetForBackup after delete = %v, %v", snap, err)
	}
	gone, err := e.backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: srcID, BackupId: b.GetId()})
	if err != nil || gone.GetSourceClusterVisible() {
		t.Errorf("backup of a deleted cluster = %v, %v, want source_cluster_visible=false", gone, err)
	}
	alphaWait(t)(e.clusters.Restore(ctx, restoreReq("from-deleted", nil)))
	_, err = e.clusters.GetForBackup(ctx, &pgpb.GetClusterForBackupRequest{Id: srcID, BackupId: "postgresqlbackup-e00missing"})
	wantFailure(t, "GetForBackup of a missing backup", err, codes.NotFound)
}

func TestMSPPostgresBackupScheduleAndRetention(t *testing.T) {
	t.Parallel()
	e := startPGWithClock(t) // 2026-03-01 12:00 UTC; the default window is 00:02, retention 7d
	e.waitSweeperParked(t)
	id := e.newCluster(t, "main", nil)
	manual := e.onDemandBackup(t, id)
	count := func() (auto, onDemand int) {
		for _, b := range e.listBackups(t, id) {
			if b.GetOnDemand() {
				onDemand++
			} else {
				auto++
			}
		}
		return auto, onDemand
	}
	if a, o := count(); a != 1 || o != 1 {
		t.Fatalf("backups after create = %d automatic, %d on-demand, want 1 and 1", a, o)
	}

	e.advance(t, 11*time.Hour+55*time.Minute) // 23:55, before the window
	if a, _ := count(); a != 1 {
		t.Errorf("automatic backups before the window = %d, want 1", a)
	}
	e.advance(t, 10*time.Minute) // 00:05 next day
	if a, _ := count(); a != 2 {
		t.Errorf("automatic backups after the window = %d, want 2", a)
	}
	e.advance(t, time.Hour)
	if a, _ := count(); a != 2 {
		t.Errorf("automatic backups later the same day = %d, want 2", a)
	}

	// Ten more days: automatic backups older than 7 days are deleted, the
	// on-demand one stays.
	for range 10 {
		e.advance(t, 24*time.Hour)
	}
	now := e.clk.Now()
	for _, b := range e.listBackups(t, id) {
		if b.GetOnDemand() {
			continue
		}
		if age := now.Sub(b.GetCreationStart().AsTime()); age > 7*24*time.Hour+time.Hour {
			t.Errorf("automatic backup %s is %v old, past the retention", b.GetId(), age)
		}
	}
	if _, err := e.backups.Get(t.Context(), &pgpb.GetBackupRequest{ClusterId: id, BackupId: manual.GetId()}); err != nil {
		t.Errorf("the on-demand backup expired: %v", err)
	}
	if a, o := count(); a < 6 || o != 1 {
		t.Errorf("backups after retention = %d automatic, %d on-demand", a, o)
	}

	// Without the cluster nothing new is taken and the automatic backups
	// run out, the on-demand one stays until it is deleted.
	alphaWait(t)(e.clusters.Delete(t.Context(), &pgpb.DeleteClusterRequest{Id: id}))
	for range 9 {
		e.advance(t, 24*time.Hour)
	}
	if a, o := count(); a != 0 || o != 1 {
		t.Errorf("backups of the deleted cluster = %d automatic, %d on-demand, want 0 and 1", a, o)
	}
	alphaWait(t)(e.backups.Delete(t.Context(), &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: manual.GetId()}))
	if a, o := count(); a+o != 0 {
		t.Errorf("backups after deleting the last one = %d", a+o)
	}
}

func TestMSPPostgresBackupWindowAndRetentionSettings(t *testing.T) {
	t.Parallel()
	e := startPGWithClock(t)
	e.waitSweeperParked(t)
	id := e.newCluster(t, "main", func(s *pgpb.ClusterSpec) {
		s.Backup = &pgpb.BackupSpec{BackupWindowStart: "13:30:00", RetentionPolicy: "7d"}
	})
	e.advance(t, 30*time.Minute) // 12:30
	if n := len(e.listBackups(t, id)); n != 1 {
		t.Fatalf("backups before the window = %d, want 1", n)
	}
	e.advance(t, 2*time.Hour) // 14:30, window 13:30 passed
	if n := len(e.listBackups(t, id)); n != 2 {
		t.Errorf("backups after the window = %d, want 2", n)
	}
}
