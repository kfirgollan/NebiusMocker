package domain

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Record constraints (docs.nebius.com/vpc/dns/resources/quotas-limits).
const (
	MaxRecordsPerZone = 1000
	// MaxRecordsPerSet is the limit of records with the same relative name
	// and type in a zone.
	MaxRecordsPerSet = 20
)

// CreateRecordCmd is the input of CreateRecord.
type CreateRecordCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *dnspb.RecordSpec
	Request  proto.Message
}

// UpdateRecordCmd is the input of UpdateRecord (full replace guarded by a reset mask).
type UpdateRecordCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *dnspb.RecordSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// DeleteRecordCmd is the input of DeleteRecord.
type DeleteRecordCmd struct {
	ID              string
	ResourceVersion int64
	// Force deletes a deletion-protected record.
	Force   bool
	Request proto.Message
}

// GetRecord returns a record.
func (s *Service) GetRecord(ctx context.Context, id string, resourceVersion int64) (*dnspb.Record, error) {
	r, err := s.records.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(r, resourceVersion); err != nil {
		return nil, err
	}
	return r, nil
}

// GetRecordByName returns the record with the resource name in a zone.
func (s *Service) GetRecordByName(ctx context.Context, zoneID, name string) (*dnspb.Record, error) {
	return s.records.GetByName(ctx, zoneID, name)
}

// ListRecords returns one page of a zone's records (including the SOA and
// NS records the service manages) in creation order.
func (s *Service) ListRecords(ctx context.Context, zoneID string, size int64, pageToken string) (Page[*dnspb.Record], error) {
	n, err := pageSize(size)
	if err != nil {
		return Page[*dnspb.Record]{}, err
	}
	if _, err := s.zones.Get(ctx, zoneID); err != nil {
		return Page[*dnspb.Record]{}, err
	}
	return s.records.List(ctx, zoneID, n, pageToken)
}

// CreateRecord creates a record in a zone.
func (s *Service) CreateRecord(ctx context.Context, cmd CreateRecordCmd) (*operations.Operation, error) {
	if cmd.Metadata.GetParentId() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if err := checkType(cmd.Spec.GetType()); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindRecord)
	if err != nil {
		return nil, fmt.Errorf("new record id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create DNS record",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.createRecord(ctx, id, cmd)
		},
	})
}

func (s *Service) createRecord(ctx context.Context, id string, cmd CreateRecordCmd) error {
	md, err := s.newMetadata(id, cmd.Metadata)
	if err != nil {
		return err
	}
	z, err := s.zones.Get(ctx, md.GetParentId())
	if err != nil {
		return fmt.Errorf("metadata.parent_id: %w", err)
	}
	spec := proto.CloneOf(cmd.Spec)
	domain := z.GetSpec().GetDomainName()
	fqdn := EffectiveFQDN(spec.GetRelativeName(), domain)
	if err := s.checkRecord(ctx, z.GetMetadata().GetId(), "", fqdn, spec); err != nil {
		return err
	}
	return s.records.Create(ctx, &dnspb.Record{
		Metadata: md,
		Spec:     spec,
		Status:   &dnspb.RecordStatus{ZoneDomainName: domain, EffectiveFqdn: fqdn},
	})
}

