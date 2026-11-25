package domain_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

const (
	tenantID      = "tenant-e00test0000000000001"
	otherTenantID = "tenant-e00test0000000000002"
	projectID     = "project-e00test0000000000001"
	bucketID      = "storagebucket-e00test0000000001"
)

const (
	region      = "eu-north1" // the default region
	otherRegion = "eu-west1"
)

var knownRegions = []string{region, otherRegion}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeHierarchy knows a fixed set of resources and their parents.
type fakeHierarchy map[string]string

func newFakeHierarchy() fakeHierarchy {
	return fakeHierarchy{tenantID: "", otherTenantID: "", projectID: tenantID}
}

func (h fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	p, ok := h[id]
	if !ok {
		return "", errs.NotFound(id)
	}
	return p, nil
}

// fakeBuckets accepts bucketID only and maps it to a backend name.
type fakeBuckets struct{}

func (fakeBuckets) Validate(_ context.Context, id, _ string) error {
	if id != bucketID {
		return errs.BadRequest(errs.Violation{Field: "bucket", Message: "unknown bucket " + id})
	}
	return nil
}

func (fakeBuckets) BackendName(_ context.Context, id string) (string, error) {
	return "backend-" + id, nil
}

// fakeExports is a mutex-guarded, insertion-ordered domain.ExportRepository.
type fakeExports struct {
	mu    sync.Mutex
	order []string
	byID  map[string]domain.Export
}

func newFakeExports() *fakeExports {
	return &fakeExports{byID: map[string]domain.Export{}}
}

func (f *fakeExports) Get(_ context.Context, id string) (domain.Export, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.byID[id]
	if !ok {
		return domain.Export{}, errs.NotFound(id)
	}
	return domain.Export{Region: e.Region, Resource: proto.CloneOf(e.Resource)}, nil
}

func (f *fakeExports) Create(_ context.Context, e domain.Export) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := e.Resource.GetMetadata().GetId()
	if _, dup := f.byID[id]; dup {
		return errs.AlreadyExists(id, "dup")
	}
	f.order = append(f.order, id)
	f.byID[id] = domain.Export{Region: e.Region, Resource: proto.CloneOf(e.Resource)}
	return nil
}

func (f *fakeExports) Modify(_ context.Context, id string, fn func(*auditv2pb.AuditEventExport) error) (*auditv2pb.AuditEventExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.byID[id]
	if !ok {
		return nil, errs.NotFound(id)
	}
	c := proto.CloneOf(e.Resource)
	if err := fn(c); err != nil {
		return nil, err
	}
	f.byID[id] = domain.Export{Region: e.Region, Resource: c}
	return proto.CloneOf(c), nil
}

// List pages by offset; the token is the decimal offset.
func (f *fakeExports) List(_ context.Context, parentID, region string, size int, token string) (domain.ExportPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*auditv2pb.AuditEventExport
	for _, id := range f.order {
		if e := f.byID[id]; e.Region == region && e.Resource.GetMetadata().GetParentId() == parentID {
			all = append(all, proto.CloneOf(e.Resource))
		}
	}
	off := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil {
			return domain.ExportPage{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "bad"})
		}
		off = n
	}
	off = min(off, len(all))
	end := min(off+size, len(all))
	page := domain.ExportPage{Items: all[off:end]}
	if end < len(all) {
		page.NextPageToken = strconv.Itoa(end)
	}
	return page, nil
}

// inlineOps runs Prepare and Work immediately and numbers operations.
type inlineOps struct {
	mu sync.Mutex
	n  int
}

func (o *inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	o.mu.Lock()
	o.n++
	id := fmt.Sprintf("operation-%d", o.n)
	o.mu.Unlock()
	op := &operations.Operation{ID: id, ResourceID: s.ResourceID}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) New(typ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("%s-e00test%04d", typ, s.n), nil
}

// failingWriter fails every Put.
type failingWriter struct{ err error }

func (w failingWriter) BucketEmpty(context.Context, string) (bool, error) { return true, nil }

func (w failingWriter) Put(context.Context, string, string, io.Reader, int64, objstore.PutOptions) (objstore.ObjectInfo, error) {
	return objstore.ObjectInfo{}, w.err
}

var errBoom = errors.New("disk on fire")

// eventSpec describes a synthetic audit event.
type eventSpec struct {
	id      string
	at      time.Time
	service string
	action  string
	resID   string
	resName string
	// hierarchy lists ancestor IDs, tenant first.
	hierarchy []string
}

func (s eventSpec) event() *auditv2pb.AuditEvent {
	ev := &auditv2pb.AuditEvent{
		Id:      s.id,
		Time:    timestamppb.New(s.at),
		Service: &auditv2pb.Service{Name: s.service},
		Action:  s.action,
		Type:    "ai.nebius." + s.service + ".thing." + s.action,
		Resource: &auditv2pb.Resource{
			Metadata: &auditv2pb.ResourceMetadata{Id: s.resID, Name: s.resName, Type: "thing"},
		},
	}
	for _, h := range s.hierarchy {
		ev.Resource.Hierarchy = append(ev.Resource.Hierarchy, &auditv2pb.ResourceMetadata{Id: h, Name: "n-" + h})
	}
	return ev
}

// newLog returns a log holding events in order (Seq 1..n).
func newLog(tb testing.TB, capacity int, specs ...eventSpec) *audit.Log {
	tb.Helper()
	l, err := audit.NewLog(capacity)
	if err != nil {
		tb.Fatal(err)
	}
	for i := range specs {
		if _, err := l.Append(specs[i].event()); err != nil {
			tb.Fatal(err)
		}
	}
	return l
}

func ids(events []*auditv2pb.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.GetId())
	}
	return out
}
