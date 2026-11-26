package dal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/dal"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func cluster(id, parent, name string) *pgpb.Cluster {
	return &pgpb.Cluster{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &pgpb.ClusterSpec{NetworkId: "vpcnetwork-1", Config: &pgpb.ConfigSpec{Version: "16"}},
		Status:   &pgpb.ClusterStatus{Phase: 2},
	}
}

func backup(id, clusterID, projectID string, start time.Time) domain.BackupRecord {
	return domain.BackupRecord{
		Backup:    &pgpb.Backup{Id: id, SourceClusterId: clusterID, CreationStart: timestamppb.New(start), OnDemand: true, SizeBytes: 7},
		ProjectID: projectID, Done: true, RetentionDays: 7, Cluster: cluster(clusterID, projectID, "src"),
	}
}

func TestClusterRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	c := cluster("postgresql-1", project, "main")
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "postgresql-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(c, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, project, "main")
	if err != nil || byName.GetMetadata().GetId() != "postgresql-1" {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	if _, err := r.GetByName(ctx, project2, "main"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName in another project: %v, want NotFound", err)
	}

	c.Status.Phase = 3
	if err := r.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := r.Get(ctx, "postgresql-1"); got.GetStatus().GetPhase() != 3 {
		t.Errorf("phase after update = %v", got.GetStatus().GetPhase())
	}
	if err := r.Update(ctx, cluster("postgresql-9", project, "x")); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Update of a missing cluster: %v, want NotFound", err)
	}

	if err := r.Delete(ctx, "postgresql-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "postgresql-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second Delete: %v, want NotFound", err)
	}
}

func TestClusterRepoUniqueNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	if err := r.Create(ctx, cluster("postgresql-1", project, "main")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, cluster("postgresql-2", project, "main")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate name: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, cluster("postgresql-1", project, "other")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate ID: %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, cluster("postgresql-3", project2, "main")); err != nil {
		t.Errorf("the same name in another project: %v", err)
	}
}

func TestClusterRepoListPaginates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewClusterRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, cluster(fmt.Sprintf("postgresql-%d", i), project, fmt.Sprintf("c%d", i))); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if err := r.Create(ctx, cluster("postgresql-x", project2, "x")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var names []string
	token := ""
	for pages := 0; ; pages++ {
		page, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, c := range page.Items {
			names = append(names, c.GetMetadata().GetName())
		}
		if token = page.NextPageToken; token == "" {
			if pages != 2 {
				t.Errorf("pages = %d, want 3", pages+1)
			}
			break
		}
	}
	if diff := cmp.Diff([]string{"c0", "c1", "c2", "c3", "c4"}, names); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
	if _, err := r.List(ctx, project, 2, "!!"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("malformed token: %v, want BadRequest", err)
	}
	all, err := r.All(ctx)
	if err != nil || len(all) != 6 {
		t.Errorf("All = %d clusters, %v, want 6", len(all), err)
	}
}

func TestBackupRepoRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewBackupRepo(store.NewMemory())
	want := backup("postgresqlbackup-1", "postgresql-1", project, t0)
	if err := r.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := r.Get(ctx, "postgresql-1", "postgresqlbackup-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("record mismatch (-want +got):\n%s", diff)
	}
	if _, err := r.Get(ctx, "postgresql-2", "postgresqlbackup-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get under another cluster: %v, want NotFound", err)
	}

	want.Done, want.Backup.SourceClusterVisible = false, true
	if err := r.Put(ctx, want); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if got, _ := r.Get(ctx, "postgresql-1", "postgresqlbackup-1"); got.Done || !got.Backup.GetSourceClusterVisible() {
		t.Errorf("record after the second Put = %+v", got)
	}

	if err := r.Delete(ctx, "postgresql-2", "postgresqlbackup-1"); err != nil {
		t.Errorf("Delete under another cluster: %v", err)
	}
	if _, err := r.Get(ctx, "postgresql-1", "postgresqlbackup-1"); err != nil {
		t.Errorf("a Delete under another cluster removed the backup: %v", err)
	}
	if err := r.Delete(ctx, "postgresql-1", "postgresqlbackup-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "postgresql-1", "postgresqlbackup-1"); err != nil {
		t.Errorf("Delete of a missing backup: %v, want nil", err)
	}
}

func TestBackupRepoListOrderAndPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewBackupRepo(store.NewMemory())
	put := func(b domain.BackupRecord) {
		t.Helper()
		if err := r.Put(ctx, b); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Equal creation_start sorts by cluster ID, then backup ID.
	put(backup("b-2", "c-1", project, t0.Add(time.Hour)))
	put(backup("b-1", "c-1", project, t0.Add(time.Hour)))
	put(backup("b-3", "c-0", project, t0.Add(time.Hour)))
	put(backup("b-4", "c-1", project, t0))
	put(backup("b-5", "c-9", project2, t0.Add(5*time.Hour)))

	var got []string
	token := ""
	for {
		page, err := r.ListByProject(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("ListByProject: %v", err)
		}
		for _, b := range page.Items {
			got = append(got, b.Backup.GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if diff := cmp.Diff([]string{"b-3", "b-1", "b-2", "b-4"}, got); diff != "" {
		t.Errorf("project order (-want +got):\n%s", diff)
	}

	page, err := r.ListByCluster(ctx, "c-1", 2, "")
	if err != nil || len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("ListByCluster page 1 = %v, %v", page, err)
	}
	next, err := r.ListByCluster(ctx, "c-1", 2, page.NextPageToken)
	if err != nil || len(next.Items) != 1 || next.Items[0].Backup.GetId() != "b-4" || next.NextPageToken != "" {
		t.Errorf("ListByCluster page 2 = %v, %v", next, err)
	}
	if all, err := r.All(ctx); err != nil || len(all) != 5 {
		t.Errorf("All = %d, %v, want 5", len(all), err)
	}
	if _, err := r.ListByProject(ctx, project, 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("malformed token: %v, want BadRequest", err)
	}
}

func TestNetworkBindingRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewNetworkBindingRepo(store.NewMemory())
	if _, err := r.Get(ctx, "postgresql-1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get of a missing binding: %v, want NotFound", err)
	}
	if err := r.Set(ctx, "postgresql-1", "vpcnetwork-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := r.Get(ctx, "postgresql-1"); err != nil || got != "vpcnetwork-1" {
		t.Errorf("Get = %q, %v", got, err)
	}
	if err := r.Delete(ctx, "postgresql-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "postgresql-1"); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
}
