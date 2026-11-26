package domain_test

import (
	"context"
	"testing"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// TestPricingPolicyAdmitSerializesWithMutations checks that a max_price
// change waits for Admit and sees the VM started inside it.
func TestPricingPolicyAdmitSerializesWithMutations(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	id := e.mustCreate(t, "main", "2.500").GetMetadata().GetId()
	inside, release := make(chan struct{}), make(chan struct{})
	admitted := make(chan error, 1)
	go func() {
		admitted <- e.svc.Admit(t.Context(), id, func(_ context.Context, p *billingpb.PricingPolicy) error {
			if st := p.GetStatus().GetState(); st != billingpb.PricingPolicyStatus_STATE_ACTIVE {
				t.Errorf("Admit: state = %v, want ACTIVE", st)
			}
			close(inside)
			<-release
			e.vms.set(id, 1) // the VM starts while the lock is held
			return nil
		})
	}()
	<-inside
	updated := make(chan error, 1)
	go func() {
		updated <- e.update(t.Context(), &commonpb.ResourceMetadata{Id: id},
			&billingpb.PricingPolicySpec{Pricing: ppSpec("", "3.000").GetPricing()}, nil)
	}()
	close(release)
	if err := <-admitted; err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantKind(t, <-updated, errs.KindBadState)

	err := e.svc.Admit(t.Context(), "pricingpolicy-e00nope", func(context.Context, *billingpb.PricingPolicy) error {
		t.Error("fn ran for a missing policy")
		return nil
	})
	wantKind(t, err, errs.KindNotFound)
}
