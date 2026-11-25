package dal_test

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/vpc/dal"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

func newRepo() *dal.Repo[*vpcpb.Network] {
	return dal.NewRepo(store.NewMemory(), domain.KindNetwork, func() *vpcpb.Network { return &vpcpb.Network{} })
}

func network(id, parent, name string) *vpcpb.Network {
	return &vpcpb.Network{Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name}}
}

func ids(items []*vpcpb.Network) []string {
	out := make([]string, 0, len(items))
	for _, n := range items {
		out = append(out, n.GetMetadata().GetId())
	}
	return out
}

func TestRepoCRUD(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()

	n := network("n1", "p1", "a")
	n.Status = &vpcpb.NetworkStatus{Default: true}
	if err := r.Create(ctx, n); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "n1")
	if err != nil || !proto.Equal(got, n) {
		t.Fatalf("Get = %v, %v, want %v", got, err, n)
	}
	byName, err := r.GetByName(ctx, "p1", "a")
	if err != nil || byName.GetMetadata().GetId() != "n1" {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	n2 := network("n1", "p1", "renamed")
	if err := r.Update(ctx, n2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := r.GetByName(ctx, "p1", "a"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName old name err = %v, want NotFound", err)
	}
	if got, err := r.GetByName(ctx, "p1", "renamed"); err != nil || got.GetMetadata().GetId() != "n1" {
		t.Errorf("GetByName new name = %v, %v", got, err)
	}

	if err := r.Delete(ctx, "n1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "n1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete err = %v, want NotFound", err)
	}
}

func TestRepoErrors(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()
	if err := r.Create(ctx, network("n1", "p1", "a")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
		want errs.Kind
	}{
		{"duplicate name same parent", r.Create(ctx, network("n2", "p1", "a")), errs.KindAlreadyExists},
		{"duplicate id", r.Create(ctx, network("n1", "p1", "other")), errs.KindAlreadyExists},
		{"update missing", r.Update(ctx, network("nX", "p1", "z")), errs.KindNotFound},
		{"delete missing", r.Delete(ctx, "nX"), errs.KindNotFound},
		{"same name other parent", r.Create(ctx, network("n3", "p2", "a")), errs.KindUnknown},
	}
	for _, tt := range tests {
		if got := errs.KindOf(tt.err); got != tt.want {
			t.Errorf("%s: kind = %v, want %v (err: %v)", tt.name, got, tt.want, tt.err)
		}
	}
	if _, err := r.Get(ctx, "missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get missing err = %v, want NotFound", err)
	}
	if _, err := r.GetByName(ctx, "p1", "missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName missing err = %v, want NotFound", err)
	}
}

