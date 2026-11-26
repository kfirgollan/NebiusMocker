package domain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/miekg/dns"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// DefaultTTL is the TTL of a record whose spec.ttl is absent (0) or
// negative (record.proto).
const DefaultTTL = 600

// EffectiveTTL is the TTL a record is served with.
func EffectiveTTL(ttl int64) uint32 {
	if ttl <= 0 {
		return DefaultTTL
	}
	return uint32(min(ttl, int64(^uint32(0)>>1))) // RFC 2181: TTLs are at most 2^31-1
}

// userTypes are the record types a user may create in a custom zone
// (docs.nebius.com/vpc/dns/manage-records, "Record types").
var userTypes = map[dnspb.RecordSpec_RecordType]bool{
	dnspb.RecordSpec_A:     true,
	dnspb.RecordSpec_MX:    true,
	dnspb.RecordSpec_TXT:   true,
	dnspb.RecordSpec_SRV:   true,
	dnspb.RecordSpec_CAA:   true,
	dnspb.RecordSpec_SVCB:  true,
	dnspb.RecordSpec_HTTPS: true,
}

// IsSystemType reports whether records of type t are managed by the service
// (the zone's SOA and NS records at the apex).
func IsSystemType(t dnspb.RecordSpec_RecordType) bool {
	return t == dnspb.RecordSpec_SOA || t == dnspb.RecordSpec_NS
}

// checkType rejects the record types the docs list as restricted, and those
// they do not list as available, by name.
func checkType(t dnspb.RecordSpec_RecordType) error {
	const field = "spec.type"
	switch {
	case userTypes[t]:
		return nil
	case t == dnspb.RecordSpec_RECORD_TYPE_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	case t == dnspb.RecordSpec_NS:
		return errs.BadRequest(errs.Violation{Field: field, Message: "NS records cannot be created: custom DNS zones do not support delegation, and the service manages the apex NS record"})
	case t == dnspb.RecordSpec_SOA:
		return errs.BadRequest(errs.Violation{Field: field, Message: "SOA records cannot be created: each zone has exactly one, managed by the service (set spec.soa_spec.negative_ttl on the zone instead)"})
	case t == dnspb.RecordSpec_CNAME:
		return errs.BadRequest(errs.Violation{Field: field, Message: "CNAME records are not supported in custom DNS zones; create a record of the required type at that name instead"})
	default:
		return errs.BadRequest(errs.Violation{Field: field, Message: t.String() + " records are not supported in custom DNS zones"})
	}
}

// relativeOrigin is the origin the data is parsed against: a domain name that
// is not written fully qualified ends up under it and is rejected.
const relativeOrigin = "nmock-relative-name.invalid."

// ParseRR parses a record's data as the RDATA of a zone-file resource record
// "<fqdn> <ttl> IN <type> <data>".
func ParseRR(fqdn string, ttl uint32, t dnspb.RecordSpec_RecordType, data string) (dns.RR, error) {
	line := fmt.Sprintf("%s %d IN %s %s", fqdn, ttl, t.String(), data)
	zp := dns.NewZoneParser(strings.NewReader(line), relativeOrigin, "")
	rr, ok := zp.Next()
	if err := zp.Err(); err != nil {
		return nil, err
	}
	if !ok || rr == nil {
		return nil, fmt.Errorf("no %s data", t)
	}
	if _, more := zp.Next(); more {
		return nil, errors.New("more than one record")
	}
	return rr, nil
}

// checkData validates spec.data for the record type: it must parse as the
// type's presentation format, domain names in it must be fully qualified
// (with a trailing dot), TXT strings must be quoted, and CAA tags are issue,
// issuewild or iodef. It returns the canonical RDATA, used to detect
// duplicates.
func checkData(fqdn string, t dnspb.RecordSpec_RecordType, data string) (string, error) {
	bad := func(msg string) error {
		return errs.BadRequest(errs.Violation{Field: "spec.data", Message: msg})
	}
	if strings.TrimSpace(data) == "" {
		return "", bad("required")
	}
	if strings.ContainsAny(data, "\n\r") {
		return "", bad("must be a single line")
	}
	if hasComment(data) {
		return "", bad("must not contain ';' outside quoted strings")
	}
	if t == dnspb.RecordSpec_TXT && !strings.HasPrefix(strings.TrimSpace(data), `"`) {
		return "", bad(`TXT data must be one or more quoted strings, e.g. "v=spf1 -all"`)
	}
	if t == dnspb.RecordSpec_TXT && hasLongString(data) {
		return "", bad(errTXTLength.Error())
	}
	rr, err := ParseRR(fqdn, DefaultTTL, t, data)
	if err != nil {
		return "", bad(fmt.Sprintf("not valid %s record data: %v", t, err))
	}
	if err := checkRR(rr); err != nil {
		return "", bad(err.Error())
	}
	hdr := rr.Header().String()
	return strings.TrimPrefix(rr.String(), hdr), nil
}

var errTXTLength = errors.New("each TXT string must have 1 to 255 characters; split longer text into several quoted strings")

// hasLongString reports whether a quoted string in data is over 255 octets.
// The zone-file parser silently splits those into several strings, so the
// length is checked on the text the user sent. An escape ("\\X" or "\\DDD")
// is one octet.
func hasLongString(data string) bool {
	quoted, n := false, 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '\\':
			if i+3 < len(data) && isDigit(data[i+1]) && isDigit(data[i+2]) && isDigit(data[i+3]) {
				i += 3
			} else {
				i++
			}
			n++
		case c == '"':
			quoted, n = !quoted, 0
		case quoted:
			n++
		}
		if n > 255 {
			return true
		}
	}
	return false
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// checkRR applies the per-type rules the parser does not enforce.
func checkRR(rr dns.RR) error {
	switch r := rr.(type) {
	case *dns.MX:
		return checkTarget("mail server", r.Mx)
	case *dns.SRV:
		return checkTarget("target", r.Target)
	case *dns.SVCB:
		return checkTarget("target", r.Target)
	case *dns.HTTPS:
		return checkTarget("target", r.Target)
	case *dns.TXT:
		for _, s := range r.Txt {
			if s == "" || len(s) > 255 {
				return errTXTLength
			}
		}
	case *dns.CAA:
		switch r.Tag {
		case "issue", "issuewild", "iodef":
		default:
			return fmt.Errorf("CAA tag must be issue, issuewild or iodef, got %q", r.Tag)
		}
	}
	return nil
}

func checkTarget(what, name string) error {
	switch {
	case name == ".":
		return fmt.Errorf("the %s must be a domain name: the '.' shorthand for the record's own name is not supported", what)
	case strings.HasSuffix(name, "."+relativeOrigin) || name == relativeOrigin:
		return fmt.Errorf("the %s %q must be fully qualified, with a trailing dot (e.g. %q)",
			what, strings.TrimSuffix(strings.TrimSuffix(name, relativeOrigin), "."), "service.my.example.")
	}
	return nil
}

// hasComment reports whether data has a ';' outside double quotes, which the
// zone-file syntax treats as the start of a comment.
func hasComment(data string) bool {
	quoted, escaped := false, false
	for _, c := range data {
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			return true
		}
	}
	return false
}
