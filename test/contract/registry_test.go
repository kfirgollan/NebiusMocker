package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/services/registry/backend/memory"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

type registryEnv struct {
	inst      *testkit.Instance
	backend   *memory.Backend
	registry  registrysvc.RegistryService
	artifacts registrysvc.ArtifactService
}

func startRegistry(t *testing.T, opts ...testkit.Option) *registryEnv {
	t.Helper()
	backend := memory.New()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), registry.New(registry.WithBackend(backend))}, opts...)
	sdk := inst.SDK(t)
	return &registryEnv{
		inst: inst, backend: backend,
		registry: registrysvc.NewRegistryService(sdk), artifacts: registrysvc.NewArtifactService(sdk),
	}
}

func (e *registryEnv) create(t *testing.T, svc registrysvc.RegistryService, name string) *registrypb.Registry {
	t.Helper()
	op, err := svc.Create(t.Context(), &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: name, Labels: map[string]string{"team": "core"}},
		Spec:     &registrypb.RegistrySpec{Description: "images"},
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if op.ResourceID() == "" {
		t.Fatalf("Create(%q): empty resource_id", name)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%q) wait: %v", name, err)
	}
	r, err := svc.Get(t.Context(), &registrypb.GetRegistryRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatalf("Get(%s): %v", op.ResourceID(), err)
	}
	return r
}

