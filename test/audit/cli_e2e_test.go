//go:build e2e

// Package audit_test runs the audit workflows from https://docs.nebius.com/audit
// with the official nebius CLI against an in-process mocker: listing audit
// events with a filter and exporting them to Object Storage as JSON Lines.
package audit_test

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	auditmod "github.com/kfirgollan/NebiusMocker/services/audit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const (
	region = "eu-north1"
	// bucket is the name of the Object Storage bucket the tests create with
	// the CLI; it is also the backend bucket name in the memory store.
	bucket = "cli-audit-export"
)

type env struct {
	inst  *testkit.Instance
	cli   *nebiuscli.CLI
	store *memory.Store
	// bucketID is the ID of the bucket created with "nebius storage bucket create".
	bucketID string
	groupID  string
	from     string
	to       string
}

// newEnv starts the mocker with an in-memory object store, creates one group
// through the SDK and the export bucket through the CLI, and returns the CLI
// pointed at the mocker.
func newEnv(t *testing.T, opts ...testkit.Option) *env {
	t.Helper()
	store := memory.New(clock.Real{})
	opts = append(opts, testkit.WithObjStore(store), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), storage.New(storage.WithS3Listen("127.0.0.1:0")), auditmod.New()}, opts...)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	op, err := iamsvc.NewGroupService(inst.SDK(t)).Create(t.Context(), &iampb.CreateGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "audited-by-cli"},
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("wait for group: %v", err)
	}

	var created struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	cli.JSON(t, &created, "storage", "bucket", "create", "--name", bucket)
	if !strings.HasPrefix(created.Metadata.ID, "storagebucket-") {
		t.Fatalf("storage bucket create returned id %q, want storagebucket-…", created.Metadata.ID)
	}
	now := clock.Real{}.Now().UTC()
	return &env{
		inst: inst, cli: cli, store: store, groupID: op.ResourceID(), bucketID: created.Metadata.ID,
		from: now.Add(-time.Hour).Format(time.RFC3339), to: now.Add(time.Hour).Format(time.RFC3339),
	}
}

// startExport runs "audit-event-export start" for the group events and
// returns the export ID. It waits for the operation and reads the export the
// CLI prints.
func (e *env) startExport(t *testing.T) string {
	t.Helper()
	var out struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	e.cli.JSON(t, &out, e.startArgs()...)
	if !strings.HasPrefix(out.Metadata.ID, "auditeventexport-") {
		t.Fatalf("export start returned id %q, want auditeventexport-…", out.Metadata.ID)
	}
	return out.Metadata.ID
}

// startExportAsync starts an export with --async, which prints only the
// operation ID, and finds the export through "audit-event-export list".
func (e *env) startExportAsync(t *testing.T) string {
	t.Helper()
	res, err := e.cli.Run(t.Context(), append(e.startArgs(), "--async")...)
	if err != nil {
		t.Fatalf("export start --async: %v\n%s", err, res.Stderr)
	}
	if op := strings.TrimSpace(res.Stdout); !strings.HasPrefix(op, "operation-") {
		t.Fatalf("export start --async printed %q, want an operation ID", op)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
		} `json:"items"`
	}
	e.cli.JSON(t, &list, "audit", "v2", "audit-event-export", "list", "--parent-id", e.inst.TenantID, "--region", region)
	if len(list.Items) != 1 {
		t.Fatalf("export list = %+v, want exactly the started export", list.Items)
	}
	return list.Items[0].Metadata.ID
}

func (e *env) startArgs() []string {
	return []string{
		"audit", "v2", "audit-event-export", "start",
		"--parent-id", e.inst.TenantID, "--region", region,
		"--params-from", e.from, "--params-to", e.to,
		"--nebius-object-storage-bucket-by-id-id", e.bucketID,
		"--params-event-type", "control_plane",
		"--params-filter", "resource.metadata.type='group'",
	}
}

// exportState returns the state of an export read with "audit-event-export get".
func (e *env) exportState(t *testing.T, id string) string {
	t.Helper()
	var exp struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	e.cli.JSON(t, &exp, "audit", "v2", "audit-event-export", "get", "--id", id)
	return exp.Status.State
}

