package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Listing page sizes (pricing_policy_service.proto).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// maxPriceDigits is the number of decimal places of a max_price
// ("a string with three decimal places").
const maxPriceDigits = 3

// policyCurrency is the currency of every price (status.currency).
const policyCurrency = "USD"

const (
	fieldPlatform = "spec.compute_instance_spec.v1.platform"
	fieldMaxPrice = "spec.pricing.max_price_v1.max_price"
)

// policyNameRE: "can only contain Latin letters and numbers, with no special
// characters" (billing docs, pricing policies).
var policyNameRE = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// PricingPolicyDeps are the dependencies of a PricingPolicyService. All are required.
type PricingPolicyDeps struct {
	Repo      PricingPolicyRepository
	Prices    *PriceList
	Regions   Regions
	Platforms Platforms
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
}

// PricingPolicyService implements nebius.billing.v1.PricingPolicyService. mu
// serializes every mutation, so the checks (name, running VMs) are atomic
// with the writes they guard.
type PricingPolicyService struct {
	mu sync.Mutex
	d  PricingPolicyDeps
}

// NewPricingPolicyService wires the service.
func NewPricingPolicyService(d PricingPolicyDeps) (*PricingPolicyService, error) {
	if d.Repo == nil || d.Prices == nil || d.Regions == nil || d.Platforms == nil || d.Hierarchy == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("billing: pricing policy dependencies must not be nil")
	}
	return &PricingPolicyService{d: d}, nil
}

// CreatePricingPolicyCmd is the input of Create.
type CreatePricingPolicyCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *billingpb.PricingPolicySpec
	Request  proto.Message
}

// UpdatePricingPolicyCmd is the input of Update (a patch guarded by a reset mask).
type UpdatePricingPolicyCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *billingpb.PricingPolicySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a pricing policy.
func (s *PricingPolicyService) Get(ctx context.Context, id string) (*billingpb.PricingPolicy, error) {
	p, err := s.d.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withScheduling(ctx, p)
}

// GetByName returns the pricing policy with the name in a project.
func (s *PricingPolicyService) GetByName(ctx context.Context, parentID, name string) (*billingpb.PricingPolicy, error) {
	p, err := s.d.Repo.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.withScheduling(ctx, p)
}

// List returns one page of a project's pricing policies in creation order.
func (s *PricingPolicyService) List(ctx context.Context, parentID string, size int64, pageToken string) (Page[*billingpb.PricingPolicy], error) {
	if parentID == "" {
		return Page[*billingpb.PricingPolicy]{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if size < 0 || size > maxPageSize {
		return Page[*billingpb.PricingPolicy]{}, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	n := int(size)
	if n == 0 {
		n = defaultPageSize
	}
	page, err := s.d.Repo.List(ctx, parentID, n, pageToken)
	if err != nil {
		return Page[*billingpb.PricingPolicy]{}, err
	}
	for i, p := range page.Items {
		if page.Items[i], err = s.withScheduling(ctx, p); err != nil {
			return Page[*billingpb.PricingPolicy]{}, err
		}
	}
	return page, nil
}

// withScheduling fills status.scheduling_state: BLOCKED while max_price is
// below the current spot price of the platform in the project's region.
func (s *PricingPolicyService) withScheduling(ctx context.Context, p *billingpb.PricingPolicy) (*billingpb.PricingPolicy, error) {
	region, err := s.d.Regions.Region(ctx, p.GetMetadata().GetParentId())
	if err != nil {
		return nil, err
	}
	if p.GetStatus() == nil {
		p.Status = &billingpb.PricingPolicyStatus{}
	}
	p.Status.SchedulingState = billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED
	platform := p.GetSpec().GetComputeInstanceSpec().GetV1().GetPlatform()
	_, price, ok := s.d.Prices.SpotPrice(platform, region)
	if !ok {
		return p, nil
	}
	if limit, perr := ParseDecimal(maxPriceOf(p)); perr == nil && limit.Cmp(price) < 0 {
		p.Status.SchedulingState = billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED
	}
	return p, nil
}

// Create creates a pricing policy: STATE_CREATING until the operation
// completes, then STATE_ACTIVE.
func (s *PricingPolicyService) Create(ctx context.Context, cmd CreatePricingPolicyCmd) (*operations.Operation, error) {
	parent := cmd.Metadata.GetParentId()
	if err := s.checkParent(ctx, parent); err != nil {
		return nil, err
	}
	if err := checkPolicyName(cmd.Metadata.GetName()); err != nil {
		return nil, err
	}
	m, err := s.validateSpec(ctx, parent, cmd.Spec)
	if err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindPricingPolicy)
	if err != nil {
		return nil, fmt.Errorf("new pricing policy id: %w", err)
	}
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create pricing policy",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.create(ctx, id, cmd, m)
		},
		Work: func(ctx context.Context) error { return s.settle(ctx, id) },
	})
}

