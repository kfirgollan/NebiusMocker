package domain

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// The values of the SOA and NS records the service creates with a zone. The
// docs describe the records but not their data; these are the mock's choice.
const (
	// Nameserver is the apex NS record's data (the reserved "ns.internal.").
	Nameserver = "ns.internal."
	// soaMailbox is the SOA RNAME (responsible mailbox).
	soaMailbox = "hostmaster.nebius.internal."
	// DefaultNegativeTTL is the negative-caching TTL used when
	// spec.soa_spec.negative_ttl is unset or below 5 (docs: 900 seconds).
	DefaultNegativeTTL = 900
	// minNegativeTTL: smaller values are ignored (zone.proto).
	minNegativeTTL = 5
	// systemTTL is the TTL of the SOA and NS records. Negative answers are
	// cached for min(SOA TTL, negative TTL) (RFC 2308), so it is above
	// the default negative TTL.
	systemTTL = 3600
)

// CreateZoneCmd is the input of CreateZone.
type CreateZoneCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *dnspb.ZoneSpec
	Request  proto.Message
}

// UpdateZoneCmd is the input of UpdateZone (full replace guarded by a reset mask).
type UpdateZoneCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *dnspb.ZoneSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// EffectiveNegativeTTL is the negative-caching TTL a zone is served with.
func EffectiveNegativeTTL(z *dnspb.Zone) uint32 {
	if n := z.GetSpec().GetSoaSpec().GetNegativeTtl(); n >= minNegativeTTL {
		return uint32(min(n, int64(^uint32(0)>>1)))
	}
	return DefaultNegativeTTL
}

// soaData is the RDATA of a zone's SOA record: MNAME RNAME SERIAL REFRESH
// RETRY EXPIRE MINIMUM, where MINIMUM is the negative-caching TTL.
func soaData(z *dnspb.Zone) string {
	return fmt.Sprintf("%s %s 1 3600 600 604800 %d", Nameserver, soaMailbox, EffectiveNegativeTTL(z))
}

// GetZone returns a zone with its record count.
func (s *Service) GetZone(ctx context.Context, id string, resourceVersion int64) (*dnspb.Zone, error) {
	z, err := s.zones.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(z, resourceVersion); err != nil {
		return nil, err
	}
	return s.withCount(ctx, z)
}

// GetZoneByName returns the zone with the name in a project.
func (s *Service) GetZoneByName(ctx context.Context, parentID, name string) (*dnspb.Zone, error) {
	z, err := s.zones.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.withCount(ctx, z)
}

// ListZones returns one page of a project's zones in creation order.
func (s *Service) ListZones(ctx context.Context, parentID string, size int64, pageToken string) (Page[*dnspb.Zone], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*dnspb.Zone]{}, err
	}
	page, err := s.zones.List(ctx, parentID, n, pageToken)
	if err != nil {
		return Page[*dnspb.Zone]{}, err
	}
	return s.countAll(ctx, page)
}

// ListZonesByNetwork returns one page of the zones bound to a network, of
// any project, in creation order.
func (s *Service) ListZonesByNetwork(ctx context.Context, networkID string, size int64, pageToken string) (Page[*dnspb.Zone], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*dnspb.Zone]{}, err
	}
	all, err := s.zones.All(ctx)
	if err != nil {
		return Page[*dnspb.Zone]{}, err
	}
	var bound []*dnspb.Zone
	for _, z := range all {
		if z.GetSpec().GetVpc().GetPrimaryNetworkId() == networkID {
			bound = append(bound, z)
		}
	}
	page, err := paginate(bound, n, pageToken)
	if err != nil {
		return Page[*dnspb.Zone]{}, err
	}
	return s.countAll(ctx, page)
}

func (s *Service) countAll(ctx context.Context, page Page[*dnspb.Zone]) (Page[*dnspb.Zone], error) {
	for i, z := range page.Items {
		c, err := s.withCount(ctx, z)
		if err != nil {
			return Page[*dnspb.Zone]{}, err
		}
		page.Items[i] = c
	}
	return page, nil
}

// withCount fills status.record_count: every record of the zone, including
// the SOA and NS records the service manages.
func (s *Service) withCount(ctx context.Context, z *dnspb.Zone) (*dnspb.Zone, error) {
	recs, err := s.records.All(ctx, z.GetMetadata().GetId())
	if err != nil {
		return nil, err
	}
	if z.GetStatus() == nil {
		z.Status = &dnspb.ZoneStatus{}
	}
	z.Status.RecordCount = int64(len(recs))
	return z, nil
}

