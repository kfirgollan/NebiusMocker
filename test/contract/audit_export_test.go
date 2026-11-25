package contract

import (
	"bufio"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

// exportBucket is the name of the Object Storage bucket the tests export into;
// it is also the name of the backend bucket in the memory object store.
const exportBucket = "audit-export"

// startStorageAudit starts the audit and storage modules over mem.
func startStorageAudit(t *testing.T, mem *memory.Store, opts ...testkit.Option) *auditEnv {
	t.Helper()
	return startAuditWith(t, []mocker.Module{storage.New(storage.WithS3Listen("127.0.0.1:0"))},
		append([]testkit.Option{testkit.WithObjStore(mem)}, opts...)...)
}

// addExportBucket creates the Object Storage bucket exports go to, in the
// project, through the storage SDK as the dev admin, and records its ID.
func (e *auditEnv) addExportBucket(t *testing.T) {
	t.Helper()
	b := createBucket(t, storagesvc.NewBucketService(e.inst.SDK(t)), e.inst.ProjectID, exportBucket, &storagepb.BucketSpec{})
	e.bucketID = b.GetMetadata().GetId()
}

// startExportEnv starts an instance writing exports to an in-memory store
// through a real storage bucket, with three groups already created.
func startExportEnv(t *testing.T, opts ...testkit.Option) (*auditEnv, *memory.Store) {
	t.Helper()
	mem := memory.New(clock.Real{})
	env := startStorageAudit(t, mem, opts...)
	env.addExportBucket(t)
	for _, name := range []string{"exp-a", "exp-b", "exp-c"} {
		createGroup(t, env.groups, env.inst.TenantID, name, nil)
	}
	return env, mem
}

// startReq asks to export into the test bucket.
func (e *auditEnv) startReq(filter, prefix string) *auditv2pb.StartRequest {
	return e.startReqTo(filter, prefix, e.bucketID)
}

func (e *auditEnv) startReqTo(filter, prefix, bucket string) *auditv2pb.StartRequest {
	from, to := auditWindow()
	return &auditv2pb.StartRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: e.inst.TenantID, Name: "export"},
		Spec: &auditv2pb.AuditEventExportSpec{
			Params: &auditv2pb.AuditEventExportParams{
				From: from, To: to, Filter: filter, EventType: auditv2pb.EventType_CONTROL_PLANE,
			},
			ExportDestination: &auditv2pb.AuditEventExportSpec_NebiusObjectStorage{
				NebiusObjectStorage: &auditv2pb.NebiusObjectStorageDestination{
					ObjectPrefix: prefix,
					Bucket: &auditv2pb.NebiusObjectStorageDestination_BucketById{
						BucketById: &auditv2pb.BucketById{Id: bucket},
					},
				},
			},
		},
		Region: auditRegion,
	}
}

// startExport starts an export and waits for its operation; it returns the
// export ID.
func (e *auditEnv) startExport(t *testing.T, req *auditv2pb.StartRequest) string {
	t.Helper()
	op, err := e.exports.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start export: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Start export wait: %v", err)
	}
	return op.ResourceID()
}

func (e *auditEnv) getExport(t *testing.T, id string) *auditv2pb.AuditEventExport {
	t.Helper()
	exp, err := e.exports.Get(t.Context(), &auditv2pb.GetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get export %s: %v", id, err)
	}
	return exp
}

