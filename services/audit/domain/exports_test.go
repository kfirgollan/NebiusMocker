package domain_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	objmemory "github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

const (
	backendBucket = "backend-" + bucketID
	exportDelay   = time.Minute
)

type rig struct {
	svc     *domain.ExportService
	exports *fakeExports
	objects *objmemory.Store
	clk     clock.Clock
}

type rigOpts struct {
	delay  time.Duration
	log    *audit.Log
	writer domain.ObjectWriter // defaults to the memory store
	clk    clock.Clock         // defaults to a manual clock
	// noBackendBucket skips creating the backend bucket.
	noBackendBucket bool
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	if o.clk == nil {
		o.clk = clock.NewManual(t0)
	}
	if o.log == nil {
		o.log = newLog(t, 100, projectEvents(3)...)
	}
	r := &rig{exports: newFakeExports(), objects: objmemory.New(o.clk), clk: o.clk}
	if !o.noBackendBucket {
		// The storage module creates the backend bucket with the bucket.
		if err := r.objects.EnsureBucket(t.Context(), backendBucket); err != nil {
			t.Fatal(err)
		}
	}
	if o.writer == nil {
		o.writer = r.objects
	}
	svc, err := domain.NewExportService(domain.ExportDeps{
		Exports: r.exports, Events: o.log, Hierarchy: newFakeHierarchy(), Buckets: fakeBuckets{},
		Writer: o.writer, Ops: &inlineOps{}, IDs: &seqIDs{}, Clock: o.clk, Delay: o.delay, Regions: knownRegions,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

// projectEvents returns n events under the test project, one per second from t0.
func projectEvents(n int) []eventSpec {
	specs := make([]eventSpec, n)
	for i := range specs {
		specs[i] = eventSpec{
			id: fmt.Sprintf("e%d", i+1), at: t0.Add(time.Duration(i) * time.Second), service: "IAM",
			action: "CREATE", resID: fmt.Sprintf("group-%d", i+1), hierarchy: []string{tenantID, projectID},
		}
	}
	return specs
}

func startCmd() domain.StartExportCmd {
	return domain.StartExportCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "nightly"},
		Region:   region,
		Spec: &auditv2pb.AuditEventExportSpec{
			Params: &auditv2pb.AuditEventExportParams{
				From: timestamppb.New(t0), To: timestamppb.New(t0.Add(24 * time.Hour)),
				EventType: auditv2pb.EventType_CONTROL_PLANE,
			},
			ExportDestination: &auditv2pb.AuditEventExportSpec_NebiusObjectStorage{
				NebiusObjectStorage: &auditv2pb.NebiusObjectStorageDestination{
					Bucket: &auditv2pb.NebiusObjectStorageDestination_BucketById{
						BucketById: &auditv2pb.BucketById{Id: bucketID},
					},
				},
			},
		},
	}
}

// startOK starts an export and returns its current state.
func (r *rig) startOK(t *testing.T, cmd domain.StartExportCmd) (opID string, exp *auditv2pb.AuditEventExport) {
	t.Helper()
	op, err := r.svc.Start(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return op.ID, r.get(t, op.ResourceID)
}

func (r *rig) get(t *testing.T, id string) *auditv2pb.AuditEventExport {
	t.Helper()
	e, err := r.svc.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return e
}

// objectKeys lists the keys in the backend bucket.
func (r *rig) objectKeys(t *testing.T) []string {
	t.Helper()
	var keys []string
	for info, err := range r.objects.List(t.Context(), backendBucket, "") {
		if errors.Is(err, objstore.ErrBucketNotFound) {
			return nil // nothing was written, so the bucket was never created
		}
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}

// readLines returns the JSON Lines of one object and checks they end in '\n'.
func (r *rig) readLines(t *testing.T, key string) []string {
	t.Helper()
	rc, info, err := r.objects.Get(t.Context(), backendBucket, key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	if info.ContentType != "application/x-ndjson" {
		t.Errorf("%s content type = %q, want application/x-ndjson", key, info.ContentType)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("%s does not end with a newline", key)
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func state(e *auditv2pb.AuditEventExport) string { return e.GetStatus().GetState().String() }

func TestStartExportsMatchingEvents(t *testing.T) {
	t.Parallel()
	specs := append(projectEvents(3),
		eventSpec{id: "other", at: at(1), service: "IAM", action: "CREATE", resID: "group-9", hierarchy: []string{otherTenantID}},
		eventSpec{id: "late", at: t0.Add(48 * time.Hour), service: "IAM", action: "CREATE", resID: "group-8", hierarchy: []string{tenantID, projectID}},
	)
	r := newRig(t, rigOpts{log: newLog(t, 100, specs...)})

	opID, exp := r.startOK(t, startCmd())

	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state = %s (%s), want DONE", got, exp.GetStatus().GetMessage())
	}
	if exp.GetStatus().GetExportOperationId() != opID || opID != "operation-1" {
		t.Errorf("export_operation_id = %q, op = %q", exp.GetStatus().GetExportOperationId(), opID)
	}
	if exp.GetStatus().GetStartedAt() == nil || exp.GetStatus().GetFinishedAt() == nil {
		t.Errorf("status = %v, want started_at and finished_at", exp.GetStatus())
	}
	if p := exp.GetSpec().GetNebiusObjectStorage().GetObjectPrefix(); p != "auditlogs" {
		t.Errorf("object_prefix = %q, want default auditlogs", p)
	}
	md := exp.GetMetadata()
	if md.GetParentId() != projectID || md.GetName() != "nightly" || md.GetResourceVersion() < 2 || !strings.HasPrefix(md.GetId(), domain.KindExport+"-") {
		t.Errorf("metadata = %v", md)
	}

	wantKey := "auditlogs/" + opID + "/audit-events-000001.jsonl"
	if keys := r.objectKeys(t); !slices.Equal(keys, []string{wantKey}) {
		t.Fatalf("keys = %v, want [%s]", keys, wantKey)
	}
	lines := r.readLines(t, wantKey)
	var gotIDs []string
	for _, l := range lines {
		ev := &auditv2pb.AuditEvent{}
		if err := protojson.Unmarshal([]byte(l), ev); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if !strings.Contains(l, `"spec_version"`) && !strings.Contains(l, `"action"`) {
			t.Errorf("line is not in proto field names: %s", l)
		}
		gotIDs = append(gotIDs, ev.GetId())
	}
	if want := []string{"e1", "e2", "e3"}; !slices.Equal(gotIDs, want) {
		t.Fatalf("exported events = %v, want %v", gotIDs, want)
	}
}

func TestStartAppliesPrefixAndFilter(t *testing.T) {
	t.Parallel()
	specs := projectEvents(3)
	specs[1].action = "DELETE"
	r := newRig(t, rigOpts{log: newLog(t, 100, specs...)})
	cmd := startCmd()
	cmd.Spec.GetNebiusObjectStorage().ObjectPrefix = "team/audit/"
	cmd.Spec.Params.Filter = "action='DELETE'"

	opID, exp := r.startOK(t, cmd)

	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state = %s", got)
	}
	key := "team/audit/" + opID + "/audit-events-000001.jsonl"
	if keys := r.objectKeys(t); !slices.Equal(keys, []string{key}) {
		t.Fatalf("keys = %v, want [%s]", keys, key)
	}
	if lines := r.readLines(t, key); len(lines) != 1 || !strings.Contains(lines[0], `"id":"e2"`) {
		t.Fatalf("lines = %v, want only e2", lines)
	}
}

func TestStartChunksObjects(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{log: newLog(t, 10_100, projectEvents(10_001)...)})

	opID, exp := r.startOK(t, startCmd())

	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state = %s (%s)", got, exp.GetStatus().GetMessage())
	}
	prefix := "auditlogs/" + opID + "/"
	want := []string{prefix + "audit-events-000001.jsonl", prefix + "audit-events-000002.jsonl"}
	if keys := r.objectKeys(t); !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if n := len(r.readLines(t, want[0])); n != 10_000 {
		t.Errorf("first object has %d events, want 10000", n)
	}
	if n := len(r.readLines(t, want[1])); n != 1 {
		t.Errorf("second object has %d events, want 1", n)
	}
}

func TestStartWithNoMatchesWritesNoObjects(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*domain.StartExportCmd){
		"filter matches nothing": func(c *domain.StartExportCmd) { c.Spec.Params.Filter = "action='NOPE'" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpts{})
			cmd := startCmd()
			mutate(&cmd)
			_, exp := r.startOK(t, cmd)
			if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
				t.Fatalf("state = %s", got)
			}
			if keys := r.objectKeys(t); len(keys) != 0 {
				t.Fatalf("keys = %v, want none", keys)
			}
		})
	}
}

