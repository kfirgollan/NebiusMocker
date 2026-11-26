package domain

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

const (
	tenantID    = "tenant-e00test0000000000001"
	project1    = "project-e00test0000000000001"
	project2    = "project-e00test0000000000002"
	projectUS   = "project-u00test0000000000003"
	regionEU    = "eu-north1"
	regionUS    = "us-central1"
	otherTenant = "tenant-e00test0000000000009"
)

// fakeRepo is an in-memory Repository keeping creation order.
type fakeRepo struct {
	mu    sync.Mutex
	items []Allowance
}

func (f *fakeRepo) find(id string) int {
	return slices.IndexFunc(f.items, func(a Allowance) bool { return a.Msg.GetMetadata().GetId() == id })
}

func clone(a Allowance) Allowance { return Allowance{Msg: proto.CloneOf(a.Msg), Region: a.Region} }

func (f *fakeRepo) Get(_ context.Context, id string) (Allowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.find(id); i >= 0 {
		return clone(f.items[i]), nil
	}
	return Allowance{}, errs.NotFound(id)
}

func (f *fakeRepo) ByKey(_ context.Context, parentID, name, region string) (Allowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.items {
		md := a.Msg.GetMetadata()
		if md.GetParentId() == parentID && md.GetName() == name && a.Region == region {
			return clone(a), nil
		}
	}
	return Allowance{}, errs.NotFound(name)
}

func (f *fakeRepo) List(_ context.Context, parentID string, pageSize int, pageToken string) (Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil {
			return Page{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed"})
		}
		start = n
	}
	var p Page
	seen := 0
	for _, a := range f.items {
		if a.Msg.GetMetadata().GetParentId() != parentID {
			continue
		}
		seen++
		if seen <= start {
			continue
		}
		if len(p.Items) == pageSize {
			p.NextPageToken = strconv.Itoa(start + pageSize)
			break
		}
		p.Items = append(p.Items, clone(a))
	}
	return p, nil
}

func (f *fakeRepo) Ensure(_ context.Context, as []Allowance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range as {
		if f.find(a.Msg.GetMetadata().GetId()) < 0 {
			f.items = append(f.items, clone(a))
		}
	}
	return nil
}

func (f *fakeRepo) Put(_ context.Context, a Allowance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(a.Msg.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(a.Msg.GetMetadata().GetId())
	}
	f.items[i] = clone(a)
	return nil
}

// fakeHierarchy maps a container ID to its parent ("" for a tenant).
type fakeHierarchy map[string]string

func (h fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	p, ok := h[id]
	if !ok {
		return "", errs.NotFound(id)
	}
	return p, nil
}

// fakeRegions maps a project ID to its region name.
type fakeRegions map[string]string

func (r fakeRegions) Region(_ context.Context, projectID string) (string, error) {
	n, ok := r[projectID]
	if !ok {
		return "", errs.NotFound(projectID)
	}
	return n, nil
}

// inlineOps runs Prepare then Work immediately, like the engine with delay 0.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error) {
	if spec.Prepare != nil {
		if err := spec.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{ID: "operation-test", ResourceID: spec.ResourceID, FinishedAt: time.Unix(1, 0)}
	if spec.Work != nil {
		op.Err = spec.Work(ctx)
	}
	return op, nil
}

// fixture is a Service over fakes: one tenant with two eu-north1 projects and
// one us-central1 project.
type fixture struct {
	svc     *Service
	repo    *fakeRepo
	sources map[string]peer.QuotaUsageSource
}

func newFixture(t *testing.T, limits map[string]uint64) *fixture {
	t.Helper()
	cat, err := quota.DefaultCatalog(limits)
	if err != nil {
		t.Fatalf("DefaultCatalog: %v", err)
	}
	f := &fixture{repo: &fakeRepo{}, sources: map[string]peer.QuotaUsageSource{}}
	f.svc, err = NewService(Deps{
		Repo: f.repo,
		Hierarchy: fakeHierarchy{
			tenantID: "", otherTenant: "", project1: tenantID, project2: tenantID, projectUS: tenantID,
		},
		Regions:    fakeRegions{project1: regionEU, project2: regionEU, projectUS: regionUS},
		Sources:    func() map[string]peer.QuotaUsageSource { return f.sources },
		Catalog:    cat,
		Ops:        inlineOps{},
		Clock:      clock.NewManual(time.Unix(1_700_000_000, 0)),
		AllRegions: []Region{{Name: regionEU, RoutingCode: "e00"}, {Name: regionUS, RoutingCode: "u00"}},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return f
}

// usage makes area report the given usages.
func (f *fixture) usage(us ...peer.QuotaUsage) {
	f.sources["compute"] = peer.QuotaFunc(func(context.Context) ([]peer.QuotaUsage, error) { return us, nil })
}
