package domain

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	testProject = "project-e00test0000000000001"
	testTenant  = "tenant-e00test00000000000001"
)

type env struct {
	regs     *fakeRegistries
	index    *fakeIndex
	backend  *fakeBackend
	registry *RegistryService
	artifact *ArtifactService
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{regs: &fakeRegistries{}, index: newFakeIndex(), backend: newFakeBackend()}
	ids := &seqIDs{}
	var err error
	e.registry, err = NewRegistryService(RegistryDeps{
		Registries: e.regs, Artifacts: e.index, Backend: e.backend,
		Hierarchy: fakeHierarchy{testProject: testTenant, testTenant: ""},
		Endpoint:  fixedEndpoint("127.0.0.1:4590"), Ops: inlineOps{}, IDs: ids,
		Clock: clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Quota: passGate{},
	})
	if err != nil {
		t.Fatalf("NewRegistryService: %v", err)
	}
	e.artifact, err = NewArtifactService(e.regs, e.index, e.backend, inlineOps{}, ids)
	if err != nil {
		t.Fatalf("NewArtifactService: %v", err)
	}
	return e
}

func (e *env) create(t *testing.T, name string) *registrypb.Registry {
	t.Helper()
	op, err := e.registry.Create(t.Context(), CreateRegistryCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name, Labels: map[string]string{"a": "1"}},
		Spec:     &registrypb.RegistrySpec{Description: "d"},
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if op.Err != nil {
		t.Fatalf("Create(%q) operation: %v", name, op.Err)
	}
	r, err := e.registry.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return r
}

func TestCreateRegistry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.create(t, "app")
	if got := r.GetStatus().GetState(); got != registrypb.RegistryStatus_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", got)
	}
	if got := r.GetStatus().GetRegistryFqdn(); got != "127.0.0.1:4590" {
		t.Errorf("registry_fqdn = %q", got)
	}
	if !e.backend.has(Namespace(r.GetMetadata().GetId())) {
		t.Errorf("backend namespace %q not created", Namespace(r.GetMetadata().GetId()))
	}
	if r.GetMetadata().GetResourceVersion() != 1 || r.GetSpec().GetDescription() != "d" {
		t.Errorf("unexpected registry %v", r)
	}
}

func TestCreateRegistryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		wantKind errs.Kind
	}{
		{"missing parent", &commonpb.ResourceMetadata{Name: "x"}, errs.KindBadRequest},
		{"tenant parent", &commonpb.ResourceMetadata{ParentId: testTenant, Name: "x"}, errs.KindBadRequest},
		{"unknown project", &commonpb.ResourceMetadata{ParentId: "project-e00nope", Name: "x"}, errs.KindNotFound},
		{"client id", &commonpb.ResourceMetadata{Id: "registry-e00x", ParentId: testProject, Name: "x"}, errs.KindBadRequest},
		{"duplicate name", &commonpb.ResourceMetadata{ParentId: testProject, Name: "taken"}, errs.KindAlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.create(t, "taken")
			_, err := e.registry.Create(t.Context(), CreateRegistryCmd{Metadata: tt.md})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
		})
	}
}

func TestCreateRegistryBackendFailureFailsOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.fail = errs.Internal("harbor down")
	op, err := e.registry.Create(t.Context(), CreateRegistryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.Err == nil {
		t.Fatal("operation succeeded, want backend failure")
	}
	r, err := e.regs.Get(t.Context(), op.ResourceID)
	if err != nil || r.GetStatus().GetState() != registrypb.RegistryStatus_CREATING {
		t.Fatalf("registry after failed create = %v, %v; want CREATING", r, err)
	}
}

func TestUpdateRegistry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.create(t, "app")
	id := r.GetMetadata().GetId()

	_, err := e.registry.Update(t.Context(), UpdateRegistryCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: id, Name: "app2"},
		Spec:      &registrypb.RegistrySpec{Description: "new"},
		ResetMask: mask.ParseMust("metadata.labels"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := e.registry.Get(t.Context(), id)
	if got.GetMetadata().GetName() != "app2" || got.GetSpec().GetDescription() != "new" || len(got.GetMetadata().GetLabels()) != 0 {
		t.Errorf("after update: %v", got)
	}
	if got.GetMetadata().GetResourceVersion() != 2 || got.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		t.Errorf("version/state after update: %v", got)
	}

	// No-op update keeps the version.
	if _, err := e.registry.Update(t.Context(), UpdateRegistryCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "app2"}, Spec: &registrypb.RegistrySpec{Description: "new"},
	}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	if got, _ := e.registry.Get(t.Context(), id); got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("no-op update bumped version to %d", got.GetMetadata().GetResourceVersion())
	}

	for name, md := range map[string]*commonpb.ResourceMetadata{
		"parent change": {Id: id, ParentId: "project-e00other"},
		"stale version": {Id: id, ResourceVersion: 1},
		"missing id":    {},
	} {
		_, err := e.registry.Update(t.Context(), UpdateRegistryCmd{Metadata: md})
		if err == nil {
			t.Errorf("Update(%s) succeeded, want error", name)
		}
	}
}