func TestRepoListPagesByParent(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()
	// Interleave two parents; creation order must be kept per parent.
	for i := range 5 {
		if err := r.Create(ctx, network(fmt.Sprintf("a%d", i), "pa", fmt.Sprintf("n%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := r.Create(ctx, network(fmt.Sprintf("b%d", i), "pb", fmt.Sprintf("n%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	token := ""
	pages := 0
	for {
		p, err := r.List(ctx, "pa", 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		got = append(got, ids(p.Items)...)
		if p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	if want := "a0,a1,a2,a3,a4"; strings.Join(got, ",") != want {
		t.Errorf("List pages = %v, want %s", got, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}

	if p, err := r.List(ctx, "pa", 5, ""); err != nil || len(p.Items) != 5 || p.NextPageToken != "" {
		t.Errorf("exact fit page = %d items, token %q, err %v; want 5, empty", len(p.Items), p.NextPageToken, err)
	}
	if p, err := r.List(ctx, "pa", 0, ""); err != nil || len(p.Items) != 5 {
		t.Errorf("page size 0 = %d items, %v; want all 5", len(p.Items), err)
	}
	if p, err := r.List(ctx, "none", 2, ""); err != nil || len(p.Items) != 0 || p.NextPageToken != "" {
		t.Errorf("unknown parent = %+v, %v; want empty", p, err)
	}
}

func TestRepoListWhereAcrossInterleavedRecords(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()
	// Every third record matches, spread over two parents.
	for i := range 12 {
		parent := "pa"
		if i%2 == 1 {
			parent = "pb"
		}
		n := network(fmt.Sprintf("n%02d", i), parent, fmt.Sprintf("n%02d", i))
		n.Status = &vpcpb.NetworkStatus{Default: i%3 == 0}
		if err := r.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	match := func(n *vpcpb.Network) bool { return n.GetStatus().GetDefault() }

	var got []string
	token := ""
	for range 10 {
		p, err := r.ListWhere(ctx, match, 3, token)
		if err != nil {
			t.Fatalf("ListWhere: %v", err)
		}
		got = append(got, ids(p.Items)...)
		if p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	if want := "n00,n03,n06,n09"; strings.Join(got, ",") != want {
		t.Errorf("ListWhere pages = %v, want %s", got, want)
	}

	p, err := r.ListWhere(ctx, match, 2, "")
	if err != nil || strings.Join(ids(p.Items), ",") != "n00,n03" || p.NextPageToken == "" {
		t.Fatalf("page 1 = %v token %q, %v", ids(p.Items), p.NextPageToken, err)
	}
	p, err = r.ListWhere(ctx, match, 2, p.NextPageToken)
	if err != nil || strings.Join(ids(p.Items), ",") != "n06,n09" || p.NextPageToken != "" {
		t.Fatalf("page 2 = %v token %q, %v", ids(p.Items), p.NextPageToken, err)
	}

	none, err := r.ListWhere(ctx, func(*vpcpb.Network) bool { return false }, 2, "")
	if err != nil || len(none.Items) != 0 || none.NextPageToken != "" {
		t.Errorf("no match = %+v, %v; want empty", none, err)
	}
}

func TestRepoAll(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()
	for i, parent := range []string{"pa", "pb", "pa", "pb", "pa"} {
		if err := r.Create(ctx, network(fmt.Sprintf("n%d", i), parent, fmt.Sprintf("n%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	all, err := r.All(ctx, func(*vpcpb.Network) bool { return true })
	if err != nil || strings.Join(ids(all), ",") != "n0,n1,n2,n3,n4" {
		t.Errorf("All(true) = %v, %v", ids(all), err)
	}
	pa, err := r.All(ctx, func(n *vpcpb.Network) bool { return n.GetMetadata().GetParentId() == "pa" })
	if err != nil || strings.Join(ids(pa), ",") != "n0,n2,n4" {
		t.Errorf("All(pa) = %v, %v", ids(pa), err)
	}
	none, err := r.All(ctx, func(*vpcpb.Network) bool { return false })
	if err != nil || len(none) != 0 {
		t.Errorf("All(false) = %v, %v", ids(none), err)
	}
}

func TestRepoMalformedPageToken(t *testing.T) {
	t.Parallel()
	r := newRepo()
	ctx := t.Context()
	for _, token := range []string{"%%%", "bm90LWEtbnVtYmVy", "!"} {
		if _, err := r.List(ctx, "pa", 2, token); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("List token %q err = %v, want BadRequest", token, err)
		}
		if _, err := r.ListWhere(ctx, func(*vpcpb.Network) bool { return true }, 2, token); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("ListWhere token %q err = %v, want BadRequest", token, err)
		}
	}
}

func TestNewReposCoversEveryKind(t *testing.T) {
	t.Parallel()
	r := dal.NewRepos(store.NewMemory())
	if r.Pools == nil || r.Networks == nil || r.Subnets == nil || r.Allocations == nil ||
		r.RouteTables == nil || r.Routes == nil || r.SecurityGroups == nil || r.SecurityRules == nil {
		t.Fatalf("NewRepos left a repository nil: %+v", r)
	}
	// Kinds are isolated: a pool is not visible as a network.
	ctx := t.Context()
	p := &vpcpb.Pool{Metadata: &commonpb.ResourceMetadata{Id: "x1", ParentId: "p", Name: "a"}}
	if err := r.Pools.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Networks.Get(ctx, "x1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Networks.Get(pool id) err = %v, want NotFound", err)
	}
}
