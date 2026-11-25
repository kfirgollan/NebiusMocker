package domain_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"
	"time"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

// token is the page token of a cursor at seq.
func token(seq string) string { return base64.RawURLEncoding.EncodeToString([]byte("v1:" + seq)) }

func at(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

// newEventService builds a service over six events:
//
//	e1..e3 tenant+project, e4 other tenant, e5 tenant only, e6 tenant+project.
func newEventService(t *testing.T) *domain.EventService {
	t.Helper()
	under := []string{tenantID, projectID}
	log := newLog(t, 100,
		eventSpec{id: "e1", at: at(0), service: "IAM", action: "CREATE", resID: "group-1", hierarchy: under},
		eventSpec{id: "e2", at: at(1), service: "IAM", action: "DELETE", resID: "group-2", hierarchy: under},
		eventSpec{id: "e3", at: at(2), service: "COMPUTE", action: "CREATE", resID: "instance-1", hierarchy: under},
		eventSpec{id: "e4", at: at(3), service: "IAM", action: "CREATE", resID: "group-9", hierarchy: []string{otherTenantID}},
		eventSpec{id: "e5", at: at(4), service: "IAM", action: "UPDATE", resID: "project-x", hierarchy: []string{tenantID}},
		eventSpec{id: "e6", at: at(5), service: "IAM", action: "CREATE", resID: "group-3", hierarchy: under},
	)
	svc, err := domain.NewEventService(log, newFakeHierarchy(), knownRegions)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestListScopeTimeAndType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		q    domain.ListEventsQuery
		want []string
	}{
		{
			"tenant sees every descendant event",
			domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(0), End: at(10)},
			[]string{"e1", "e2", "e3", "e5", "e6"},
		},
		{
			"project excludes tenant-level events",
			domain.ListEventsQuery{Region: region, ParentID: projectID, Start: at(0), End: at(10)},
			[]string{"e1", "e2", "e3", "e6"},
		},
		{
			"other tenant",
			domain.ListEventsQuery{Region: region, ParentID: otherTenantID, Start: at(0), End: at(10)},
			[]string{"e4"},
		},
		{
			"start inclusive, end exclusive",
			domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(1), End: at(4)},
			[]string{"e2", "e3"},
		},
		{
			"end after last event includes it",
			domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(5), End: at(5).Add(time.Nanosecond)},
			[]string{"e6"},
		},
		{
			"tenant-only event is outside the project",
			domain.ListEventsQuery{Region: region, ParentID: projectID, Start: at(4), End: at(5)},
			[]string{},
		},
		{
			"control plane",
			domain.ListEventsQuery{Region: region, ParentID: projectID, Start: at(0), End: at(10), EventType: auditv2pb.EventType_CONTROL_PLANE},
			[]string{"e1", "e2", "e3", "e6"},
		},
		{
			"another known region has no events",
			domain.ListEventsQuery{Region: otherRegion, ParentID: tenantID, Start: at(0), End: at(10)},
			[]string{},
		},
		{
			"filter",
			domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(0), End: at(10), Filter: "service.name='IAM' AND action='CREATE'"},
			[]string{"e1", "e6"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, next, err := newEventService(t).List(t.Context(), tt.q)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !slices.Equal(ids(got), tt.want) || next != "" {
				t.Fatalf("List = %v (next %q), want %v", ids(got), next, tt.want)
			}
		})
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	svc := newEventService(t)
	q := domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(0), End: at(10), PageSize: 2}
	// Tenant matches have Seq 1, 2, 3, 5, 6.
	wantPages := []struct {
		ids  []string
		next string
	}{
		{[]string{"e1", "e2"}, token("2")},
		{[]string{"e3", "e5"}, token("5")},
		{[]string{"e6"}, ""},
	}
	for i, want := range wantPages {
		got, next, err := svc.List(t.Context(), q)
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if !slices.Equal(ids(got), want.ids) || next != want.next {
			t.Fatalf("page %d = %v next %q, want %v next %q", i+1, ids(got), next, want.ids, want.next)
		}
		q.PageToken = next
	}
}

func TestListExactPageEndHasNoToken(t *testing.T) {
	t.Parallel()
	got, next, err := newEventService(t).List(t.Context(), domain.ListEventsQuery{
		Region:   region,
		ParentID: otherTenantID, Start: at(0), End: at(10), PageSize: 1,
	})
	if err != nil || len(got) != 1 || next != "" {
		t.Fatalf("List = %v, %q, %v; want one event and no token", ids(got), next, err)
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	base := domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(0), End: at(1)}
	with := func(f func(*domain.ListEventsQuery)) domain.ListEventsQuery {
		q := base
		f(&q)
		return q
	}
	tests := []struct {
		name  string
		q     domain.ListEventsQuery
		kind  errs.Kind
		field string
	}{
		{"page size too large", with(func(q *domain.ListEventsQuery) { q.PageSize = 501 }), errs.KindBadRequest, "page_size"},
		{"negative page size", with(func(q *domain.ListEventsQuery) { q.PageSize = -1 }), errs.KindBadRequest, "page_size"},
		{"end equals start", with(func(q *domain.ListEventsQuery) { q.End = q.Start }), errs.KindBadRequest, "end"},
		{"end before start", with(func(q *domain.ListEventsQuery) { q.End = at(-1) }), errs.KindBadRequest, "end"},
		{"bad filter", with(func(q *domain.ListEventsQuery) { q.Filter = "nope='x'" }), errs.KindBadRequest, ""},
		{"bad token", with(func(q *domain.ListEventsQuery) { q.PageToken = "!!!" }), errs.KindBadRequest, "page_token"},
		{"token without version", with(func(q *domain.ListEventsQuery) {
			q.PageToken = base64.RawURLEncoding.EncodeToString([]byte("7"))
		}), errs.KindBadRequest, "page_token"},
		{"unknown region", with(func(q *domain.ListEventsQuery) { q.Region = "mars-1" }), errs.KindBadRequest, "region"},
		{"missing region", with(func(q *domain.ListEventsQuery) { q.Region = "" }), errs.KindBadRequest, "region"},
		{"data plane", with(func(q *domain.ListEventsQuery) { q.EventType = auditv2pb.EventType_DATA_PLANE }), errs.KindBadRequest, "event_type"},
		{"unknown parent", with(func(q *domain.ListEventsQuery) { q.ParentID = "tenant-missing" }), errs.KindNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := newEventService(t).List(t.Context(), tt.q)
			if !errs.IsKind(err, tt.kind) {
				t.Fatalf("err = %v, want kind %v", err, tt.kind)
			}
			if tt.field != "" {
				if e, ok := errors.AsType[*errs.Error](err); !ok || len(e.Violations) == 0 || e.Violations[0].Field != tt.field {
					t.Fatalf("err = %#v, want violation on %q", err, tt.field)
				}
			}
		})
	}
}

func TestListDefaultPageSize(t *testing.T) {
	t.Parallel()
	specs := make([]eventSpec, 150)
	for i := range specs {
		specs[i] = eventSpec{id: "e", at: at(0), service: "IAM", action: "CREATE", resID: "group-1", hierarchy: []string{tenantID}}
	}
	svc, err := domain.NewEventService(newLog(t, 200, specs...), newFakeHierarchy(), knownRegions)
	if err != nil {
		t.Fatal(err)
	}
	got, next, err := svc.List(t.Context(), domain.ListEventsQuery{Region: region, ParentID: tenantID, Start: at(0), End: at(1)})
	if err != nil || len(got) != 100 || next != token("100") {
		t.Fatalf("List = %d events, next %q, err %v; want 100 events and token(100)", len(got), next, err)
	}
}
