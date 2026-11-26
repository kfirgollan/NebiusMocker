package contract

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	billingv1 "github.com/nebius/gosdk/services/nebius/billing/v1"
	billingalpha "github.com/nebius/gosdk/services/nebius/billing/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

// billingCalcEnv is a mocker with IAM, compute (its platform catalog) and
// billing, and the billing clients of the SDK (with and without a token).
type billingCalcEnv struct {
	inst  *testkit.Instance
	v1    billingv1.CalculatorService
	alpha billingalpha.CalculatorService
	spot  billingalpha.SpotPriceService
	// anonSpot and anonCalc call without any token.
	anonSpot  billingalpha.SpotPriceService
	anonCalc  billingv1.CalculatorService
	anonAlpha billingalpha.CalculatorService
}

func startBillingCalc(t *testing.T, opts ...testkit.Option) *billingCalcEnv {
	t.Helper()
	opts = append([]testkit.Option{
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(10 * time.Millisecond),
	}, opts...)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), billing.New()}, opts...)
	sdk, anon := inst.SDK(t), inst.SDKAs(t, "")
	return &billingCalcEnv{
		inst: inst,
		v1:   billingv1.NewCalculatorService(sdk), alpha: billingalpha.NewCalculatorService(sdk), spot: billingalpha.NewSpotPriceService(sdk),
		anonSpot: billingalpha.NewSpotPriceService(anon), anonCalc: billingv1.NewCalculatorService(anon), anonAlpha: billingalpha.NewCalculatorService(anon),
	}
}

// billingCalcViolations returns the BadRequest violation fields carried by err.
func billingCalcViolations(err error) []string {
	var fields []string
	for _, d := range status.Convert(err).Details() {
		if se, ok := d.(*commonpb.ServiceError); ok {
			for _, v := range se.GetBadRequest().GetViolations() {
				fields = append(fields, v.GetField())
			}
		}
	}
	return fields
}

// billingCalcWantInvalid fails unless err is InvalidArgument naming field.
func billingCalcWantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument on %s (err: %v)", got, field, err)
	}
	for _, f := range billingCalcViolations(err) {
		if f == field {
			return
		}
	}
	t.Fatalf("violations %v do not name %s (err: %v)", billingCalcViolations(err), field, err)
}

// billingNow is the wall clock of the test process (the mocker runs on the
// real clock too).
func billingNow() time.Time { return clock.Real{}.Now() }
