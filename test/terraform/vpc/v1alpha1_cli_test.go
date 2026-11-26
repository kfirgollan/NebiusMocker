//go:build e2e

package vpc

import (
	"slices"
	"testing"

	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// cliAlphaResource is the part of a v1alpha1 resource the CLI test reads.
type cliAlphaResource struct {
	Metadata struct {
		ID     string            `json:"id"`
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status map[string]any `json:"status"`
}

type cliAlphaList struct {
	Items []cliAlphaResource `json:"items"`
}

func (l cliAlphaList) find(t *testing.T, what, name string) cliAlphaResource {
	t.Helper()
	i := slices.IndexFunc(l.Items, func(r cliAlphaResource) bool { return r.Metadata.Name == name })
	if i < 0 {
		t.Fatalf("%s = %+v, want an item named %s", what, l.Items, name)
	}
	return l.Items[i]
}

// TestV1alpha1CLI exercises nebius.vpc.v1alpha1 with the official nebius CLI:
// the read-only network, subnet, pool and scope commands and the allocation
// create/get/update/delete flow, checking results with the v1alpha1 Go SDK.
func TestV1alpha1CLI(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New()}, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	a := newAlphaClients(t, inst)
	p := inst.ProjectID
	ctx := t.Context()

	// Network: list, get-by-name, get-default agree with the SDK.
	var nets cliAlphaList
	cli.JSON(t, &nets, "vpc", "v1alpha1", "network", "list")
	net := nets.find(t, "network list", "default-network")
	var byName cliAlphaResource
	var def struct {
		Network cliAlphaResource `json:"network"`
		Subnet  cliAlphaResource `json:"subnet"`
	}
	cli.JSON(t, &byName, "vpc", "v1alpha1", "network", "get-by-name", "--name", "default-network")
	// get-default takes --parent-id explicitly: `nebius vpc v1alpha1 network
	// get-default --help` lists "--parent-id (string) [required]", and without
	// it the CLI sends an empty GetDefaultNetworkRequest (it does not fill the
	// profile's parent_id here, unlike list and get-by-name; the Go SDK does
	// not either). The proto marks parent_id required, so the API rejects that
	// request with InvalidArgument "parent_id: value is required", as the mock
	// does.
	cli.JSON(t, &def, "vpc", "v1alpha1", "network", "get-default", "--parent-id", p)
	if byName.Metadata.ID != net.Metadata.ID || def.Network.Metadata.ID != net.Metadata.ID || def.Subnet.Metadata.Name != "default-subnet" {
		t.Fatalf("network ids: list %s get-by-name %s get-default %+v, want identical and the default subnet", net.Metadata.ID, byName.Metadata.ID, def)
	}
	sdkNet, err := a.nets.GetDefault(ctx, &vpcv1alpha1pb.GetDefaultNetworkRequest{ParentId: p})
	if err != nil || sdkNet.GetNetwork().GetMetadata().GetId() != net.Metadata.ID {
		t.Fatalf("SDK GetDefault network = %v (%v), want %s", sdkNet.GetNetwork().GetMetadata().GetId(), err, net.Metadata.ID)
	}

	// Subnet: list, get-by-name, list-by-network.
	var subs cliAlphaList
	cli.JSON(t, &subs, "vpc", "v1alpha1", "subnet", "list")
	sub := subs.find(t, "subnet list", "default-subnet")
	var subByName cliAlphaResource
	cli.JSON(t, &subByName, "vpc", "v1alpha1", "subnet", "get-by-name", "--name", "default-subnet")
	if subByName.Metadata.ID != sub.Metadata.ID {
		t.Fatalf("subnet get-by-name id = %s, want %s", subByName.Metadata.ID, sub.Metadata.ID)
	}
	var subsByNet cliAlphaList
	cli.JSON(t, &subsByNet, "vpc", "v1alpha1", "subnet", "list-by-network", "--network-id", net.Metadata.ID)
	subsByNet.find(t, "subnet list-by-network", "default-subnet")
	sdkSub, err := a.subs.Get(ctx, &vpcv1alpha1pb.GetSubnetRequest{Id: sub.Metadata.ID})
	if err != nil || sdkSub.GetSpec().GetNetworkId() != net.Metadata.ID {
		t.Fatalf("SDK Get subnet = %v (%v), want network %s", sdkSub.GetSpec(), err, net.Metadata.ID)
	}

	// Pool: list and get.
	var pools cliAlphaList
	cli.JSON(t, &pools, "vpc", "v1alpha1", "pool", "list")
	pool := pools.find(t, "pool list", "default-network-pool")
	pools.find(t, "pool list", "default-public-pool")
	var poolGot cliAlphaResource
	cli.JSON(t, &poolGot, "vpc", "v1alpha1", "pool", "get", "--id", pool.Metadata.ID)
	sdkPool, err := a.pools.Get(ctx, &vpcv1alpha1pb.GetPoolRequest{Id: pool.Metadata.ID})
	if err != nil || poolGot.Metadata.Name != sdkPool.GetMetadata().GetName() {
		t.Fatalf("pool get = %+v, SDK %v (%v)", poolGot, sdkPool.GetMetadata(), err)
	}

	// Scope: list and get-by-name.
	var scopes cliAlphaList
	cli.JSON(t, &scopes, "vpc", "v1alpha1", "scope", "list")
	scopes.find(t, "scope list", "default-private-scope")
	scopes.find(t, "scope list", "default-public-scope")
	var scope cliAlphaResource
	cli.JSON(t, &scope, "vpc", "v1alpha1", "scope", "get-by-name", "--name", "default-public-scope")
	sdkScope, err := a.scopes.Get(ctx, &vpcv1alpha1pb.GetScopeRequest{Id: scope.Metadata.ID})
	if err != nil || sdkScope.GetSpec().GetType() != vpcv1alpha1pb.ScopeSpec_PUBLIC {
		t.Fatalf("SDK Get scope = %v (%v), want PUBLIC", sdkScope.GetSpec(), err)
	}

	// Allocation: create, get, update labels, delete.
	var created cliAlphaResource
	cli.JSON(t, &created, "vpc", "v1alpha1", "allocation", "create",
		"--parent-id", p, "--name", "cli-alpha-alloc", "--labels", "team=cli",
		"--ipv4-private-subnet-id", sub.Metadata.ID)
	// create returns the finished operation's resource or the operation; find the id via the SDK.
	got, err := a.allocs.GetByName(ctx, &vpcv1alpha1pb.GetAllocationByNameRequest{ParentId: p, Name: "cli-alpha-alloc"})
	if err != nil {
		t.Fatalf("SDK GetByName after CLI create: %v", err)
	}
	id := got.GetMetadata().GetId()
	if got.GetStatus().GetState() != vpcv1alpha1pb.AllocationStatus_ALLOCATED || got.GetMetadata().GetLabels()["team"] != "cli" ||
		got.GetSpec().GetIpv4Private().GetSubnetId() != sub.Metadata.ID {
		t.Fatalf("CLI-created allocation = %v, want ALLOCATED, label team=cli, subnet %s", got, sub.Metadata.ID)
	}

	var gotCLI cliAlphaResource
	cli.JSON(t, &gotCLI, "vpc", "v1alpha1", "allocation", "get", "--id", id)
	if gotCLI.Metadata.ID != id || gotCLI.Metadata.Name != "cli-alpha-alloc" || gotCLI.Status["state"] != "ALLOCATED" {
		t.Fatalf("allocation get = %+v, want id %s ALLOCATED", gotCLI, id)
	}

	// The CLI sends a patch, metadata {id, labels} with no X-ResetMask: the
	// name must be kept, not rejected by the name format rule or cleared.
	if _, err := cli.Run(ctx, "vpc", "v1alpha1", "allocation", "update", "--id", id, "--labels", "team=cli2,env=dev"); err != nil {
		t.Fatalf("allocation update: %v", err)
	}
	got = a.allocation(t, id)
	if got.GetMetadata().GetId() != id || got.GetMetadata().GetName() != "cli-alpha-alloc" ||
		got.GetMetadata().GetLabels()["team"] != "cli2" || got.GetMetadata().GetLabels()["env"] != "dev" {
		t.Fatalf("after CLI update metadata = %v, want same id, name cli-alpha-alloc, labels team=cli2 env=dev", got.GetMetadata())
	}
	// The v1 command sends the same patch.
	if _, err := cli.Run(ctx, "vpc", "allocation", "update", "--id", id, "--labels", "team=cli3"); err != nil {
		t.Fatalf("v1 allocation update: %v", err)
	}
	if got = a.allocation(t, id); got.GetMetadata().GetName() != "cli-alpha-alloc" || got.GetMetadata().GetLabels()["team"] != "cli3" {
		t.Fatalf("after v1 CLI update metadata = %v, want name cli-alpha-alloc, label team=cli3", got.GetMetadata())
	}

	if _, err := cli.Run(ctx, "vpc", "v1alpha1", "allocation", "delete", "--id", id); err != nil {
		t.Fatalf("allocation delete: %v", err)
	}
	a.allocationGone(t, id)
}
