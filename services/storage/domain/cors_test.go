package domain_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func corsRule(origins, methods, headers []string) *storagepb.CORSRule {
	return &storagepb.CORSRule{AllowedOrigins: origins, AllowedMethods: methods, AllowedHeaders: headers}
}

func corsOf(rs ...*storagepb.CORSRule) *storagepb.CORSConfiguration {
	return &storagepb.CORSConfiguration{Rules: rs}
}

func TestValidateCORS(t *testing.T) {
	t.Parallel()
	get := []string{"GET"}
	origin := []string{"https://a.example"}
	negAge := corsRule(origin, get, nil)
	negAge.MaxAgeSeconds = ptr(int32(-1))
	tests := []struct {
		name string
		cfg  *storagepb.CORSConfiguration
		want []string
	}{
		{"nil", nil, nil},
		{"all methods", corsOf(corsRule(origin, []string{"GET", "PUT", "POST", "DELETE", "HEAD"}, []string{"*"})), nil},
		{
			"bad method PATCH", corsOf(corsRule(origin, []string{"GET", "PATCH"}, nil)),
			[]string{"spec.cors.rules[0].allowed_methods[1]"},
		},
		{
			"lowercase method", corsOf(corsRule(origin, []string{"get"}, nil)),
			[]string{"spec.cors.rules[0].allowed_methods[0]"},
		},
		{
			"two wildcards in origin", corsOf(corsRule([]string{"https://*.*.example"}, get, nil)),
			[]string{"spec.cors.rules[0].allowed_origins[0]"},
		},
		{
			"empty origin", corsOf(corsRule([]string{""}, get, nil)),
			[]string{"spec.cors.rules[0].allowed_origins[0]"},
		},
		{
			"two wildcards in header", corsOf(corsRule(origin, get, []string{"x-*-*"})),
			[]string{"spec.cors.rules[0].allowed_headers[0]"},
		},
		{
			"negative max age", corsOf(negAge),
			[]string{"spec.cors.rules[0].max_age_seconds"},
		},
		{
			"missing origins and methods", corsOf(corsRule(nil, nil, nil)),
			[]string{"spec.cors.rules[0].allowed_methods", "spec.cors.rules[0].allowed_origins"},
		},
		{
			"second rule indexed", corsOf(corsRule(origin, get, nil), corsRule(origin, []string{"X"}, nil)),
			[]string{"spec.cors.rules[1].allowed_methods[0]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := domain.ValidateCORS(tt.cfg)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidateCORS = %v, want nil", err)
				}
				return
			}
			if diff := cmp.Diff(tt.want, violationFields(t, err)); diff != "" {
				t.Errorf("violation fields mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMatchCORS(t *testing.T) {
	t.Parallel()
	withExtras := corsRule([]string{"https://a.example"}, []string{"GET", "HEAD"}, []string{"Content-Type"})
	withExtras.ExposeHeaders = []string{"ETag"}
	withExtras.MaxAgeSeconds = ptr(int32(600))
	subdomains := corsOf(corsRule([]string{"https://*.example.com"}, []string{"PUT"}, nil))
	amz := corsOf(corsRule([]string{"*"}, []string{"PUT"}, []string{"x-amz-*"}))

	tests := []struct {
		name    string
		cfg     *storagepb.CORSConfiguration
		origin  string
		method  string
		headers []string
		want    domain.CORSMatch
		ok      bool
	}{
		{
			name:   "exact origin echoes it with credentials",
			cfg:    corsOf(corsRule([]string{"https://a.example"}, []string{"GET"}, nil)),
			origin: "https://a.example", method: "GET",
			want: domain.CORSMatch{AllowOrigin: "https://a.example", AllowCredentials: true, AllowMethods: []string{"GET"}}, ok: true,
		},
		{
			name:   "wildcard origin answers * without credentials",
			cfg:    corsOf(corsRule([]string{"*"}, []string{"GET"}, nil)),
			origin: "https://anything.example", method: "GET",
			want: domain.CORSMatch{AllowOrigin: "*", AllowMethods: []string{"GET"}}, ok: true,
		},
		{
			name: "origin pattern matches a subdomain", cfg: subdomains,
			origin: "https://app.example.com", method: "PUT",
			want: domain.CORSMatch{AllowOrigin: "https://app.example.com", AllowCredentials: true, AllowMethods: []string{"PUT"}}, ok: true,
		},
		{name: "origin pattern rejects another domain", cfg: subdomains, origin: "https://app.example.org", method: "PUT"},
		{name: "origin pattern rejects a shorter origin", cfg: subdomains, origin: "https://example.com", method: "PUT"},
		{
			name: "unknown origin", cfg: corsOf(corsRule([]string{"https://a.example"}, []string{"GET"}, nil)),
			origin: "https://b.example", method: "GET",
		},
		{
			name: "method not allowed", cfg: corsOf(corsRule([]string{"*"}, []string{"GET"}, nil)),
			origin: "https://a.example", method: "DELETE",
		},
		{
			name: "requested header not allowed", cfg: corsOf(corsRule([]string{"*"}, []string{"GET"}, []string{"content-type"})),
			origin: "https://a.example", method: "GET", headers: []string{"x-custom"},
		},
		{
			name: "requested header without any allowed headers", cfg: corsOf(corsRule([]string{"*"}, []string{"GET"}, nil)),
			origin: "https://a.example", method: "GET", headers: []string{"x-custom"},
		},
		{
			name: "header wildcard prefix", cfg: amz,
			origin: "https://a.example", method: "PUT", headers: []string{"x-amz-meta-foo", "X-Amz-Date"},
			want: domain.CORSMatch{AllowOrigin: "*", AllowMethods: []string{"PUT"}, AllowHeaders: []string{"x-amz-meta-foo", "x-amz-date"}}, ok: true,
		},
		{
			name: "header wildcard does not match other prefix", cfg: amz,
			origin: "https://a.example", method: "PUT", headers: []string{"authorization"},
		},
		{
			name:   "header names are case-insensitive",
			cfg:    corsOf(corsRule([]string{"*"}, []string{"GET"}, []string{"Content-Type"})),
			origin: "https://a.example", method: "GET", headers: []string{"CONTENT-TYPE"},
			want: domain.CORSMatch{AllowOrigin: "*", AllowMethods: []string{"GET"}, AllowHeaders: []string{"content-type"}}, ok: true,
		},
		{
			name: "first matching rule wins",
			cfg: corsOf(
				corsRule([]string{"https://other.example"}, []string{"GET"}, nil),
				corsRule([]string{"*"}, []string{"GET"}, nil),
				corsRule([]string{"https://a.example"}, []string{"GET", "PUT"}, nil),
			),
			origin: "https://a.example", method: "GET",
			want: domain.CORSMatch{AllowOrigin: "*", AllowMethods: []string{"GET"}}, ok: true,
		},
		{
			name:   "expose headers and max age are copied",
			cfg:    corsOf(withExtras),
			origin: "https://a.example", method: "HEAD", headers: []string{"content-type"},
			want: domain.CORSMatch{
				AllowOrigin: "https://a.example", AllowCredentials: true, AllowMethods: []string{"GET", "HEAD"},
				AllowHeaders: []string{"content-type"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: ptr(int32(600)),
			}, ok: true,
		},
		{name: "nil configuration", cfg: nil, origin: "https://a.example", method: "GET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := domain.MatchCORS(tt.cfg, tt.origin, tt.method, tt.headers)
			if ok != tt.ok {
				t.Fatalf("MatchCORS ok = %v, want %v (match %+v)", ok, tt.ok, got)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("match mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
