package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func startPGEnforcing(t *testing.T) *pgEnv {
	t.Helper()
	return startPG(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
}

func TestMSPPostgresViewerReadsButCannotWrite(t *testing.T) {
	t.Parallel()
	e := startPGEnforcing(t)
	ctx := t.Context()
	id := e.newCluster(t, "guarded", nil)
	b := e.onDemandBackup(t, id)
	clusters, backups := e.as(t, config.DefaultViewerToken)

	if _, err := clusters.Get(ctx, &pgpb.GetClusterRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "guarded"}); err != nil {
		t.Errorf("viewer GetByName: %v", err)
	}
	if l, err := clusters.List(ctx, &pgpb.ListClustersRequest{ParentId: e.project}); err != nil || len(l.GetClusters()) != 1 {
		t.Errorf("viewer List = %v, %v", l, err)
	}
	if _, err := clusters.GetForBackup(ctx, &pgpb.GetClusterForBackupRequest{Id: id, BackupId: b.GetId()}); err != nil {
		t.Errorf("viewer GetForBackup: %v", err)
	}
	if _, err := backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id, BackupId: b.GetId()}); err != nil {
		t.Errorf("viewer backup Get: %v", err)
	}
	if l, err := backups.List(ctx, &pgpb.ListBackupsRequest{ParentId: e.project}); err != nil || len(l.GetBackups()) != 2 {
		t.Errorf("viewer backup List = %v, %v", l, err)
	}
	if l, err := backups.ListByCluster(ctx, &pgpb.ListBackupsByClusterRequest{ClusterId: id}); err != nil || len(l.GetBackups()) != 2 {
		t.Errorf("viewer backup ListByCluster = %v, %v", l, err)
	}

	// Writes are denied.
	cur := e.getCluster(t, id)
	_, err := clusters.Create(ctx, e.createReq("nope", nil))
	wantFailure(t, "viewer Create", err, codes.PermissionDenied)
	_, err = clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	wantFailure(t, "viewer Update", err, codes.PermissionDenied)
	_, err = clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id})
	wantFailure(t, "viewer Stop", err, codes.PermissionDenied)
	_, err = clusters.Start(ctx, &pgpb.StartClusterRequest{Id: id})
	wantFailure(t, "viewer Start", err, codes.PermissionDenied)
	_, err = clusters.Restore(ctx, &pgpb.RestoreClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: "r"}, Spec: pgSpec(), SourceClusterId: id, BackupId: b.GetId(),
	})
	wantFailure(t, "viewer Restore", err, codes.PermissionDenied)
	_, err = clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: id})
	wantFailure(t, "viewer Delete", err, codes.PermissionDenied)
	_, err = backups.Create(ctx, &pgpb.CreateBackupRequest{ClusterId: id})
	wantFailure(t, "viewer backup Create", err, codes.PermissionDenied)
	_, err = backups.Delete(ctx, &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: b.GetId()})
	wantFailure(t, "viewer backup Delete", err, codes.PermissionDenied)

	// Nothing changed.
	if after := e.getCluster(t, id); after.GetStatus().GetPhase() != cur.GetStatus().GetPhase() || after.GetMetadata().GetResourceVersion() != cur.GetMetadata().GetResourceVersion() {
		t.Errorf("a denied call changed the cluster: %v", after)
	}
	if n := len(e.listBackups(t, id)); n != 2 {
		t.Errorf("backups = %d, want 2", n)
	}
}

func TestMSPPostgresEditorManagesClusters(t *testing.T) {
	t.Parallel()
	e := startPGEnforcing(t)
	ctx := t.Context()
	clusters, backups := e.as(t, config.DefaultEditorToken)

	id := alphaWait(t)(clusters.Create(ctx, e.createReq("edited", nil)))
	cur := e.getCluster(t, id)
	cur.Spec.Config.PublicAccess = true
	alphaWait(t)(clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()}))
	alphaWait(t)(clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id}))
	alphaWait(t)(clusters.Start(ctx, &pgpb.StartClusterRequest{Id: id}))

	alphaWait(t)(backups.Create(ctx, &pgpb.CreateBackupRequest{ClusterId: id}))
	var onDemand *pgpb.Backup
	for _, b := range e.listBackups(t, id) {
		if b.GetOnDemand() {
			onDemand = b
		}
	}
	if onDemand == nil {
		t.Fatal("editor backup Create made no on-demand backup")
	}
	restored := alphaWait(t)(clusters.Restore(ctx, &pgpb.RestoreClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.project, Name: "copy"}, Spec: pgSpec(), SourceClusterId: id, BackupId: onDemand.GetId(),
	}))
	alphaWait(t)(backups.Delete(ctx, &pgpb.DeleteBackupRequest{ClusterId: id, BackupId: onDemand.GetId()}))
	alphaWait(t)(clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: restored}))
	alphaWait(t)(clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: id}))
	if _, err := clusters.Get(ctx, &pgpb.GetClusterRequest{Id: id}); err == nil {
		t.Error("editor Delete left the cluster")
	}
}

func TestMSPPostgresBackupsOfADeletedClusterStayAuthorized(t *testing.T) {
	t.Parallel()
	e := startPGEnforcing(t)
	ctx := t.Context()
	id := e.newCluster(t, "short-lived", nil)
	b := e.listBackups(t, id)[0]
	alphaWait(t)(e.clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: id}))

	clusters, backups := e.as(t, config.DefaultViewerToken)
	if got, err := backups.Get(ctx, &pgpb.GetBackupRequest{ClusterId: id, BackupId: b.GetId()}); err != nil || got.GetSourceClusterVisible() {
		t.Errorf("viewer Get of a deleted cluster's backup = %v, %v", got, err)
	}
	if l, err := backups.ListByCluster(ctx, &pgpb.ListBackupsByClusterRequest{ClusterId: id}); err != nil || len(l.GetBackups()) != 1 {
		t.Errorf("viewer ListByCluster = %v, %v", l, err)
	}
	if _, err := clusters.GetForBackup(ctx, &pgpb.GetClusterForBackupRequest{Id: id, BackupId: b.GetId()}); err != nil {
		t.Errorf("viewer GetForBackup after delete: %v", err)
	}
	// Anonymous callers get nothing.
	anon, _ := e.as(t, "")
	_, err := anon.Get(ctx, &pgpb.GetClusterRequest{Id: id})
	wantFailure(t, "anonymous Get", err, codes.Unauthenticated)
}
