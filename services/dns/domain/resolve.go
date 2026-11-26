package domain

import (
	"context"
	"strings"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
)

// ZoneData is a zone with all its records, as the DNS responder serves it.
type ZoneData struct {
	Zone    *dnspb.Zone
	Records []*dnspb.Record
}

// FindZone returns the zone that is authoritative for a fully-qualified,
// lowercase name: the zone with the longest domain name that equals the name
// or is one of its ancestors (docs.nebius.com/vpc/dns/overview: "the zone
// with the longest matching domain name answers"). ok is false when no zone
// covers the name.
//
// The mock's responder is not inside any network, so it serves the zones of
// every network. When zones of several networks share the domain name, the
// oldest one answers (see docs/services/dns.md).
func (s *Service) FindZone(ctx context.Context, name string) (ZoneData, bool, error) {
	all, err := s.zones.All(ctx)
	if err != nil {
		return ZoneData{}, false, err
	}
	var best *dnspb.Zone
	for _, z := range all {
		d := z.GetSpec().GetDomainName()
		if name != d && !strings.HasSuffix(name, "."+d) {
			continue
		}
		if best == nil || len(d) > len(best.GetSpec().GetDomainName()) {
			best = z
		}
	}
	if best == nil {
		return ZoneData{}, false, nil
	}
	recs, err := s.records.All(ctx, best.GetMetadata().GetId())
	if err != nil {
		return ZoneData{}, false, err
	}
	return ZoneData{Zone: best, Records: recs}, true, nil
}
