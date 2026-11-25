package contract

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/nebius/gosdk/operations"
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"
	kmssvc "github.com/nebius/gosdk/services/nebius/kms/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// stateKind drives the create, update and delete of one resource kind.
type stateKind struct {
	name string
	// create, update and delete start an operation. update changes the labels.
	create func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error)
	update func(ctx context.Context, id, name string, labels map[string]string) (operations.Operation, error)
	delete func(ctx context.Context, id string) (operations.Operation, error)
}

// stateKinds are the kinds under test, as seen by one SDK connection.
func stateKinds(t *testing.T, env *auditEnv) []stateKind {
	t.Helper()
	sdk := env.inst.SDK(t)
	groups := iamsvc.NewGroupService(sdk)
	networks := vpcsvc.NewNetworkService(sdk)
	keys := kmssvc.NewSymmetricKeyService(sdk)
	tenant, project := env.inst.TenantID, env.inst.ProjectID
	return []stateKind{
		{
			name: "iam group",
			create: func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
				return groups.Create(ctx, &iampb.CreateGroupRequest{
					Metadata: &commonpb.ResourceMetadata{ParentId: tenant, Name: name, Labels: labels},
					Spec:     &iampb.GroupSpec{},
				})
			},
			update: func(ctx context.Context, id, name string, labels map[string]string) (operations.Operation, error) {
				return groups.Update(ctx, &iampb.UpdateGroupRequest{
					Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: tenant, Name: name, Labels: labels},
					Spec:     &iampb.GroupSpec{},
				})
			},
			delete: func(ctx context.Context, id string) (operations.Operation, error) {
				return groups.Delete(ctx, &iampb.DeleteGroupRequest{Id: id})
			},
		},
		{
			name: "vpc network",
			create: func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
				return networks.Create(ctx, &vpcpb.CreateNetworkRequest{
					Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name, Labels: labels},
					Spec:     &vpcpb.NetworkSpec{},
				})
			},
			update: func(ctx context.Context, id, name string, labels map[string]string) (operations.Operation, error) {
				cur, err := networks.Get(ctx, &vpcpb.GetNetworkRequest{Id: id})
				if err != nil {
					return nil, err
				}
				return networks.Update(ctx, &vpcpb.UpdateNetworkRequest{
					Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: name, Labels: labels},
					Spec:     cur.GetSpec(),
				})
			},
			delete: func(ctx context.Context, id string) (operations.Operation, error) {
				return networks.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: id})
			},
		},
		{
			name: "kms symmetric key",
			create: func(ctx context.Context, name string, labels map[string]string) (operations.Operation, error) {
				return keys.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
					Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name, Labels: labels},
					Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, Description: "created"},
				})
			},
			update: func(ctx context.Context, id, name string, labels map[string]string) (operations.Operation, error) {
				return keys.Update(ctx, &kmspb.UpdateSymmetricKeyRequest{
					Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: name, Labels: labels},
					Spec: &kmspb.SymmetricKeySpec{
						Algorithm: kmspb.SymmetricAlgorithm_AES_256, Description: "updated",
					},
				})
			},
			delete: func(ctx context.Context, id string) (operations.Operation, error) {
				return keys.Delete(ctx, &kmspb.DeleteSymmetricKeyRequest{Id: id})
			},
		},
	}
}

func startStateAudit(t *testing.T, opts ...testkit.Option) *auditEnv {
	t.Helper()
	opts = append([]testkit.Option{testkit.WithAuthz(authz.ModeEnforce)}, opts...)
	return startAuditWith(t, []mocker.Module{vpc.New(), kms.New()}, opts...)
}

// stateOf returns resource.state.previous and current of an event.
func stateOf(ev *auditv2pb.AuditEvent) (previous, current *structpb.Struct) {
	st := ev.GetResource().GetState()
	return st.GetPrevious(), st.GetCurrent()
}