func (s *PricingPolicyService) create(ctx context.Context, id string, cmd CreatePricingPolicyCmd, m *SpotMarket) error {
	in := cmd.Metadata
	if in.GetId() != "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if in.GetResourceVersion() != 0 {
		return errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	now := timestamppb.New(s.d.Clock.Now())
	return s.d.Repo.Create(ctx, &billingpb.PricingPolicy{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: in.GetParentId(), Name: in.GetName(), Labels: in.GetLabels(),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: proto.CloneOf(cmd.Spec),
		// running_vm_count stays 0: NebiusMocker's compute rejects
		// spec.spot_pricing_policy, so no VM can run under a policy.
		Status: &billingpb.PricingPolicyStatus{
			State: billingpb.PricingPolicyStatus_STATE_CREATING, SkuId: m.SKU, Currency: policyCurrency,
		},
	})
}

// settle moves a policy from CREATING or UPDATING to ACTIVE (the work of an
// operation). A policy that is gone or in another state is left alone.
func (s *PricingPolicyService) settle(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.d.Repo.Get(ctx, id)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return nil
		}
		return err
	}
	if st := p.GetStatus().GetState(); st != billingpb.PricingPolicyStatus_STATE_CREATING && st != billingpb.PricingPolicyStatus_STATE_UPDATING {
		return nil
	}
	p.Status.State = billingpb.PricingPolicyStatus_STATE_ACTIVE
	return s.d.Repo.Update(ctx, p)
}

// Update changes a pricing policy's name, labels and max_price. The compute
// instance spec is immutable (an unchanged copy is accepted); max_price
// changes need no running VM. Update patches the stored policy by the
// request and X-ResetMask, so the CLI's partial updates work too.
func (s *PricingPolicyService) Update(ctx context.Context, cmd UpdatePricingPolicyCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	var priceChanged bool
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update pricing policy",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			var err error
			priceChanged, err = s.update(ctx, cmd)
			return err
		},
		Work: func(ctx context.Context) error {
			if !priceChanged {
				return nil
			}
			return s.settle(ctx, id)
		},
	})
}

// update applies cmd and reports whether max_price changed (STATE_UPDATING).
func (s *PricingPolicyService) update(ctx context.Context, cmd UpdatePricingPolicyCmd) (bool, error) {
	stored, err := s.d.Repo.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return false, err
	}
	smd := stored.GetMetadata()
	if p := cmd.Metadata.GetParentId(); p != "" && p != smd.GetParentId() {
		return false, immutable("metadata.parent_id")
	}
	if want := cmd.Metadata.GetResourceVersion(); want != 0 && want != smd.GetResourceVersion() {
		return false, errs.BadState(smd.GetId(), fmt.Sprintf("resource_version %d does not match the current version %d", want, smd.GetResourceVersion()))
	}
	updated, err := patchPolicy(stored, &billingpb.PricingPolicy{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}, cmd.ResetMask)
	if err != nil {
		return false, err
	}
	if !proto.Equal(updated.GetSpec().GetComputeInstanceSpec(), stored.GetSpec().GetComputeInstanceSpec()) {
		return false, immutable("spec.compute_instance_spec")
	}
	if err := checkPolicyName(updated.GetMetadata().GetName()); err != nil {
		return false, err
	}
	if _, err := s.validateSpec(ctx, smd.GetParentId(), updated.GetSpec()); err != nil {
		return false, err
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return false, nil // nothing changed: keep resource_version and updated_at
	}
	priceChanged := maxPriceOf(updated) != maxPriceOf(stored)
	if priceChanged {
		if n := stored.GetStatus().GetRunningVmCount(); n > 0 {
			return false, errs.BadState(smd.GetId(), fmt.Sprintf("max_price cannot change while %d VMs run under the policy", n))
		}
		updated.Status.State = billingpb.PricingPolicyStatus_STATE_UPDATING
	}
	updated.Metadata.ResourceVersion++
	updated.Metadata.UpdatedAt = timestamppb.New(s.d.Clock.Now())
	if err := s.d.Repo.Update(ctx, updated); err != nil {
		return false, err
	}
	return priceChanged, nil
}

func maxPriceOf(p *billingpb.PricingPolicy) string {
	return p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice()
}

// patchPolicy applies the mutable fields in patchMsg to a copy of stored
// using the X-ResetMask, then restores the server-owned metadata.
func patchPolicy(stored, patchMsg *billingpb.PricingPolicy, resetMask *mask.Mask) (*billingpb.PricingPolicy, error) {
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	if err := fieldmaskpb.PatchWithResetMask(updated, patchMsg, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	md, umd := stored.GetMetadata(), updated.GetMetadata()
	if umd == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata", Message: "must not be reset"})
	}
	umd.Id, umd.ParentId = md.GetId(), md.GetParentId()
	umd.ResourceVersion = md.GetResourceVersion()
	umd.CreatedAt, umd.UpdatedAt = md.GetCreatedAt(), md.GetUpdatedAt()
	return updated, nil
}