// objects returns key → content of every object in the export bucket.
func (e *env) objects(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for info, err := range e.store.List(t.Context(), bucket, "") {
		if errors.Is(err, objstore.ErrBucketNotFound) {
			return out // nothing was written, so the bucket was never created
		}
		if err != nil {
			t.Fatalf("list bucket: %v", err)
		}
		rc, _, err := e.store.Get(t.Context(), bucket, info.Key)
		if err != nil {
			t.Fatalf("get %s: %v", info.Key, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", info.Key, err)
		}
		out[info.Key] = string(b)
	}
	return out
}

// TestAuditEventList follows "Get audit events": list with the CLI, with and
// without a filter.
func TestAuditEventList(t *testing.T) {
	e := newEnv(t)
	list := func(filter string) []*auditv2pb.AuditEvent {
		t.Helper()
		args := []string{"audit", "v2", "audit-event", "list", "--parent-id", e.inst.TenantID, "--start", e.from, "--end", e.to, "--region", region}
		if filter != "" {
			args = append(args, "--filter", filter)
		}
		res, err := e.cli.Run(t.Context(), append(args, "--format", "json")...)
		if err != nil {
			t.Fatalf("nebius %s: %v\n%s", strings.Join(args, " "), err, res.Stderr)
		}
		var resp auditv2pb.ListAuditEventResponse
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(res.Stdout), &resp); err != nil {
			t.Fatalf("decode %q: %v", res.Stdout, err)
		}
		return resp.GetItems()
	}

	got := list("action='CREATE'")
	var ev *auditv2pb.AuditEvent
	for _, x := range got {
		if x.GetResource().GetMetadata().GetId() == e.groupID {
			ev = x
		}
		if x.GetAction() != "CREATE" {
			t.Errorf("filter action='CREATE' returned a %s event", x.GetAction())
		}
	}
	if ev == nil {
		t.Fatalf("no CREATE event for group %s in %d events", e.groupID, len(got))
	}
	if ev.GetService().GetName() != "IAM" || ev.GetResource().GetMetadata().GetName() != "audited-by-cli" ||
		ev.GetAuthentication().GetSubject().GetTenantUserId() != config.DefaultDevPrincipal || ev.GetStatus() != auditv2pb.Status_DONE {
		t.Errorf("group CREATE event = %v", ev)
	}
	if h := ev.GetResource().GetHierarchy(); len(h) != 1 || h[0].GetId() != e.inst.TenantID {
		t.Errorf("hierarchy = %v, want [%s]", h, e.inst.TenantID)
	}

	if n := len(list("action='CREATE' AND resource.metadata.id='" + e.groupID + "'")); n != 1 {
		t.Errorf("filter by group id = %d events, want 1", n)
	}
	if n := len(list("action='DELETE'")); n != 0 {
		t.Errorf("filter action='DELETE' = %d events, want 0", n)
	}
	if all := list(""); len(all) < len(got) {
		t.Errorf("unfiltered list = %d events, fewer than the %d CREATE events", len(all), len(got))
	}
}

// TestAuditEventExport follows "Export audit events": start, get, list and
// check the JSON Lines objects.
func TestAuditEventExport(t *testing.T) {
	e := newEnv(t)
	id := e.startExport(t)

	if st := e.exportState(t, id); st != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("export state = %s, want AUDIT_EVENT_EXPORT_STATE_DONE", st)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
		} `json:"items"`
	}
	e.cli.JSON(t, &list, "audit", "v2", "audit-event-export", "list", "--parent-id", e.inst.TenantID, "--region", region)
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != id {
		t.Fatalf("export list = %+v, want only %s", list.Items, id)
	}

	objs := e.objects(t)
	if len(objs) != 1 {
		t.Fatalf("bucket %s holds %d objects, want 1: %v", bucket, len(objs), objs)
	}
	for key, body := range objs {
		if !strings.HasPrefix(key, "auditlogs/") || !strings.HasSuffix(key, ".jsonl") {
			t.Errorf("object key = %q, want auditlogs/…jsonl", key)
		}
		sc := bufio.NewScanner(strings.NewReader(body))
		lines := 0
		for sc.Scan() {
			lines++
			ev := &auditv2pb.AuditEvent{}
			if err := protojson.Unmarshal(sc.Bytes(), ev); err != nil {
				t.Fatalf("line %q is not an audit event: %v", sc.Text(), err)
			}
			if ev.GetResource().GetMetadata().GetType() != "group" || ev.GetAction() != "CREATE" || ev.GetResource().GetMetadata().GetId() != e.groupID {
				t.Errorf("exported event = %v, want the CREATE of group %s", ev, e.groupID)
			}
		}
		if lines != 1 {
			t.Errorf("object has %d events, want 1 (the group CREATE)", lines)
		}
	}
}

// TestAuditEventExportCancel starts an export that stays RUNNING, then
// cancels it: nothing is written.
func TestAuditEventExportCancel(t *testing.T) {
	e := newEnv(t, testkit.WithAuditExportDuration(time.Hour))
	id := e.startExportAsync(t) // the export stays RUNNING for an hour
	if st := e.exportState(t, id); st != "AUDIT_EVENT_EXPORT_STATE_RUNNING" {
		t.Fatalf("export state = %s, want AUDIT_EVENT_EXPORT_STATE_RUNNING", st)
	}
	res, err := e.cli.Run(t.Context(), "audit", "v2", "audit-event-export", "cancel", "--id", id)
	if err != nil {
		t.Fatalf("cancel: %v\n%s", err, res.Stderr)
	}
	if st := e.exportState(t, id); st != "AUDIT_EVENT_EXPORT_STATE_CANCELED" {
		t.Fatalf("export state after cancel = %s, want AUDIT_EVENT_EXPORT_STATE_CANCELED", st)
	}
	if objs := e.objects(t); len(objs) != 0 {
		t.Fatalf("canceled export wrote %d objects: %v", len(objs), objs)
	}
}
