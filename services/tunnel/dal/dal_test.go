package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/dal"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func tunnel(id, parent, name string) *tunnelpb.Tunnel {
	return &tunnelpb.Tunnel{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec:     &tunnelpb.TunnelSpec{Title: "Web", Description: "demo"},
		Status: &tunnelpb.TunnelStatus{
			State: tunnelpb.TunnelStatus_CREATED, ConnectionState: tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED,
		},
	}
}

func TestTunnelRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewTunnelRepo(store.NewMemory())
	tn := tunnel("applicationtunnel-1", project, "")
	if err := r.Create(ctx, tn); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "applicationtunnel-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(tn, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	if err := r.Create(ctx, tn); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("duplicate Create kind = %v, want AlreadyExists", errs.KindOf(err))
	}

	tn.Spec.Title = "Web 2"
	if err := r.Update(ctx, tn); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = r.Get(ctx, "applicationtunnel-1")
	if got.GetSpec().GetTitle() != "Web 2" {
		t.Errorf("Update not stored: %v", got)
	}
	if err := r.Update(ctx, tunnel("applicationtunnel-9", project, "")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update missing kind = %v, want NotFound", errs.KindOf(err))
	}

	if err := r.Delete(ctx, "applicationtunnel-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "applicationtunnel-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get after Delete kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "applicationtunnel-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("second Delete kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestTunnelsAreUnnamed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewTunnelRepo(store.NewMemory())
	for _, id := range []string{"applicationtunnel-1", "applicationtunnel-2"} {
		if err := r.Create(ctx, tunnel(id, project, "same-name")); err != nil {
			t.Fatalf("Create %s with a repeated name: %v", id, err)
		}
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewTunnelRepo(store.NewMemory())
	for _, c := range []struct{ id, parent string }{
		{"applicationtunnel-1", project}, {"applicationtunnel-2", project2}, {"applicationtunnel-3", project},
	} {
		if err := r.Create(ctx, tunnel(c.id, c.parent, "")); err != nil {
			t.Fatal(err)
		}
	}
	in, err := r.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range in {
		ids = append(ids, x.GetMetadata().GetId())
	}
	if diff := cmp.Diff([]string{"applicationtunnel-1", "applicationtunnel-3"}, ids); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}
}
