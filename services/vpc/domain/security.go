package domain

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Security groups and rules are stored and validated, never enforced: the
// mocker has no packet path.

// SecurityGroupService implements nebius.vpc.v1.SecurityGroupService.
type SecurityGroupService struct {
	*kind[*vpcpb.SecurityGroup, *vpcpb.SecurityGroupSpec]
}

func newSecurityGroupService(c *core) *SecurityGroupService {
	s := &SecurityGroupService{}
	s.kind = &kind[*vpcpb.SecurityGroup, *vpcpb.SecurityGroupSpec]{
		c: c, prefix: KindSecurityGroup, what: "security group", repo: c.r.SecurityGroups,
		newMsg:  func() *vpcpb.SecurityGroup { return &vpcpb.SecurityGroup{} },
		newSpec: func() *vpcpb.SecurityGroupSpec { return &vpcpb.SecurityGroupSpec{} },
		build:   s.build, apply: s.apply, checkDelete: s.checkDelete, remove: s.cascade,
	}
	return s
}

// ListByNetwork returns one page of the security groups of a network.
func (s *SecurityGroupService) ListByNetwork(ctx context.Context, networkID string, pageSize int64, pageToken string) (Page[*vpcpb.SecurityGroup], error) {
	return s.listWhere(ctx, func(sg *vpcpb.SecurityGroup) bool { return sg.GetSpec().GetNetworkId() == networkID }, pageSize, pageToken)
}

func (s *SecurityGroupService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.SecurityGroupSpec) (*vpcpb.SecurityGroup, error) {
	if err := s.c.checkProject(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	n, err := ref(ctx, s.c.r.Networks, "spec.network_id", spec.GetNetworkId())
	if err != nil {
		return nil, err
	}
	if err := sameParent("spec.network_id", md.GetParentId(), n); err != nil {
		return nil, err
	}
	return &vpcpb.SecurityGroup{Metadata: md, Spec: spec, Status: &vpcpb.SecurityGroupStatus{State: vpcpb.SecurityGroupStatus_READY}}, nil
}

func (s *SecurityGroupService) apply(_ context.Context, stored, updated *vpcpb.SecurityGroup) error {
	if stored.GetSpec().GetNetworkId() != updated.GetSpec().GetNetworkId() {
		return immutable("spec.network_id")
	}
	return nil
}

func (s *SecurityGroupService) checkDelete(ctx context.Context, sg *vpcpb.SecurityGroup) error {
	id := sg.GetMetadata().GetId()
	if sg.GetStatus().GetDefault() {
		return errs.BadState(id, "the default security group is deleted with its network")
	}
	refs, err := s.c.r.SecurityRules.All(ctx, func(r *vpcpb.SecurityRule) bool {
		return r.GetMetadata().GetParentId() != id && referencedGroup(r.GetSpec()) == id
	})
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return inUse(id, "security group", idsOf(refs))
	}
	return nil
}

// cascade deletes the security group and its rules.
func (s *SecurityGroupService) cascade(ctx context.Context, sg *vpcpb.SecurityGroup) error {
	id := sg.GetMetadata().GetId()
	rules, err := s.c.r.SecurityRules.All(ctx, func(r *vpcpb.SecurityRule) bool { return r.GetMetadata().GetParentId() == id })
	if err != nil {
		return err
	}
	for _, r := range rules {
		if err := s.c.r.SecurityRules.Delete(ctx, r.GetMetadata().GetId()); err != nil {
			return fmt.Errorf("delete security rule %s: %w", r.GetMetadata().GetId(), err)
		}
	}
	return s.repo.Delete(ctx, id)
}

func referencedGroup(spec *vpcpb.SecurityRuleSpec) string {
	if in := spec.GetIngress(); in != nil {
		return in.GetSourceSecurityGroupId()
	}
	return spec.GetEgress().GetDestinationSecurityGroupId()
}

// SecurityRuleService implements nebius.vpc.v1.SecurityRuleService.
type SecurityRuleService struct {
	*kind[*vpcpb.SecurityRule, *vpcpb.SecurityRuleSpec]
}

func newSecurityRuleService(c *core) *SecurityRuleService {
	s := &SecurityRuleService{}
	s.kind = &kind[*vpcpb.SecurityRule, *vpcpb.SecurityRuleSpec]{
		c: c, prefix: KindSecurityRule, what: "security rule", repo: c.r.SecurityRules,
		newMsg:  func() *vpcpb.SecurityRule { return &vpcpb.SecurityRule{} },
		newSpec: func() *vpcpb.SecurityRuleSpec { return &vpcpb.SecurityRuleSpec{} },
		build:   s.build, apply: s.apply,
	}
	return s
}

