package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
)

func TestTenantViewer(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	ctx := t.Context()
	tenant := env.inst.TenantID

	t.Run("v1", func(t *testing.T) {
		t.Parallel()
		svc := iamsvc.NewTenantService(env.viewer)
		r, err := svc.List(ctx, &iampb.ListTenantsRequest{PageSize: 1})
		if err != nil || len(r.GetItems()) != 1 || r.GetItems()[0].GetMetadata().GetId() != tenant || r.GetNextPageToken() != "" {
			t.Fatalf("List(1) = %v, %q, %v; want exactly %s and no next token", r.GetItems(), r.GetNextPageToken(), err, tenant)
		}
		g, err := svc.Get(ctx, &iampb.GetTenantRequest{Id: tenant})
		if err != nil || g.GetMetadata().GetId() != tenant {
			t.Fatalf("Get = %v, %v", g, err)
		}
		_, err = svc.Get(ctx, &iampb.GetTenantRequest{Id: "tenant-e00doesnotexist0000"})
		assertCode(t, err, codes.NotFound)
	})

	t.Run("v2", func(t *testing.T) {
		t.Parallel()
		svc := iamv2svc.NewTenantService(env.viewer)
		r, err := svc.List(ctx, &iamv2pb.ListTenantsRequest{PageSize: 1})
		if err != nil || len(r.GetItems()) != 1 || r.GetItems()[0].GetMetadata().GetId() != tenant || r.GetNextPageToken() != "" {
			t.Fatalf("List(1) = %v, %q, %v; want exactly %s and no next token", r.GetItems(), r.GetNextPageToken(), err, tenant)
		}
		var seen []string
		for tn, err := range svc.Filter(ctx, &iamv2pb.ListTenantsRequest{PageSize: 1}) {
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			seen = append(seen, tn.GetMetadata().GetId())
		}
		if len(seen) != 1 || seen[0] != tenant {
			t.Errorf("Filter = %v, want [%s]", seen, tenant)
		}
		g, err := svc.Get(ctx, &iamv2pb.GetTenantRequest{Id: tenant})
		if err != nil || g.GetMetadata().GetId() != tenant {
			t.Fatalf("Get = %v, %v", g, err)
		}
		if g.GetStatus().GetTenantState() != iamv2pb.TenantStatus_ACTIVE {
			t.Errorf("tenant_state = %v, want ACTIVE", g.GetStatus().GetTenantState())
		}
		byName, err := svc.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{Name: "default"})
		if err != nil || byName.GetMetadata().GetId() != tenant {
			t.Errorf("GetByName(default) = %v, %v; want %s", byName.GetMetadata(), err, tenant)
		}
		own, err := svc.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{ParentId: tenant, Name: "default"})
		if err != nil || own.GetMetadata().GetId() != tenant {
			t.Errorf("GetByName(parent=own ID) = %v, %v; want %s", own.GetMetadata(), err, tenant)
		}
		_, err = svc.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{ParentId: env.inst.ProjectID, Name: "default"})
		assertCode(t, err, codes.InvalidArgument)
		_, err = svc.GetByName(ctx, &iamv2pb.GetTenantByNameRequest{Name: "nope"})
		assertCode(t, err, codes.NotFound)
	})
}

func TestTenantV2UpdateUnimplemented(t *testing.T) {
	t.Parallel()
	env := startProjEnv(t)
	_, err := iamv2svc.NewTenantService(env.admin).Update(t.Context(), &iamv2pb.UpdateTenantRequest{
		Metadata: &commonpb.ResourceMetadata{Id: env.inst.TenantID, Name: "renamed"},
	})
	assertCode(t, err, codes.Unimplemented)
}
