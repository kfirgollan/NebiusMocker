//go:build e2e

package vpc

import (
	"testing"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// coreAll are the resource names of the stack used by the update and replace
// scenarios (the stack's pieces share their names).
var coreAll = []string{"pool", "net", "net2", "sub", "sub2", "alloc", "rt", "route", "sg", "rule"}

// corePieces is the stack: a pool, two networks, two subnets, an allocation,
// a route table with a route, and a security group with a rule.
var corePieces = []string{"pool", "net", "net2", "sub", "sub2", "alloc", "rt", "route", "sg", "rule"}

// coreStack is the stack's template data, with the pool wired to the network.
func coreStack(t *testing.T, project, source string) stack {
	t.Helper()
	return stack{project: project}.with("pool_source", source, "net_pools", netUsesPool)
}

// TestInPlaceUpdates changes labels and name of every resource type, and the
// type-specific mutable fields (route description, pool CIDRs), and checks
// that each change is planned as an update, keeps the ID, bumps
// resource_version, and that a removed label is cleared.
func TestInPlaceUpdates(t *testing.T) {
	inst, c, tf := startVPC(t)
	base := coreStack(t, inst.ProjectID, rootPoolID(t, c, inst))

	tf.writeConfig(t, base.render(t, corePieces...))
	tf.init(t)
	tf.expectChanges(t, nil, createAll(coreAll))
	tf.apply(t)
	st := tf.state(t)
	c.matchState(t, st)
	tf.expectNoChanges(t)

	resources := []struct {
		name   string // piece and Terraform name
		prefix string // template key prefix
		extra  []string
	}{
		{"pool", "pool", nil},
		{"net", "net", nil},
		{"sub", "sub", nil},
		{"alloc", "alloc", nil},
		{"rt", "rt", nil},
		{"route", "route", []string{"route_desc"}},
		{"sg", "sg", nil},
		{"rule", "rule", nil},
	}
	for _, r := range resources {
		t.Run(r.name, func(t *testing.T) {
			res := st[r.name]
			ver := c.version(t, res.Kind, res.ID)
			// Step 1: rename, add a label, change another.
			kv := []string{r.prefix + "_name", "renamed-" + r.name, r.prefix + "_labels", `team = "infra", tier = "1"`}
			for _, k := range r.extra {
				kv = append(kv, k, "changed description")
			}
			base = base.with(kv...)
			tf.writeConfig(t, base.render(t, corePieces...))
			tf.expectChanges(t, coreAll, map[string]string{r.name: "update"})
			tf.apply(t)
			assertSameID(t, tf, r.name, res.ID)
			name, labels := c.nameLabels(t, res.Kind, res.ID)
			if name != "renamed-"+r.name || labels["team"] != "infra" || labels["tier"] != "1" || len(labels) != 2 {
				t.Fatalf("mock %s name=%q labels=%v, want renamed with team=infra tier=1", r.name, name, labels)
			}
			if got := c.version(t, res.Kind, res.ID); got != ver+1 {
				t.Errorf("resource_version = %d, want %d", got, ver+1)
			}
			tf.expectNoChanges(t)
			ver++

			// Step 2: removing a label must clear it.
			base = base.with(r.prefix+"_labels", `tier = "1"`)
			tf.writeConfig(t, base.render(t, corePieces...))
			tf.expectChanges(t, coreAll, map[string]string{r.name: "update"})
			tf.apply(t)
			assertSameID(t, tf, r.name, res.ID)
			if _, labels := c.nameLabels(t, res.Kind, res.ID); len(labels) != 1 || labels["tier"] != "1" {
				t.Fatalf("mock %s labels=%v, want only tier=1 (team cleared)", r.name, labels)
			}
			if got := c.version(t, res.Kind, res.ID); got != ver+1 {
				t.Errorf("resource_version = %d, want %d", got, ver+1)
			}
			tf.expectNoChanges(t)
		})
	}

	t.Run("route description", func(t *testing.T) {
		route, err := c.routes.Get(t.Context(), &vpcpb.GetRouteRequest{Id: st["route"].ID})
		if err != nil {
			t.Fatalf("Get route: %v", err)
		}
		if got := route.GetSpec().GetDescription(); got != "changed description" {
			t.Errorf("route description = %q, want the updated one", got)
		}
	})

	t.Run("pool second cidr", func(t *testing.T) {
		ver := c.version(t, "pool", st["pool"].ID)
		base = base.with("pool_cidrs", `{ cidr = "/16" }, { cidr = "/20" }`)
		tf.writeConfig(t, base.render(t, corePieces...))
		tf.expectChanges(t, coreAll, map[string]string{"pool": "update"})
		tf.apply(t)
		assertSameID(t, tf, "pool", st["pool"].ID)
		pool, err := c.pools.Get(t.Context(), &vpcpb.GetPoolRequest{Id: st["pool"].ID})
		if err != nil {
			t.Fatalf("Get pool: %v", err)
		}
		if len(pool.GetSpec().GetCidrs()) != 2 || len(pool.GetStatus().GetCidrs()) != 2 {
			t.Fatalf("pool spec cidrs %v / status cidrs %v, want two each", pool.GetSpec().GetCidrs(), pool.GetStatus().GetCidrs())
		}
		if got := c.version(t, "pool", st["pool"].ID); got != ver+1 {
			t.Errorf("resource_version = %d, want %d", got, ver+1)
		}
		tf.expectNoChanges(t)
	})

	// The one allocation spec change upstream accepts: subnet_id to the pool
	// the address comes from (status.details.pool_id). The provider plans it
	// in place, the address and the route through it stay.
	t.Run("allocation subnet to its pool", func(t *testing.T) {
		before, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: st["alloc"].ID})
		if err != nil {
			t.Fatalf("Get allocation: %v", err)
		}
		if got := before.GetStatus().GetDetails().GetPoolId(); got != st["pool"].ID {
			t.Fatalf("status.details.pool_id = %s, want the stack's pool %s", got, st["pool"].ID)
		}
		base = base.with("alloc_pool", "nebius_vpc_v1_pool.pool.id")
		tf.writeConfig(t, base.render(t, corePieces...))
		tf.expectChanges(t, coreAll, map[string]string{"alloc": "update"})
		tf.apply(t)
		assertSameID(t, tf, "alloc", st["alloc"].ID)
		after, err := c.allocs.Get(t.Context(), &vpcpb.GetAllocationRequest{Id: st["alloc"].ID})
		if err != nil {
			t.Fatalf("Get allocation: %v", err)
		}
		if p := after.GetSpec().GetIpv4Private(); p.GetPoolId() != st["pool"].ID || p.GetSubnetId() != "" {
			t.Errorf("spec = %v, want pool_id %s and no subnet_id", after.GetSpec(), st["pool"].ID)
		}
		if got, want := after.GetStatus().GetDetails().GetAllocatedCidr(), before.GetStatus().GetDetails().GetAllocatedCidr(); got != want {
			t.Errorf("allocated_cidr = %s, want %s kept", got, want)
		}
		if d := after.GetStatus().GetDetails(); d.GetPoolId() != st["pool"].ID || d.GetSubnetId() != "" {
			t.Errorf("status.details = %v, want pool %s and no subnet", d, st["pool"].ID)
		}
		if got := after.GetMetadata().GetResourceVersion(); got != before.GetMetadata().GetResourceVersion()+1 {
			t.Errorf("resource_version = %d, want %d", got, before.GetMetadata().GetResourceVersion()+1)
		}
		tf.expectNoChanges(t)
	})

	tf.destroy(t)
	for _, r := range st {
		c.gone(t, r.Kind, r.ID)
	}
}

func createAll(names []string) map[string]string {
	m := map[string]string{}
	for _, n := range names {
		m[n] = "create"
	}
	return m
}

func assertSameID(t *testing.T, tf *tfRun, name, id string) {
	t.Helper()
	if got := tf.state(t)[name].ID; got != id {
		t.Fatalf("%s was recreated: id %s → %s", name, id, got)
	}
}
