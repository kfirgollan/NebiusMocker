package domain

import (
	"context"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// fakePricing is the billing pricing policies capability.
type fakePricing struct {
	mu       sync.Mutex
	admit    sync.Mutex
	disabled bool
	policies map[string]peer.PricingPolicy
}

func newFakePricing() *fakePricing { return &fakePricing{policies: map[string]peer.PricingPolicy{}} }

func (f *fakePricing) put(p peer.PricingPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies[p.ID] = p
}

func (f *fakePricing) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, id)
}

func (f *fakePricing) PricingPolicies() (peer.PricingPolicies, bool) {
	if f.disabled {
		return nil, false
	}
	return f, true
}

func (f *fakePricing) PricingPolicy(_ context.Context, id string) (peer.PricingPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[id]
	if !ok {
		return peer.PricingPolicy{}, errs.NotFound(id)
	}
	return p, nil
}

// Admit runs fn under one admission lock, as billing does.
func (f *fakePricing) Admit(ctx context.Context, id string, fn func(ctx context.Context, p peer.PricingPolicy) error) error {
	f.admit.Lock()
	defer f.admit.Unlock()
	p, err := f.PricingPolicy(ctx, id)
	if err != nil {
		return err
	}
	return fn(ctx, p)
}
