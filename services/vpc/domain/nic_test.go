package domain

import (
	"errors"
	"slices"
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const nicInstance = "computeinstance-e00test"

func (e *env) nicBinding(sn *vpcpb.Subnet, name string) NICBinding {
	return NICBinding{InstanceID: nicInstance, ProjectID: testProject, Name: name, SubnetID: idOf(sn)}
}

func (e *env) bind(t *testing.T, b NICBinding) NICAddresses {
	t.Helper()
	out, err := e.svc.NICs().BindInterface(t.Context(), b)
	if err != nil {
		t.Fatalf("BindInterface: %v", err)
	}
	return out
}

func (e *env) allocs(t *testing.T) []*vpcpb.Allocation {
	t.Helper()
	all, err := e.svc.c.r.Allocations.All(t.Context(), func(*vpcpb.Allocation) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func (e *env) alloc(t *testing.T, id string) *vpcpb.Allocation {
	t.Helper()
	a, err := e.svc.Allocations().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get allocation %s: %v", id, err)
	}
	return a
}

func wantAssigned(t *testing.T, a *vpcpb.Allocation, typ vpcpb.NetworkInterfaceAssignment_Type, subnetID string) {
	t.Helper()
	ni := a.GetStatus().GetAssignment().GetNetworkInterface()
	if a.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED || ni.GetInstanceId() != nicInstance || ni.GetType() != typ ||
		a.GetStatus().GetDetails().GetSubnetId() != subnetID {
		t.Errorf("allocation %s status = %v, want ASSIGNED %v in %s", idOf(a), a.GetStatus(), typ, subnetID)
	}
}

func TestNICEphemeralPrivate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	out := e.bind(t, e.nicBinding(sn, "eth0"))
	if out.PrivateAddress != "10.0.0.2" || out.NetworkID != idOf(e.def.Network) {
		t.Fatalf("addresses = %+v, want 10.0.0.2", out)
	}
	a := e.alloc(t, out.PrivateAllocationID)
	wantAssigned(t, a, vpcpb.NetworkInterfaceAssignment_PRIMARY, idOf(sn))
	if a.GetStatus().GetStatic() || a.GetMetadata().GetParentId() != testProject || a.GetMetadata().GetName() != nicInstance+"-eth0-private" {
		t.Errorf("ephemeral allocation = %v", a)
	}
	// A second NIC takes the next address.
	if got := e.bind(t, e.nicBinding(sn, "eth1")).PrivateAddress; got != "10.0.0.3" {
		t.Errorf("second NIC address = %s, want 10.0.0.3", got)
	}
	// The subnet cannot be deleted while NIC addresses live in it.
	wantKind(t, "subnet delete", tryDelete(t.Context(), e.svc.Subnets().Delete, idOf(sn)), errs.KindBadState)
}

func TestNICDefaultSecurityGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	def, err := e.svc.SecurityGroups().repo.All(t.Context(), func(sg *vpcpb.SecurityGroup) bool { return sg.GetStatus().GetDefault() })
	if err != nil || len(def) != 1 {
		t.Fatalf("default security groups = %v, %v", def, err)
	}
	if got := e.bind(t, e.nicBinding(sn, "eth0")).SecurityGroupIDs; !slices.Equal(got, []string{idOf(def[0])}) {
		t.Errorf("security groups = %v, want the default %s", got, idOf(def[0]))
	}
	custom := e.createSecurityGroup(t, "web", idOf(e.def.Network))
	b := e.nicBinding(sn, "eth0")
	b.SecurityGroupIDs = []string{idOf(custom)}
	if got := e.bind(t, b).SecurityGroupIDs; !slices.Equal(got, []string{idOf(custom)}) {
		t.Errorf("security groups = %v, want %s", got, idOf(custom))
	}
}

