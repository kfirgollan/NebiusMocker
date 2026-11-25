package audit

import (
	"log/slog"
	"regexp"
	"strconv"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var t0 = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func newTestRecorder(t *testing.T) (*Recorder, *Log, *clock.Manual) {
	t.Helper()
	l, err := NewLog(100)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewManual(t0)
	r, err := NewRecorder(l, clk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return r, l, clk
}

func startedEvent(id, name string) *auditv2pb.AuditEvent {
	return &auditv2pb.AuditEvent{
		Action: "CREATE",
		Status: auditv2pb.Status_STARTED,
		Resource: &auditv2pb.Resource{
			Metadata: &auditv2pb.ResourceMetadata{Id: id, Name: name},
		},
	}
}

func events(t *testing.T, l *Log) []*auditv2pb.AuditEvent {
	t.Helper()
	var out []*auditv2pb.AuditEvent
	for e := range l.Since(0) {
		ev, err := e.Event()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestNewRecorderValidation(t *testing.T) {
	t.Parallel()
	l, err := NewLog(1)
	if err != nil {
		t.Fatal(err)
	}
	lg := slog.New(slog.DiscardHandler)
	clk := clock.NewManual(t0)
	if _, err := NewRecorder(nil, clk, lg); err == nil {
		t.Error("nil sink: error = nil")
	}
	if _, err := NewRecorder(l, nil, lg); err == nil {
		t.Error("nil clock: error = nil")
	}
	if _, err := NewRecorder(l, clk, nil); err == nil {
		t.Error("nil logger: error = nil")
	}
}

func TestRecordStampsEvent(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.Record(t.Context(), startedEvent("group-1", "g"), "")
	evs := events(t, l)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if !uuidV4.MatchString(ev.GetId()) {
		t.Errorf("id = %q, want UUID v4", ev.GetId())
	}
	if !ev.GetTime().AsTime().Equal(t0) {
		t.Errorf("time = %v, want %v", ev.GetTime().AsTime(), t0)
	}
	if ev.GetSpecVersion() != SpecVersion || ev.GetEventVersion() != EventVersion {
		t.Errorf("versions = %q/%q, want %q/%q", ev.GetSpecVersion(), ev.GetEventVersion(), SpecVersion, EventVersion)
	}
}

func TestRecordIDsUnique(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	for range 20 {
		r.Record(t.Context(), startedEvent("g-1", "g"), "")
	}
	seen := map[string]bool{}
	for _, ev := range events(t, l) {
		if seen[ev.GetId()] {
			t.Fatalf("duplicate id %q", ev.GetId())
		}
		seen[ev.GetId()] = true
	}
}

func TestRecordFillsNames(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.RememberName("tenant-t", "Tenant T")

	first := startedEvent("group-1", "my-group")
	first.Resource.Hierarchy = []*auditv2pb.ResourceMetadata{{Id: "tenant-t"}, {Id: "project-unknown"}}
	r.Record(t.Context(), first, "")

	second := startedEvent("group-1", "") // e.g. a Delete: id only
	second.Resource.Hierarchy = []*auditv2pb.ResourceMetadata{{Id: "tenant-t", Name: "explicit"}, {Id: "project-unknown"}}
	r.Record(t.Context(), second, "")

	third := startedEvent("group-1", "")
	third.Resource.Hierarchy = []*auditv2pb.ResourceMetadata{{Id: "tenant-t"}}
	r.Record(t.Context(), third, "")

	evs := events(t, l)
	if got := evs[0].GetResource().GetHierarchy()[0].GetName(); got != "Tenant T" {
		t.Errorf("hierarchy name via RememberName = %q, want Tenant T", got)
	}
	if got := evs[0].GetResource().GetHierarchy()[1].GetName(); got != "" {
		t.Errorf("unknown hierarchy name = %q, want empty", got)
	}
	if got := evs[1].GetResource().GetMetadata().GetName(); got != "my-group" {
		t.Errorf("remembered resource name = %q, want my-group", got)
	}
	if got := evs[1].GetResource().GetHierarchy()[0].GetName(); got != "explicit" {
		t.Errorf("explicit hierarchy name overwritten: %q", got)
	}
	if got := evs[2].GetResource().GetHierarchy()[0].GetName(); got != "Tenant T" {
		t.Errorf("hierarchy name = %q, want Tenant T", got)
	}
}

func TestRememberNameIgnoresEmpty(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.RememberName("", "x")
	r.RememberName("a-1", "")
	r.Record(t.Context(), startedEvent("a-1", ""), "")
	if got := events(t, l)[0].GetResource().GetMetadata().GetName(); got != "" {
		t.Errorf("name = %q, want empty", got)
	}
}

func TestOperationFinished(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		c          code.Code
		msg        string
		wantStatus auditv2pb.Status
	}{
		{name: "ok", c: code.Code_OK, wantStatus: auditv2pb.Status_DONE},
		{name: "error", c: code.Code_INTERNAL, msg: "boom", wantStatus: auditv2pb.Status_ERROR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, l, clk := newTestRecorder(t)
			r.Record(t.Context(), startedEvent("group-1", "g"), "op-1")
			clk.Advance(time.Minute)
			r.OperationFinished(t.Context(), "op-1", tt.c, tt.msg)
			evs := events(t, l)
			if len(evs) != 2 {
				t.Fatalf("events = %d, want 2", len(evs))
			}
			if evs[0].GetStatus() != auditv2pb.Status_STARTED {
				t.Errorf("first status = %v, want STARTED", evs[0].GetStatus())
			}
			done := evs[1]
			if done.GetStatus() != tt.wantStatus {
				t.Errorf("status = %v, want %v", done.GetStatus(), tt.wantStatus)
			}
			if done.GetResponse().GetStatusCode() != tt.c || done.GetResponse().GetErrorMessage() != tt.msg {
				t.Errorf("response = %v/%q, want %v/%q", done.GetResponse().GetStatusCode(), done.GetResponse().GetErrorMessage(), tt.c, tt.msg)
			}
			if done.GetId() == evs[0].GetId() {
				t.Error("finish event reuses the STARTED event id")
			}
			if !done.GetTime().AsTime().Equal(t0.Add(time.Minute)) {
				t.Errorf("finish time = %v, want %v", done.GetTime().AsTime(), t0.Add(time.Minute))
			}
			if done.GetResource().GetMetadata().GetId() != "group-1" {
				t.Errorf("resource id = %q, want group-1", done.GetResource().GetMetadata().GetId())
			}
			// A second report finds nothing pending.
			r.OperationFinished(t.Context(), "op-1", tt.c, tt.msg)
			if got := len(events(t, l)); got != 2 {
				t.Errorf("events after duplicate finish = %d, want 2", got)
			}
		})
	}
}

func TestOperationFinishedBeforeRecord(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.OperationFinished(t.Context(), "op-1", code.Code_OK, "")
	if got := len(events(t, l)); got != 0 {
		t.Fatalf("events before Record = %d, want 0", got)
	}
	r.Record(t.Context(), startedEvent("group-1", "g"), "op-1")
	evs := events(t, l)
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	if evs[1].GetStatus() != auditv2pb.Status_DONE {
		t.Errorf("second status = %v, want DONE", evs[1].GetStatus())
	}
	// The early result was consumed: a later finish emits nothing.
	r.OperationFinished(t.Context(), "op-1", code.Code_OK, "")
	if got := len(events(t, l)); got != 2 {
		t.Errorf("events = %d, want 2", got)
	}
}

func TestOperationFinishedUnknown(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.OperationFinished(t.Context(), "op-nobody", code.Code_OK, "")
	if got := l.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestForget(t *testing.T) {
	t.Parallel()
	r, l, _ := newTestRecorder(t)
	r.OperationFinished(t.Context(), "op-1", code.Code_OK, "")
	r.Forget("op-1")
	r.Record(t.Context(), startedEvent("group-1", "g"), "op-1")
	if got := l.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 (early result forgotten)", got)
	}
}

func TestEarlyBounded(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRecorder(t)
	for i := range maxEarly * 2 {
		r.OperationFinished(t.Context(), "op-"+strconv.Itoa(i), code.Code_OK, "")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.early) > maxEarly || len(r.order) > maxEarly {
		t.Errorf("early = %d, order = %d, want <= %d", len(r.early), len(r.order), maxEarly)
	}
	if _, ok := r.early["op-0"]; ok {
		t.Error("oldest early result not evicted")
	}
	if _, ok := r.early["op-"+strconv.Itoa(maxEarly*2-1)]; !ok {
		t.Error("newest early result missing")
	}
}

func TestEarlyReFinishedStaysBounded(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRecorder(t)
	// Reporting the same operation repeatedly must not grow the bookkeeping.
	for range maxEarly * 3 {
		r.OperationFinished(t.Context(), "op-same", code.Code_OK, "")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.early) != 1 {
		t.Errorf("len(early) = %d, want 1", len(r.early))
	}
	if len(r.order) > 1 {
		t.Errorf("len(order) = %d, want 1", len(r.order))
	}
}

func TestEarlyClaimedEntriesDoNotEvictLiveOnes(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRecorder(t)
	// Claimed and forgotten results must not count against the cap.
	for i := range maxEarly * 2 {
		id := "op-" + strconv.Itoa(i)
		r.OperationFinished(t.Context(), id, code.Code_OK, "")
		r.Forget(id)
	}
	r.OperationFinished(t.Context(), "op-live", code.Code_OK, "")
	for i := range maxEarly - 1 {
		r.OperationFinished(t.Context(), "op-fill-"+strconv.Itoa(i), code.Code_OK, "")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.early["op-live"]; !ok {
		t.Error("live early result evicted although the cap was not exceeded")
	}
	if len(r.order) > maxEarly {
		t.Errorf("len(order) = %d, want <= %d", len(r.order), maxEarly)
	}
}
