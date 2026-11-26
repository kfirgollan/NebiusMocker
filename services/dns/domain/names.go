package domain

import (
	"fmt"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Apex is the relative name of records at the zone apex.
const Apex = "@"

// maxNameLength is the longest domain name in presentation format, without
// the trailing dot (RFC 1035: 255 octets on the wire).
const maxNameLength = 253

// reservedDomains are the domain names Nebius owns: a zone at one of them or
// below fails with PermissionDenied (docs.nebius.com/vpc/dns/resources/
// quotas-limits, "Zone domain name restrictions"). "internal." itself is a
// single label, which is rejected before this list is consulted; other names
// under it (e.g. "corp.internal.") are allowed.
var reservedDomains = []string{
	"ns.internal.",
	"compute.internal.",
	"nebius.internal.",
	"console.nebius.",
	"example.invalid.",
}

// checkZoneDomain validates spec.domain_name beyond the proto pattern
// (lowercase FQDN ending with "."): non-empty labels of at most 63
// characters, at least two labels, and not a reserved name.
func checkZoneDomain(name string) error {
	const field = "spec.domain_name"
	if !strings.HasSuffix(name, ".") || len(name) >= maxNameLength {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a fully-qualified domain name of at most 252 characters ending with '.'"})
	}
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("label %q must have 1 to 63 characters", l)})
		}
	}
	if len(labels) < 2 {
		return errs.BadRequest(errs.Violation{Field: field, Message: "single-label domain names such as 'example.' are not available; use at least two labels, e.g. 'my.example.'"})
	}
	for _, r := range reservedDomains {
		if name == r || strings.HasSuffix(name, "."+r) {
			e := errs.PermissionDenied(name)
			e.Message = "the domain name " + r + " and its subdomains are reserved by Nebius AI Cloud"
			return e
		}
	}
	return nil
}

// EffectiveFQDN joins a record's relative name to its zone's domain name.
func EffectiveFQDN(relativeName, zoneDomain string) string {
	if relativeName == Apex {
		return zoneDomain
	}
	return relativeName + "." + zoneDomain
}

// checkFQDNLength rejects records whose effective name is too long.
func checkFQDNLength(fqdn string) error {
	if len(strings.TrimSuffix(fqdn, ".")) > maxNameLength {
		return errs.BadRequest(errs.Violation{
			Field: "spec.relative_name", Message: fmt.Sprintf("the record name %s is longer than %d characters", fqdn, maxNameLength),
		})
	}
	return nil
}
