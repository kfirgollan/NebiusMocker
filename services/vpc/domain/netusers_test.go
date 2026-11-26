package domain

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// fakeNetworkUsers is an in-memory NetworkUserRepository.
type fakeNetworkUsers struct {
	mu    sync.Mutex
	users map[string][]string
}

func newFakeNetworkUsers() *fakeNetworkUsers {
	return &fakeNetworkUsers{users: map[string][]string{}}
}

func (f *fakeNetworkUsers) Add(_ context.Context, networkID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.users[networkID], userID) {
		f.users[networkID] = append(f.users[networkID], userID)
	}
	return nil
}

func (f *fakeNetworkUsers) Remove(_ context.Context, networkID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[networkID] = slices.DeleteFunc(f.users[networkID], func(u string) bool { return u == userID })
	return nil
}

func (f *fakeNetworkUsers) List(_ context.Context, networkID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.users[networkID]), nil
}

func TestBindUnknownNetwork(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	err := e.svc.NetworkBindings().Bind(t.Context(), "vpcnetwork-e00missing", "dnszone-e00a")
	wantKind(t, "bind unknown network", err, errs.KindNotFound)
	wantKind(t, "network unknown", func() error {
		_, err := e.svc.NetworkBindings().Network(t.Context(), "vpcnetwork-e00missing")
		return err
	}(), errs.KindNotFound)
}

func TestBindTwiceIsNoOp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	n := e.createNetwork(t, "bound")
	b := e.svc.NetworkBindings()
	for range 2 {
		if err := b.Bind(ctx, idOf(n), "dnszone-e00a"); err != nil {
			t.Fatalf("Bind: %v", err)
		}
	}
	got, err := e.svc.c.r.NetworkUsers.List(ctx, idOf(n))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(got, []string{"dnszone-e00a"}) {
		t.Errorf("bindings = %v, want one dnszone-e00a", got)
	}
}

func TestNetworkDeleteRefusedWhileBound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	n := e.createNetwork(t, "bound")
	b := e.svc.NetworkBindings()
	if err := b.Bind(ctx, idOf(n), "dnszone-e00a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	err := tryDelete(ctx, e.svc.Networks().Delete, idOf(n))
	wantKind(t, "delete bound network", err, errs.KindBadState)
	if err == nil || !strings.Contains(err.Error(), "dnszone-e00a") {
		t.Errorf("error %v does not name the bound user", err)
	}

	if err := b.Unbind(ctx, idOf(n), "dnszone-e00a"); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	if err := tryDelete(ctx, e.svc.Networks().Delete, idOf(n)); err != nil {
		t.Errorf("delete after unbind: %v", err)
	}
}

func TestUnbindMissingIsOK(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.svc.NetworkBindings()
	if err := b.Unbind(t.Context(), "vpcnetwork-e00missing", "dnszone-e00a"); err != nil {
		t.Errorf("Unbind missing network: %v", err)
	}
	n := e.createNetwork(t, "plain")
	if err := b.Unbind(t.Context(), idOf(n), "dnszone-e00a"); err != nil {
		t.Errorf("Unbind missing binding: %v", err)
	}
}