// Delete deletes a pricing policy that no VM runs under: STATE_DELETING,
// then gone when the operation completes.
func (s *PricingPolicyService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete pricing policy",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			p, err := s.d.Repo.Get(ctx, id)
			if err != nil {
				return err
			}
			if n := p.GetStatus().GetRunningVmCount(); n > 0 {
				return errs.BadState(id, fmt.Sprintf("%d VMs still run under the policy", n))
			}
			p.Status.State = billingpb.PricingPolicyStatus_STATE_DELETING
			return s.d.Repo.Update(ctx, p)
		},
		Work: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := s.d.Repo.Delete(ctx, id); err != nil && !errs.IsKind(err, errs.KindNotFound) {
				return err
			}
			return nil
		},
	})
}

// checkParent requires an existing project as a policy's parent.
func (s *PricingPolicyService) checkParent(ctx context.Context, parentID string) error {
	const field = "metadata.parent_id"
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	if parentID == s.d.Config.ProjectID {
		return nil
	}
	if _, err := s.d.Hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("pricing policy parent %s: %w", parentID, err)
	}
	return nil
}

func checkPolicyName(name string) error {
	const field = "metadata.name"
	switch {
	case name == "":
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	case !policyNameRE.MatchString(name):
		return errs.BadRequest(errs.Violation{Field: field, Message: "can only contain Latin letters and numbers, with no special characters"})
	}
	return nil
}

// validateSpec checks a spec for the project's region and returns the spot
// market of its platform.
func (s *PricingPolicyService) validateSpec(ctx context.Context, parentID string, spec *billingpb.PricingPolicySpec) (*SpotMarket, error) {
	required := func(field string) error {
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	switch {
	case spec == nil:
		return nil, required("spec")
	case spec.GetComputeInstanceSpec() == nil:
		return nil, required("spec.compute_instance_spec")
	case spec.GetComputeInstanceSpec().GetV1() == nil:
		return nil, required("spec.compute_instance_spec.v1")
	case spec.GetComputeInstanceSpec().GetV1().GetPlatform() == "":
		return nil, required(fieldPlatform)
	case spec.GetPricing() == nil:
		return nil, required("spec.pricing")
	case spec.GetPricing().GetMaxPriceV1() == nil:
		return nil, required("spec.pricing.max_price_v1")
	case spec.GetPricing().GetMaxPriceV1().GetMaxPrice() == "":
		return nil, required(fieldMaxPrice)
	}
	platform := spec.GetComputeInstanceSpec().GetV1().GetPlatform()
	region, err := s.d.Regions.Region(ctx, parentID)
	if err != nil {
		return nil, err
	}
	cp, _, ok := s.d.Prices.SpotPrice(platform, region)
	if !ok {
		return nil, errs.BadRequest(errs.Violation{Field: fieldPlatform, Message: fmt.Sprintf(
			"platform %q has no spot market in region %s; platforms with one: %s", platform, region, s.spotPlatforms(region))})
	}
	if err := s.checkPlatform(ctx, parentID, platform); err != nil {
		return nil, err
	}
	return cp.Spot, checkMaxPrice(spec.GetPricing().GetMaxPriceV1().GetMaxPrice(), cp.Spot)
}

func (s *PricingPolicyService) spotPlatforms(region string) string {
	var names []string
	for _, c := range s.d.Prices.SpotMarkets() {
		if _, _, ok := s.d.Prices.SpotPrice(c.Platform, region); ok {
			names = append(names, c.Platform)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// checkPlatform asks the compute catalog (when compute is enabled) whether
// the platform is offered in the region and allowed for preemptible VMs.
func (s *PricingPolicyService) checkPlatform(ctx context.Context, parentID, platform string) error {
	pl, ok, err := s.d.Platforms.Platform(ctx, parentID, platform)
	switch {
	case !ok && err == nil:
		return nil
	case errs.IsKind(err, errs.KindNotFound):
		return errs.BadRequest(errs.Violation{Field: fieldPlatform, Message: fmt.Sprintf("platform %q is not offered in the project's region", platform)})
	case err != nil:
		return err
	case !pl.AllowedForPreemptibles:
		return errs.BadRequest(errs.Violation{Field: fieldPlatform, Message: fmt.Sprintf("platform %q does not support preemptible VMs", platform)})
	}
	return nil
}

func checkMaxPrice(s string, m *SpotMarket) error {
	v, err := ParseDecimal(s)
	if err != nil {
		return errs.BadRequest(errs.Violation{Field: fieldMaxPrice, Message: err.Error()})
	}
	if FractionDigits(s) > maxPriceDigits {
		return errs.BadRequest(errs.Violation{Field: fieldMaxPrice, Message: fmt.Sprintf("at most %d decimal places, like \"3.515\"", maxPriceDigits)})
	}
	if v.Cmp(m.MinPrice) < 0 || v.Cmp(m.MaxPrice) > 0 {
		return errs.OutOfRange(errs.Violation{Field: fieldMaxPrice, Message: fmt.Sprintf(
			"%s is outside the allowed range %s to %s USD per GPU-hour", s, m.MinPrice, m.MaxPrice)})
	}
	return nil
}

func immutable(field string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: "field is immutable"})
}