func TestStartExportsOnlyTheRequestedEventType(t *testing.T) {
	t.Parallel()
	under := []string{tenantID, projectID}
	tests := []struct {
		name string
		typ  auditv2pb.EventType
		want []string
	}{
		{"control plane", auditv2pb.EventType_CONTROL_PLANE, []string{"c1", "c2"}},
		{"data plane", auditv2pb.EventType_DATA_PLANE, []string{"d1", "d2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			log := newLog(t, 100,
				eventSpec{id: "c1", at: at(0), service: "IAM", action: "CREATE", resID: "group-1", hierarchy: under},
				eventSpec{id: "d1", at: at(1), service: "STORAGE", action: "PUT_OBJECT", resID: bucketID, hierarchy: under, dataPlane: true},
				eventSpec{id: "c2", at: at(2), service: "IAM", action: "DELETE", resID: "group-1", hierarchy: under},
				eventSpec{id: "d2", at: at(3), service: "STORAGE", action: "GET_OBJECT", resID: bucketID, hierarchy: under, dataPlane: true},
			)
			r := newRig(t, rigOpts{log: log})
			cmd := startCmd()
			cmd.Spec.Params.EventType = tt.typ
			_, exp := r.startOK(t, cmd)
			if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
				t.Fatalf("state = %s (%s), want DONE", got, exp.GetStatus().GetMessage())
			}
			var got []string
			for _, key := range r.objectKeys(t) {
				for _, l := range r.readLines(t, key) {
					ev := &auditv2pb.AuditEvent{}
					if err := protojson.Unmarshal([]byte(l), ev); err != nil {
						t.Fatalf("line %q: %v", l, err)
					}
					got = append(got, ev.GetId())
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("exported events = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartFailsWhenWriterFails(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{writer: failingWriter{err: errBoom}})

	_, exp := r.startOK(t, startCmd())

	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_FAILED" {
		t.Fatalf("state = %s, want FAILED", got)
	}
	if msg := exp.GetStatus().GetMessage(); !strings.Contains(msg, errBoom.Error()) {
		t.Errorf("message = %q, want it to contain %q", msg, errBoom)
	}
	if exp.GetStatus().GetFinishedAt() == nil {
		t.Error("finished_at not set")
	}
}

func TestStartValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.StartExportCmd)
		kind   errs.Kind
	}{
		{"id set", func(c *domain.StartExportCmd) { c.Metadata.Id = "x" }, errs.KindBadRequest},
		{"parent missing", func(c *domain.StartExportCmd) { c.Metadata.ParentId = "" }, errs.KindBadRequest},
		{"parent unknown", func(c *domain.StartExportCmd) { c.Metadata.ParentId = "project-nope" }, errs.KindNotFound},
		{"no params", func(c *domain.StartExportCmd) { c.Spec.Params = nil }, errs.KindBadRequest},
		{"no from", func(c *domain.StartExportCmd) { c.Spec.Params.From = nil }, errs.KindBadRequest},
		{"from equals to", func(c *domain.StartExportCmd) { c.Spec.Params.To = c.Spec.GetParams().GetFrom() }, errs.KindBadRequest},
		{"from after to", func(c *domain.StartExportCmd) { c.Spec.Params.From = timestamppb.New(t0.Add(48 * time.Hour)) }, errs.KindBadRequest},
		{"bad filter", func(c *domain.StartExportCmd) { c.Spec.Params.Filter = "nope='x'" }, errs.KindBadRequest},
		{"event type unspecified", func(c *domain.StartExportCmd) { c.Spec.Params.EventType = 0 }, errs.KindBadRequest},
		{"no destination", func(c *domain.StartExportCmd) { c.Spec.ExportDestination = nil }, errs.KindBadRequest},
		{"no bucket id", func(c *domain.StartExportCmd) {
			c.Spec.GetNebiusObjectStorage().Bucket = &auditv2pb.NebiusObjectStorageDestination_BucketById{BucketById: &auditv2pb.BucketById{}}
		}, errs.KindBadRequest},
		{"unknown event type", func(c *domain.StartExportCmd) { c.Spec.Params.EventType = 7 }, errs.KindBadRequest},
		{"unknown region", func(c *domain.StartExportCmd) { c.Region = "mars-1" }, errs.KindBadRequest},
		{"missing region", func(c *domain.StartExportCmd) { c.Region = "" }, errs.KindBadRequest},
		{"bucket rejected", func(c *domain.StartExportCmd) {
			c.Spec.GetNebiusObjectStorage().GetBucketById().Id = "storagebucket-other"
		}, errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpts{})
			cmd := startCmd()
			tt.mutate(&cmd)
			if _, err := r.svc.Start(t.Context(), cmd); !errs.IsKind(err, tt.kind) {
				t.Fatalf("Start err = %v, want kind %v", err, tt.kind)
			}
			if len(r.exports.order) != 0 {
				t.Fatalf("a rejected Start created %d exports", len(r.exports.order))
			}
		})
	}
}

