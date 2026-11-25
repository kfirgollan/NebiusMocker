//go:build e2e

package vpc

import (
	"slices"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type cliResource struct {
	Metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"metadata"`
}

// TestCLIFindNetworkAndSubnetIDs runs the commands of
// https://docs.nebius.com/vpc/networking/resources with the official CLI: list
// networks and subnets and look them up by name to get their IDs.
func TestCLIFindNetworkAndSubnetIDs(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	for _, kind := range []struct{ resource, name string }{
		{"network", "default-network"},
		{"subnet", "default-subnet"},
	} {
		var list struct {
			Items []cliResource `json:"items"`
		}
		cli.JSON(t, &list, "vpc", kind.resource, "list")
		i := slices.IndexFunc(list.Items, func(r cliResource) bool { return r.Metadata.Name == kind.name })
		if i < 0 {
			t.Fatalf("nebius vpc %s list = %+v, want %s", kind.resource, list.Items, kind.name)
		}
		var got cliResource
		cli.JSON(t, &got, "vpc", kind.resource, "get-by-name", "--name", kind.name)
		if got.Metadata.ID == "" || got.Metadata.ID != list.Items[i].Metadata.ID {
			t.Fatalf("nebius vpc %s get-by-name = %+v, want id %s", kind.resource, got, list.Items[i].Metadata.ID)
		}
	}
}