// CreateZone creates a zone bound to a VPC network, with its SOA and NS
// records at the apex.
func (s *Service) CreateZone(ctx context.Context, cmd CreateZoneCmd) (*operations.Operation, error) {
	if err := s.checkParent(ctx, cmd.Metadata.GetParentId()); err != nil {
		return nil, err
	}
	if err := checkZoneDomain(cmd.Spec.GetDomainName()); err != nil {
		return nil, err
	}
	if cmd.Spec.GetVpc().GetPrimaryNetworkId() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.vpc.primary_network_id", Message: "required: custom DNS zones are private and attached to one network"})
	}
	id, err := s.ids.New(KindZone)
	if err != nil {
		return nil, fmt.Errorf("new zone id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create DNS zone",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.createZone(ctx, id, cmd)
		},
	})
}

func (s *Service) createZone(ctx context.Context, id string, cmd CreateZoneCmd) error {
	md, err := s.newMetadata(id, cmd.Metadata)
	if err != nil {
		return err
	}
	networkID := cmd.Spec.GetVpc().GetPrimaryNetworkId()
	if err := s.checkNetwork(ctx, networkID, md.GetParentId()); err != nil {
		return err
	}
	domain := cmd.Spec.GetDomainName()
	if err := s.checkDomainFree(ctx, networkID, domain); err != nil {
		return err
	}
	z := &dnspb.Zone{
		Metadata: md,
		Spec: &dnspb.ZoneSpec{
			DomainName: domain,
			Scope:      &dnspb.ZoneSpec_Vpc{Vpc: &dnspb.VpcZoneScope{PrimaryNetworkId: networkID}},
			SoaSpec:    proto.CloneOf(cmd.Spec.GetSoaSpec()),
		},
		Status: &dnspb.ZoneStatus{},
	}
	if err := s.networks.Bind(ctx, networkID, id); err != nil {
		return fmt.Errorf("spec.vpc.primary_network_id: %w", err)
	}
	claim := peer.QuotaClaim{ContainerID: md.GetParentId(), Amounts: map[string]int64{quota.DNSZoneCount: 1}}
	if err := s.gate.Admit(ctx, claim, func(ctx context.Context) error { return s.zones.Create(ctx, z) }); err != nil {
		return s.unbindAfter(ctx, networkID, id, err)
	}
	for _, sys := range []struct {
		t    dnspb.RecordSpec_RecordType
		data string
	}{{dnspb.RecordSpec_SOA, soaData(z)}, {dnspb.RecordSpec_NS, Nameserver}} {
		if err := s.createSystemRecord(ctx, z, sys.t, sys.data); err != nil {
			_ = s.removeZone(ctx, z) // best-effort rollback; the record error is what matters
			return err
		}
	}
	return nil
}

// unbindAfter releases a binding made for a zone that could not be stored.
func (s *Service) unbindAfter(ctx context.Context, networkID, zoneID string, cause error) error {
	if err := s.networks.Unbind(ctx, networkID, zoneID); err != nil {
		return fmt.Errorf("%w (and releasing network %s: %w)", cause, networkID, err)
	}
	return cause
}

// checkNetwork requires an existing network in the zone's project.
func (s *Service) checkNetwork(ctx context.Context, networkID, projectID string) error {
	const field = "spec.vpc.primary_network_id"
	if !strings.HasPrefix(networkID, KindNetwork+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a VPC network ID"})
	}
	n, err := s.networks.Network(ctx, networkID)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if n.ProjectID != projectID {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("network %s belongs to %s, not to the zone's project %s", networkID, n.ProjectID, projectID)})
	}
	return nil
}

// checkDomainFree enforces "a network can't contain two zones with the same
// domain name" (docs.nebius.com/vpc/dns/overview). Nested names are allowed.
func (s *Service) checkDomainFree(ctx context.Context, networkID, domain string) error {
	all, err := s.zones.All(ctx)
	if err != nil {
		return err
	}
	for _, z := range all {
		if z.GetSpec().GetVpc().GetPrimaryNetworkId() == networkID && z.GetSpec().GetDomainName() == domain {
			return errs.AlreadyExists(z.GetMetadata().GetId(), fmt.Sprintf("network %s already has a zone for %s", networkID, domain))
		}
	}
	return nil
}

