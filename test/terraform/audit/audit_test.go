//go:build e2e

// Package audit checks that changes driven by the official Terraform binary
// and the official nebius/nebius provider leave the right events in
// NebiusMocker's audit log. There are no Terraform objects for audit itself:
// the events are read back with AuditEventService.List.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package audit

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	auditsvc "github.com/nebius/gosdk/services/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
)

const region = "eu-north1"

// TestMain gives this binary a private Terraform plugin cache (see tfcache).
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

func start(t *testing.T, opts ...testkit.Option) *testkit.Instance {
	t.Helper()
	opts = append(opts, testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	return testkit.StartT(t, []mocker.Module{iam.New(), kms.New(), auditmod.New()}, opts...)
}

// eventLog reads audit events back through the SDK.
type eventLog struct {
	t      *testing.T
	svc    auditsvc.AuditEventService
	tenant string
	from   time.Time
}

func newEventLog(t *testing.T, inst *testkit.Instance) *eventLog {
	t.Helper()
	return &eventLog{t: t, svc: auditsvc.NewAuditEventService(inst.SDK(t)), tenant: inst.TenantID, from: clock.Real{}.Now().Add(-time.Hour)}
}

// list returns the events matching filter, oldest first.
func (l *eventLog) list(filter string) []*auditv2pb.AuditEvent {
	l.t.Helper()
	var out []*auditv2pb.AuditEvent
	token := ""
	for {
		resp, err := l.svc.List(l.t.Context(), &auditv2pb.ListAuditEventRequest{
			ParentId: l.tenant, Region: region, Filter: filter, PageToken: token, PageSize: 100,
			Start: timestamppb.New(l.from), End: timestamppb.New(clock.Real{}.Now().Add(time.Hour)),
		})
		if err != nil {
			l.t.Fatalf("AuditEventService.List(%q): %v", filter, err)
		}
		out = append(out, resp.GetItems()...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	slices.SortStableFunc(out, func(a, b *auditv2pb.AuditEvent) int { return a.GetTime().AsTime().Compare(b.GetTime().AsTime()) })
	return out
}

// expect asserts that filter matches exactly n events and returns them.
func (l *eventLog) expect(n int, filter string) []*auditv2pb.AuditEvent {
	l.t.Helper()
	got := l.list(filter)
	if len(got) != n {
		l.t.Fatalf("List(%q) = %d events, want %d:\n%s", filter, len(got), n, describe(got))
	}
	return got
}

func describe(evs []*auditv2pb.AuditEvent) string {
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "  %s %s %s status=%s resource=%s ua=%q\n", e.GetTime().AsTime().Format(time.RFC3339Nano),
			e.GetService().GetName(), e.GetType(), e.GetStatus(), e.GetResource().GetMetadata().GetId(), e.GetRequest().GetUserAgent())
	}
	return b.String()
}

// checkCommon asserts what every Terraform-driven event must carry.
func checkCommon(t *testing.T, ev *auditv2pb.AuditEvent, tenant string) {
	t.Helper()
	sub := ev.GetAuthentication().GetSubject()
	if !ev.GetAuthentication().GetAuthenticated() || sub.GetName() != config.DefaultDevPrincipal || sub.GetTenantUserId() != config.DefaultDevPrincipal {
		t.Errorf("event %s authentication = %v, want principal %s", ev.GetType(), ev.GetAuthentication(), config.DefaultDevPrincipal)
	}
	if ua := ev.GetRequest().GetUserAgent(); !strings.Contains(ua, "terraform-provider-nebius") {
		t.Errorf("event %s user_agent = %q, want it to contain terraform-provider-nebius", ev.GetType(), ua)
	}
	if got := ev.GetResponse().GetStatusCode(); got != code.Code_OK {
		t.Errorf("event %s response status = %v, want OK", ev.GetType(), got)
	}
	h := ev.GetResource().GetHierarchy()
	if len(h) == 0 || h[0].GetId() != tenant {
		t.Errorf("event %s hierarchy = %v, want it to start with tenant %s", ev.GetType(), h, tenant)
	}
	if ev.GetRequest().GetRequestId() == "" {
		t.Errorf("event %s has no request_id", ev.GetType())
	}
}

func hierarchyIDs(ev *auditv2pb.AuditEvent) []string {
	var ids []string
	for _, h := range ev.GetResource().GetHierarchy() {
		ids = append(ids, h.GetId())
	}
	return ids
}

const resources = `
resource "nebius_iam_v1_group" "g" {
  parent_id = %[1]q
  name      = %[2]q
  labels    = { team = %[3]q }
}

resource "nebius_kms_v1_symmetric_key" "k" {
  parent_id   = %[4]q
  name        = "audit-key"
  description = %[5]q
  algorithm   = "AES_256"
}
`

// TestTerraformLifecycleAudit applies, updates and destroys a group and a KMS
// key and checks the audit events of every step. Operations are asynchronous,
// so each mutation records a STARTED and a DONE event.
func TestTerraformLifecycleAudit(t *testing.T) {
	inst := start(t, testkit.WithOperationDelay(300*time.Millisecond))
	events := newEventLog(t, inst)
	tf := tfharness.New(t, inst.Addr, inst.Token)
	cfg := func(name, team, desc string) string {
		return fmt.Sprintf(resources, inst.TenantID, name, team, inst.ProjectID, desc)
	}

	// Create.
	tf.Write(t, cfg("audit-group", "a", "first"))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{"nebius_iam_v1_group.g": "create", "nebius_kms_v1_symmetric_key.k": "create"})
	tf.Apply(t)
	groupID := stateID(t, tf, "nebius_iam_v1_group.g")
	keyID := stateID(t, tf, "nebius_kms_v1_symmetric_key.k")
	tf.ExpectNoChanges(t)

	for _, c := range []struct {
		id, service, rtype string
		hier               []string
		want               map[string]string
	}{
		{groupID, "IAM", "group", []string{inst.TenantID}, map[string]string{"metadata.name": "audit-group", "metadata.labels.team": "a"}},
		{keyID, "KMS", "kmssymkey", []string{inst.TenantID, inst.ProjectID}, map[string]string{"metadata.name": "audit-key", "spec.description": "first", "spec.algorithm": "AES_256"}},
	} {
		started := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='CREATE' AND status='STARTED'", c.id))
		done := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='CREATE' AND status='DONE'", c.id))
		for _, ev := range append(started, done...) {
			checkCommon(t, ev, inst.TenantID)
			if ev.GetService().GetName() != c.service || ev.GetResource().GetMetadata().GetType() != c.rtype {
				t.Errorf("create event of %s: service=%q resource type=%q, want %s/%s", c.id, ev.GetService().GetName(), ev.GetResource().GetMetadata().GetType(), c.service, c.rtype)
			}
			if !slices.Equal(hierarchyIDs(ev), c.hier) {
				t.Errorf("create event of %s: hierarchy = %v, want %v", c.id, hierarchyIDs(ev), c.hier)
			}
		}
		checkStartedState(t, "create STARTED "+c.id, started[0], nil)
		checkCreateState(t, "create DONE "+c.id, c.id, done[0], c.want)
		if done[0].GetTime().AsTime().Before(started[0].GetTime().AsTime()) {
			t.Errorf("DONE event of %s (%v) is before STARTED (%v)", c.id, done[0].GetTime().AsTime(), started[0].GetTime().AsTime())
		}
	}
	// The same events found through service and type filters.
	events.expect(2, "service.name='KMS' AND resource.metadata.type='kmssymkey' AND action='CREATE'")
	events.expect(0, "resource.metadata.id='"+groupID+"' AND action!='CREATE'")
	events.expect(0, "action='UPDATE'")
	events.expect(0, "action='DELETE'")
	events.expect(0, "action='GET'") // Terraform's own reads are not audited
	events.expect(0, "status='ERROR'")

	// Update in place.
	tf.Write(t, cfg("audit-group-renamed", "b", "second"))
	tf.ExpectPlan(t, map[string]string{"nebius_iam_v1_group.g": "update", "nebius_kms_v1_symmetric_key.k": "update"})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	oldVals := map[string]map[string]string{
		groupID: {"metadata.name": "audit-group", "metadata.labels.team": "a"},
		keyID:   {"metadata.name": "audit-key", "spec.description": "first", "spec.algorithm": "AES_256"},
	}
	newVals := map[string]map[string]string{
		groupID: {"metadata.id": groupID, "metadata.name": "audit-group-renamed", "metadata.labels.team": "b"},
		keyID:   {"metadata.id": keyID, "metadata.name": "audit-key", "spec.description": "second", "spec.algorithm": "AES_256"},
	}
	for _, id := range []string{groupID, keyID} {
		started := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='UPDATE' AND status='STARTED'", id))
		checkStartedState(t, "update STARTED "+id, started[0], oldVals[id])
		done := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='UPDATE' AND status='DONE'", id))
		checkCommon(t, done[0], inst.TenantID)
		checkUpdateState(t, "update DONE "+id, done[0], oldVals[id], newVals[id])
		// The CREATE events are untouched by the update.
		events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='CREATE' AND status='DONE'", id))
	}
	events.expect(2, "service.name='KMS' AND action='UPDATE'")
	events.expect(2, "service.name='IAM' AND resource.metadata.type='group' AND action='UPDATE'")
	events.expect(0, "action='DELETE'")
	events.expect(0, "action='GET'")

	// Destroy.
	tf.Destroy(t)
	for _, id := range []string{groupID, keyID} {
		started := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='DELETE' AND status='STARTED'", id))
		checkStartedState(t, "delete STARTED "+id, started[0], newVals[id])
		done := events.expect(1, fmt.Sprintf("resource.metadata.id='%s' AND action='DELETE' AND status='DONE'", id))
		checkCommon(t, done[0], inst.TenantID)
		checkDeleteState(t, "delete DONE "+id, done[0], newVals[id])
	}
	// Both resources are under the tenant; only the key is under the project.
	del := events.expect(1, "action='DELETE' AND status='DONE' AND resource.hierarchy.id='"+inst.ProjectID+"'")
	if del[0].GetResource().GetMetadata().GetId() != keyID {
		t.Errorf("DELETE under the project is %s, want the key %s", del[0].GetResource().GetMetadata().GetId(), keyID)
	}
	events.expect(2, "action='DELETE' AND status='DONE' AND resource.hierarchy.id='"+inst.TenantID+"'")
	events.expect(0, "action='GET'")
	events.expect(0, "status='ERROR'")

	// The whole run: exactly the mutations Terraform made, nothing extra.
	for _, action := range []string{"CREATE", "UPDATE", "DELETE"} {
		events.expect(2, fmt.Sprintf("action='%s' AND status='DONE' AND authentication.subject.tenant_user_id='%s'", action, config.DefaultDevPrincipal))
	}
}

