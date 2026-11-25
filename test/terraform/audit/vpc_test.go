//go:build e2e

package audit

import (
	"fmt"
	"slices"
	"testing"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const vpcResources = `
resource "nebius_vpc_v1_network" "net" {
  parent_id = %[1]q
  name      = "audit-net"
  labels    = { env = %[2]q }
}

resource "nebius_vpc_v1_subnet" "sub" {
  parent_id          = %[1]q
  name               = "audit-sub"
  network_id         = nebius_vpc_v1_network.net.id
  ipv4_private_pools = { pools = [{ cidrs = [{ cidr = "/24" }] }] }
}
`

// TestTerraformVPCAudit applies, updates and destroys a VPC network and
// subnet: every change is recorded under service VIRTUAL_NETWORKS with the
// project hierarchy, and is found with the documented filters.
func TestTerraformVPCAudit(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), auditmod.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	events := newEventLog(t, inst)
	tf := tfharness.New(t, inst.Addr, inst.Token)

	tf.Write(t, fmt.Sprintf(vpcResources, inst.ProjectID, "dev"))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{"nebius_vpc_v1_network.net": "create", "nebius_vpc_v1_subnet.sub": "create"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	netID := stateID(t, tf, "nebius_vpc_v1_network.net")
	subID := stateID(t, tf, "nebius_vpc_v1_subnet.sub")

	for _, r := range []struct{ id, kind, name string }{{netID, "vpcnetwork", "audit-net"}, {subID, "vpcsubnet", "audit-sub"}} {
		ev := events.expect(1, "resource.metadata.id='"+r.id+"' AND action='CREATE'")[0]
		checkCommon(t, ev, inst.TenantID)
		checkVPC(t, ev, r.kind, r.name, "create", inst.ProjectID)
	}

	tf.Write(t, fmt.Sprintf(vpcResources, inst.ProjectID, "prod"))
	tf.ExpectPlan(t, map[string]string{"nebius_vpc_v1_network.net": "update", "nebius_vpc_v1_subnet.sub": "no-op"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	ev := events.expect(1, "service.name='VIRTUAL_NETWORKS' AND action='UPDATE'")[0]
	checkCommon(t, ev, inst.TenantID)
	checkVPC(t, ev, "vpcnetwork", "audit-net", "update", inst.ProjectID)

	tf.Destroy(t)
	deletes := events.expect(2, "action='DELETE' AND resource.hierarchy.id='"+inst.ProjectID+"' AND regex(resource.metadata.type, '^vpc(network|subnet)$')")
	for _, ev := range deletes {
		checkCommon(t, ev, inst.TenantID)
	}
	if got := []string{deletes[0].GetResource().GetMetadata().GetId(), deletes[1].GetResource().GetMetadata().GetId()}; !slices.Contains(got, netID) || !slices.Contains(got, subID) {
		t.Errorf("deleted resources = %v, want %s and %s", got, netID, subID)
	}
	events.expect(5, "service.name='VIRTUAL_NETWORKS' AND status='DONE'")
	events.expect(0, "service.name='VIRTUAL_NETWORKS' AND action='GET'")
}

func checkVPC(t *testing.T, ev *auditv2pb.AuditEvent, kind, name, action, project string) {
	t.Helper()
	md := ev.GetResource().GetMetadata()
	if ev.GetService().GetName() != "VIRTUAL_NETWORKS" || md.GetType() != kind || md.GetName() != name ||
		ev.GetType() != "ai.nebius.vpc."+kind+"."+action || ev.GetStatus() != auditv2pb.Status_DONE {
		t.Errorf("event = service %s type %s resource %v status %s, want VIRTUAL_NETWORKS ai.nebius.vpc.%s.%s %s DONE",
			ev.GetService().GetName(), ev.GetType(), md, ev.GetStatus(), kind, action, name)
	}
	if ids := hierarchyIDs(ev); len(ids) != 2 || ids[1] != project {
		t.Errorf("event %s hierarchy = %v, want [tenant, %s]", ev.GetType(), ids, project)
	}
}
