package s3

import (
	"encoding/xml"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
)

// s3Namespace is the XML namespace S3 puts on response root elements.
const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// s3DateLayout is the timestamp layout S3 emits for lifecycle dates.
const s3DateLayout = "2006-01-02T15:04:05.000Z"

// Error codes carried by xmlError.
const (
	codeMalformedXML    = "MalformedXML"
	codeInvalidArgument = "InvalidArgument"
)

// xmlError is a request-body problem the front maps to an S3 error response.
type xmlError struct{ Code, Message string }

func (e *xmlError) Error() string { return e.Message }

func badXMLBody(format string, args ...any) *xmlError {
	return &xmlError{Code: codeMalformedXML, Message: fmt.Sprintf(format, args...)}
}

func badXMLArgument(format string, args ...any) *xmlError {
	return &xmlError{Code: codeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

// marshalConfig renders v with the XML header.
func marshalConfig(v any) ([]byte, error) {
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal xml: %w", err)
	}
	return append([]byte(xml.Header), b...), nil
}

// unmarshalConfig decodes body into v, mapping decode failures to MalformedXML.
func unmarshalConfig(body []byte, v any) error {
	if err := xml.Unmarshal(body, v); err != nil {
		return badXMLBody("the XML you provided was not well-formed or did not validate against our published schema: %v", err)
	}
	return nil
}

// ---- Lifecycle ----

type lcConfigXML struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Rules   []lcRule `xml:"Rule"`
}