func TestNICBindExistingPrivate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	pre := e.createAlloc(t, "mine", idOf(sn), "10.0.0.50/32")
	b := e.nicBinding(sn, "eth0")
	b.PrivateAllocationID = idOf(pre)
	out := e.bind(t, b)
	if out.PrivateAddress != "10.0.0.50" || out.PrivateAllocationID != idOf(pre) {
		t.Fatalf("addresses = %+v", out)
	}
	wantAssigned(t, e.alloc(t, idOf(pre)), vpcpb.NetworkInterfaceAssignment_PRIMARY, idOf(sn))
	if n := len(e.allocs(t)); n != 1 {
		t.Errorf("allocations = %d, want 1 (nothing created)", n)
	}
	// Idempotent.
	if again := e.bind(t, b); again.PrivateAddress != out.PrivateAddress || len(e.allocs(t)) != 1 {
		t.Errorf("second bind = %+v, allocations %d", again, len(e.allocs(t)))
	}
	// Deleting an assigned allocation is refused.
	wantKind(t, "delete assigned", tryDelete(t.Context(), e.svc.Allocations().Delete, idOf(pre)), errs.KindBadState)
	// Releasing unassigns and keeps it.
	if err := e.svc.NICs().ReleaseInterface(t.Context(), nicInstance, "eth0"); err != nil {
		t.Fatal(err)
	}
	a := e.alloc(t, idOf(pre))
	if a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED || a.GetStatus().GetAssignment() != nil || a.GetStatus().GetDetails().GetSubnetId() != idOf(sn) {
		t.Errorf("after release status = %v", a.GetStatus())
	}
}

func TestNICIdempotentEphemeral(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	b := e.nicBinding(sn, "eth0")
	first := e.bind(t, b)
	second := e.bind(t, b)
	if first.PrivateAddress != second.PrivateAddress || first.PrivateAllocationID != second.PrivateAllocationID || len(e.allocs(t)) != 1 {
		t.Errorf("first %+v, second %+v, allocations %d", first, second, len(e.allocs(t)))
	}
}

func TestNICPublic(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	b := e.nicBinding(sn, "eth0")
	b.Public = &NICPublic{}
	out := e.bind(t, b)
	if out.PublicAddress == "" || out.PublicAllocationID == "" {
		t.Fatalf("addresses = %+v, want a public address", out)
	}
	wantAssigned(t, e.alloc(t, out.PublicAllocationID), vpcpb.NetworkInterfaceAssignment_PUBLIC, idOf(sn))
	// Binding again keeps the same public address.
	if again := e.bind(t, b); again.PublicAllocationID != out.PublicAllocationID || len(e.allocs(t)) != 2 {
		t.Errorf("second bind = %+v, allocations %d", again, len(e.allocs(t)))
	}
	// Public=nil releases and deletes the ephemeral address.
	b.Public = nil
	if got := e.bind(t, b); got.PublicAddress != "" || got.PublicAllocationID != "" {
		t.Errorf("addresses = %+v, want no public address", got)
	}
	if _, err := e.svc.Allocations().Get(t.Context(), out.PublicAllocationID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ephemeral public allocation after release: err = %v, want NotFound", err)
	}
}

func TestNICExplicitPublicSurvivesRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	pub, err := e.tryAlloc(t, "ip", publicAllocSpec("/32"))
	if err != nil {
		t.Fatal(err)
	}
	b := e.nicBinding(sn, "eth0")
	b.Public = &NICPublic{AllocationID: idOf(pub)}
	out := e.bind(t, b)
	if out.PublicAllocationID != idOf(pub) {
		t.Fatalf("addresses = %+v", out)
	}
	wantAssigned(t, e.alloc(t, idOf(pub)), vpcpb.NetworkInterfaceAssignment_PUBLIC, idOf(sn))
	if err := e.svc.NICs().ReleaseInterface(t.Context(), nicInstance, "eth0"); err != nil {
		t.Fatal(err)
	}
	a := e.alloc(t, idOf(pub))
	if a.GetStatus().GetState() != vpcpb.AllocationStatus_ALLOCATED || a.GetStatus().GetAssignment() != nil || a.GetStatus().GetDetails().GetSubnetId() != "" {
		t.Errorf("after release status = %v", a.GetStatus())
	}
	// The ephemeral private address went away with the NIC.
	if n := len(e.allocs(t)); n != 1 {
		t.Errorf("allocations = %d, want only the explicit public one", n)
	}
}