// checkRecord validates a record's name and data, and enforces the
// per-zone rules against the zone's other records (self is the record being
// updated, "" on create): no exact duplicate (same relative name, type and
// data: AlreadyExists), at most MaxRecordsPerSet records per name and type,
// and at most MaxRecordsPerZone records.
func (s *Service) checkRecord(ctx context.Context, zoneID, self, fqdn string, spec *dnspb.RecordSpec) error {
	if err := checkFQDNLength(fqdn); err != nil {
		return err
	}
	canonical, err := checkData(fqdn, spec.GetType(), spec.GetData())
	if err != nil {
		return err
	}
	recs, err := s.records.All(ctx, zoneID)
	if err != nil {
		return err
	}
	inSet := 0
	for _, r := range recs {
		if r.GetMetadata().GetId() == self {
			continue
		}
		rs := r.GetSpec()
		if rs.GetRelativeName() != spec.GetRelativeName() || rs.GetType() != spec.GetType() {
			continue
		}
		inSet++
		other, err := checkData(r.GetStatus().GetEffectiveFqdn(), rs.GetType(), rs.GetData())
		if err == nil && other == canonical {
			return errs.AlreadyExists(r.GetMetadata().GetId(), fmt.Sprintf("a %s record for %s with this data already exists", spec.GetType(), fqdn))
		}
	}
	if inSet >= MaxRecordsPerSet {
		return errs.BadState(zoneID, fmt.Sprintf("the zone already has %d %s records for %s (the limit)", MaxRecordsPerSet, spec.GetType(), fqdn))
	}
	if self == "" && len(recs) >= MaxRecordsPerZone {
		return errs.BadState(zoneID, fmt.Sprintf("the zone already has %d records (the limit)", MaxRecordsPerZone))
	}
	return nil
}

// UpdateRecord changes a record's data, TTL, deletion protection, name and
// labels. relative_name and type are immutable: an unchanged copy is
// accepted, a different value is rejected. The SOA and NS records are
// managed by the service and cannot be updated.
func (s *Service) UpdateRecord(ctx context.Context, cmd UpdateRecordCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update DNS record",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.updateRecord(ctx, cmd)
		},
	})
}

func (s *Service) updateRecord(ctx context.Context, cmd UpdateRecordCmd) error {
	stored, err := s.records.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	id, ss := stored.GetMetadata().GetId(), stored.GetSpec()
	if IsSystemType(ss.GetType()) {
		return errs.BadState(id, fmt.Sprintf("the %s record is managed by the service and cannot be changed", ss.GetType()))
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	if n := cmd.Spec.GetRelativeName(); n != "" && n != ss.GetRelativeName() {
		return immutable("spec.relative_name")
	}
	if t := cmd.Spec.GetType(); t != dnspb.RecordSpec_RECORD_TYPE_UNSPECIFIED && t != ss.GetType() {
		return immutable("spec.type")
	}
	updated, err := patch(stored, &dnspb.Record{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec: &dnspb.RecordSpec{
			Data: cmd.Spec.GetData(), Ttl: cmd.Spec.GetTtl(), DeletionProtection: cmd.Spec.GetDeletionProtection(),
		},
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	if updated.GetSpec() == nil {
		updated.Spec = &dnspb.RecordSpec{}
	}
	updated.Spec.RelativeName, updated.Spec.Type = ss.GetRelativeName(), ss.GetType()
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	if updated.GetSpec().GetData() != ss.GetData() {
		if err := s.checkRecord(ctx, stored.GetMetadata().GetParentId(), id, stored.GetStatus().GetEffectiveFqdn(), updated.GetSpec()); err != nil {
			return err
		}
	}
	s.bump(updated.GetMetadata())
	return s.records.Update(ctx, updated)
}

// DeleteRecord deletes a record. A deletion-protected record needs Force;
// the SOA and NS records go only with their zone.
func (s *Service) DeleteRecord(ctx context.Context, cmd DeleteRecordCmd) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  cmd.ID,
		Description: "Delete DNS record",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			r, err := s.records.Get(ctx, cmd.ID)
			if err != nil {
				return err
			}
			if err := checkVersion(r, cmd.ResourceVersion); err != nil {
				return err
			}
			if t := r.GetSpec().GetType(); IsSystemType(t) {
				return errs.BadState(cmd.ID, fmt.Sprintf("the %s record is managed by the service; it is deleted with its zone", t))
			}
			if r.GetSpec().GetDeletionProtection() && !cmd.Force {
				return errs.BadState(cmd.ID, "the record is deletion-protected; set force to delete it, or turn off spec.deletion_protection first")
			}
			return s.records.Delete(ctx, cmd.ID)
		},
	})
}