func (s *Service) createSystemRecord(ctx context.Context, z *dnspb.Zone, t dnspb.RecordSpec_RecordType, data string) error {
	id, err := s.ids.New(KindRecord)
	if err != nil {
		return fmt.Errorf("new record id: %w", err)
	}
	md, err := s.newMetadata(id, &commonpb.ResourceMetadata{ParentId: z.GetMetadata().GetId()})
	if err != nil {
		return err
	}
	domain := z.GetSpec().GetDomainName()
	return s.records.Create(ctx, &dnspb.Record{
		Metadata: md,
		Spec:     &dnspb.RecordSpec{RelativeName: Apex, Type: t, Data: data, Ttl: systemTTL},
		Status:   &dnspb.RecordStatus{ZoneDomainName: domain, EffectiveFqdn: domain},
	})
}

// UpdateZone changes a zone's name, labels and SOA settings. domain_name and
// vpc.primary_network_id are immutable: an unchanged copy (Terraform resends
// them) is accepted, a different value is rejected.
func (s *Service) UpdateZone(ctx context.Context, cmd UpdateZoneCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update DNS zone",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.updateZone(ctx, cmd)
		},
	})
}

func (s *Service) updateZone(ctx context.Context, cmd UpdateZoneCmd) error {
	stored, err := s.zones.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	if d := cmd.Spec.GetDomainName(); d != "" && d != stored.GetSpec().GetDomainName() {
		return immutable("spec.domain_name")
	}
	if n := cmd.Spec.GetVpc().GetPrimaryNetworkId(); n != "" && n != stored.GetSpec().GetVpc().GetPrimaryNetworkId() {
		return immutable("spec.vpc.primary_network_id")
	}
	updated, err := patch(stored, &dnspb.Zone{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &dnspb.ZoneSpec{SoaSpec: cmd.Spec.GetSoaSpec()},
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &dnspb.ZoneSpec{}
	}
	updated.Spec.DomainName = stored.GetSpec().GetDomainName()
	updated.Spec.Scope = stored.GetSpec().GetScope()
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	s.bump(updated.GetMetadata())
	if err := s.zones.Update(ctx, updated); err != nil {
		return err
	}
	if EffectiveNegativeTTL(updated) != EffectiveNegativeTTL(stored) {
		return s.refreshSOA(ctx, updated)
	}
	return nil
}

// refreshSOA rewrites the SOA record after the negative-caching TTL changed.
func (s *Service) refreshSOA(ctx context.Context, z *dnspb.Zone) error {
	recs, err := s.records.All(ctx, z.GetMetadata().GetId())
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.GetSpec().GetType() != dnspb.RecordSpec_SOA {
			continue
		}
		r.Spec.Data = soaData(z)
		s.bump(r.GetMetadata())
		return s.records.Update(ctx, r)
	}
	return errs.Internal("zone " + z.GetMetadata().GetId() + " has no SOA record")
}

// DeleteZone deletes a zone whose only records are the SOA and NS records
// the service manages (docs.nebius.com/vpc/dns/manage-zones: delete all other
// records first), and releases its network.
func (s *Service) DeleteZone(ctx context.Context, id string, resourceVersion int64, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete DNS zone",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			z, err := s.zones.Get(ctx, id)
			if err != nil {
				return err
			}
			if err := checkVersion(z, resourceVersion); err != nil {
				return err
			}
			recs, err := s.records.All(ctx, id)
			if err != nil {
				return err
			}
			var users []string
			for _, r := range recs {
				if !IsSystemType(r.GetSpec().GetType()) {
					users = append(users, r.GetMetadata().GetId())
				}
			}
			if len(users) > 0 {
				return errs.BadState(id, fmt.Sprintf("the zone still has %d records (%s); delete every record except the default SOA and NS records first",
					len(users), strings.Join(users, ", ")))
			}
			return s.removeZone(ctx, z)
		},
	})
}

// removeZone deletes a zone, its records and its network binding.
func (s *Service) removeZone(ctx context.Context, z *dnspb.Zone) error {
	id := z.GetMetadata().GetId()
	recs, err := s.records.All(ctx, id)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := s.records.Delete(ctx, r.GetMetadata().GetId()); err != nil {
			return err
		}
	}
	if err := s.zones.Delete(ctx, id); err != nil {
		return err
	}
	return s.networks.Unbind(ctx, z.GetSpec().GetVpc().GetPrimaryNetworkId(), id)
}
