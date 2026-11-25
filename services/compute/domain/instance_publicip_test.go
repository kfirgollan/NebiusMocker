package domain

import (
	"testing"
	"time"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

// Dynamic public addresses follow docs.nebius.com/compute/virtual-machines/network:
// released once the VM is STOPPED for more than one hour.

// newDynamicPublic creates a RUNNING instance with a dynamic public address
// on eth0 and returns its ID and that address.
func newDynamicPublic(t *testing.T, f *instFixture) (id, addr string) {
	t.Helper()
	spec := baseSpec()
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	inst := f.mustCreate(t, "vm", spec)
	pub := inst.GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress()
	if pub.GetAddress() == "" || pub.GetStatic() {
		t.Fatalf("public ip = %v, want a dynamic address", pub)
	}
	return inst.GetMetadata().GetId(), pub.GetAddress()
}

func publicAddr(t *testing.T, f *instFixture, id string) string {
	t.Helper()
	return f.get(t, id).GetStatus().GetNetworkInterfaces()[0].GetPublicIpAddress().GetAddress()
}

func (f *instFixture) sweep(t *testing.T) int {
	t.Helper()
	n, err := f.svc.SweepPublicIPs(f.ctx)
	if err != nil {
		t.Fatalf("SweepPublicIPs: %v", err)
	}
	return n
}

func TestInstanceStopKeepsDynamicPublicIPDuringGrace(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, addr := newDynamicPublic(t, f)
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if got := publicAddr(t, f, id); got != addr {
		t.Fatalf("stopped instance public address = %q, want %q kept", got, addr)
	}
	if b, ok := f.net.binding(id, "eth0"); !ok || b.Public == nil {
		t.Fatalf("binding while stopped = %+v (bound %v), want the public address assigned", b, ok)
	}
	f.clk.Advance(time.Hour) // exactly one hour is not "more than one hour"
	if n := f.sweep(t); n != 0 {
		t.Fatalf("sweep at 1h released %d instances, want 0", n)
	}
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	if got := publicAddr(t, f, id); got != addr {
		t.Fatalf("public address after a Start within the grace period = %q, want %q", got, addr)
	}
	// Running again: no stop time is left, so a later sweep changes nothing.
	f.clk.Advance(2 * time.Hour)
	if n := f.sweep(t); n != 0 || publicAddr(t, f, id) != addr {
		t.Fatalf("sweep of a running instance released %d, address %q", n, publicAddr(t, f, id))
	}
}

func TestInstanceSweepReleasesDynamicPublicIPAfterGrace(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, addr := newDynamicPublic(t, f)
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Hour + time.Second)
	if n := f.sweep(t); n != 1 {
		t.Fatalf("sweep released %d instances, want 1", n)
	}
	inst := f.get(t, id)
	nic := inst.GetStatus().GetNetworkInterfaces()[0]
	if nic.GetPublicIpAddress() != nil || nic.GetIpAddress().GetAddress() != "10.0.0.1" {
		t.Fatalf("after release: public %v, private %q; want no public, private kept",
			nic.GetPublicIpAddress(), nic.GetIpAddress().GetAddress())
	}
	if st := inst.GetStatus().GetState(); st != computepb.InstanceStatus_STOPPED {
		t.Fatalf("state = %s, want STOPPED", st)
	}
	if b, ok := f.net.binding(id, "eth0"); !ok || b.Public != nil {
		t.Fatalf("binding after release = %+v, want no public address", b)
	}
	if n := f.sweep(t); n != 0 {
		t.Fatalf("second sweep released %d, want 0", n)
	}
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	if got := publicAddr(t, f, id); got == "" || got == addr {
		t.Fatalf("public address after Start = %q, want a new one (was %q)", got, addr)
	}
}

func TestInstanceStartAfterGraceWithoutSweepGetsNewAddress(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, addr := newDynamicPublic(t, f)
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(2 * time.Hour) // the sweep has not run
	if err := f.start(id); err != nil {
		t.Fatal(err)
	}
	if got := publicAddr(t, f, id); got == "" || got == addr {
		t.Fatalf("public address after a late Start = %q, want a new one (was %q)", got, addr)
	}
}

