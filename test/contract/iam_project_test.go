package contract

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// TestProjectGet covers ProjectService.Get on the seeded project, as
// `nebius iam project get` uses it (e.g. the Object Storage quick start).
func TestProjectGet(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	projects := iamsvc.NewProjectService(inst.SDK(t))
	p, err := projects.Get(t.Context(), &iampb.GetProjectRequest{Id: inst.ProjectID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.GetMetadata().GetId() != inst.ProjectID || p.GetMetadata().GetParentId() != inst.TenantID {
		t.Fatalf("Get = %v; want project %s in tenant %s", p.GetMetadata(), inst.ProjectID, inst.TenantID)
	}
	if _, err := projects.Get(t.Context(), &iampb.GetProjectRequest{Id: "project-e00doesnotexist0000"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get unknown: %v, want NotFound", err)
	}
}