func TestCancelFinishedExportIsBadState(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	_, exp := r.startOK(t, startCmd())

	_, err := r.svc.Cancel(t.Context(), exp.GetMetadata().GetId(), nil)

	if !errs.IsKind(err, errs.KindBadState) || !strings.Contains(err.Error(), "export is DONE") {
		t.Fatalf("Cancel err = %v, want BadState \"export is DONE\"", err)
	}
	if got := state(r.get(t, exp.GetMetadata().GetId())); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state after rejected cancel = %s", got)
	}
}

func TestCancelUnknownExport(t *testing.T) {
	t.Parallel()
	if _, err := newRig(t, rigOpts{}).svc.Cancel(t.Context(), "auditeventexport-nope", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Cancel err = %v, want NotFound", err)
	}
}

func TestDelayedExportFinishesAfterDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOpts{delay: exportDelay, clk: clock.Real{}})
		r.svc.StartRunner()
		defer r.svc.StopRunner()

		opID, exp := r.startOK(t, startCmd())
		synctest.Wait()
		if got := state(r.get(t, exp.GetMetadata().GetId())); got != "AUDIT_EVENT_EXPORT_STATE_RUNNING" {
			t.Fatalf("state before delay = %s, want RUNNING", got)
		}

		<-r.clk.After(exportDelay + time.Second)
		synctest.Wait()

		if got := state(r.get(t, exp.GetMetadata().GetId())); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
			t.Fatalf("state after delay = %s, want DONE", got)
		}
		if keys := r.objectKeys(t); len(keys) != 1 || !strings.Contains(keys[0], opID) {
			t.Fatalf("keys = %v", keys)
		}
	})
}

func TestCancelRunningExportStopsTheRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOpts{delay: exportDelay, clk: clock.Real{}})
		r.svc.StartRunner()
		defer r.svc.StopRunner()
		_, exp := r.startOK(t, startCmd())
		id := exp.GetMetadata().GetId()

		if _, err := r.svc.Cancel(t.Context(), id, nil); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		canceled := r.get(t, id)
		if got := state(canceled); got != "AUDIT_EVENT_EXPORT_STATE_CANCELED" {
			t.Fatalf("state right after Cancel = %s, want CANCELED", got)
		}
		if canceled.GetStatus().GetFinishedAt() == nil || canceled.GetStatus().GetMessage() != "canceled by user" {
			t.Errorf("status = %v", canceled.GetStatus())
		}

		<-r.clk.After(exportDelay + time.Second)
		synctest.Wait()

		if got := r.get(t, id); !proto.Equal(got, canceled) {
			t.Fatalf("export changed after the delayed run:\n got %v\nwant %v", got, canceled)
		}
		if keys := r.objectKeys(t); len(keys) != 0 {
			t.Fatalf("a canceled export wrote %v", keys)
		}
	})
}

func TestScheduleWithoutRunnerRunsImmediately(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{delay: exportDelay})
	_, exp := r.startOK(t, startCmd())
	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state = %s, want DONE: nothing would run it later", got)
	}
}