// exportedObjects returns the keys written under prefix, in key order.
func exportedObjects(t *testing.T, mem *memory.Store, prefix string) []string {
	t.Helper()
	var keys []string
	for info, err := range mem.List(t.Context(), exportBucket, prefix) {
		if errors.Is(err, objstore.ErrBucketNotFound) {
			return nil
		}
		if err != nil {
			t.Fatalf("list %s/%s: %v", exportBucket, prefix, err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}

// exportedEvents parses every JSON Lines object under prefix.
func exportedEvents(t *testing.T, mem *memory.Store, prefix string) []*auditv2pb.AuditEvent {
	t.Helper()
	var evs []*auditv2pb.AuditEvent
	for _, key := range exportedObjects(t, mem, prefix) {
		rc, _, err := mem.Get(t.Context(), exportBucket, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			ev := &auditv2pb.AuditEvent{}
			if err := protojson.Unmarshal(sc.Bytes(), ev); err != nil {
				t.Fatalf("object %s has a line that is not an AuditEvent: %v\n%s", key, err, sc.Bytes())
			}
			evs = append(evs, ev)
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("scan %s: %v", key, err)
		}
	}
	return evs
}

func TestAuditExportWritesMatchingEvents(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t)
	filter := "resource.metadata.type='group'"

	op, err := env.exports.Start(t.Context(), env.startReq(filter, ""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	exp := env.getExport(t, op.ResourceID())

	if got := exp.GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v, want DONE (message: %q)", got, exp.GetStatus().GetMessage())
	}
	if got := exp.GetStatus().GetExportOperationId(); got != op.ID() {
		t.Errorf("export_operation_id = %q, want the operation id %q", got, op.ID())
	}
	st := exp.GetStatus()
	if st.GetStartedAt() == nil || st.GetFinishedAt() == nil {
		t.Errorf("started_at/finished_at = %v/%v, want both set", st.GetStartedAt(), st.GetFinishedAt())
	} else if st.GetFinishedAt().AsTime().Before(st.GetStartedAt().AsTime()) {
		t.Errorf("finished_at %v is before started_at %v", st.GetFinishedAt().AsTime(), st.GetStartedAt().AsTime())
	}
	if got := exp.GetMetadata().GetParentId(); got != env.inst.TenantID {
		t.Errorf("parent_id = %q, want %q", got, env.inst.TenantID)
	}
	if got := exp.GetSpec().GetNebiusObjectStorage().GetObjectPrefix(); got != "auditlogs" {
		t.Errorf("object_prefix = %q, want the default auditlogs", got)
	}

	prefix := "auditlogs/" + st.GetExportOperationId() + "/"
	keys := exportedObjects(t, mem, prefix)
	if len(keys) == 0 {
		t.Fatalf("no objects under %s in bucket %s", prefix, exportBucket)
	}
	got := eventIDs(exportedEvents(t, mem, prefix))
	want := eventIDs(env.list(t, filter))
	if len(want) != 3 {
		t.Fatalf("List returned %d group events, want 3", len(want))
	}
	if !slices.Equal(got, want) {
		t.Errorf("exported event ids = %v, want the List result %v", got, want)
	}
	for _, ev := range exportedEvents(t, mem, prefix) {
		if ev.GetResource().GetMetadata().GetType() != "group" {
			t.Errorf("exported event %s has type %q, want group", ev.GetId(), ev.GetResource().GetMetadata().GetType())
		}
	}
}

func TestAuditExportCustomPrefix(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t)
	id := env.startExport(t, env.startReq("action='CREATE'", "my/custom-prefix/"))
	exp := env.getExport(t, id)
	if exp.GetStatus().GetState() != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v, want DONE", exp.GetStatus().GetState())
	}
	prefix := "my/custom-prefix/" + exp.GetStatus().GetExportOperationId() + "/"
	keys := exportedObjects(t, mem, prefix)
	if len(keys) == 0 {
		t.Fatalf("no objects under %s", prefix)
	}
	if got := len(exportedObjects(t, mem, "auditlogs/")); got != 0 {
		t.Errorf("%d objects under the default prefix, want 0", got)
	}
	want := eventIDs(env.list(t, "action='CREATE'"))
	if got := eventIDs(exportedEvents(t, mem, prefix)); !slices.Equal(got, want) {
		t.Errorf("exported ids = %v, want %v", got, want)
	}
}

func TestAuditExportNoMatchWritesNothing(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t)
	id := env.startExport(t, env.startReq("resource.metadata.name='nothing-like-this'", ""))
	exp := env.getExport(t, id)
	if exp.GetStatus().GetState() != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v, want DONE", exp.GetStatus().GetState())
	}
	if keys := exportedObjects(t, mem, "auditlogs/"); len(keys) != 0 {
		t.Errorf("objects = %v, want none for an export that matches nothing", keys)
	}
}

