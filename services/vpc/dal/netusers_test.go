package dal_test

import (
	"slices"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/vpc/dal"
)

func TestNetworkUserRepo(t *testing.T) {
	t.Parallel()
	r := dal.NewNetworkUserRepo(store.NewMemory())
	ctx := t.Context()

	for _, u := range []string{"dnszone-a", "dnszone-b", "dnszone-a"} {
		if err := r.Add(ctx, "net1", u); err != nil {
			t.Fatalf("Add %s: %v", u, err)
		}
	}
	if err := r.Add(ctx, "net2", "dnszone-c"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := r.List(ctx, "net1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(got)
	if want := []string{"dnszone-a", "dnszone-b"}; !slices.Equal(got, want) {
		t.Errorf("net1 users = %v, want %v", got, want)
	}

	if err := r.Remove(ctx, "net1", "dnszone-a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := r.Remove(ctx, "net1", "dnszone-a"); err != nil {
		t.Errorf("Remove missing: %v", err)
	}
	got, err = r.List(ctx, "net1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"dnszone-b"}; !slices.Equal(got, want) {
		t.Errorf("net1 users after remove = %v, want %v", got, want)
	}
	if got, _ := r.List(ctx, "none"); len(got) != 0 {
		t.Errorf("unknown network users = %v, want none", got)
	}
}