func TestNICAliases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	a1 := e.createAlloc(t, "a1", idOf(sn), "10.0.0.60/32")
	a2 := e.createAlloc(t, "a2", idOf(sn), "10.0.0.61/32")
	b := e.nicBinding(sn, "eth0")
	b.AliasAllocationIDs = []string{idOf(a1), idOf(a2)}
	if got := e.bind(t, b).AliasCIDRs; !slices.Equal(got, []string{"10.0.0.60/32", "10.0.0.61/32"}) {
		t.Fatalf("alias CIDRs = %v", got)
	}
	wantAssigned(t, e.alloc(t, idOf(a1)), vpcpb.NetworkInterfaceAssignment_ALIAS, idOf(sn))
	b.AliasAllocationIDs = []string{idOf(a2)}
	if got := e.bind(t, b).AliasCIDRs; !slices.Equal(got, []string{"10.0.0.61/32"}) {
		t.Fatalf("alias CIDRs = %v", got)
	}
	if got := e.alloc(t, idOf(a1)).GetStatus(); got.GetAssignment() != nil || got.GetState() != vpcpb.AllocationStatus_ALLOCATED {
		t.Errorf("removed alias status = %v", got)
	}
}

func TestNICPrivateChangeReleasesPrevious(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	b := e.nicBinding(sn, "eth0")
	first := e.bind(t, b)
	pre := e.createAlloc(t, "mine", idOf(sn), "10.0.0.50/32")
	b.PrivateAllocationID = idOf(pre)
	out := e.bind(t, b)
	if out.PrivateAddress != "10.0.0.50" {
		t.Fatalf("addresses = %+v", out)
	}
	if _, err := e.svc.Allocations().Get(t.Context(), first.PrivateAllocationID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("previous ephemeral allocation: err = %v, want NotFound", err)
	}
}

func TestNICRejections(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	other := e.createSubnet(t, "other", "10.0.1.0/24")
	otherNet := e.createNetwork(t, "other-net")
	foreignSG := e.createSecurityGroup(t, "foreign", idOf(otherNet))
	inOther := e.createAlloc(t, "in-other", idOf(other), "10.0.1.5/32")
	inSpaceNoSubnet := e.createAlloc(t, "spare", idOf(sn), "10.0.0.70/32")
	outside := mustCreate(t, e.svc.Allocations().Create, e.svc.Allocations().Get, testProject, "outside", &vpcpb.AllocationSpec{
		IpSpec: &vpcpb.AllocationSpec_Ipv4Private{Ipv4Private: &vpcpb.IPv4PrivateAllocationSpec{
			Cidr: "10.0.9.9/32", Pool: &vpcpb.IPv4PrivateAllocationSpec_PoolId{PoolId: idOf(e.poolByName(t, DefaultPrivatePoolName))},
		}},
	})
	pub, err := e.tryAlloc(t, "pub", publicAllocSpec("/32"))
	if err != nil {
		t.Fatal(err)
	}
	holder := e.createAlloc(t, "held", idOf(sn), "10.0.0.80/32")
	hb := e.nicBinding(sn, "other-nic")
	hb.PrivateAllocationID = idOf(holder)
	e.bind(t, hb)

	tests := []struct {
		name   string
		mutate func(*NICBinding)
		kind   errs.Kind
		field  string
	}{
		{"security group of another network", func(b *NICBinding) { b.SecurityGroupIDs = []string{idOf(foreignSG)} }, errs.KindBadRequest, "security_groups[0].id"},
		{"missing security group", func(b *NICBinding) { b.SecurityGroupIDs = []string{"vpcsecuritygroup-nope"} }, errs.KindNotFound, ""},
		{"missing private allocation", func(b *NICBinding) { b.PrivateAllocationID = "vpcallocation-nope" }, errs.KindNotFound, ""},
		{"allocation of another subnet", func(b *NICBinding) { b.PrivateAllocationID = idOf(inOther) }, errs.KindBadRequest, "ip_address.allocation_id"},
		{"allocation outside the subnet space", func(b *NICBinding) { b.PrivateAllocationID = idOf(outside) }, errs.KindBadRequest, "ip_address.allocation_id"},
		{"public allocation as private", func(b *NICBinding) { b.PrivateAllocationID = idOf(pub) }, errs.KindBadRequest, "ip_address.allocation_id"},
		{"private allocation as public", func(b *NICBinding) { b.Public = &NICPublic{AllocationID: idOf(inSpaceNoSubnet)} }, errs.KindBadRequest, "public_ip_address.allocation_id"},
		{"assigned to another NIC", func(b *NICBinding) { b.PrivateAllocationID = idOf(holder) }, errs.KindBadState, ""},
		{"alias assigned to another NIC", func(b *NICBinding) { b.AliasAllocationIDs = []string{idOf(holder)} }, errs.KindBadState, ""},
		{"alias twice", func(b *NICBinding) {
			b.AliasAllocationIDs = []string{idOf(inSpaceNoSubnet), idOf(inSpaceNoSubnet)}
		}, errs.KindBadRequest, "aliases[1].allocation_id"},
		{"subnet in another project", func(b *NICBinding) { b.ProjectID = "project-e00other" }, errs.KindBadRequest, "subnet_id"},
		{"missing subnet", func(b *NICBinding) { b.SubnetID = "vpcsubnet-nope" }, errs.KindNotFound, ""},
		// A bad alias after a valid ephemeral private and public address.
		{"bad alias leaves nothing changed", func(b *NICBinding) {
			b.Public = &NICPublic{}
			b.AliasAllocationIDs = []string{idOf(inOther)}
		}, errs.KindBadRequest, "aliases[0].allocation_id"},
	}
	before := len(e.allocs(t))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := e.nicBinding(sn, "eth0")
			tc.mutate(&b)
			_, err := e.svc.NICs().BindInterface(t.Context(), b)
			wantKind(t, tc.name, err, tc.kind)
			if tc.field != "" {
				var found bool
				if ee, ok := errors.AsType[*errs.Error](err); ok {
					for _, v := range ee.Violations {
						found = found || v.Field == tc.field
					}
				}
				if !found {
					t.Errorf("violations of %v do not name %q", err, tc.field)
				}
			}
			if got := len(e.allocs(t)); got != before {
				t.Errorf("allocations = %d, want %d: a failed bind changed state", got, before)
			}
		})
	}
	// Allocations of subtests share the env, so the sub-tests above are sequential.
}

func TestNICReleaseDeletesEphemeralOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	alias := e.createAlloc(t, "alias", idOf(sn), "10.0.0.60/32")
	b := e.nicBinding(sn, "eth0")
	b.Public = &NICPublic{}
	b.AliasAllocationIDs = []string{idOf(alias)}
	e.bind(t, b)
	if n := len(e.allocs(t)); n != 3 {
		t.Fatalf("allocations = %d, want 3", n)
	}
	if err := e.svc.NICs().ReleaseInterface(t.Context(), nicInstance, "eth0"); err != nil {
		t.Fatal(err)
	}
	all := e.allocs(t)
	if len(all) != 1 || idOf(all[0]) != idOf(alias) || all[0].GetStatus().GetAssignment() != nil {
		t.Errorf("allocations after release = %v, want only the unassigned alias", all)
	}
	// Releasing again, or an unknown NIC, is not an error.
	for _, name := range []string{"eth0", "nope"} {
		if err := e.svc.NICs().ReleaseInterface(t.Context(), nicInstance, name); err != nil {
			t.Errorf("ReleaseInterface(%s): %v", name, err)
		}
	}
}

func TestNICSubnet(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	got, err := e.svc.NICs().Subnet(t.Context(), idOf(sn))
	if err != nil || got.ProjectID != testProject || got.NetworkID != idOf(e.def.Network) {
		t.Errorf("Subnet = %+v, %v", got, err)
	}
	if _, err := e.svc.NICs().Subnet(t.Context(), "vpcsubnet-nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("missing subnet err = %v, want NotFound", err)
	}
}

func TestNICUpdateKeepsAssignment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sn := e.createSubnet(t, "nic", "10.0.0.0/24")
	b := e.nicBinding(sn, "eth0")
	out := e.bind(t, b)
	a := e.alloc(t, out.PrivateAllocationID)
	a.Metadata.Labels = map[string]string{"k": "v"}
	updated, err := tryUpdate(t.Context(), e.svc.Allocations().Update, e.svc.Allocations().Get, UpdateCmd[*vpcpb.AllocationSpec]{Metadata: a.GetMetadata(), Spec: a.GetSpec()})
	if err != nil {
		t.Fatal(err)
	}
	wantAssigned(t, updated, vpcpb.NetworkInterfaceAssignment_PRIMARY, idOf(sn))
}