func TestAuditExportList(t *testing.T) {
	t.Parallel()
	env, _ := startExportEnv(t)
	var want []string
	for i := range 3 {
		req := env.startReq("", "")
		req.Metadata.Name = "export-" + string(rune('a'+i))
		want = append(want, env.startExport(t, req))
	}
	list := func(token string) *auditv2pb.ListAuditEventExportResponse {
		t.Helper()
		resp, err := env.exports.List(t.Context(), &auditv2pb.ListRequest{
			ParentId: env.inst.TenantID, PageSize: 2, PageToken: token, Region: auditRegion,
		})
		if err != nil {
			t.Fatalf("List exports: %v", err)
		}
		return resp
	}
	ids := func(items []*auditv2pb.AuditEventExport) []string {
		var out []string
		for _, e := range items {
			out = append(out, e.GetMetadata().GetId())
		}
		return out
	}
	p1 := list("")
	if len(p1.GetItems()) != 2 || p1.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %d items, token %q; want 2 items and a token", len(p1.GetItems()), p1.GetNextPageToken())
	}
	p2 := list(p1.GetNextPageToken())
	if len(p2.GetItems()) != 1 || p2.GetNextPageToken() != "" {
		t.Fatalf("page 2 = %d items, token %q; want 1 item and no token", len(p2.GetItems()), p2.GetNextPageToken())
	}
	if got := append(ids(p1.GetItems()), ids(p2.GetItems())...); !slices.Equal(got, want) {
		t.Errorf("paged export ids = %v, want %v", got, want)
	}

	var viaIter []string
	for e, err := range env.exports.Filter(t.Context(), &auditv2pb.ListRequest{
		ParentId: env.inst.TenantID, PageSize: 1, Region: auditRegion,
	}) {
		if err != nil {
			t.Fatalf("Filter iterator: %v", err)
		}
		viaIter = append(viaIter, e.GetMetadata().GetId())
	}
	if !slices.Equal(viaIter, want) {
		t.Errorf("iterator export ids = %v, want %v", viaIter, want)
	}
}

func TestAuditExportCancel(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t, testkit.WithAuditExportDuration(time.Hour))
	ctx := t.Context()

	op, err := env.exports.Start(ctx, env.startReq("", ""))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := op.ResourceID()
	if got := env.getExport(t, id).GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_RUNNING {
		t.Fatalf("state after Start = %v, want RUNNING", got)
	}

	cancel, err := env.exports.Cancel(ctx, &auditv2pb.CancelRequest{Id: id})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := cancel.Wait(ctx); err != nil {
		t.Fatalf("Cancel wait: %v", err)
	}
	exp := env.getExport(t, id)
	if got := exp.GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_CANCELED {
		t.Errorf("state after Cancel = %v, want CANCELED", got)
	}
	if exp.GetStatus().GetFinishedAt() == nil {
		t.Error("finished_at is not set on a canceled export")
	}
	if keys := exportedObjects(t, mem, "auditlogs/"); len(keys) != 0 {
		t.Errorf("a canceled export wrote %v", keys)
	}

	_, err = env.exports.Cancel(ctx, &auditv2pb.CancelRequest{Id: id})
	assertCode(t, err, codes.FailedPrecondition)
	_, err = env.exports.Cancel(ctx, &auditv2pb.CancelRequest{Id: "auditeventexport-e00missing00000000"})
	assertCode(t, err, codes.NotFound)
}