// DefaultRulePriority is used when a rule's priority is 0 or unset.
const DefaultRulePriority = 500

func (s *SecurityRuleService) build(ctx context.Context, md *commonpb.ResourceMetadata, spec *vpcpb.SecurityRuleSpec) (*vpcpb.SecurityRule, error) {
	sg, err := ref(ctx, s.c.r.SecurityGroups, "metadata.parent_id", md.GetParentId())
	if err != nil {
		return nil, err
	}
	normalizeRule(spec)
	if err := s.validate(ctx, sg, spec); err != nil {
		return nil, err
	}
	return &vpcpb.SecurityRule{Metadata: md, Spec: spec, Status: ruleStatus(sg.GetMetadata().GetId(), spec)}, nil
}

// normalizeRule fills the NON_EMPTY_DEFAULT fields.
func normalizeRule(spec *vpcpb.SecurityRuleSpec) {
	if spec.GetPriority() == 0 {
		spec.Priority = DefaultRulePriority
	}
	if spec.GetType() == vpcpb.RuleType_RULE_TYPE_UNSPECIFIED {
		spec.Type = vpcpb.RuleType_STATEFUL
	}
}

func (s *SecurityRuleService) validate(ctx context.Context, sg *vpcpb.SecurityGroup, spec *vpcpb.SecurityRuleSpec) error {
	switch {
	case spec.GetAccess() == vpcpb.RuleAccessAction_ACCESS_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: "spec.access", Message: "required"})
	case spec.GetProtocol() == vpcpb.RuleProtocol_PROTOCOL_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: "spec.protocol", Message: "required"})
	case spec.GetIngress() == nil && spec.GetEgress() == nil:
		return errs.BadRequest(errs.Violation{Field: "spec.match", Message: "one of ingress or egress is required"})
	}
	cidrs, ports := spec.GetIngress().GetSourceCidrs(), spec.GetIngress().GetDestinationPorts()
	field := "spec.ingress"
	if spec.GetEgress() != nil {
		cidrs, ports, field = spec.GetEgress().GetDestinationCidrs(), spec.GetEgress().GetDestinationPorts(), "spec.egress"
	}
	for i, c := range cidrs {
		if _, err := parseCIDR(c); err != nil {
			return errs.BadRequest(errs.Violation{Field: fmt.Sprintf("%s.cidrs[%d]", field, i), Message: err.Error()})
		}
	}
	if len(ports) > 0 && spec.GetProtocol() == vpcpb.RuleProtocol_ICMP {
		return errs.BadRequest(errs.Violation{Field: field + ".destination_ports", Message: "ports are not allowed with ICMP"})
	}
	if other := referencedGroup(spec); other != "" {
		g, err := ref(ctx, s.c.r.SecurityGroups, field+".security_group_id", other)
		if err != nil {
			return err
		}
		if g.GetSpec().GetNetworkId() != sg.GetSpec().GetNetworkId() {
			return errs.BadRequest(errs.Violation{Field: field + ".security_group_id", Message: "must be in the same network"})
		}
	}
	return nil
}

// ruleStatus computes the effective evaluation order and the match summary.
// At equal priority DENY is evaluated before ALLOW.
func ruleStatus(groupID string, spec *vpcpb.SecurityRuleSpec) *vpcpb.SecurityRuleStatus {
	eff := spec.GetPriority() * 2
	if spec.GetAccess() == vpcpb.RuleAccessAction_ALLOW {
		eff++
	}
	st := &vpcpb.SecurityRuleStatus{State: vpcpb.SecurityRuleStatus_READY, EffectivePriority: eff}
	if in := spec.GetIngress(); in != nil {
		st.Direction = vpcpb.RuleDirection_INGRESS
		st.Source = &vpcpb.RuleMatchStatus{SecurityGroupId: in.GetSourceSecurityGroupId(), Cidrs: in.GetSourceCidrs()}
		st.Destination = &vpcpb.RuleMatchStatus{SecurityGroupId: groupID, Ports: in.GetDestinationPorts()}
		return st
	}
	eg := spec.GetEgress()
	st.Direction = vpcpb.RuleDirection_EGRESS
	st.Source = &vpcpb.RuleMatchStatus{SecurityGroupId: groupID}
	st.Destination = &vpcpb.RuleMatchStatus{
		SecurityGroupId: eg.GetDestinationSecurityGroupId(), Cidrs: eg.GetDestinationCidrs(), Ports: eg.GetDestinationPorts(),
	}
	return st
}

func (s *SecurityRuleService) apply(_ context.Context, stored, updated *vpcpb.SecurityRule) error {
	normalizeRule(updated.GetSpec())
	if !proto.Equal(stored.GetSpec(), updated.GetSpec()) {
		return immutable("spec")
	}
	return nil
}
