package domain_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// --- AccessKey *ByAwsID ---

func TestAccessKeyByAwsID(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "app")
	k := f.newKey(t, &iamv2pb.AccessKeySpec{Account: saAccount(sa)})
	aws := k.GetStatus().GetAwsAccessKeyId()

	if _, err := f.keys.DeactivateByAwsID(t.Context(), aws, nil); err != nil {
		t.Fatalf("DeactivateByAwsID: %v", err)
	}
	got, _ := f.keys.Get(t.Context(), k.GetMetadata().GetId())
	if got.GetStatus().GetState() != iamv2pb.AccessKeyStatus_INACTIVE {
		t.Fatalf("state after deactivate = %v", got.GetStatus().GetState())
	}
	if _, err := f.keys.ActivateByAwsID(t.Context(), aws, nil); err != nil {
		t.Fatalf("ActivateByAwsID: %v", err)
	}
	got, _ = f.keys.Get(t.Context(), k.GetMetadata().GetId())
	if got.GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE {
		t.Fatalf("state after activate = %v", got.GetStatus().GetState())
	}
	if _, err := f.keys.DeleteByAwsID(t.Context(), aws, nil); err != nil {
		t.Fatalf("DeleteByAwsID: %v", err)
	}
	if _, err := f.keys.Get(t.Context(), k.GetMetadata().GetId()); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get after delete err = %v, want NotFound", err)
	}
}

func TestAccessKeyByAwsIDErrors(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	calls := map[string]func(string) error{
		"delete": func(id string) error { _, err := f.keys.DeleteByAwsID(t.Context(), id, nil); return err },
		"activate": func(id string) error {
			_, err := f.keys.ActivateByAwsID(t.Context(), id, nil)
			return err
		},
		"deactivate": func(id string) error {
			_, err := f.keys.DeactivateByAwsID(t.Context(), id, nil)
			return err
		},
	}
	for name, call := range calls {
		if err := call("NMKUNKNOWN"); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("%s unknown id: err = %v, want NotFound", name, err)
		}
		if err := call(""); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("%s empty id: err = %v, want BadRequest", name, err)
		}
	}
}

// --- TenantService.Update ---

type updatableTenants struct {
	namedTenants
	stored  *iampb.Container
	updates int
}

func (u *updatableTenants) Get(_ context.Context, id string) (*iampb.Container, error) {
	if id != tenantID {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(u.stored), nil
}

func (u *updatableTenants) Update(_ context.Context, c *iampb.Container) error {
	if c.GetMetadata().GetName() == "taken" {
		return errs.AlreadyExists("tenant-other", "name taken")
	}
	u.stored = proto.CloneOf(c)
	u.updates++
	return nil
}

func newTenantUpdateFixture(t *testing.T) (*domain.TenantService, *updatableTenants, *clock.Manual) {
	t.Helper()
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	repo := &updatableTenants{stored: &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "default", Labels: map[string]string{"k": "v"}, ResourceVersion: 1},
		Spec:     &iampb.ContainerSpec{Region: "eu-north1"},
		Status:   &iampb.ContainerStatus{ContainerState: iampb.State_ACTIVE},
	}}
	svc, err := domain.NewTenantService(repo, newProjectFixture(t).vis, inlineOps{}, clk)
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, clk
}

func TestTenantUpdate(t *testing.T) {
	t.Parallel()
	svc, repo, clk := newTenantUpdateFixture(t)
	clk.Advance(time.Minute)

	// Rename keeps labels (not in the mask) and bumps the version.
	if _, err := svc.Update(t.Context(), domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "renamed"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	md := repo.stored.GetMetadata()
	if md.GetName() != "renamed" || md.GetLabels()["k"] != "v" || md.GetResourceVersion() != 2 {
		t.Fatalf("after rename: %v", md)
	}
	if repo.stored.GetSpec().GetRegion() != "eu-north1" || repo.stored.GetStatus().GetContainerState() != iampb.State_ACTIVE {
		t.Fatalf("spec/status changed: %v %v", repo.stored.GetSpec(), repo.stored.GetStatus())
	}

	// Labels in the reset mask and absent from the request are cleared.
	if _, err := svc.Update(t.Context(), domain.UpdateTenantCmd{
		Metadata:  &commonpb.ResourceMetadata{Id: tenantID, Name: "renamed"},
		ResetMask: mask.ParseMust("metadata.labels"),
	}); err != nil {
		t.Fatalf("Update with reset mask: %v", err)
	}
	if got := repo.stored.GetMetadata(); len(got.GetLabels()) != 0 || got.GetResourceVersion() != 3 {
		t.Fatalf("after reset: %v", got)
	}

	// A no-op update neither writes nor bumps the version.
	before := repo.updates
	if _, err := svc.Update(t.Context(), domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "renamed"}}); err != nil {
		t.Fatal(err)
	}
	if repo.updates != before || repo.stored.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("no-op update wrote: updates %d version %d", repo.updates, repo.stored.GetMetadata().GetResourceVersion())
	}
}

