// Package api is the billing gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
)

// Service names of the billing gRPC services served by this module.
const (
	pricingPolicyService   = "nebius.billing.v1.PricingPolicyService"
	calculatorService      = "nebius.billing.v1.CalculatorService"
	calculatorAlphaService = "nebius.billing.v1alpha1.CalculatorService"
	spotPriceService       = "nebius.billing.v1alpha1.SpotPriceService"
	oneTimeExportService   = "nebius.billing.v1alpha1.OneTimeExportService"
)

// RegisterAuthzRules declares the permission each billing RPC needs.
//
//   - Pricing policies are project resources (billing.pricingPolicies.*):
//     viewers read them, editors manage them (docs: manage-pricing-policy).
//   - Spot prices need no authentication (spot_price_service.proto); the
//     authn interceptor exempts SpotPriceService, and the rules allow anyone.
//   - The calculator prices public list prices: any authenticated caller may
//     estimate (billing.calculator.estimate is not checked on a resource).
//   - OneTimeExportService is not implemented; any authenticated caller gets
//     UNIMPLEMENTED rather than a denial on a contract the mock lacks.
func RegisterAuthzRules(r *authz.Rules) {
	r.CRUD(pricingPolicyService, "billing", "pricingPolicies")

	anyone := func(perm authz.Permission) authz.Rule {
		return authz.Rule{
			Permission: perm,
			AllowIf:    func(context.Context, string, any) bool { return true },
			Target:     func(context.Context, any) (string, error) { return "", nil },
		}
	}
	add := func(service, method string, perm authz.Permission) { r.Add("/"+service+"/"+method, anyone(perm)) }

	estimate := authz.Perm("billing", "calculator", "estimate")
	add(calculatorService, "EstimateBatch", estimate)
	add(calculatorAlphaService, "Estimate", estimate)
	add(calculatorAlphaService, "EstimateBatch", estimate)

	spot := authz.Perm("billing", "spotPrices", authz.VerbList)
	add(spotPriceService, "List", spot)
	add(spotPriceService, "ListSeries", spot)

	add(oneTimeExportService, "Create", authz.Perm("billing", "oneTimeExports", authz.VerbCreate))
	add(oneTimeExportService, "Get", authz.Perm("billing", "oneTimeExports", authz.VerbGet))
	add(oneTimeExportService, "List", authz.Perm("billing", "oneTimeExports", authz.VerbList))
}