func TestAuditExportCancelFinished(t *testing.T) {
	t.Parallel()
	env, _ := startExportEnv(t)
	id := env.startExport(t, env.startReq("", ""))
	if got := env.getExport(t, id).GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v, want DONE", got)
	}
	_, err := env.exports.Cancel(t.Context(), &auditv2pb.CancelRequest{Id: id})
	assertCode(t, err, codes.FailedPrecondition)
	if got := env.getExport(t, id).GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Errorf("state after rejected Cancel = %v, want still DONE", got)
	}
}

func TestAuditExportInvalidRequests(t *testing.T) {
	t.Parallel()
	env, _ := startExportEnv(t)
	ctx := t.Context()

	mod := func(fn func(*auditv2pb.StartRequest)) *auditv2pb.StartRequest {
		req := env.startReq("", "")
		fn(req)
		return req
	}
	tests := []struct {
		name string
		req  *auditv2pb.StartRequest
	}{
		{"invalid filter", env.startReq("action ~ 'x'", "")},
		{"unknown filter field", env.startReq("bogus='x'", "")},
		{"bad regex", env.startReq("regex(action, '(')", "")},
		{"bucket id of another kind", env.startReqTo("", "", "project-x")},
		{"unknown bucket", env.startReqTo("", "", "storagebucket-e00missing00000000")},
		{"data plane events", mod(func(r *auditv2pb.StartRequest) { r.Spec.Params.EventType = auditv2pb.EventType_DATA_PLANE })},
		{"unknown region", mod(func(r *auditv2pb.StartRequest) { r.Region = "mars-1" })},
		{"event type unspecified", mod(func(r *auditv2pb.StartRequest) {
			r.Spec.Params.EventType = auditv2pb.EventType_EVENT_TYPE_UNSPECIFIED
		})},
		{"to before from", mod(func(r *auditv2pb.StartRequest) {
			from, to := r.GetSpec().GetParams().GetFrom(), r.GetSpec().GetParams().GetTo()
			r.Spec.Params.From, r.Spec.Params.To = to, from
		})},
		{"missing params", mod(func(r *auditv2pb.StartRequest) { r.Spec.Params = nil })},
		{"missing destination", mod(func(r *auditv2pb.StartRequest) { r.Spec.ExportDestination = nil })},
		{"missing region", mod(func(r *auditv2pb.StartRequest) { r.Region = "" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.exports.Start(ctx, tt.req)
			assertCode(t, err, codes.InvalidArgument)
		})
	}

	t.Run("unknown parent", func(t *testing.T) {
		t.Parallel()
		req := env.startReq("", "")
		req.Metadata.ParentId = "tenant-e00missing00000000"
		_, err := env.exports.Start(ctx, req)
		assertCode(t, err, codes.NotFound)
	})
	t.Run("get unknown id", func(t *testing.T) {
		t.Parallel()
		_, err := env.exports.Get(ctx, &auditv2pb.GetRequest{Id: "auditeventexport-e00missing00000000"})
		assertCode(t, err, codes.NotFound)
	})
}

// TestAuditExportDataPlaneRejected checks that DATA_PLANE exports are refused by name.
func TestAuditExportDataPlaneRejected(t *testing.T) {
	t.Parallel()
	env, _ := startExportEnv(t)
	req := env.startReq("", "")
	req.Spec.Params.EventType = auditv2pb.EventType_DATA_PLANE
	_, err := env.exports.Start(t.Context(), req)
	assertCode(t, err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "DATA_PLANE") {
		t.Errorf("err = %v, want it to name DATA_PLANE", err)
	}
}

// TestAuditExportNeedsStorageModule checks that exports refuse to start when
// the storage module, which owns the destination buckets, is not enabled.
func TestAuditExportNeedsStorageModule(t *testing.T) {
	t.Parallel()
	env := startAudit(t, testkit.WithObjStore(memory.New(clock.Real{})))
	_, err := env.exports.Start(t.Context(), env.startReqTo("", "", "storagebucket-e00anything000000"))
	assertCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "enable the storage module") {
		t.Errorf("err = %v, want it to say how to enable storage", err)
	}
	list, err := env.exports.List(t.Context(), &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: auditRegion})
	if err != nil || len(list.GetItems()) != 0 {
		t.Errorf("List exports = %v, %v; want none", list, err)
	}
}

