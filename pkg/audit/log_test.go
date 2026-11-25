package audit

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

func testEvent(id string) *auditv2pb.AuditEvent {
	return &auditv2pb.AuditEvent{
		Id:      id,
		Time:    timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		Action:  "CREATE",
		Type:    "ai.nebius.iam.group.create",
		Status:  auditv2pb.Status_DONE,
		Service: &auditv2pb.Service{Name: "IAM"},
		Resource: &auditv2pb.Resource{
			Metadata: &auditv2pb.ResourceMetadata{Id: "group-1", Name: "g", Type: "group"},
			Hierarchy: []*auditv2pb.ResourceMetadata{
				{Id: "tenant-a", Name: "ta"},
				{Id: "project-b", Name: "pb"},
			},
		},
	}
}

func seqs(l *Log, after uint64) []uint64 {
	var out []uint64
	for e := range l.Since(after) {
		out = append(out, e.Seq)
	}
	return out
}

func TestNewLogRejectsBadCapacity(t *testing.T) {
	t.Parallel()
	for _, c := range []int{0, -1} {
		if _, err := NewLog(c); err == nil {
			t.Errorf("NewLog(%d) error = nil, want error", c)
		}
	}
}

func TestLogAppendAssignsSeq(t *testing.T) {
	t.Parallel()
	l, err := NewLog(10)
	if err != nil {
		t.Fatal(err)
	}
	for want := uint64(1); want <= 3; want++ {
		e, err := l.Append(testEvent("e"))
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq != want {
			t.Errorf("Append seq = %d, want %d", e.Seq, want)
		}
	}
}

func TestLogEviction(t *testing.T) {
	t.Parallel()
	l, err := NewLog(3)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := l.Append(testEvent("e")); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Len(); got != 3 {
		t.Errorf("Len() = %d, want 3", got)
	}
	tests := []struct {
		after uint64
		want  []uint64
	}{
		{0, []uint64{3, 4, 5}},
		{3, []uint64{4, 5}},
		{5, nil},
		{99, nil},
	}
	for _, tt := range tests {
		if got := seqs(l, tt.after); !slices.Equal(got, tt.want) {
			t.Errorf("Since(%d) = %v, want %v", tt.after, got, tt.want)
		}
	}
}

func TestLogSinceStopsEarly(t *testing.T) {
	t.Parallel()
	l, err := NewLog(5)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := l.Append(testEvent("e")); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range l.Since(0) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("iterations = %d, want 1", n)
	}
}

func TestEntryEventRoundTrip(t *testing.T) {
	t.Parallel()
	l, err := NewLog(2)
	if err != nil {
		t.Fatal(err)
	}
	want := testEvent("evt-1")
	e, err := l.Append(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Event()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("Event() mismatch (-want +got):\n%s", diff)
	}
	if !e.Time.Equal(want.GetTime().AsTime()) {
		t.Errorf("Entry.Time = %v, want %v", e.Time, want.GetTime().AsTime())
	}
}

func TestEntryFields(t *testing.T) {
	t.Parallel()
	l, err := NewLog(2)
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Append(testEvent("evt-1"))
	if err != nil {
		t.Fatal(err)
	}
	want := Fields{
		Action: "CREATE", Type: "ai.nebius.iam.group.create", Status: "DONE", Service: "IAM",
		ResourceID: "group-1", ResourceName: "g", ResourceType: "group",
		HierarchyIDs:   []string{"tenant-a", "project-b"},
		HierarchyNames: []string{"ta", "pb"},
	}
	if diff := cmp.Diff(want, e.Fields); diff != "" {
		t.Errorf("Fields mismatch (-want +got):\n%s", diff)
	}
}

func TestLogAppendWithoutTime(t *testing.T) {
	t.Parallel()
	l, err := NewLog(2)
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent("e")
	ev.Time = nil
	if _, err := l.Append(ev); err == nil {
		t.Fatal("Append(no time) error = nil, want error")
	}
	if l.Len() != 0 {
		t.Errorf("Len() = %d after failed append, want 0", l.Len())
	}
}

func TestLogConcurrent(t *testing.T) {
	t.Parallel()
	l, err := NewLog(50)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := l.Append(testEvent("e")); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 100 {
				prev := uint64(0)
				for e := range l.Since(0) {
					if e.Seq <= prev {
						t.Errorf("Since not ascending: %d after %d", e.Seq, prev)
						return
					}
					prev = e.Seq
				}
				_ = l.Len()
			}
		})
	}
	wg.Wait()
	if got := l.Len(); got != 50 {
		t.Errorf("Len() = %d, want 50", got)
	}
	got := seqs(l, 0)
	if len(got) != 50 || got[len(got)-1] != 800 {
		t.Errorf("seqs = %v, want 50 ending at 800", got)
	}
}