func TestStopRunnerLeavesWaitingExportRunning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, rigOpts{delay: exportDelay, clk: clock.Real{}})
		r.svc.StartRunner()
		_, exp := r.startOK(t, startCmd())
		synctest.Wait()

		r.svc.StopRunner()
		r.svc.StopRunner() // idempotent

		if got := state(r.get(t, exp.GetMetadata().GetId())); got != "AUDIT_EVENT_EXPORT_STATE_RUNNING" {
			t.Fatalf("state = %s, want RUNNING", got)
		}
	})
}

func TestExportListPaging(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	var want []string
	for i := range 3 {
		cmd := startCmd()
		cmd.Metadata.Name = fmt.Sprintf("export-%d", i)
		_, exp := r.startOK(t, cmd)
		want = append(want, exp.GetMetadata().GetId())
	}

	var got []string
	token := ""
	for pages := 1; ; pages++ {
		page, err := r.svc.List(t.Context(), projectID, region, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, e := range page.Items {
			got = append(got, e.GetMetadata().GetId())
		}
		if page.NextPageToken == "" {
			if pages != 2 {
				t.Fatalf("listing took %d pages, want 2", pages)
			}
			break
		}
		token = page.NextPageToken
	}
	if !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}

	if _, err := r.svc.List(t.Context(), projectID, region, 1001, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List page_size 1001 err = %v, want BadRequest", err)
	}
	if page, err := r.svc.List(t.Context(), otherTenantID, region, 0, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("List other parent = %v, %v", page, err)
	}
}

func TestGetUnknownExport(t *testing.T) {
	t.Parallel()
	if _, err := newRig(t, rigOpts{}).svc.Get(t.Context(), "auditeventexport-nope"); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get err = %v, want NotFound", err)
	}
}

func TestExportListUnknownRegion(t *testing.T) {
	t.Parallel()
	if _, err := newRig(t, rigOpts{}).svc.List(t.Context(), projectID, "mars-1", 0, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("List err = %v, want BadRequest", err)
	}
}

func TestExportInAnotherRegionWritesNothingAndListsThere(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	cmd := startCmd()
	cmd.Region = otherRegion

	_, exp := r.startOK(t, cmd)

	if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_DONE" {
		t.Fatalf("state = %s (%s), want DONE", got, exp.GetStatus().GetMessage())
	}
	if keys := r.objectKeys(t); len(keys) != 0 {
		t.Fatalf("keys = %v, want none: every event belongs to the default region", keys)
	}
	id := exp.GetMetadata().GetId()
	in, err := r.svc.List(t.Context(), projectID, otherRegion, 0, "")
	if err != nil || len(in.Items) != 1 || in.Items[0].GetMetadata().GetId() != id {
		t.Fatalf("List(%s) = %v, %v; want the export", otherRegion, in, err)
	}
	def, err := r.svc.List(t.Context(), projectID, region, 0, "")
	if err != nil || len(def.Items) != 0 {
		t.Fatalf("List(%s) = %v, %v; want none", region, def, err)
	}
}

func TestStartFailsWhenBackendBucketIsMissing(t *testing.T) {
	t.Parallel()
	for name, filter := range map[string]string{"events match": "", "nothing matches": "action='NOPE'"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpts{noBackendBucket: true})
			cmd := startCmd()
			cmd.Spec.Params.Filter = filter

			_, exp := r.startOK(t, cmd)

			if got := state(exp); got != "AUDIT_EVENT_EXPORT_STATE_FAILED" {
				t.Fatalf("state = %s, want FAILED", got)
			}
			if msg := exp.GetStatus().GetMessage(); !strings.Contains(msg, backendBucket) {
				t.Errorf("message = %q, want it to name %s", msg, backendBucket)
			}
		})
	}
}