func TestInstanceUpdateWhileStoppedKeepsStopTime(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, _ := newDynamicPublic(t, f)
	// Stopping through Update (spec.stopped) starts the grace period too.
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.Stopped = true }, nil); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(30 * time.Minute)
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.CloudInitUserData = "#cloud-config" }, nil); err != nil {
		t.Fatal(err)
	}
	if publicAddr(t, f, id) == "" {
		t.Fatal("an update within the grace period dropped the public address")
	}
	f.clk.Advance(31 * time.Minute)
	if n := f.sweep(t); n != 1 || publicAddr(t, f, id) != "" {
		t.Fatalf("sweep 61m after the stop released %d, address %q; want released", n, publicAddr(t, f, id))
	}
}

func TestInstanceUpdateAfterGraceReleasesFirst(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, _ := newDynamicPublic(t, f)
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(2 * time.Hour)
	if err := f.update(t, id, func(s *computepb.InstanceSpec) { s.CloudInitUserData = "#cloud-config" }, nil); err != nil {
		t.Fatal(err)
	}
	if got := publicAddr(t, f, id); got != "" {
		t.Fatalf("public address after an update past the grace period = %q, want released", got)
	}
}

func TestInstanceDeleteWhileStoppedReleasesAndClearsStopTime(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	id, _ := newDynamicPublic(t, f)
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if err := f.remove(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.net.binding(id, "eth0"); ok {
		t.Fatal("NIC still bound after delete")
	}
	f.clk.Advance(2 * time.Hour)
	if n := f.sweep(t); n != 0 {
		t.Fatalf("sweep after delete released %d, want 0", n)
	}
	if ids, _ := f.repo.StoppedBefore(f.ctx, f.clk.Now()); len(ids) != 0 {
		t.Fatalf("stop times left after delete: %v", ids)
	}
}

func TestInstanceStopWithoutDynamicPublicRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{Static: true}
	id := f.mustCreate(t, "vm", spec).GetMetadata().GetId()
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	if ids, _ := f.repo.StoppedBefore(f.ctx, f.clk.Now().Add(time.Hour)); len(ids) != 0 {
		t.Fatalf("stop times for a static address: %v", ids)
	}
}

func TestInstanceSweepReleasesOnlyDynamicPublicIPs(t *testing.T) {
	t.Parallel()
	f := newInstFixture(t)
	spec := baseSpec()
	spec.NetworkInterfaces[0].PublicIpAddress = &computepb.PublicIPAddress{}
	static := nicSpec("eth1", "subnet-b")
	static.PublicIpAddress = &computepb.PublicIPAddress{Static: true}
	spec.NetworkInterfaces = append(spec.NetworkInterfaces, static)
	inst := f.mustCreate(t, "vm", spec)
	id := inst.GetMetadata().GetId()
	staticAddr := inst.GetStatus().GetNetworkInterfaces()[1].GetPublicIpAddress().GetAddress()
	if err := f.stop(id); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(2 * time.Hour)
	if n := f.sweep(t); n != 1 {
		t.Fatalf("sweep released %d instances, want 1", n)
	}
	if b, _ := f.net.binding(id, "eth0"); b.Public != nil {
		t.Fatalf("eth0 binding = %+v, want the dynamic address released", b)
	}
	if b, _ := f.net.binding(id, "eth1"); b.Public == nil {
		t.Fatalf("eth1 binding = %+v, want the static address kept", b)
	}
	nics := f.get(t, id).GetStatus().GetNetworkInterfaces()
	if nics[0].GetPublicIpAddress() != nil || nics[1].GetPublicIpAddress().GetAddress() != staticAddr || staticAddr == "" {
		t.Fatalf("status after release: eth0 %v, eth1 %v; want eth0 none, eth1 %q", nics[0].GetPublicIpAddress(), nics[1].GetPublicIpAddress(), staticAddr)
	}
}