// fieldOf returns the nested field at the dotted path of s ("" if absent).
func fieldOf(s *structpb.Struct, path string) string {
	var v *structpb.Value
	cur := s
	for part := range strings.SplitSeq(path, ".") {
		if cur == nil {
			return ""
		}
		v = cur.GetFields()[part]
		cur = v.GetStructValue()
	}
	switch v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return v.GetStringValue()
	case *structpb.Value_NumberValue:
		return strconv.FormatFloat(v.GetNumberValue(), 'f', -1, 64)
	default:
		return ""
	}
}

func resourceVersion(t *testing.T, s *structpb.Struct) int64 {
	t.Helper()
	v, err := strconv.ParseInt(fieldOf(s, "metadata.resource_version"), 10, 64)
	if err != nil {
		t.Fatalf("metadata.resource_version of %v: %v", s, err)
	}
	return v
}

// stateEvents returns the events of resource id by action, then status.
func stateEvents(evs []*auditv2pb.AuditEvent, action string, status auditv2pb.Status) *auditv2pb.AuditEvent {
	for _, ev := range evs {
		if ev.GetAction() == action && ev.GetStatus() == status {
			return ev
		}
	}
	return nil
}

func TestAuditResourceState(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name  string
		delay time.Duration
	}{
		{"sync", 0},
		{"delayed", 200 * time.Millisecond},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			var opts []testkit.Option
			if mode.delay > 0 {
				opts = append(opts, testkit.WithOperationDelay(mode.delay))
			}
			env := startStateAudit(t, opts...)
			for _, k := range stateKinds(t, env) {
				t.Run(k.name, func(t *testing.T) {
					t.Parallel()
					checkResourceState(t, env, k, mode.delay)
				})
			}
		})
	}
}

func checkResourceState(t *testing.T, env *auditEnv, k stateKind, delay time.Duration) {
	t.Helper()
	delayed := delay > 0
	ctx := t.Context()
	name := "state-" + strings.ReplaceAll(k.name, " ", "-")
	id := wait(t)(k.create(ctx, name, map[string]string{"env": "old"}))
	wait(t)(k.update(ctx, id, name, map[string]string{"env": "new"}))
	wait(t)(k.delete(ctx, id))

	perCall := 1
	if delayed {
		perCall = 2 // STARTED, then DONE
	}
	evs := waitEvents(t, env.events, env.inst.TenantID, "resource.metadata.id='"+id+"'", 3*perCall)

	type want struct {
		action                string
		hasPrevious, hasFinal bool // previous / current present on the DONE event
	}
	for _, w := range []want{
		{"CREATE", false, true},
		{"UPDATE", true, true},
		{"DELETE", true, false},
	} {
		t.Run(w.action, func(t *testing.T) {
			done := stateEvents(evs, w.action, auditv2pb.Status_DONE)
			if done == nil {
				t.Fatalf("no DONE %s event in %v", w.action, evs)
			}
			prev, cur := stateOf(done)
			if (prev != nil) != w.hasPrevious || (cur != nil) != w.hasFinal {
				t.Fatalf("DONE %s state previous=%v current=%v, want previous present=%v current present=%v",
					w.action, prev, cur, w.hasPrevious, w.hasFinal)
			}
			if delayed {
				started := stateEvents(evs, w.action, auditv2pb.Status_STARTED)
				if started == nil {
					t.Fatalf("no STARTED %s event in %v", w.action, evs)
				}
				sp, sc := stateOf(started)
				if (sp != nil) != w.hasPrevious || sc != nil {
					t.Errorf("STARTED %s state previous=%v current=%v, want previous present=%v and no current",
						w.action, sp, sc, w.hasPrevious)
				}
				if sp != nil && prev != nil && fieldOf(sp, "metadata.labels.env") != fieldOf(prev, "metadata.labels.env") {
					t.Errorf("STARTED previous labels %v differ from DONE previous %v", sp, prev)
				}
			}
			checkStateContent(t, w.action, id, name, prev, cur)
		})
	}
}

