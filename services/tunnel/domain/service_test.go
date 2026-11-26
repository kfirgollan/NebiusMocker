package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/tunnel/domain"
)

func TestCreateStoresTunnel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	got := e.newTunnel(t, "web", "Web", "demo")
	md := got.GetMetadata()
	if !strings.HasPrefix(md.GetId(), domain.KindTunnel+"-") || md.GetParentId() != projectID || md.GetName() != "web" {
		t.Errorf("metadata = %v", md)
	}
	if md.GetResourceVersion() != 1 || !md.GetCreatedAt().AsTime().Equal(t0) || !md.GetUpdatedAt().AsTime().Equal(t0) {
		t.Errorf("version/timestamps = %v", md)
	}
	if got.GetSpec().GetTitle() != "Web" || got.GetSpec().GetDescription() != "demo" {
		t.Errorf("spec = %v", got.GetSpec())
	}
	st := got.GetStatus()
	if st.GetState() != tunnelpb.TunnelStatus_CREATED ||
		st.GetConnectionState() != tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED || len(st.GetServices()) != 0 {
		t.Errorf("status = %v, want CREATED, DISCONNECTED, no services", st)
	}
}

func TestCreateUnnamedAllowsDuplicateNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.newTunnel(t, "same", "", "")
	b := e.newTunnel(t, "same", "", "")
	if a.GetMetadata().GetId() == b.GetMetadata().GetId() {
		t.Fatal("two tunnels share an ID")
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		md    *commonpb.ResourceMetadata
		kind  errs.Kind
		field string
	}{
		{"no parent", &commonpb.ResourceMetadata{}, errs.KindBadRequest, "metadata.parent_id"},
		{"nil metadata", nil, errs.KindBadRequest, "metadata.parent_id"},
		{"not a project", &commonpb.ResourceMetadata{ParentId: "tenant-e00x"}, errs.KindBadRequest, "metadata.parent_id"},
		{"missing project", &commonpb.ResourceMetadata{ParentId: missingProject}, errs.KindNotFound, ""},
		{"other project", &commonpb.ResourceMetadata{ParentId: otherProjectID}, errs.KindUnknown, ""},
		{"id set", &commonpb.ResourceMetadata{ParentId: projectID, Id: "applicationtunnel-x"}, errs.KindBadRequest, "metadata.id"},
		{"version set", &commonpb.ResourceMetadata{ParentId: projectID, ResourceVersion: 3}, errs.KindBadRequest, "metadata.resource_version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			_, err := e.svc.Create(t.Context(), domain.CreateCmd{Metadata: tt.md})
			wantKind(t, "create", err, tt.kind)
			if tt.field != "" && !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name %s", err, tt.field)
			}
			if want := map[bool]int{true: 1, false: 0}[tt.kind == errs.KindUnknown]; len(e.tunnels.items) != want {
				t.Errorf("stored %d tunnels, want %d", len(e.tunnels.items), want)
			}
		})
	}
}

func TestUpdateFullReplace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tn := e.newTunnel(t, "web", "Web", "demo")
	id := tn.GetMetadata().GetId()
	e.clk.Advance(time.Minute)

	_, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "web2", Labels: map[string]string{"env": "prod"}},
		Spec:     &tunnelpb.TunnelSpec{Title: "Web 2", Description: "demo"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := e.svc.Get(t.Context(), id)
	md := got.GetMetadata()
	if md.GetName() != "web2" || md.GetLabels()["env"] != "prod" || got.GetSpec().GetTitle() != "Web 2" {
		t.Errorf("update not applied: %v", got)
	}
	if md.GetResourceVersion() != 2 || !md.GetUpdatedAt().AsTime().Equal(t0.Add(time.Minute)) || !md.GetCreatedAt().AsTime().Equal(t0) {
		t.Errorf("version/timestamps = %v", md)
	}
	if md.GetParentId() != projectID {
		t.Errorf("parent changed: %v", md)
	}

	// The same content again changes nothing.
	e.clk.Advance(time.Minute)
	if _, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "web2", Labels: map[string]string{"env": "prod"}, ResourceVersion: 2},
		Spec:     &tunnelpb.TunnelSpec{Title: "Web 2", Description: "demo"},
	}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	got, _ = e.svc.Get(t.Context(), id)
	if got.GetMetadata().GetResourceVersion() != 2 || !got.GetMetadata().GetUpdatedAt().AsTime().Equal(t0.Add(time.Minute)) {
		t.Errorf("no-op update bumped the tunnel: %v", got.GetMetadata())
	}
}

func TestUpdateResetMaskClearsFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tn := e.newTunnel(t, "web", "Web", "demo")
	id := tn.GetMetadata().GetId()

	// Without the reset mask an empty description is "not sent": it stays.
	if _, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "web"},
		Spec:     &tunnelpb.TunnelSpec{Title: "Web"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := e.svc.Get(t.Context(), id)
	if got.GetSpec().GetDescription() != "demo" || got.GetMetadata().GetLabels()["env"] != "dev" {
		t.Errorf("fields not sent were cleared without a reset mask: %v", got)
	}

	if _, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id, Name: "web"},
		Spec:      &tunnelpb.TunnelSpec{Title: "Web"},
		ResetMask: mask.ParseMust("spec.description,metadata.labels"),
	}); err != nil {
		t.Fatalf("update with reset mask: %v", err)
	}
	got, _ = e.svc.Get(t.Context(), id)
	if got.GetSpec().GetDescription() != "" || len(got.GetMetadata().GetLabels()) != 0 || got.GetSpec().GetTitle() != "Web" {
		t.Errorf("reset mask not applied: %v", got)
	}
}

func TestUpdateResetMaskKeepsServerFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tn := e.newTunnel(t, "web", "Web", "")
	id := tn.GetMetadata().GetId()
	if _, err := e.svc.Update(t.Context(), domain.UpdateCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id},
		Spec:      &tunnelpb.TunnelSpec{Title: "New"},
		ResetMask: mask.ParseMust("metadata.created_at,metadata.parent_id,status"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := e.svc.Get(t.Context(), id)
	if got.GetMetadata().GetParentId() != projectID || got.GetMetadata().GetCreatedAt() == nil ||
		got.GetStatus().GetConnectionState() != tunnelpb.ConnectionState_CONNECTION_STATE_DISCONNECTED {
		t.Errorf("server-owned fields were reset: %v", got)
	}
}

func TestUpdateErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newTunnel(t, "web", "Web", "").GetMetadata().GetId()
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		kind errs.Kind
	}{
		{"no id", &commonpb.ResourceMetadata{}, errs.KindBadRequest},
		{"missing", &commonpb.ResourceMetadata{Id: "applicationtunnel-e00missing"}, errs.KindNotFound},
		{"move", &commonpb.ResourceMetadata{Id: id, ParentId: otherProjectID}, errs.KindBadRequest},
		{"stale version", &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}, errs.KindConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.svc.Update(t.Context(), domain.UpdateCmd{Metadata: tt.md, Spec: &tunnelpb.TunnelSpec{}})
			wantKind(t, "update", err, tt.kind)
		})
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newTunnel(t, "web", "", "").GetMetadata().GetId()
	if _, err := e.svc.Delete(t.Context(), id, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := e.svc.Get(t.Context(), id)
	wantKind(t, "get after delete", err, errs.KindNotFound)
	_, err = e.svc.Delete(t.Context(), id, nil)
	wantKind(t, "second delete", err, errs.KindNotFound)
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var ids []string
	for range 3 {
		ids = append(ids, e.newTunnel(t, "", "", "").GetMetadata().GetId())
	}
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("pagination does not terminate")
		}
		page, err := e.svc.List(t.Context(), domain.ListCmd{ParentID: projectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, it := range page.Items {
			got = append(got, it.GetMetadata().GetId())
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("listed %v, want %v", got, ids)
	}
	page, err := e.svc.List(t.Context(), domain.ListCmd{ParentID: otherProjectID})
	if err != nil || len(page.Items) != 0 {
		t.Errorf("other project = %v, %v; want empty", page.Items, err)
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, tt := range []struct {
		name string
		cmd  domain.ListCmd
		kind errs.Kind
	}{
		{"filter", domain.ListCmd{ParentID: projectID, Filter: "x"}, errs.KindBadRequest},
		{"page size", domain.ListCmd{ParentID: projectID, PageSize: -1}, errs.KindBadRequest},
		{"token", domain.ListCmd{ParentID: projectID, PageToken: "!!"}, errs.KindBadRequest},
		{"no parent", domain.ListCmd{}, errs.KindBadRequest},
		{"missing parent", domain.ListCmd{ParentID: missingProject}, errs.KindNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.svc.List(t.Context(), tt.cmd)
			wantKind(t, "list", err, tt.kind)
		})
	}
}
