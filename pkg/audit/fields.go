// Package audit is the shared core of NebiusMocker's audit log: the event
// index the List filter runs on, the filter language, a bounded in-memory
// event log, payload redaction and the Recorder that turns RPCs into events.
//
// The gRPC side (which RPCs become events, reading metadata) lives in
// pkg/interceptors; the AuditEventService API lives in services/audit.
// See docs/services/audit.md.
package audit

import (
	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

// Fields are the event attributes the List filter can address, extracted once
// when an event is recorded so that queries never decode stored events.
type Fields struct {
	Action           string // "CREATE", "UPDATE", "DELETE", custom verbs upper-cased
	Type             string // "ai.nebius.<service>.<resource type>.<action>"
	Status           string // "STARTED", "DONE", "ERROR"
	Service          string // service.name, e.g. "IAM", "VIRTUAL_NETWORKS"
	SubjectName      string
	ServiceAccountID string
	TenantUserID     string
	MaskedToken      string
	StaticKeyID      string
	ResourceID       string
	ResourceName     string
	ResourceType     string
	HierarchyIDs     []string // tenant first
	HierarchyNames   []string // same order as HierarchyIDs
}

// FieldsOf extracts the filterable fields of ev.
func FieldsOf(ev *auditv2pb.AuditEvent) Fields {
	authn := ev.GetAuthentication()
	res := ev.GetResource()
	f := Fields{
		Action:           ev.GetAction(),
		Type:             ev.GetType(),
		Status:           statusName(ev.GetStatus()),
		Service:          ev.GetService().GetName(),
		SubjectName:      authn.GetSubject().GetName(),
		ServiceAccountID: authn.GetSubject().GetServiceAccountId(),
		TenantUserID:     authn.GetSubject().GetTenantUserId(),
		MaskedToken:      authn.GetTokenCredential().GetMaskedToken(),
		StaticKeyID:      authn.GetStaticKeyCredential().GetId(),
		ResourceID:       res.GetMetadata().GetId(),
		ResourceName:     res.GetMetadata().GetName(),
		ResourceType:     res.GetMetadata().GetType(),
	}
	for _, h := range res.GetHierarchy() {
		f.HierarchyIDs = append(f.HierarchyIDs, h.GetId())
		f.HierarchyNames = append(f.HierarchyNames, h.GetName())
	}
	return f
}

func statusName(s auditv2pb.Status) string {
	if s == auditv2pb.Status_RESPONSE_STATUS_UNSPECIFIED {
		return ""
	}
	return s.String()
}

// field is one filterable path: a single value or, for hierarchy paths, many.
type field struct {
	one  func(*Fields) string
	many func(*Fields) []string
}

// fields maps every filter path documented in audit_event_service.proto to
// its accessor.
var fields = map[string]field{
	"action":                      {one: func(f *Fields) string { return f.Action }},
	"type":                        {one: func(f *Fields) string { return f.Type }},
	"status":                      {one: func(f *Fields) string { return f.Status }},
	"service.name":                {one: func(f *Fields) string { return f.Service }},
	"authentication.subject.name": {one: func(f *Fields) string { return f.SubjectName }},
	"authentication.subject.service_account_id":    {one: func(f *Fields) string { return f.ServiceAccountID }},
	"authentication.subject.tenant_user_id":        {one: func(f *Fields) string { return f.TenantUserID }},
	"authentication.token_credential.masked_token": {one: func(f *Fields) string { return f.MaskedToken }},
	"authentication.static_key_credential.id":      {one: func(f *Fields) string { return f.StaticKeyID }},
	"resource.metadata.id":                         {one: func(f *Fields) string { return f.ResourceID }},
	"resource.metadata.name":                       {one: func(f *Fields) string { return f.ResourceName }},
	"resource.name":                                {one: func(f *Fields) string { return f.ResourceName }}, // alias used in the Nebius docs
	"resource.metadata.type":                       {one: func(f *Fields) string { return f.ResourceType }},
	"resource.hierarchy.id":                        {many: func(f *Fields) []string { return f.HierarchyIDs }},
	"resource.hierarchy.name":                      {many: func(f *Fields) []string { return f.HierarchyNames }},
}
