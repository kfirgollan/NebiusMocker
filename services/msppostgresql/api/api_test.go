package api

import (
	"testing"

	"google.golang.org/protobuf/proto"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

func TestClusterViewNeverShowsThePassword(t *testing.T) {
	t.Parallel()
	c := &pgpb.Cluster{Spec: &pgpb.ClusterSpec{Bootstrap: &pgpb.BootstrapSpec{UserName: "u", UserPassword: "secret"}}}
	got := clusterView(c)
	if got.GetSpec().GetBootstrap().GetUserPassword() != "" || got.GetSpec().GetBootstrap().GetUserName() != "u" {
		t.Errorf("view = %v", got)
	}
	if c.GetSpec().GetBootstrap().GetUserPassword() != "secret" {
		t.Error("the view modified the stored cluster")
	}
	clean := &pgpb.Cluster{Spec: &pgpb.ClusterSpec{}}
	if clusterView(clean) != clean {
		t.Error("a cluster without a password was copied")
	}
}

func TestBackupViewsKeepOrderAndToken(t *testing.T) {
	t.Parallel()
	page := domain.Page[domain.BackupRecord]{
		Items:         []domain.BackupRecord{{Backup: &pgpb.Backup{Id: "b-1"}}, {Backup: &pgpb.Backup{Id: "b-2"}}},
		NextPageToken: "next",
	}
	got := backupViews(page)
	if got.GetNextPageToken() != "next" || len(got.GetBackups()) != 2 || got.GetBackups()[0].GetId() != "b-1" || !proto.Equal(got.GetBackups()[1], page.Items[1].Backup) {
		t.Errorf("view = %v", got)
	}
}

func TestEveryRPCHasAnAuthzRule(t *testing.T) {
	t.Parallel()
	rules := authz.NewRules()
	RegisterAuthzRules(rules)
	for _, m := range []string{
		"ClusterService/Get", "ClusterService/GetByName", "ClusterService/GetForBackup", "ClusterService/List", "ClusterService/Create",
		"ClusterService/Delete", "ClusterService/Update", "ClusterService/Restore", "ClusterService/Stop", "ClusterService/Start",
		"BackupService/Get", "BackupService/List", "BackupService/ListByCluster", "BackupService/Create", "BackupService/Delete",
	} {
		if _, ok := rules.Lookup("/nebius.msp.postgresql.v1alpha1." + m); !ok {
			t.Errorf("no authz rule for %s", m)
		}
	}
	for method, want := range map[string]authz.Permission{
		"ClusterService/Stop":    "postgresql.clusters.stop",
		"ClusterService/Start":   "postgresql.clusters.start",
		"ClusterService/Restore": "postgresql.clusters.create",
		"BackupService/Create":   "postgresql.backups.create",
		"BackupService/Delete":   "postgresql.backups.delete",
		"BackupService/List":     "postgresql.backups.list",
	} {
		if r, _ := rules.Lookup("/nebius.msp.postgresql.v1alpha1." + method); r.Permission != want {
			t.Errorf("%s needs %q, want %q", method, r.Permission, want)
		}
	}
}
