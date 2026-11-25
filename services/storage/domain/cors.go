package domain

import (
	"fmt"
	"slices"
	"strings"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// corsMethods are the methods a CORS rule may allow.
var corsMethods = []string{"GET", "PUT", "POST", "DELETE", "HEAD"}

// ValidateCORS checks a CORS configuration: methods are GET, PUT, POST,
// DELETE or HEAD; origins and allowed headers hold at most one "*" wildcard.
func ValidateCORS(c *storagepb.CORSConfiguration) error {
	var vs []errs.Violation
	for i, r := range c.GetRules() {
		vs = append(vs, corsRuleViolations(fmt.Sprintf("spec.cors.rules[%d]", i), r)...)
	}
	if len(vs) > 0 {
		return errs.BadRequest(vs...)
	}
	return nil
}

func corsRuleViolations(field string, r *storagepb.CORSRule) []errs.Violation {
	var vs []errs.Violation
	if len(r.GetAllowedOrigins()) == 0 {
		vs = append(vs, errs.Violation{Field: field + ".allowed_origins", Message: "required"})
	}
	if len(r.GetAllowedMethods()) == 0 {
		vs = append(vs, errs.Violation{Field: field + ".allowed_methods", Message: "required"})
	}
	for j, m := range r.GetAllowedMethods() {
		if !slices.Contains(corsMethods, m) {
			vs = append(vs, errs.Violation{Field: fmt.Sprintf("%s.allowed_methods[%d]", field, j), Message: fmt.Sprintf("unsupported method %q; allowed: %s", m, strings.Join(corsMethods, ", "))})
		}
	}
	for j, o := range r.GetAllowedOrigins() {
		if o == "" || strings.Count(o, "*") > 1 {
			vs = append(vs, errs.Violation{Field: fmt.Sprintf("%s.allowed_origins[%d]", field, j), Message: "must be non-empty with at most one \"*\" wildcard"})
		}
	}
	for j, h := range r.GetAllowedHeaders() {
		if strings.Count(h, "*") > 1 {
			vs = append(vs, errs.Violation{Field: fmt.Sprintf("%s.allowed_headers[%d]", field, j), Message: "at most one \"*\" wildcard"})
		}
	}
	if r.MaxAgeSeconds != nil && r.GetMaxAgeSeconds() < 0 {
		vs = append(vs, errs.Violation{Field: field + ".max_age_seconds", Message: "must not be negative"})
	}
	return vs
}

// CORSMatch is the outcome of matching a request against CORS rules: the
// values of the Access-Control-* response headers.
type CORSMatch struct {
	AllowOrigin      string // "*" for a wildcard rule, else the request origin
	AllowCredentials bool
	AllowMethods     []string
	AllowHeaders     []string // the requested headers (preflight)
	ExposeHeaders    []string
	MaxAgeSeconds    *int32
}

// MatchCORS returns the first rule that allows origin, method and every
// requested header (S3 semantics; header names are case-insensitive).
func MatchCORS(c *storagepb.CORSConfiguration, origin, method string, requestHeaders []string) (CORSMatch, bool) {
	for _, r := range c.GetRules() {
		if !slices.Contains(r.GetAllowedMethods(), method) {
			continue
		}
		matched := ""
		for _, o := range r.GetAllowedOrigins() {
			if wildcardMatch(o, origin) {
				matched = o
				break
			}
		}
		if matched == "" || !allHeadersAllowed(r.GetAllowedHeaders(), requestHeaders) {
			continue
		}
		m := CORSMatch{
			AllowOrigin: origin, AllowCredentials: true,
			AllowMethods: slices.Clone(r.GetAllowedMethods()), ExposeHeaders: slices.Clone(r.GetExposeHeaders()),
			MaxAgeSeconds: r.MaxAgeSeconds,
		}
		if matched == "*" {
			m.AllowOrigin, m.AllowCredentials = "*", false
		}
		for _, h := range requestHeaders {
			m.AllowHeaders = append(m.AllowHeaders, strings.ToLower(h))
		}
		return m, true
	}
	return CORSMatch{}, false
}

func allHeadersAllowed(allowed, requested []string) bool {
	for _, h := range requested {
		if !slices.ContainsFunc(allowed, func(a string) bool { return wildcardMatch(strings.ToLower(a), strings.ToLower(h)) }) {
			return false
		}
	}
	return true
}

// wildcardMatch matches s against a pattern with at most one "*".
func wildcardMatch(pattern, s string) bool {
	before, after, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == s
	}
	return len(s) >= len(before)+len(after) && strings.HasPrefix(s, before) && strings.HasSuffix(s, after)
}
