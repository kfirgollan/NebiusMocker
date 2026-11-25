package dal_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/audit/dal"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

func export(id, name string) *auditv2pb.AuditEventExport {
	return &auditv2pb.AuditEventExport{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: "project-1", Name: name, ResourceVersion: 1},
		Status:   &auditv2pb.AuditEventExportStatus{State: auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_RUNNING},
	}
}

func inRegion(e *auditv2pb.AuditEventExport, region string) domain.Export {
	return domain.Export{Region: region, Resource: e}
}

func TestExportRepoRoundTripAndErrors(t *testing.T) {
	t.Parallel()
	r := dal.NewExportRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, inRegion(export("e1", "a"), "eu-north1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, inRegion(export("e1", "b"), "eu-north1")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate id err = %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, inRegion(export("e2", "a"), "eu-north1")); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name err = %v, want AlreadyExists", err)
	}
	if err := r.Create(ctx, inRegion(export("e3", ""), "eu-north1")); err != nil {
		t.Fatalf("unnamed export: %v", err)
	}
	if err := r.Create(ctx, inRegion(export("e4", ""), "eu-north1")); err != nil {
		t.Fatalf("second unnamed export: %v", err)
	}
	got, err := r.Get(ctx, "e1")
	if err != nil || got.Region != "eu-north1" || !proto.Equal(got.Resource, export("e1", "a")) {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := r.Get(ctx, "missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get missing err = %v, want NotFound", err)
	}
}

func TestExportRepoModify(t *testing.T) {
	t.Parallel()
	r := dal.NewExportRepo(store.NewMemory())
	ctx := t.Context()
	if err := r.Create(ctx, inRegion(export("e1", "a"), "eu-north1")); err != nil {
		t.Fatal(err)
	}

	updated, err := r.Modify(ctx, "e1", func(e *auditv2pb.AuditEventExport) error {
		e.Status.State = auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE
		e.Metadata.ResourceVersion++
		return nil
	})
	if err != nil || updated.GetStatus().GetState() != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("Modify = %v, %v", updated, err)
	}
	stored, err := r.Get(ctx, "e1")
	if err != nil || stored.Region != "eu-north1" || !proto.Equal(stored.Resource, updated) || stored.Resource.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("stored = %v, %v; want the modified export", stored, err)
	}

	sentinel := errors.New("abort")
	_, err = r.Modify(ctx, "e1", func(e *auditv2pb.AuditEventExport) error {
		e.Status.Message = "must not be stored"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Modify err = %v, want the callback's error", err)
	}
	if stored, _ := r.Get(ctx, "e1"); stored.Resource.GetStatus().GetMessage() != "" {
		t.Fatalf("an aborted Modify was stored: %v", stored)
	}

	if _, err := r.Modify(ctx, "missing", func(*auditv2pb.AuditEventExport) error { return nil }); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Modify missing err = %v, want NotFound", err)
	}
}

func TestExportRepoPaging(t *testing.T) {
	t.Parallel()
	r := dal.NewExportRepo(store.NewMemory())
	for i := range 3 {
		if err := r.Create(t.Context(), inRegion(export(fmt.Sprintf("e%d", i+1), fmt.Sprintf("n%d", i+1)), "eu-north1")); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := r.List(t.Context(), "project-1", "eu-north1", 2, "")
	if err != nil || len(p1.Items) != 2 || p1.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", p1, err)
	}
	p2, err := r.List(t.Context(), "project-1", "eu-north1", 2, p1.NextPageToken)
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != "" || p2.Items[0].GetMetadata().GetId() != "e3" {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	if other, err := r.List(t.Context(), "project-2", "eu-north1", 2, ""); err != nil || len(other.Items) != 0 {
		t.Fatalf("other parent = %+v, %v", other, err)
	}
	if _, err := r.List(t.Context(), "project-1", "eu-north1", 2, "%%%"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("bad token err = %v, want BadRequest", err)
	}
}

func TestExportRepoListFiltersByRegion(t *testing.T) {
	t.Parallel()
	r := dal.NewExportRepo(store.NewMemory())
	ctx := t.Context()
	// Interleave regions so a page of one region spans records of the other.
	regions := []string{"eu-north1", "eu-west1", "eu-west1", "eu-north1", "eu-west1", "eu-west1", "eu-north1"}
	for i, rg := range regions {
		if err := r.Create(ctx, inRegion(export(fmt.Sprintf("e%d", i+1), ""), rg)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	token := ""
	for range 5 {
		page, err := r.List(ctx, "project-1", "eu-west1", 3, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Items {
			got = append(got, e.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if want := []string{"e2", "e3", "e5", "e6"}; !slices.Equal(got, want) {
		t.Fatalf("eu-west1 exports = %v, want %v", got, want)
	}
	if page, err := r.List(ctx, "project-1", "us-central1", 3, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("empty region = %+v, %v", page, err)
	}
}
