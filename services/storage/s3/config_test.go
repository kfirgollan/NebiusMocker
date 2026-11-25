package s3

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
)

const nsAttr = ` xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`

func int32p(v int32) *int32 { return &v }

func strp(v string) *string { return &v }

func lcRuleProto(mutate func(r *storagepb.LifecycleRule)) *storagepb.LifecycleConfiguration {
	r := &storagepb.LifecycleRule{Id: "r", Status: storagepb.LifecycleRule_ENABLED, Filter: &storagepb.LifecycleFilter{}}
	mutate(r)
	return &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{r}}
}

func TestParseLifecycleXML(t *testing.T) {
	t.Parallel()
	date := timestamppb.New(testDate())
	tests := []struct {
		name string
		xml  string
		want *storagepb.LifecycleConfiguration
	}{
		{
			name: "aws cli expire example",
			xml: `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><Expiration><Days>5</Days></Expiration>` +
				`<Filter></Filter><ID>expire</ID><Status>Enabled</Status></Rule></LifecycleConfiguration>`,
			want: &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{{
				Id: "expire", Status: storagepb.LifecycleRule_ENABLED, Filter: &storagepb.LifecycleFilter{},
				Expiration: &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 5}},
			}}},
		},
		{
			name: "no namespace, disabled, prefix filter",
			xml: `<LifecycleConfiguration><Rule><ID>r</ID><Status>Disabled</Status><Filter><Prefix>logs/</Prefix></Filter>` +
				`<Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Status = storagepb.LifecycleRule_DISABLED
				r.Filter = &storagepb.LifecycleFilter{Prefix: "logs/"}
				r.Expiration = &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 1}}
			}),
		},
		{
			name: "single tag",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter>` +
				`</Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: "k", Value: "v"}}}
			}),
		},
		{
			name: "size filters",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter>` +
				`<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan></Filter></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{ObjectSizeGreaterThanBytes: 10}
			}),
		},
		{
			name: "and filter",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter><And><Prefix>p/</Prefix>` +
				`<Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag>` +
				`<ObjectSizeGreaterThan>5</ObjectSizeGreaterThan><ObjectSizeLessThan>500</ObjectSizeLessThan></And></Filter></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{
					Prefix: "p/", ObjectSizeGreaterThanBytes: 5, ObjectSizeLessThanBytes: 500,
					Tags: []*storagepb.LifecycleFilter_Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}},
				}
			}),
		},
		{
			name: "expiration date with millis and delete marker",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<Expiration><Date>2026-12-01T00:00:00.000Z</Date><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>` +
				`</Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Expiration = &storagepb.LifecycleExpiration{
					ExpiredWith:               &storagepb.LifecycleExpiration_Date{Date: date},
					ExpiredObjectDeleteMarker: true,
				}
			}),
		},
		{
			name: "expiration date without millis",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<Expiration><Date>2026-12-01T00:00:00Z</Date></Expiration></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Expiration = &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Date{Date: date}}
			}),
		},
		{
			name: "noncurrent and abort",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration>` +
				`<AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload>` +
				`</Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.NoncurrentVersionExpiration = &storagepb.LifecycleNoncurrentVersionExpiration{
					NoncurrentDays: 7, NewerNoncurrentVersions: int32p(3),
				}
				r.AbortIncompleteMultipartUpload = &storagepb.LifecycleAbortIncompleteMultipartUpload{DaysAfterInitiation: 2}
			}),
		},
		{
			name: "noncurrent without newer versions",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.NoncurrentVersionExpiration = &storagepb.LifecycleNoncurrentVersionExpiration{NoncurrentDays: 7}
			}),
		},
		{
			name: "transitions",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<Transition><Days>30</Days><StorageClass>ENHANCED_THROUGHPUT</StorageClass></Transition>` +
				`<NoncurrentVersionTransition><NoncurrentDays>9</NoncurrentDays><NewerNoncurrentVersions>1</NewerNoncurrentVersions>` +
				`<StorageClass>INTELLIGENT</StorageClass></NoncurrentVersionTransition></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Transition = &storagepb.LifecycleTransition{
					TransitedWith: &storagepb.LifecycleTransition_Days{Days: 30},
					StorageClass:  storagepb.StorageClass_ENHANCED_THROUGHPUT,
				}
				r.NoncurrentVersionTransition = &storagepb.LifecycleNoncurrentVersionTransition{
					NoncurrentDays: 9, NewerNoncurrentVersions: int32p(1), StorageClass: storagepb.StorageClass_INTELLIGENT,
				}
			}),
		},
		{
			name: "transition date",
			xml: `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID><Status>Enabled</Status><Filter/>` +
				`<Transition><Date>2026-12-01T00:00:00.000Z</Date><StorageClass>STANDARD</StorageClass></Transition></Rule></LifecycleConfiguration>`,
			want: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Transition = &storagepb.LifecycleTransition{
					TransitedWith: &storagepb.LifecycleTransition_Date{Date: date},
					StorageClass:  storagepb.StorageClass_STANDARD,
				}
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLifecycleXML([]byte(tc.xml))
			if err != nil {
				t.Fatalf("parseLifecycleXML: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("config mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseLifecycleXMLErrors(t *testing.T) {
	t.Parallel()
	rule := func(inner string) string {
		return `<LifecycleConfiguration` + nsAttr + `><Rule><ID>r</ID>` + inner + `</Rule></LifecycleConfiguration>`
	}
	tests := []struct {
		name     string
		xml      string
		wantCode string
		wantMsg  string
	}{
		{"not xml", `{"Rules":[]}`, "MalformedXML", ""},
		{"empty body", ``, "MalformedXML", ""},
		{"wrong root", `<Other/>`, "MalformedXML", ""},
		{"bad number", rule(`<Status>Enabled</Status><Expiration><Days>abc</Days></Expiration>`), "MalformedXML", ""},
		{"bad status", rule(`<Status>Maybe</Status>`), "InvalidArgument", "Status"},
		{"missing status", rule(``), "InvalidArgument", "Status"},
		{"deprecated prefix", rule(`<Status>Enabled</Status><Prefix>x/</Prefix>`), "InvalidArgument", "deprecated rule-level Prefix"},
		{"two transitions", rule(`<Status>Enabled</Status><Filter/>` +
			`<Transition><Days>1</Days><StorageClass>STANDARD</StorageClass></Transition>` +
			`<Transition><Days>2</Days><StorageClass>INTELLIGENT</StorageClass></Transition>`), "InvalidArgument", "only one transition"},
		{
			"bad storage class", rule(`<Status>Enabled</Status><Transition><Days>1</Days><StorageClass>GLACIER</StorageClass></Transition>`),
			"InvalidArgument", "StorageClass",
		},
		{"bad date", rule(`<Status>Enabled</Status><Expiration><Date>yesterday</Date></Expiration>`), "InvalidArgument", "Date"},
		{
			"days and date", rule(`<Status>Enabled</Status><Expiration><Days>1</Days><Date>2026-12-01T00:00:00Z</Date></Expiration>`),
			"InvalidArgument", "both",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLifecycleXML([]byte(tc.xml))
			assertXMLError(t, err, tc.wantCode, tc.wantMsg)
		})
	}
}

func assertXMLError(t *testing.T, err error, wantCode, wantMsg string) {
	t.Helper()
	xe, ok := errors.AsType[*xmlError](err)
	if !ok {
		t.Fatalf("error = %v, want *xmlError", err)
	}
	if xe.Code != wantCode {
		t.Errorf("code = %q, want %q (%v)", xe.Code, wantCode, err)
	}
	if !strings.Contains(xe.Error(), wantMsg) {
		t.Errorf("message %q does not contain %q", xe.Error(), wantMsg)
	}
}

func TestLifecycleXMLRoundTrip(t *testing.T) {
	t.Parallel()
	date := timestamppb.New(testDate())
	tag := func(k, v string) *storagepb.LifecycleFilter_Tag {
		return &storagepb.LifecycleFilter_Tag{Key: k, Value: v}
	}
	tests := []struct {
		name string
		cfg  *storagepb.LifecycleConfiguration
		want []string // substrings of the rendered XML
	}{
		{
			name: "empty filter with days",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Expiration = &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 5}}
			}),
			want: []string{`<Filter><Prefix></Prefix></Filter>`, `<Days>5</Days>`, `<Status>Enabled</Status>`, nsAttr},
		},
		{
			name: "prefix only is direct",
			cfg:  lcRuleProto(func(r *storagepb.LifecycleRule) { r.Filter = &storagepb.LifecycleFilter{Prefix: "a/"} }),
			want: []string{`<Filter><Prefix>a/</Prefix></Filter>`},
		},
		{
			name: "single tag is direct",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Status = storagepb.LifecycleRule_DISABLED
				r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{tag("k", "v")}}
			}),
			want: []string{`<Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter>`, `<Status>Disabled</Status>`},
		},
		{
			name: "size only is direct",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{ObjectSizeLessThanBytes: 9}
			}),
			want: []string{`<Filter><ObjectSizeLessThan>9</ObjectSizeLessThan></Filter>`},
		},
		{
			name: "several predicates use And",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{
					Prefix: "p/", ObjectSizeGreaterThanBytes: 1, Tags: []*storagepb.LifecycleFilter_Tag{tag("a", "1"), tag("b", "2")},
				}
			}),
			want: []string{`<Filter><And><Prefix>p/</Prefix><Tag><Key>a</Key>`, `</Tag><Tag><Key>b</Key>`, `<ObjectSizeGreaterThan>1</ObjectSizeGreaterThan></And></Filter>`},
		},
		{
			name: "two tags use And",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{tag("a", "1"), tag("b", "2")}}
			}),
			want: []string{`<Filter><And><Tag>`},
		},
		{
			name: "expiration date and marker",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.Expiration = &storagepb.LifecycleExpiration{
					ExpiredWith: &storagepb.LifecycleExpiration_Date{Date: date}, ExpiredObjectDeleteMarker: true,
				}
			}),
			want: []string{`<Date>2026-12-01T00:00:00.000Z</Date>`, `<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>`},
		},
		{
			name: "noncurrent abort transitions",
			cfg: lcRuleProto(func(r *storagepb.LifecycleRule) {
				r.NoncurrentVersionExpiration = &storagepb.LifecycleNoncurrentVersionExpiration{NoncurrentDays: 7, NewerNoncurrentVersions: int32p(3)}
				r.AbortIncompleteMultipartUpload = &storagepb.LifecycleAbortIncompleteMultipartUpload{DaysAfterInitiation: 2}
				r.Transition = &storagepb.LifecycleTransition{
					TransitedWith: &storagepb.LifecycleTransition_Days{Days: 30}, StorageClass: storagepb.StorageClass_INTELLIGENT,
				}
				r.NoncurrentVersionTransition = &storagepb.LifecycleNoncurrentVersionTransition{
					NoncurrentDays: 9, StorageClass: storagepb.StorageClass_STANDARD,
				}
			}),
			want: []string{
				`<NoncurrentDays>7</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions>`,
				`<DaysAfterInitiation>2</DaysAfterInitiation>`,
				`<Transition><Days>30</Days><StorageClass>INTELLIGENT</StorageClass></Transition>`,
				`<NoncurrentVersionTransition><NoncurrentDays>9</NoncurrentDays><StorageClass>STANDARD</StorageClass>`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := lifecycleXML(tc.cfg)
			if err != nil {
				t.Fatalf("lifecycleXML: %v", err)
			}
			if !strings.HasPrefix(string(body), "<?xml") {
				t.Errorf("missing xml header: %s", body)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(body), w) {
					t.Errorf("body missing %q:\n%s", w, body)
				}
			}
			back, err := parseLifecycleXML(body)
			if err != nil {
				t.Fatalf("parseLifecycleXML(rendered): %v", err)
			}
			if diff := cmp.Diff(tc.cfg, back, protocmp.Transform()); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLifecycleXMLDaysSinceLastAccessUnsupported(t *testing.T) {
	t.Parallel()
	cfg := lcRuleProto(func(r *storagepb.LifecycleRule) {
		r.Transition = &storagepb.LifecycleTransition{TransitedWith: &storagepb.LifecycleTransition_DaysSinceLastAccess{DaysSinceLastAccess: 3}}
	})
	_, err := lifecycleXML(cfg)
	assertXMLError(t, err, "InvalidArgument", "days_since_last_access")
}

func testDate() time.Time { return time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC) }

func TestCORSXML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		xml  string
		want *storagepb.CORSConfiguration
	}{
		{
			name: "full rule",
			xml: `<CORSConfiguration` + nsAttr + `><CORSRule><ID>web</ID><AllowedHeader>*</AllowedHeader><AllowedHeader>x-amz-meta-a</AllowedHeader>` +
				`<AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod><AllowedOrigin>https://a.example</AllowedOrigin>` +
				`<ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`,
			want: &storagepb.CORSConfiguration{Rules: []*storagepb.CORSRule{{
				Id: strp("web"), AllowedHeaders: []string{"*", "x-amz-meta-a"}, AllowedMethods: []string{"GET", "PUT"},
				AllowedOrigins: []string{"https://a.example"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: int32p(3000),
			}}},
		},
		{
			name: "minimal rules without namespace",
			xml: `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule>` +
				`<CORSRule><AllowedMethod>HEAD</AllowedMethod><AllowedOrigin>http://b</AllowedOrigin></CORSRule></CORSConfiguration>`,
			want: &storagepb.CORSConfiguration{Rules: []*storagepb.CORSRule{
				{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"}},
				{AllowedMethods: []string{"HEAD"}, AllowedOrigins: []string{"http://b"}},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCORSXML([]byte(tc.xml))
			if err != nil {
				t.Fatalf("parseCORSXML: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("parse mismatch (-want +got):\n%s", diff)
			}
			body, err := corsXML(got)
			if err != nil {
				t.Fatalf("corsXML: %v", err)
			}
			if !strings.Contains(string(body), nsAttr) || !strings.HasPrefix(string(body), "<?xml") {
				t.Errorf("missing namespace or header: %s", body)
			}
			back, err := parseCORSXML(body)
			if err != nil {
				t.Fatalf("parseCORSXML(rendered): %v", err)
			}
			if diff := cmp.Diff(tc.want, back, protocmp.Transform()); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseCORSXMLMalformed(t *testing.T) {
	t.Parallel()
	_, err := parseCORSXML([]byte(`<CORSConfiguration><CORSRule>`))
	assertXMLError(t, err, "MalformedXML", "")
}

func TestVersioningXML(t *testing.T) {
	t.Parallel()
	parseTests := []struct {
		name     string
		xml      string
		want     storagepb.VersioningPolicy
		wantCode string
		wantMsg  string
	}{
		{name: "enabled", xml: `<VersioningConfiguration` + nsAttr + `><Status>Enabled</Status></VersioningConfiguration>`, want: storagepb.VersioningPolicy_ENABLED},
		{name: "suspended no ns", xml: `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`, want: storagepb.VersioningPolicy_SUSPENDED},
		{name: "mfa disabled ok", xml: `<VersioningConfiguration><Status>Enabled</Status><MfaDelete>Disabled</MfaDelete></VersioningConfiguration>`, want: storagepb.VersioningPolicy_ENABLED},
		{name: "mfa enabled", xml: `<VersioningConfiguration><Status>Enabled</Status><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`, wantCode: "InvalidArgument", wantMsg: "MFADelete"},
		{name: "bad status", xml: `<VersioningConfiguration><Status>Off</Status></VersioningConfiguration>`, wantCode: "InvalidArgument", wantMsg: "Status"},
		{name: "no status", xml: `<VersioningConfiguration/>`, wantCode: "MalformedXML"},
		{name: "garbage", xml: `nope`, wantCode: "MalformedXML"},
	}
	for _, tc := range parseTests {
		t.Run("parse "+tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseVersioningXML([]byte(tc.xml))
			if tc.wantCode != "" {
				assertXMLError(t, err, tc.wantCode, tc.wantMsg)
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got (%v, %v), want %v", got, err, tc.want)
			}
		})
	}

	renderTests := []struct {
		policy storagepb.VersioningPolicy
		want   string
	}{
		{storagepb.VersioningPolicy_ENABLED, `<VersioningConfiguration` + nsAttr + `><Status>Enabled</Status></VersioningConfiguration>`},
		{storagepb.VersioningPolicy_SUSPENDED, `<VersioningConfiguration` + nsAttr + `><Status>Suspended</Status></VersioningConfiguration>`},
		{storagepb.VersioningPolicy_DISABLED, `<VersioningConfiguration` + nsAttr + `></VersioningConfiguration>`},
		{storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED, `<VersioningConfiguration` + nsAttr + `></VersioningConfiguration>`},
	}
	for _, tc := range renderTests {
		t.Run("render "+tc.policy.String(), func(t *testing.T) {
			t.Parallel()
			body, err := versioningXML(tc.policy)
			if err != nil {
				t.Fatalf("versioningXML: %v", err)
			}
			if !strings.HasSuffix(string(body), tc.want) {
				t.Errorf("body = %s, want suffix %s", body, tc.want)
			}
		})
	}
}
