package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

// A dynamic public address (public_ip_address = {}) is released once the
// instance has been STOPPED for more than one hour
// (docs.nebius.com/compute/virtual-machines/network); until then it stays
// assigned and a Start keeps it. Driven on a manual clock, under enforce.

func startPublicIPClock(t *testing.T) (instClients, *clock.Manual) {
	t.Helper()
	clk := clock.NewManual(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	// Operations complete inline: on a manual clock a delay would never pass.
	return startInstances(t, testkit.WithClock(clk), testkit.WithOperationDelay(0), testkit.WithAuthz(authz.ModeEnforce)), clk
}

func (c instClients) dynamicPublic(t *testing.T, name string) (id string, pub *computepb.PublicIPAddressStatus) {
	t.Helper()
	spec := c.cpuSpec(name)
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := c.mustCreate(t, name, spec)
	pub = inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if pub.GetAddress() == "" || pub.GetStatic() {
		t.Fatalf("public ip = %v, want a dynamic address", pub)
	}
	return inst.GetMetadata().GetId(), pub
}

func (c instClients) publicOf(t *testing.T, id string) *computepb.PublicIPAddressStatus {
	t.Helper()
	return c.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
}

// poll waits (in real time, up to 30 s) until cond holds, calling tick
// before each check.
func poll(t *testing.T, what string, tick func(), cond func() bool) {
	t.Helper()
	deadline := clock.Real{}.Now().Add(30 * time.Second)
	for {
		tick()
		if cond() {
			return
		}
		if (clock.Real{}).Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("test ended waiting for %s", what)
		case <-clock.Real{}.After(20 * time.Millisecond):
		}
	}
}

func TestInstancePublicIPStartWithinGraceKeepsAddress(t *testing.T) {
	t.Parallel()
	c, clk := startPublicIPClock(t)
	id, pub := c.dynamicPublic(t, "grace")
	if err := c.stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	clk.Advance(59 * time.Minute)
	if got := c.publicOf(t, id); got.GetAddress() != pub.GetAddress() || got.GetAllocationId() != pub.GetAllocationId() {
		t.Fatalf("public ip 59m after stop = %v, want %v", got, pub)
	}
	if a := c.alloc(t, pub.GetAllocationId()); a.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED ||
		a.GetStatus().GetAssignment().GetNetworkInterface().GetInstanceId() != id {
		t.Fatalf("allocation while stopped = %v, want ASSIGNED to %s", a.GetStatus(), id)
	}
	if err := c.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := c.publicOf(t, id); got.GetAddress() != pub.GetAddress() {
		t.Fatalf("public ip after a Start within the hour = %v, want %s", got, pub.GetAddress())
	}
	// Running: the address is not released, however long it runs.
	clk.Advance(3 * time.Hour)
	if got := c.publicOf(t, id); got.GetAddress() != pub.GetAddress() {
		t.Fatalf("public ip of a running instance = %v, want %s", got, pub.GetAddress())
	}
	c.mustDelete(t, id)
	_, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: pub.GetAllocationId()})
	wantCode(t, "dynamic public allocation after delete", err, codes.NotFound, "")
}

func TestInstancePublicIPReleasedAfterGrace(t *testing.T) {
	t.Parallel()
	c, clk := startPublicIPClock(t)
	id, pub := c.dynamicPublic(t, "late")
	if err := c.stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	// The module's sweep runs every minute of the mock clock: advance a
	// minute at a time until the VPC allocation is gone.
	poll(t, "the dynamic public allocation to be released", func() { clk.Advance(time.Minute) }, func() bool {
		_, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: pub.GetAllocationId()})
		if err != nil && status.Code(err) != codes.NotFound {
			t.Fatalf("get allocation: %v", err)
		}
		return status.Code(err) == codes.NotFound
	})
	inst := c.get(t, id)
	nic := inst.GetStatus().GetNetworkInterfaces()[0]
	if inst.GetStatus().GetState() != computepb.InstanceStatus_STOPPED || nic.GetPublicIpAddress() != nil || nic.GetIpAddress().GetAddress() == "" {
		t.Fatalf("after release: %v; want STOPPED, no public address, private kept", inst.GetStatus())
	}
	if err := c.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	got := c.publicOf(t, id)
	if got.GetAddress() == "" || got.GetAllocationId() == pub.GetAllocationId() || got.GetStatic() {
		t.Fatalf("public ip after Start = %v, want a new dynamic allocation (old %v)", got, pub)
	}
	if a := c.alloc(t, got.GetAllocationId()); a.GetStatus().GetState() != vpcpb.AllocationStatus_ASSIGNED {
		t.Fatalf("new allocation = %v, want ASSIGNED", a.GetStatus())
	}
}

func TestInstancePublicIPStaticSurvivesGrace(t *testing.T) {
	t.Parallel()
	c, clk := startPublicIPClock(t)
	spec := c.cpuSpec("static")
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{Static: true}
	inst := c.mustCreate(t, "static", spec)
	id, pub := inst.GetMetadata().GetId(), inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if err := c.stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for range 90 { // 90 minutes: past the grace period, the sweep has run
		clk.Advance(time.Minute)
	}
	if got := c.publicOf(t, id); got.GetAddress() != pub.GetAddress() || !got.GetStatic() {
		t.Fatalf("static public ip after 90m stopped = %v, want %v", got, pub)
	}
}