func TestDeleteRegistry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.create(t, "app")
	id := r.GetMetadata().GetId()
	e.backend.put(Namespace(id), Image{Repository: "web", Digest: "sha256:1"})

	_, err := e.registry.Delete(t.Context(), DeleteRegistryCmd{ID: id})
	if got := errs.KindOf(err); got != errs.KindBadState {
		t.Fatalf("Delete non-empty kind = %v, want BadState (err: %v)", got, err)
	}
	op, err := e.registry.Delete(t.Context(), DeleteRegistryCmd{ID: id, Force: true})
	if err != nil || op.Err != nil {
		t.Fatalf("Delete force: %v / %v", err, op.Err)
	}
	if _, err := e.registry.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete: %v, want NotFound", err)
	}
	if e.backend.has(Namespace(id)) {
		t.Error("backend namespace survived delete")
	}
	if _, err := e.registry.Delete(t.Context(), DeleteRegistryCmd{ID: id}); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second Delete: %v, want NotFound", err)
	}
}

func TestImagesCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "app").GetMetadata().GetId()
	e.backend.put(Namespace(id), Image{Repository: "a", Digest: "sha256:1"}, Image{Repository: "b", Digest: "sha256:2"})
	r, err := e.registry.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.GetStatus().GetImagesCount() != 2 || r.GetSpec().GetImagesCount() != 2 {
		t.Errorf("images_count = %d/%d, want 2", r.GetStatus().GetImagesCount(), r.GetSpec().GetImagesCount())
	}
}

func TestNamespace(t *testing.T) {
	t.Parallel()
	if got := Namespace("registry-e00abc"); got != "e00abc" {
		t.Errorf("Namespace = %q", got)
	}
	if got := RegistryIDOf("e00abc"); got != "registry-e00abc" {
		t.Errorf("RegistryIDOf = %q", got)
	}
}

func TestArtifacts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "app").GetMetadata().GetId()
	pushed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	e.backend.put(Namespace(id),
		Image{Repository: "web", Digest: "sha256:b", Tags: []string{"v2", "latest"}, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 10, PushedAt: pushed},
		Image{Repository: "api", Digest: "sha256:a", List: true},
		Image{Repository: "web", Digest: "sha256:a"},
	)

	page, err := e.artifact.List(t.Context(), ListQuery{ParentID: id, PageSize: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("first page = %d items, token %q", len(page.Items), page.NextPageToken)
	}
	rest, err := e.artifact.List(t.Context(), ListQuery{ParentID: id, PageSize: 2, PageToken: page.NextPageToken})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	all := slices.Concat(page.Items, rest.Items)
	var names []string
	for _, a := range all {
		names = append(names, a.GetName()+"@"+a.GetDigest())
	}
	if want := []string{"api@sha256:a", "web@sha256:a", "web@sha256:b"}; len(rest.Items) != 1 || rest.NextPageToken != "" || !equal(names, want) {
		t.Fatalf("listing = %v (next %q), want %v", names, rest.NextPageToken, want)
	}
	if all[0].GetType() != registrypb.Artifact_MANIFEST_LIST || all[2].GetType() != registrypb.Artifact_MANIFEST {
		t.Errorf("types = %v, %v", all[0].GetType(), all[2].GetType())
	}

	web := all[2]
	got, err := e.artifact.Get(t.Context(), web.GetId())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !equal(got.GetTags(), []string{"latest", "v2"}) || got.GetSize() != 10 || !got.GetCreatedAt().AsTime().Equal(pushed) ||
		got.GetStatus() != registrypb.Artifact_ACTIVE {
		t.Errorf("Get = %v", got)
	}
	// IDs are stable across listings.
	again, _ := e.artifact.List(t.Context(), ListQuery{ParentID: id})
	if again.Items[2].GetId() != web.GetId() {
		t.Errorf("artifact ID changed: %s → %s", web.GetId(), again.Items[2].GetId())
	}
	if reg, err := e.artifact.RegistryOf(t.Context(), web.GetId()); err != nil || reg != id {
		t.Errorf("RegistryOf = %q, %v", reg, err)
	}

	op, err := e.artifact.Delete(t.Context(), web.GetId(), nil)
	if err != nil || op.Err != nil {
		t.Fatalf("Delete: %v / %v", err, op.Err)
	}
	if _, err := e.artifact.Get(t.Context(), web.GetId()); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete: %v, want NotFound", err)
	}
	if _, err := e.artifact.List(t.Context(), ListQuery{ParentID: "registry-e00missing"}); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("List of missing registry: %v, want NotFound", err)
	}
	if _, err := e.artifact.List(t.Context(), ListQuery{ParentID: id, PageToken: "!!"}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("List with bad token: %v, want BadRequest", err)
	}
}