func TestTenantUpdateRejections(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTenantUpdateFixture(t)
	tests := []struct {
		name   string
		cmd    domain.UpdateTenantCmd
		want   errs.Kind
		inText string
	}{
		{"missing id", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Name: "a"}}, errs.KindBadRequest, "metadata.id"},
		{"unknown id", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: "tenant-nope", Name: "a"}}, errs.KindNotFound, ""},
		{"name_prefix", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "a"}, NamePrefix: "p"}, errs.KindBadRequest, "name_prefix"},
		{"name required", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID}, ResetMask: mask.ParseMust("metadata.name")}, errs.KindBadRequest, "metadata.name"},
		{"parent change", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, ParentId: "x", Name: "a"}}, errs.KindBadRequest, "parent_id"},
		{"stale version", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "a", ResourceVersion: 9}}, errs.KindConflict, ""},
		{"name taken", domain.UpdateTenantCmd{Metadata: &commonpb.ResourceMetadata{Id: tenantID, Name: "taken"}}, errs.KindAlreadyExists, ""},
	}
	for _, tt := range tests {
		_, err := svc.Update(t.Context(), tt.cmd)
		if errs.KindOf(err) != tt.want || !strings.Contains(errText(err), tt.inText) {
			t.Errorf("%s: err = %v, want kind %v containing %q", tt.name, err, tt.want, tt.inText)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// --- SessionService.Revoke ---

// knownTUAs is a TenantUserAccountRepository that only knows Get.
type knownTUAs struct {
	domain.TenantUserAccountRepository
	ids []string
}

func (k knownTUAs) Get(_ context.Context, id string) (*iampb.TenantUserAccount, error) {
	if !slices.Contains(k.ids, id) {
		return nil, errs.NotFound(id)
	}
	return &iampb.TenantUserAccount{Metadata: &commonpb.ResourceMetadata{Id: id}}, nil
}

func TestSessionRevoke(t *testing.T) {
	t.Parallel()
	f := newCredFixture(t)
	sa := f.newSA(t, "app")
	svc, err := domain.NewSessionService(f.sar, knownTUAs{ids: []string{"tenantuseraccount-e00u"}}, f.rev)
	if err != nil {
		t.Fatal(err)
	}
	withCaller := reqctx.WithInfo(t.Context(), &reqctx.Info{Principal: reqctx.Principal{ID: "serviceaccount-caller"}})

	tests := []struct {
		name string
		auth bool // the request carries a caller
		cmd  domain.RevokeSessionCmd
		want errs.Kind // KindUnknown: success
		revs string    // principal revoked on success
	}{
		{"service account", true, domain.RevokeSessionCmd{ServiceAccountID: sa}, errs.KindUnknown, sa},
		{"tenant user account", true, domain.RevokeSessionCmd{TenantUserAccountID: "tenantuseraccount-e00u"}, errs.KindUnknown, "tenantuseraccount-e00u"},
		{"all my active", true, domain.RevokeSessionCmd{AllMyActive: true}, errs.KindUnknown, "serviceaccount-caller"},
		{"unknown service account", true, domain.RevokeSessionCmd{ServiceAccountID: "serviceaccount-nope"}, errs.KindNotFound, ""},
		{"unknown tenant user account", true, domain.RevokeSessionCmd{TenantUserAccountID: "tenantuseraccount-nope"}, errs.KindNotFound, ""},
		{"nothing set", true, domain.RevokeSessionCmd{}, errs.KindBadRequest, ""},
		{"two set", true, domain.RevokeSessionCmd{ServiceAccountID: sa, AllMyActive: true}, errs.KindBadRequest, ""},
		{"all my active, no caller", false, domain.RevokeSessionCmd{AllMyActive: true}, errs.KindUnauthenticated, ""},
	}
	for _, tt := range tests {
		f.rev.principals = nil
		ctx := t.Context()
		if tt.auth {
			ctx = withCaller
		}
		err := svc.Revoke(ctx, tt.cmd)
		if errs.KindOf(err) != tt.want {
			t.Errorf("%s: err = %v, want kind %v", tt.name, err, tt.want)
			continue
		}
		want := []string(nil)
		if tt.revs != "" {
			want = []string{tt.revs}
		}
		if !slices.Equal(f.rev.principals, want) {
			t.Errorf("%s: revoked %v, want %v", tt.name, f.rev.principals, want)
		}
	}
}