// TestAuditExportParentProject checks that an export started on the project
// exports into a bucket of that project.
func TestAuditExportParentProject(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t)
	req := env.startReq("", "")
	req.Metadata.ParentId = env.inst.ProjectID
	id := env.startExport(t, req)
	exp := env.getExport(t, id)
	if got := exp.GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v (%s), want DONE", got, exp.GetStatus().GetMessage())
	}
	prefix := "auditlogs/" + exp.GetStatus().GetExportOperationId() + "/"
	if len(exportedEvents(t, mem, prefix)) == 0 {
		t.Errorf("no events exported for the project under %s", prefix)
	}
}

func TestAuditUnknownRegion(t *testing.T) {
	t.Parallel()
	env, _ := startExportEnv(t)
	ctx := t.Context()

	listReq := auditListReq(env.inst.TenantID, "")
	listReq.Region = "mars-1"
	_, err := env.events.List(ctx, listReq)
	assertCode(t, err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "unknown region mars-1; known: eu-north1") {
		t.Errorf("List err = %v, want the unknown region and the known ones", err)
	}

	startReq := env.startReq("", "")
	startReq.Region = "mars-1"
	_, err = env.exports.Start(ctx, startReq)
	assertCode(t, err, codes.InvalidArgument)

	_, err = env.exports.List(ctx, &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: "mars-1"})
	assertCode(t, err, codes.InvalidArgument)
}

// TestAuditSecondRegion checks that the mocker records every event in its
// default region: another known region lists nothing, an export started there
// writes nothing and is listed only there.
func TestAuditSecondRegion(t *testing.T) {
	t.Parallel()
	env, mem := startExportEnv(t)
	ctx := t.Context()
	const other = "eu-west1"

	if got := len(env.list(t, "")); got == 0 {
		t.Fatalf("default region has no events; the check below would prove nothing")
	}
	listReq := auditListReq(env.inst.TenantID, "")
	listReq.Region = other
	resp, err := env.events.List(ctx, listReq)
	if err != nil {
		t.Fatalf("List in %s: %v", other, err)
	}
	if len(resp.GetItems()) != 0 {
		t.Errorf("List in %s returned %d events, want 0", other, len(resp.GetItems()))
	}

	req := env.startReq("", "")
	req.Region = other
	id := env.startExport(t, req)
	exp := env.getExport(t, id)
	if got := exp.GetStatus().GetState(); got != auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE {
		t.Fatalf("state = %v (%s), want DONE", got, exp.GetStatus().GetMessage())
	}
	if keys := exportedObjects(t, mem, "auditlogs/"); len(keys) != 0 {
		t.Errorf("export in %s wrote %v, want nothing", other, keys)
	}

	inOther, err := env.exports.List(ctx, &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: other})
	if err != nil || len(inOther.GetItems()) != 1 || inOther.GetItems()[0].GetMetadata().GetId() != id {
		t.Fatalf("List exports in %s = %v, %v; want the export", other, inOther, err)
	}
	inDefault, err := env.exports.List(ctx, &auditv2pb.ListRequest{ParentId: env.inst.TenantID, Region: auditRegion})
	if err != nil || len(inDefault.GetItems()) != 0 {
		t.Errorf("List exports in %s = %v, %v; want none", auditRegion, inDefault, err)
	}

	// The same export in the default region does write.
	defReq := env.startReq("", "")
	defReq.Metadata.Name = "export-default"
	defID := env.startExport(t, defReq)
	defExp := env.getExport(t, defID)
	if keys := exportedObjects(t, mem, "auditlogs/"+defExp.GetStatus().GetExportOperationId()+"/"); len(keys) == 0 {
		t.Errorf("export in %s wrote nothing", auditRegion)
	}
}