// TestTerraformSyncAudit runs without operation delay: every call finishes
// inline, so each mutation records a single DONE event and no STARTED one.
func TestTerraformSyncAudit(t *testing.T) {
	inst := start(t)
	events := newEventLog(t, inst)
	tf := tfharness.New(t, inst.Addr, inst.Token)
	tf.Write(t, fmt.Sprintf(resources, inst.TenantID, "sync-group", "a", inst.ProjectID, "d"))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{"nebius_iam_v1_group.g": "create", "nebius_kms_v1_symmetric_key.k": "create"})
	tf.Apply(t)
	groupID := stateID(t, tf, "nebius_iam_v1_group.g")
	tf.ExpectNoChanges(t)

	got := events.expect(1, "resource.metadata.id='"+groupID+"' AND action='CREATE'")
	checkCommon(t, got[0], inst.TenantID)
	if got[0].GetStatus() != auditv2pb.Status_DONE {
		t.Errorf("status = %v, want DONE", got[0].GetStatus())
	}
	events.expect(0, "status='STARTED'")
	events.expect(0, "action='GET'")
	checkCreateState(t, "sync create", groupID, got[0], map[string]string{"metadata.name": "sync-group", "metadata.labels.team": "a"})

	tf.Write(t, fmt.Sprintf(resources, inst.TenantID, "sync-group", "b", inst.ProjectID, "d"))
	tf.ExpectPlan(t, map[string]string{"nebius_iam_v1_group.g": "update", "nebius_kms_v1_symmetric_key.k": "no-op"})
	tf.Apply(t)
	upd := events.expect(1, "resource.metadata.id='"+groupID+"' AND action='UPDATE'")
	checkUpdateState(t, "sync update", upd[0], map[string]string{"metadata.name": "sync-group", "metadata.labels.team": "a"},
		map[string]string{"metadata.id": groupID, "metadata.name": "sync-group", "metadata.labels.team": "b"})

	tf.Destroy(t)
	events.expect(2, "action='DELETE' AND status='DONE'")
	del := events.expect(1, "resource.metadata.id='"+groupID+"' AND action='DELETE'")
	checkDeleteState(t, "sync delete", del[0], map[string]string{"metadata.id": groupID, "metadata.labels.team": "b"})
}

func stateID(t *testing.T, tf *tfharness.Run, addr string) string {
	t.Helper()
	id, _ := tf.State(t)[addr]["id"].(string)
	if id == "" {
		t.Fatalf("no id for %s in the Terraform state", addr)
	}
	return id
}
