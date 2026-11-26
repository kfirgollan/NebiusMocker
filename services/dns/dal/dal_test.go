package dal_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/dns/dal"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func zone(id, parent, name, domainName string) *dnspb.Zone {
	return &dnspb.Zone{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &dnspb.ZoneSpec{DomainName: domainName},
		Status:   &dnspb.ZoneStatus{},
	}
}

func record(id, zoneID, name, data string) *dnspb.Record {
	return &dnspb.Record{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: zoneID, Name: name, ResourceVersion: 1},
		Spec:     &dnspb.RecordSpec{RelativeName: "www", Type: dnspb.RecordSpec_A, Data: data},
		Status:   &dnspb.RecordStatus{EffectiveFqdn: "www.my.example."},
	}
}

func TestZoneRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewZoneRepo(store.NewMemory())
	z := zone("dnszone-1", project, "main", "my.example.")
	if err := r.Create(ctx, z); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "dnszone-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(z, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, project, "main")
	if err != nil || byName.GetMetadata().GetId() != "dnszone-1" {
		t.Errorf("GetByName = %v, %v, want dnszone-1", byName, err)
	}
	if _, err := r.GetByName(ctx, project2, "main"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(other project) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Create(ctx, z); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(same id) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}

	z.Spec.SoaSpec = &dnspb.SoaSpec{NegativeTtl: 60}
	if err := r.Update(ctx, z); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ = r.Get(ctx, "dnszone-1"); got.GetSpec().GetSoaSpec().GetNegativeTtl() != 60 {
		t.Errorf("negative_ttl after update = %d, want 60", got.GetSpec().GetSoaSpec().GetNegativeTtl())
	}
	if err := r.Update(ctx, zone("dnszone-x", project, "", "x.example.")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}

	if err := r.Delete(ctx, "dnszone-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "dnszone-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "dnszone-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestZoneNameUniquePerProject(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewZoneRepo(store.NewMemory())
	if err := r.Create(ctx, zone("dnszone-1", project, "main", "a.example.")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := r.Create(ctx, zone("dnszone-2", project, "main", "b.example."))
	if errs.KindOf(err) != errs.KindAlreadyExists {
		t.Fatalf("Create(same name, same project) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Create(ctx, zone("dnszone-3", project2, "main", "b.example.")); err != nil {
		t.Errorf("Create(same name, other project): %v", err)
	}
	// Nameless zones never collide.
	for i := range 3 {
		if err := r.Create(ctx, zone(fmt.Sprintf("dnszone-n%d", i), project, "", "n.example.")); err != nil {
			t.Errorf("Create(nameless %d): %v", i, err)
		}
	}
	// Renaming onto a taken name fails; renaming to a free one works.
	if err := r.Create(ctx, zone("dnszone-4", project, "other", "c.example.")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Update(ctx, zone("dnszone-4", project, "main", "c.example.")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Update(to a taken name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Update(ctx, zone("dnszone-4", project, "free", "c.example.")); err != nil {
		t.Errorf("Update(to a free name): %v", err)
	}
}

func TestZoneListAndAll(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewZoneRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, zone(fmt.Sprintf("dnszone-%d", i), project, "", fmt.Sprintf("z%d.example.", i))); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if err := r.Create(ctx, zone("dnszone-o", project2, "", "o.example.")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var got []string
	token := ""
	pages := 0
	for {
		page, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, z := range page.Items {
			got = append(got, z.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	want := []string{"dnszone-0", "dnszone-1", "dnszone-2", "dnszone-3", "dnszone-4"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("paged List order (-want +got):\n%s", diff)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (2+2+1)", pages)
	}
	if page, err := r.List(ctx, project, 5, ""); err != nil || len(page.Items) != 5 || page.NextPageToken != "" {
		t.Errorf("List(exactly one page) = %d items, token %q, err %v; want 5, empty, nil", len(page.Items), page.NextPageToken, err)
	}
	if page, err := r.List(ctx, "project-empty", 10, ""); err != nil || len(page.Items) != 0 || page.NextPageToken != "" {
		t.Errorf("List(empty project) = %v, %v, want no items", page, err)
	}
	if _, err := r.List(ctx, project, 2, "%%%"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest (err: %v)", errs.KindOf(err), err)
	}

	all, err := r.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 6 || all[0].GetMetadata().GetId() != "dnszone-0" || all[5].GetMetadata().GetId() != "dnszone-o" {
		t.Errorf("All = %d zones, want 6 across projects in creation order", len(all))
	}
}

func TestRecordRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewRecordRepo(store.NewMemory())
	rec := record("dnsrecord-1", "dnszone-1", "web", "10.0.0.1")
	if err := r.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "dnsrecord-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, "dnszone-1", "web")
	if err != nil || byName.GetMetadata().GetId() != "dnsrecord-1" {
		t.Errorf("GetByName = %v, %v, want dnsrecord-1", byName, err)
	}
	if _, err := r.GetByName(ctx, "dnszone-2", "web"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(other zone) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := r.Get(ctx, "dnsrecord-x"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Create(ctx, rec); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(same id) kind = %v, want AlreadyExists", errs.KindOf(err))
	}

	rec.Spec.Data = "10.0.0.2"
	if err := r.Update(ctx, rec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ = r.Get(ctx, "dnsrecord-1"); got.GetSpec().GetData() != "10.0.0.2" {
		t.Errorf("data after update = %q, want 10.0.0.2", got.GetSpec().GetData())
	}
	if err := r.Update(ctx, record("dnsrecord-x", "dnszone-1", "", "10.0.0.3")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "dnsrecord-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, "dnsrecord-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestRecordNamesUniquePerZoneAndNamelessDoNotCollide(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewRecordRepo(store.NewMemory())
	if err := r.Create(ctx, record("dnsrecord-1", "dnszone-1", "web", "10.0.0.1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, record("dnsrecord-2", "dnszone-1", "web", "10.0.0.2")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(same name, same zone) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Create(ctx, record("dnsrecord-3", "dnszone-2", "web", "10.0.0.3")); err != nil {
		t.Errorf("Create(same name, other zone): %v", err)
	}
	for i := range 4 {
		if err := r.Create(ctx, record(fmt.Sprintf("dnsrecord-n%d", i), "dnszone-1", "", "10.0.1.1")); err != nil {
			t.Errorf("Create(nameless %d): %v", i, err)
		}
	}
	if err := r.Update(ctx, record("dnsrecord-n0", "dnszone-1", "web", "10.0.1.1")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Update(onto a taken name) kind = %v, want AlreadyExists", errs.KindOf(err))
	}
	if _, err := r.GetByName(ctx, "dnszone-1", "missing"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestRecordListAndAll(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewRecordRepo(store.NewMemory())
	for i := range 7 {
		zoneID := "dnszone-1"
		if i%3 == 2 {
			zoneID = "dnszone-2"
		}
		if err := r.Create(ctx, record(fmt.Sprintf("dnsrecord-%d", i), zoneID, "", "10.0.0.1")); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	all, err := r.All(ctx, "dnszone-1")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var ids []string
	for _, rec := range all {
		ids = append(ids, rec.GetMetadata().GetId())
	}
	if diff := cmp.Diff([]string{"dnsrecord-0", "dnsrecord-1", "dnsrecord-3", "dnsrecord-4", "dnsrecord-6"}, ids); diff != "" {
		t.Errorf("All(zone-1) (-want +got):\n%s", diff)
	}
	if none, err := r.All(ctx, "dnszone-9"); err != nil || len(none) != 0 {
		t.Errorf("All(empty zone) = %v, %v, want none", none, err)
	}

	var paged []string
	token := ""
	for {
		page, err := r.List(ctx, "dnszone-1", 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, rec := range page.Items {
			paged = append(paged, rec.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if diff := cmp.Diff(ids, paged); diff != "" {
		t.Errorf("paged List differs from All (-all +paged):\n%s", diff)
	}
	if _, err := r.List(ctx, "dnszone-1", 2, "not-a-token"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest (err: %v)", errs.KindOf(err), err)
	}
}