func TestRegistryLifecycle(t *testing.T) {
	t.Parallel()
	e := startRegistry(t, testkit.WithOperationDelay(50*time.Millisecond))
	ctx := t.Context()

	r := e.create(t, e.registry, "app")
	id := r.GetMetadata().GetId()
	if r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE || r.GetStatus().GetRegistryFqdn() == "" ||
		r.GetMetadata().GetResourceVersion() != 1 || r.GetSpec().GetDescription() != "images" {
		t.Fatalf("created registry = %v", r)
	}
	byName, err := e.registry.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "app"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	_, err = e.registry.Create(ctx, &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "app"}, Spec: &registrypb.RegistrySpec{},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate Create: %v, want AlreadyExists", err)
	}
	_, err = e.registry.Create(ctx, &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00mock00000000missing", Name: "x"}, Spec: &registrypb.RegistrySpec{},
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("Create in missing project: %v, want NotFound", err)
	}

	e.create(t, e.registry, "second")
	var seen []string
	token := ""
	for {
		page, err := e.registry.List(ctx, &registrypb.ListRegistriesRequest{ParentId: e.inst.ProjectID, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, x := range page.GetItems() {
			seen = append(seen, x.GetMetadata().GetName())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(seen) != 2 || seen[0] != "app" || seen[1] != "second" {
		t.Errorf("paged List = %v, want [app second]", seen)
	}

	// Update: new description, labels cleared through the reset mask.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.registry.Update(maskCtx, &registrypb.UpdateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: e.inst.ProjectID, Name: "app"},
		Spec:     &registrypb.RegistrySpec{Description: "changed"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Update wait: %v", err)
	}
	got, _ := e.registry.Get(ctx, &registrypb.GetRegistryRequest{Id: id})
	if got.GetSpec().GetDescription() != "changed" || len(got.GetMetadata().GetLabels()) != 0 || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after Update: %v", got)
	}

	// A non-empty registry is not deleted without force.
	if err := e.backend.Put(domain.Namespace(id), domain.Image{Repository: "web", Digest: "sha256:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.registry.Delete(ctx, &registrypb.DeleteRegistryRequest{Id: id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Delete non-empty: %v, want FailedPrecondition", err)
	}
	dop, err := e.registry.Delete(ctx, &registrypb.DeleteRegistryRequest{Id: id, Force: true})
	if err != nil {
		t.Fatalf("Delete force: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	if _, err := e.registry.Get(ctx, &registrypb.GetRegistryRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete: %v, want NotFound", err)
	}
}

func TestArtifacts(t *testing.T) {
	t.Parallel()
	e := startRegistry(t)
	ctx := t.Context()
	id := e.create(t, e.registry, "app").GetMetadata().GetId()
	ns := domain.Namespace(id)
	pushed := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, img := range []domain.Image{
		{Repository: "web", Digest: "sha256:aa", Tags: []string{"v1", "latest"}, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 100, PushedAt: pushed},
		{Repository: "api", Digest: "sha256:bb", Tags: []string{"v2"}},
	} {
		if err := e.backend.Put(ns, img); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := e.registry.Get(ctx, &registrypb.GetRegistryRequest{Id: id})
	if r.GetStatus().GetImagesCount() != 2 {
		t.Errorf("images_count = %d, want 2", r.GetStatus().GetImagesCount())
	}

	var all []*registrypb.Artifact
	for a, err := range e.artifacts.Filter(ctx, &registrypb.ListArtifactsRequest{ParentId: id, PageSize: 1}) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		all = append(all, a)
	}
	if len(all) != 2 || all[0].GetName() != "api" || all[1].GetName() != "web" {
		t.Fatalf("artifacts = %v", all)
	}
	web, err := e.artifacts.Get(ctx, &registrypb.GetArtifactRequest{Id: all[1].GetId()})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if web.GetDigest() != "sha256:aa" || len(web.GetTags()) != 2 || web.GetSize() != 100 || web.GetType() != registrypb.Artifact_MANIFEST ||
		!web.GetCreatedAt().AsTime().Equal(pushed) {
		t.Errorf("Get = %v", web)
	}
	op, err := e.artifacts.Delete(ctx, &registrypb.DeleteArtifactRequest{Id: web.GetId()})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	if _, err := e.artifacts.Get(ctx, &registrypb.GetArtifactRequest{Id: web.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete: %v, want NotFound", err)
	}
	if _, err := e.artifacts.List(ctx, &registrypb.ListArtifactsRequest{ParentId: "registry-e00mock000000missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("List in missing registry: %v, want NotFound", err)
	}
}

func TestRegistryAuthz(t *testing.T) {
	t.Parallel()
	e := startRegistry(t, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	ctx := t.Context()
	viewer := e.inst.SDKAs(t, config.DefaultViewerToken)
	editor := e.inst.SDKAs(t, config.DefaultEditorToken)

	r := e.create(t, registrysvc.NewRegistryService(editor), "by-editor")
	id := r.GetMetadata().GetId()
	if err := e.backend.Put(domain.Namespace(id), domain.Image{Repository: "web", Digest: "sha256:1"}); err != nil {
		t.Fatal(err)
	}

	vreg := registrysvc.NewRegistryService(viewer)
	if _, err := vreg.Get(ctx, &registrypb.GetRegistryRequest{Id: id}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := vreg.List(ctx, &registrypb.ListRegistriesRequest{ParentId: e.inst.ProjectID}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	_, err := vreg.Create(ctx, &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.ProjectID, Name: "nope"}, Spec: &registrypb.RegistrySpec{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("viewer Create: %v, want PermissionDenied", err)
	}
	vart := registrysvc.NewArtifactService(viewer)
	page, err := vart.List(ctx, &registrypb.ListArtifactsRequest{ParentId: id})
	if err != nil || len(page.GetItems()) != 1 {
		t.Fatalf("viewer List artifacts = %v, %v", page, err)
	}
	artID := page.GetItems()[0].GetId()
	if _, err := vart.Get(ctx, &registrypb.GetArtifactRequest{Id: artID}); err != nil {
		t.Errorf("viewer Get artifact: %v", err)
	}
	if _, err := vart.Delete(ctx, &registrypb.DeleteArtifactRequest{Id: artID}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("viewer Delete artifact: %v, want PermissionDenied", err)
	}
	op, err := registrysvc.NewArtifactService(editor).Delete(ctx, &registrypb.DeleteArtifactRequest{Id: artID})
	if err != nil {
		t.Fatalf("editor Delete artifact: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("editor Delete wait: %v", err)
	}
	if _, err := vreg.Delete(ctx, &registrypb.DeleteRegistryRequest{Id: id}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("viewer Delete registry: %v, want PermissionDenied", err)
	}
}

func TestProfile(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{"sa-token": "serviceaccount-e00mock000000000robot"}))
	resp, err := iamsvc.NewProfileService(inst.SDKAs(t, config.DefaultViewerToken)).Get(t.Context(), &iampb.GetProfileRequest{})
	if err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	up := resp.GetUserProfile()
	if up.GetId() != config.DefaultViewerPrincipal || len(up.GetTenants()) != 1 || up.GetTenants()[0].GetTenantId() != inst.TenantID {
		t.Errorf("user profile = %v", resp)
	}
	resp, err = iamsvc.NewProfileService(inst.SDKAs(t, "sa-token")).Get(t.Context(), &iampb.GetProfileRequest{})
	if err != nil {
		t.Fatalf("Get SA profile: %v", err)
	}
	if got := resp.GetServiceAccountProfile().GetInfo().GetMetadata().GetId(); got != "serviceaccount-e00mock000000000robot" {
		t.Errorf("service account profile = %v", resp)
	}
}