type lcRule struct {
	ID                             string                `xml:"ID,omitempty"`
	Status                         string                `xml:"Status"`
	Prefix                         *string               `xml:"Prefix,omitempty"`
	Filter                         *lcFilter             `xml:"Filter,omitempty"`
	Expiration                     *lcExpiration         `xml:"Expiration,omitempty"`
	NoncurrentVersionExpiration    *lcNoncurrentExp      `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncompleteMultipartUpload *lcAbort              `xml:"AbortIncompleteMultipartUpload,omitempty"`
	Transitions                    []lcTransition        `xml:"Transition,omitempty"`
	NoncurrentVersionTransitions   []lcNoncurrentTransit `xml:"NoncurrentVersionTransition,omitempty"`
}

type lcTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type lcFilter struct {
	Prefix                *string `xml:"Prefix,omitempty"`
	Tag                   *lcTag  `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64  `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64  `xml:"ObjectSizeLessThan,omitempty"`
	And                   *lcAnd  `xml:"And,omitempty"`
}

type lcAnd struct {
	Prefix                *string `xml:"Prefix,omitempty"`
	Tags                  []lcTag `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64  `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64  `xml:"ObjectSizeLessThan,omitempty"`
}

type lcExpiration struct {
	Days                      *int32  `xml:"Days,omitempty"`
	Date                      *string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lcNoncurrentExp struct {
	NoncurrentDays          int32  `xml:"NoncurrentDays"`
	NewerNoncurrentVersions *int32 `xml:"NewerNoncurrentVersions,omitempty"`
}

type lcAbort struct {
	DaysAfterInitiation int32 `xml:"DaysAfterInitiation"`
}

type lcTransition struct {
	Days         *int32  `xml:"Days,omitempty"`
	Date         *string `xml:"Date,omitempty"`
	StorageClass string  `xml:"StorageClass"`
}

type lcNoncurrentTransit struct {
	NoncurrentDays          int32  `xml:"NoncurrentDays"`
	NewerNoncurrentVersions *int32 `xml:"NewerNoncurrentVersions,omitempty"`
	StorageClass            string `xml:"StorageClass"`
}

// parseLifecycleXML converts a PutBucketLifecycleConfiguration body to the proto form.
func parseLifecycleXML(body []byte) (*storagepb.LifecycleConfiguration, error) {
	var in lcConfigXML
	if err := unmarshalConfig(body, &in); err != nil {
		return nil, err
	}
	cfg := &storagepb.LifecycleConfiguration{}
	for i := range in.Rules {
		rule, err := parseLifecycleRule(&in.Rules[i])
		if err != nil {
			return nil, err
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	return cfg, nil
}

func parseLifecycleRule(in *lcRule) (*storagepb.LifecycleRule, error) {
	if in.Prefix != nil {
		return nil, badXMLArgument("the deprecated rule-level Prefix element is not supported; use Filter")
	}
	status, err := parseRuleStatus(in.Status)
	if err != nil {
		return nil, err
	}
	rule := &storagepb.LifecycleRule{Id: in.ID, Status: status, Filter: parseLifecycleFilter(in.Filter)}
	if rule.Expiration, err = parseExpiration(in.Expiration); err != nil {
		return nil, err
	}
	if in.NoncurrentVersionExpiration != nil {
		rule.NoncurrentVersionExpiration = &storagepb.LifecycleNoncurrentVersionExpiration{
			NoncurrentDays:          in.NoncurrentVersionExpiration.NoncurrentDays,
			NewerNoncurrentVersions: in.NoncurrentVersionExpiration.NewerNoncurrentVersions,
		}
	}
	if in.AbortIncompleteMultipartUpload != nil {
		rule.AbortIncompleteMultipartUpload = &storagepb.LifecycleAbortIncompleteMultipartUpload{
			DaysAfterInitiation: in.AbortIncompleteMultipartUpload.DaysAfterInitiation,
		}
	}
	if rule.Transition, err = parseTransition(in.Transitions); err != nil {
		return nil, err
	}
	rule.NoncurrentVersionTransition, err = parseNoncurrentTransition(in.NoncurrentVersionTransitions)
	if err != nil {
		return nil, err
	}
	return rule, nil
}

func parseRuleStatus(s string) (storagepb.LifecycleRule_Status, error) {
	switch s {
	case "Enabled":
		return storagepb.LifecycleRule_ENABLED, nil
	case "Disabled":
		return storagepb.LifecycleRule_DISABLED, nil
	default:
		return storagepb.LifecycleRule_STATUS_UNSPECIFIED, badXMLArgument("invalid rule Status %q: must be Enabled or Disabled", s)
	}
}

// parseLifecycleFilter merges the direct and And predicates; a missing Filter is the empty filter.
func parseLifecycleFilter(in *lcFilter) *storagepb.LifecycleFilter {
	out := &storagepb.LifecycleFilter{}
	if in == nil {
		return out
	}
	addFilterPredicates(out, in.Prefix, in.ObjectSizeGreaterThan, in.ObjectSizeLessThan)
	if in.Tag != nil {
		out.Tags = append(out.Tags, &storagepb.LifecycleFilter_Tag{Key: in.Tag.Key, Value: in.Tag.Value})
	}
	if in.And == nil {
		return out
	}
	addFilterPredicates(out, in.And.Prefix, in.And.ObjectSizeGreaterThan, in.And.ObjectSizeLessThan)
	for _, t := range in.And.Tags {
		out.Tags = append(out.Tags, &storagepb.LifecycleFilter_Tag{Key: t.Key, Value: t.Value})
	}
	return out
}

func addFilterPredicates(out *storagepb.LifecycleFilter, prefix *string, gt, lt *int64) {
	if prefix != nil {
		out.Prefix = *prefix
	}
	if gt != nil {
		out.ObjectSizeGreaterThanBytes = *gt
	}
	if lt != nil {
		out.ObjectSizeLessThanBytes = *lt
	}
}

func parseExpiration(in *lcExpiration) (*storagepb.LifecycleExpiration, error) {
	if in == nil {
		return nil, nil //nolint:nilnil // absent element maps to an absent message
	}
	out := &storagepb.LifecycleExpiration{}
	if in.ExpiredObjectDeleteMarker != nil {
		out.ExpiredObjectDeleteMarker = *in.ExpiredObjectDeleteMarker
	}
	switch {
	case in.Days != nil && in.Date != nil:
		return nil, badXMLArgument("Expiration must not specify both Days and Date")
	case in.Days != nil:
		out.ExpiredWith = &storagepb.LifecycleExpiration_Days{Days: *in.Days}
	case in.Date != nil:
		ts, err := parseS3Date(*in.Date)
		if err != nil {
			return nil, err
		}
		out.ExpiredWith = &storagepb.LifecycleExpiration_Date{Date: ts}
	}
	return out, nil
}

func parseTransition(in []lcTransition) (*storagepb.LifecycleTransition, error) {
	switch len(in) {
	case 0:
		return nil, nil //nolint:nilnil // absent element maps to an absent message
	case 1:
	default:
		return nil, badXMLArgument("only one transition per rule is supported")
	}
	class, err := parseStorageClass(in[0].StorageClass)
	if err != nil {
		return nil, err
	}
	out := &storagepb.LifecycleTransition{StorageClass: class}
	switch {
	case in[0].Days != nil && in[0].Date != nil:
		return nil, badXMLArgument("Transition must not specify both Days and Date")
	case in[0].Days != nil:
		out.TransitedWith = &storagepb.LifecycleTransition_Days{Days: *in[0].Days}
	case in[0].Date != nil:
		ts, err := parseS3Date(*in[0].Date)
		if err != nil {
			return nil, err
		}
		out.TransitedWith = &storagepb.LifecycleTransition_Date{Date: ts}
	}
	return out, nil
}

func parseNoncurrentTransition(in []lcNoncurrentTransit) (*storagepb.LifecycleNoncurrentVersionTransition, error) {
	switch len(in) {
	case 0:
		return nil, nil //nolint:nilnil // absent element maps to an absent message
	case 1:
	default:
		return nil, badXMLArgument("only one noncurrent version transition per rule is supported")
	}
	class, err := parseStorageClass(in[0].StorageClass)
	if err != nil {
		return nil, err
	}
	return &storagepb.LifecycleNoncurrentVersionTransition{
		NoncurrentDays:          in[0].NoncurrentDays,
		NewerNoncurrentVersions: in[0].NewerNoncurrentVersions,
		StorageClass:            class,
	}, nil
}

func parseStorageClass(s string) (storagepb.StorageClass, error) {
	switch s {
	case "STANDARD":
		return storagepb.StorageClass_STANDARD, nil
	case "ENHANCED_THROUGHPUT":
		return storagepb.StorageClass_ENHANCED_THROUGHPUT, nil
	case "INTELLIGENT":
		return storagepb.StorageClass_INTELLIGENT, nil
	default:
		return storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED, badXMLArgument("unsupported StorageClass %q", s)
	}
}

func parseS3Date(s string) (*timestamppb.Timestamp, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, badXMLArgument("invalid Date %q: must be ISO 8601", s)
	}
	return timestamppb.New(t), nil
}

// lifecycleXML renders a GetBucketLifecycleConfiguration response body.
func lifecycleXML(cfg *storagepb.LifecycleConfiguration) ([]byte, error) {
	out := lcConfigXML{Xmlns: s3Namespace}
	for _, r := range cfg.GetRules() {
		rule, err := lifecycleRuleXML(r)
		if err != nil {
			return nil, err
		}
		out.Rules = append(out.Rules, rule)
	}
	return marshalConfig(&out)
}

func lifecycleRuleXML(r *storagepb.LifecycleRule) (lcRule, error) {
	out := lcRule{
		ID:         r.GetId(),
		Status:     ruleStatusName(r.GetStatus()),
		Filter:     lifecycleFilterXML(r.GetFilter()),
		Expiration: expirationXML(r.GetExpiration()),
	}
	if e := r.GetNoncurrentVersionExpiration(); e != nil {
		out.NoncurrentVersionExpiration = &lcNoncurrentExp{
			NoncurrentDays:          e.GetNoncurrentDays(),
			NewerNoncurrentVersions: e.NewerNoncurrentVersions,
		}
	}
	if a := r.GetAbortIncompleteMultipartUpload(); a != nil {
		out.AbortIncompleteMultipartUpload = &lcAbort{DaysAfterInitiation: a.GetDaysAfterInitiation()}
	}
	if t := r.GetTransition(); t != nil {
		tr, err := transitionXML(t)
		if err != nil {
			return lcRule{}, err
		}
		out.Transitions = []lcTransition{tr}
	}
	if t := r.GetNoncurrentVersionTransition(); t != nil {
		out.NoncurrentVersionTransitions = []lcNoncurrentTransit{{
			NoncurrentDays:          t.GetNoncurrentDays(),
			NewerNoncurrentVersions: t.NewerNoncurrentVersions,
			StorageClass:            t.GetStorageClass().String(),
		}}
	}
	return out, nil
}

func ruleStatusName(s storagepb.LifecycleRule_Status) string {
	if s == storagepb.LifecycleRule_ENABLED {
		return "Enabled"
	}
	return "Disabled"
}

// lifecycleFilterXML emits the predicate directly when there is one, inside And when
// there are several, and as an empty Prefix when there are none.
func lifecycleFilterXML(f *storagepb.LifecycleFilter) *lcFilter {
	tags := make([]lcTag, 0, len(f.GetTags()))
	for _, t := range f.GetTags() {
		tags = append(tags, lcTag{Key: t.GetKey(), Value: t.GetValue()})
	}
	var prefix *string
	if p := f.GetPrefix(); p != "" {
		prefix = &p
	}
	gt, lt := optInt64(f.GetObjectSizeGreaterThanBytes()), optInt64(f.GetObjectSizeLessThanBytes())

	count := len(tags)
	for _, set := range []bool{prefix != nil, gt != nil, lt != nil} {
		if set {
			count++
		}
	}
	switch {
	case count == 0:
		empty := ""
		return &lcFilter{Prefix: &empty}
	case count > 1:
		return &lcFilter{And: &lcAnd{Prefix: prefix, Tags: tags, ObjectSizeGreaterThan: gt, ObjectSizeLessThan: lt}}
	case len(tags) == 1:
		return &lcFilter{Tag: &tags[0]}
	default:
		return &lcFilter{Prefix: prefix, ObjectSizeGreaterThan: gt, ObjectSizeLessThan: lt}
	}
}

func optInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func expirationXML(e *storagepb.LifecycleExpiration) *lcExpiration {
	if e == nil {
		return nil
	}
	out := &lcExpiration{}
	if e.GetExpiredObjectDeleteMarker() {
		marker := true
		out.ExpiredObjectDeleteMarker = &marker
	}
	switch w := e.GetExpiredWith().(type) {
	case *storagepb.LifecycleExpiration_Days:
		out.Days = &w.Days
	case *storagepb.LifecycleExpiration_Date:
		d := formatS3Date(w.Date)
		out.Date = &d
	}
	return out
}

func transitionXML(t *storagepb.LifecycleTransition) (lcTransition, error) {
	out := lcTransition{StorageClass: t.GetStorageClass().String()}
	switch w := t.GetTransitedWith().(type) {
	case *storagepb.LifecycleTransition_Days:
		out.Days = &w.Days
	case *storagepb.LifecycleTransition_Date:
		d := formatS3Date(w.Date)
		out.Date = &d
	case *storagepb.LifecycleTransition_DaysSinceLastAccess:
		return lcTransition{}, badXMLArgument("days_since_last_access transitions have no S3 representation")
	}
	return out, nil
}

func formatS3Date(ts *timestamppb.Timestamp) string {
	return ts.AsTime().UTC().Format(s3DateLayout)
}

// ---- CORS ----

type corsConfigXML struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	Xmlns   string     `xml:"xmlns,attr,omitempty"`
	Rules   []corsRule `xml:"CORSRule"`
}

type corsRule struct {
	ID             *string  `xml:"ID,omitempty"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	AllowedMethods []string `xml:"AllowedMethod,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  *int32   `xml:"MaxAgeSeconds,omitempty"`
}

// parseCORSXML converts a PutBucketCors body to the proto form.
func parseCORSXML(body []byte) (*storagepb.CORSConfiguration, error) {
	var in corsConfigXML
	if err := unmarshalConfig(body, &in); err != nil {
		return nil, err
	}
	cfg := &storagepb.CORSConfiguration{}
	for _, r := range in.Rules {
		cfg.Rules = append(cfg.Rules, &storagepb.CORSRule{
			Id:             r.ID,
			AllowedHeaders: r.AllowedHeaders,
			AllowedMethods: r.AllowedMethods,
			AllowedOrigins: r.AllowedOrigins,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return cfg, nil
}

// corsXML renders a GetBucketCors response body.
func corsXML(cfg *storagepb.CORSConfiguration) ([]byte, error) {
	out := corsConfigXML{Xmlns: s3Namespace}
	for _, r := range cfg.GetRules() {
		out.Rules = append(out.Rules, corsRule{
			ID:             r.Id,
			AllowedHeaders: r.GetAllowedHeaders(),
			AllowedMethods: r.GetAllowedMethods(),
			AllowedOrigins: r.GetAllowedOrigins(),
			ExposeHeaders:  r.GetExposeHeaders(),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return marshalConfig(&out)
}

// ---- Versioning ----

type versioningXMLDoc struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Xmlns     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MFADelete string   `xml:"MfaDelete,omitempty"`
}

// parseVersioningXML converts a PutBucketVersioning body to a versioning policy.
func parseVersioningXML(body []byte) (storagepb.VersioningPolicy, error) {
	const unspecified = storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED
	var in versioningXMLDoc
	if err := unmarshalConfig(body, &in); err != nil {
		return unspecified, err
	}
	if in.MFADelete == "Enabled" {
		return unspecified, badXMLArgument("MFADelete is not supported")
	}
	switch in.Status {
	case "Enabled":
		return storagepb.VersioningPolicy_ENABLED, nil
	case "Suspended":
		return storagepb.VersioningPolicy_SUSPENDED, nil
	case "":
		return unspecified, badXMLBody("VersioningConfiguration requires a Status")
	default:
		return unspecified, badXMLArgument("invalid versioning Status %q: must be Enabled or Suspended", in.Status)
	}
}

// versioningXML renders a GetBucketVersioning response body; a never-versioned
// bucket yields an empty configuration like S3.
func versioningXML(p storagepb.VersioningPolicy) ([]byte, error) {
	out := versioningXMLDoc{Xmlns: s3Namespace}
	switch p {
	case storagepb.VersioningPolicy_ENABLED:
		out.Status = "Enabled"
	case storagepb.VersioningPolicy_SUSPENDED:
		out.Status = "Suspended"
	case storagepb.VersioningPolicy_VERSIONING_POLICY_UNSPECIFIED, storagepb.VersioningPolicy_DISABLED:
	}
	return marshalConfig(&out)
}