func checkStateContent(t *testing.T, action, id, name string, prev, cur *structpb.Struct) {
	t.Helper()
	for label, s := range map[string]*structpb.Struct{"previous": prev, "current": cur} {
		if s == nil {
			continue
		}
		if got := fieldOf(s, "metadata.id"); got != id {
			t.Errorf("%s %s metadata.id = %q, want %q", action, label, got, id)
		}
		if got := fieldOf(s, "metadata.name"); got != name {
			t.Errorf("%s %s metadata.name = %q, want %q", action, label, got, name)
		}
		if s.GetFields()["spec"] == nil {
			t.Errorf("%s %s has no spec: %v", action, label, s)
		}
	}
	switch action {
	case "CREATE":
		if got := fieldOf(cur, "metadata.labels.env"); got != "old" {
			t.Errorf("CREATE current labels.env = %q, want old", got)
		}
	case "UPDATE":
		if got := fieldOf(prev, "metadata.labels.env"); got != "old" {
			t.Errorf("UPDATE previous labels.env = %q, want old", got)
		}
		if got := fieldOf(cur, "metadata.labels.env"); got != "new" {
			t.Errorf("UPDATE current labels.env = %q, want new", got)
		}
		if resourceVersion(t, cur) <= resourceVersion(t, prev) {
			t.Errorf("UPDATE resource_version %d -> %d, want an increase", resourceVersion(t, prev), resourceVersion(t, cur))
		}
	case "DELETE":
		if got := fieldOf(prev, "metadata.labels.env"); got != "new" {
			t.Errorf("DELETE previous labels.env = %q, want new", got)
		}
	}
}

// TestAuditDeniedUpdateHasNoState checks that a denied mutation carries no state.
func TestAuditDeniedUpdateHasNoState(t *testing.T) {
	t.Parallel()
	env := startStateAudit(t)
	g := createGroup(t, env.groups, env.inst.TenantID, "protected", map[string]string{"env": "old"})
	id := g.GetMetadata().GetId()

	viewer := env.as(t, config.DefaultViewerToken)
	_, err := viewer.groups.Update(t.Context(), &iampb.UpdateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: env.inst.TenantID, Name: "protected", Labels: map[string]string{"env": "new"}},
	})
	assertCode(t, err, codes.PermissionDenied)

	evs := env.list(t, "status='ERROR' AND action='UPDATE'")
	if len(evs) != 1 {
		t.Fatalf("ERROR UPDATE events = %d, want 1: %v", len(evs), evs)
	}
	if st := evs[0].GetResource().GetState(); st.GetPrevious() != nil || st.GetCurrent() != nil {
		t.Errorf("denied update has state %v, want none", st)
	}
	// The creation by the admin still has its state.
	created := env.list(t, "status='DONE' AND action='CREATE' AND resource.metadata.id='"+id+"'")
	if len(created) != 1 {
		t.Fatalf("DONE CREATE events = %d, want 1", len(created))
	}
	if _, cur := stateOf(created[0]); fieldOf(cur, "metadata.id") != id {
		t.Errorf("CREATE current = %v, want the group", cur)
	}
}

// TestAuditKeyStateHasNoKeyMaterial checks that a KMS key's state is the
// key's Get view (metadata, spec, status) and nothing secret.
func TestAuditKeyStateHasNoKeyMaterial(t *testing.T) {
	t.Parallel()
	env := startStateAudit(t)
	var key stateKind
	for _, k := range stateKinds(t, env) {
		if k.name == "kms symmetric key" {
			key = k
		}
	}
	ctx := t.Context()
	id := wait(t)(key.create(ctx, "crypto-key", nil))
	wait(t)(key.update(ctx, id, "crypto-key", map[string]string{"a": "b"}))

	evs := env.list(t, "resource.metadata.id='"+id+"'")
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	allowed := []string{"metadata", "spec", "status"}
	for _, ev := range evs {
		prev, cur := stateOf(ev)
		for _, s := range []*structpb.Struct{prev, cur} {
			if s == nil {
				continue
			}
			for field := range s.GetFields() {
				if !slices.Contains(allowed, field) {
					t.Errorf("%s state has field %q, want only %v", ev.GetAction(), field, allowed)
				}
			}
			raw, err := protojson.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"material", "secret", "private", "plaintext", "key_value", "public_key"} {
				if strings.Contains(strings.ToLower(string(raw)), bad) {
					t.Errorf("%s state mentions %q: %s", ev.GetAction(), bad, raw)
				}
			}
		}
	}
}