func TestArtifactVanishedFromBackend(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "app").GetMetadata().GetId()
	e.backend.put(Namespace(id), Image{Repository: "web", Digest: "sha256:a"})
	page, _ := e.artifact.List(t.Context(), ListQuery{ParentID: id})
	artID := page.Items[0].GetId()
	_ = e.backend.DeleteImage(t.Context(), Namespace(id), "web", "sha256:a")
	if _, err := e.artifact.Get(t.Context(), artID); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get of vanished image: %v, want NotFound", err)
	}
	if _, err := e.index.Get(t.Context(), artID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("stale index entry kept: %v", err)
	}
}

func TestAuthorizeDocker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "app").GetMetadata().GetId()
	ns := Namespace(id)
	tokens := fakeTokens{"viewer": "tenantuseraccount-v", "editor": "tenantuseraccount-e"}
	authorizer := fakeAuthorizer{"tenantuseraccount-v": {"get", "list"}, "tenantuseraccount-e": {"get", "list", "create", "delete"}}
	tests := []struct {
		name     string
		mode     authz.Mode
		req      AccessRequest
		wantKind errs.Kind
	}{
		{"no token", authz.ModeEnforce, AccessRequest{Namespace: ns, Action: ActionPull}, errs.KindUnauthenticated},
		{"bad token", authz.ModeEnforce, AccessRequest{Token: "x", Namespace: ns, Action: ActionPull}, errs.KindUnauthenticated},
		{"viewer pulls", authz.ModeEnforce, AccessRequest{Token: "viewer", Namespace: ns, Action: ActionPull}, errs.KindUnknown},
		{"viewer pushes", authz.ModeEnforce, AccessRequest{Token: "viewer", Namespace: ns, Action: ActionPush}, errs.KindPermissionDenied},
		{"viewer pushes in audit", authz.ModeAudit, AccessRequest{Token: "viewer", Namespace: ns, Action: ActionPush}, errs.KindUnknown},
		{"viewer pushes with authz off", authz.ModeOff, AccessRequest{Token: "viewer", Namespace: ns, Action: ActionPush}, errs.KindUnknown},
		{"editor pushes", authz.ModeEnforce, AccessRequest{Token: "editor", Namespace: ns, Action: ActionPush}, errs.KindUnknown},
		{"editor deletes", authz.ModeEnforce, AccessRequest{Token: "editor", Namespace: ns, Action: ActionDelete}, errs.KindUnknown},
		{"unknown registry", authz.ModeEnforce, AccessRequest{Token: "editor", Namespace: "e00nope", Action: ActionPull}, errs.KindNotFound},
		{"no namespace", authz.ModeEnforce, AccessRequest{Token: "editor", Action: ActionPull}, errs.KindNotFound},
		{"no action", authz.ModeEnforce, AccessRequest{Token: "editor", Namespace: ns}, errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, err := NewAccessService(tokens, authorizer, e.regs, tt.mode, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewAccessService: %v", err)
			}
			if got := errs.KindOf(svc.Authorize(t.Context(), tt.req)); got != tt.wantKind {
				t.Fatalf("Authorize(%+v) kind = %v, want %v", tt.req, got, tt.wantKind)
			}
		})
	}
}

func TestListRejectsFilter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "app").GetMetadata().GetId()
	for _, f := range []string{"name = \"app\"", "anything"} {
		_, err := e.registry.List(t.Context(), ListQuery{ParentID: testProject, Filter: f})
		if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "filter") {
			t.Errorf("registry List(filter=%q) = %v, want BadRequest naming filter", f, err)
		}
		_, err = e.artifact.List(t.Context(), ListQuery{ParentID: id, Filter: f})
		if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), "filter") {
			t.Errorf("artifact List(filter=%q) = %v, want BadRequest naming filter", f, err)
		}
	}
	if page, err := e.registry.List(t.Context(), ListQuery{ParentID: testProject}); err != nil || len(page.Items) != 1 {
		t.Errorf("List without filter = %v, %v", page, err)
	}
}

func TestPermissionFor(t *testing.T) {
	t.Parallel()
	want := map[Action]authz.Permission{
		ActionPull: "registry.artifacts.get", ActionPush: "registry.artifacts.create", ActionDelete: "registry.artifacts.delete",
	}
	for a, p := range want {
		if got, ok := PermissionFor(a); !ok || got != p {
			t.Errorf("PermissionFor(%v) = %q, %v; want %q", a, got, ok, p)
		}
	}
	if _, ok := PermissionFor(ActionUnspecified); ok {
		t.Error("PermissionFor(unspecified) ok")
	}
	// Viewers may pull, editors push, through the default role catalog.
	cat := authz.DefaultCatalog()
	if !cat.Allows(authz.RoleViewer, want[ActionPull]) || cat.Allows(authz.RoleViewer, want[ActionPush]) ||
		!cat.Allows(authz.RoleEditor, want[ActionPush]) {
		t.Error("default catalog does not map viewer→pull, editor→push")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
