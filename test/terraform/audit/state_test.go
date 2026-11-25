//go:build e2e

package audit

import (
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

// at returns the value at a dotted path ("metadata.labels.team") of s.
func at(s *structpb.Struct, path string) (*structpb.Value, bool) {
	if s == nil {
		return nil, false
	}
	cur := structpb.NewStructValue(s)
	for key := range strings.SplitSeq(path, ".") {
		sv := cur.GetStructValue()
		if sv == nil {
			return nil, false
		}
		next, ok := sv.GetFields()[key]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// str returns the string at path, or "" when absent.
func str(s *structpb.Struct, path string) string {
	v, _ := at(s, path)
	return v.GetStringValue()
}

// version returns metadata.resource_version (a string or number) of s.
func version(t *testing.T, s *structpb.Struct) int64 {
	t.Helper()
	v, ok := at(s, "metadata.resource_version")
	if !ok {
		t.Fatalf("state has no metadata.resource_version: %v", s)
	}
	if n, ok := v.GetKind().(*structpb.Value_NumberValue); ok {
		return int64(n.NumberValue)
	}
	n, err := strconv.ParseInt(v.GetStringValue(), 10, 64)
	if err != nil {
		t.Fatalf("metadata.resource_version = %v: %v", v, err)
	}
	return n
}

// wantFields asserts every path in want holds the given string in s.
func wantFields(t *testing.T, what string, s *structpb.Struct, want map[string]string) {
	t.Helper()
	if s == nil {
		t.Errorf("%s: state is absent, want %v", what, want)
		return
	}
	for path, w := range want {
		if got := str(s, path); got != w {
			t.Errorf("%s: %s = %q, want %q (state: %v)", what, path, got, w, s)
		}
	}
}

func states(ev *auditv2pb.AuditEvent) (prev, cur *structpb.Struct) {
	st := ev.GetResource().GetState()
	return st.GetPrevious(), st.GetCurrent()
}

// checkCreateState: no previous, current holds id and want.
func checkCreateState(t *testing.T, what, id string, ev *auditv2pb.AuditEvent, want map[string]string) {
	t.Helper()
	prev, cur := states(ev)
	if prev != nil {
		t.Errorf("%s: previous = %v, want absent", what, prev)
	}
	wantFields(t, what+" current", cur, want)
	if got := str(cur, "metadata.id"); got != id {
		t.Errorf("%s: current metadata.id = %q, want %q", what, got, id)
	}
}

// checkUpdateState: previous holds the old values, current the new ones, and
// the resource version grew.
func checkUpdateState(t *testing.T, what string, ev *auditv2pb.AuditEvent, before, after map[string]string) {
	t.Helper()
	prev, cur := states(ev)
	wantFields(t, what+" previous", prev, before)
	wantFields(t, what+" current", cur, after)
	if prev != nil && cur != nil {
		if p, c := version(t, prev), version(t, cur); c <= p {
			t.Errorf("%s: resource_version current %d <= previous %d", what, c, p)
		}
	}
}

// checkStartedState: an async STARTED event has previous (when before is
// non-nil) and never current.
func checkStartedState(t *testing.T, what string, ev *auditv2pb.AuditEvent, before map[string]string) {
	t.Helper()
	prev, cur := states(ev)
	if cur != nil {
		t.Errorf("%s: STARTED current = %v, want absent", what, cur)
	}
	if before == nil {
		if prev != nil {
			t.Errorf("%s: STARTED previous = %v, want absent", what, prev)
		}
		return
	}
	wantFields(t, what+" previous", prev, before)
}

// checkDeleteState: previous holds the last values, current is absent.
func checkDeleteState(t *testing.T, what string, ev *auditv2pb.AuditEvent, last map[string]string) {
	t.Helper()
	prev, cur := states(ev)
	if cur != nil {
		t.Errorf("%s: current = %v, want absent", what, cur)
	}
	wantFields(t, what+" previous", prev, last)
}
