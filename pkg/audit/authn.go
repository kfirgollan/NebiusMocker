package audit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/types/known/structpb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

// Bounds on what a failed authentication may put into an event. Everything
// an unauthenticated caller sends is untrusted, so it is clipped: a flood of
// refused requests costs at most the log's capacity times a few KB.
const (
	maxUntrusted   = 256  // IDs, subjects, parameter values, user agent
	maxMaskedToken = 2048 // a masked token (a JWT header and claims)
	maxMessage     = 1024 // the error message, which quotes presented values
	maxParameters  = 16
	maxAncestry    = 16
)

// AuthnFailure describes one refused authentication: a bearer token the
// gRPC edge rejected, or a token exchange that issued no token. It carries
// what the caller presented, unverified, and never a signature or secret:
// Token is masked (MaskToken) before it reaches the event.
type AuthnFailure struct {
	// Source, Area, ServiceName and Action name the call, as for every
	// event: "nebius.iam.v1.GroupService/Create", "iam", "IAM", "CREATE".
	Source      string
	Area        string
	ServiceName string
	Action      string

	// ResourceID is what the call was about: the request's resource for a
	// rejected bearer token, the presented credential (auth public key) or
	// the claimed service account for a refused exchange. ResourceType is
	// the type to use when ResourceID has no NID prefix.
	ResourceID   string
	ResourceType string
	// Hierarchy are the resource's ancestors, root first. When empty,
	// AuthnRecorder resolves it.
	Hierarchy []string

	// Token is the presented token, raw; the event carries MaskToken(Token).
	Token string
	// Subject is the principal the credential claims (unverified).
	Subject string
	// Parameters are non-secret request parameters, e.g. the JWT's kid,
	// iss and sub, the grant and token types.
	Parameters map[string]string

	ClientIP  string
	UserAgent string
	RequestID string

	// Code and Message are the refusal; Payload is further response detail
	// (e.g. the OAuth error code of the HTTP exchange).
	Code    code.Code
	Message string
	Payload map[string]string
}

// AuthnFailureEvent builds the CONTROL_PLANE event of f: status ERROR,
// authentication.authenticated false, the masked token and the claimed
// subject. Untrusted fields are clipped.
func AuthnFailureEvent(f AuthnFailure) *auditv2pb.AuditEvent {
	resID := clip(f.ResourceID, maxUntrusted)
	resType := f.ResourceType
	if kind, _, ok := strings.Cut(resID, "-"); ok && kind != "" {
		resType = kind
	}
	r := &auditv2pb.Resource{Metadata: &auditv2pb.ResourceMetadata{Id: resID, Type: resType}}
	for _, h := range f.Hierarchy {
		kind, _, _ := strings.Cut(h, "-")
		r.Hierarchy = append(r.Hierarchy, &auditv2pb.ResourceMetadata{Id: h, Type: kind})
	}
	return &auditv2pb.AuditEvent{
		Source:         f.Source,
		Type:           "ai.nebius." + f.Area + "." + resType + "." + strings.ToLower(f.Action),
		Service:        &auditv2pb.Service{Name: f.ServiceName},
		Action:         f.Action,
		Authentication: FailedAuthentication(f.Token, f.Subject),
		Resource:       r,
		Request: &auditv2pb.Request{
			ClientIp:   clip(f.ClientIP, maxUntrusted),
			UserAgent:  clip(f.UserAgent, maxUntrusted),
			RequestId:  f.RequestID,
			TraceId:    f.RequestID,
			Parameters: clippedStruct(f.Parameters),
		},
		Response: &auditv2pb.Response{
			StatusCode:   f.Code,
			ErrorMessage: clip(f.Message, maxMessage),
			Payload:      clippedStruct(f.Payload),
		},
		Status: auditv2pb.Status_ERROR,
	}
}

// FailedAuthentication is the authentication of a refused credential:
// authenticated false, the masked token (if any) and the principal it
// claims (if known).
func FailedAuthentication(token, claimed string) *auditv2pb.Authentication {
	a := &auditv2pb.Authentication{Authenticated: false}
	if token != "" {
		a.AuthenticationType = auditv2pb.AuthenticationType_ACCESS_TOKEN
		a.Credential = &auditv2pb.Authentication_TokenCredential{
			TokenCredential: &auditv2pb.AccessToken{MaskedToken: MaskToken(token)},
		}
	}
	if claimed = clip(claimed, maxUntrusted); claimed != "" {
		a.Subject = SubjectOf(claimed)
	}
	return a
}

// SubjectOf is the event subject of a principal ID: a service account or,
// otherwise, a tenant user.
func SubjectOf(principalID string) *auditv2pb.Subject {
	sub := &auditv2pb.Subject{Name: principalID}
	if strings.HasPrefix(principalID, "serviceaccount-") {
		sub.Id = &auditv2pb.Subject_ServiceAccountId{ServiceAccountId: principalID}
	} else {
		sub.Id = &auditv2pb.Subject_TenantUserId{TenantUserId: principalID}
	}
	return sub
}

// Hierarchy resolves a resource's parent ("" for a root). *authz.Engine
// implements it.
type Hierarchy interface {
	Parent(ctx context.Context, id string) (string, error)
}

// Ancestry returns the ancestors of id, root first; it stops at the first
// unresolvable link, so an unknown id has none.
func Ancestry(ctx context.Context, h Hierarchy, id string) []string {
	var up []string
	for range maxAncestry {
		p, err := h.Parent(ctx, id)
		if err != nil || p == "" {
			break
		}
		up = append(up, p)
		id = p
	}
	for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
		up[i], up[j] = up[j], up[i]
	}
	return up
}

// AuthnRecorder records failed authentications reported outside the gRPC
// interceptor chain (the token exchange). It resolves each event's hierarchy
// so that AuditEventService.List scoped to the tenant finds it.
type AuthnRecorder struct {
	rec    *Recorder
	h      Hierarchy
	tenant string
}

// NewAuthnRecorder returns an AuthnRecorder appending through rec.
// defaultTenant owns events about resources that cannot be resolved.
func NewAuthnRecorder(rec *Recorder, h Hierarchy, defaultTenant string) (*AuthnRecorder, error) {
	if rec == nil || h == nil {
		return nil, errors.New("audit: authn recorder needs a recorder and a hierarchy")
	}
	return &AuthnRecorder{rec: rec, h: h, tenant: defaultTenant}, nil
}

// RecordAuthnFailure records f. Without a Hierarchy, the event belongs to
// the ancestors of f.ResourceID, else to those of the claimed subject (and
// the subject itself), else to the default tenant: the presented key or
// account may not exist, as with a canary key that was never real.
func (a *AuthnRecorder) RecordAuthnFailure(ctx context.Context, f AuthnFailure) {
	if len(f.Hierarchy) == 0 {
		f.Hierarchy = a.hierarchy(ctx, clip(f.ResourceID, maxUntrusted), clip(f.Subject, maxUntrusted))
	}
	a.rec.Record(ctx, AuthnFailureEvent(f), "")
}

func (a *AuthnRecorder) hierarchy(ctx context.Context, resourceID, subject string) []string {
	if resourceID != "" {
		if up := Ancestry(ctx, a.h, resourceID); len(up) > 0 {
			return up
		}
	}
	if subject != "" && subject != resourceID {
		if up := Ancestry(ctx, a.h, subject); len(up) > 0 {
			return append(up, subject)
		}
	}
	if a.tenant == "" {
		return nil
	}
	return []string{a.tenant}
}

// clip truncates s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for s != "" && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// clippedStruct converts m to a Struct of clipped strings, keeping at most
// maxParameters keys (in sorted order); nil for an empty map.
func clippedStruct(m map[string]string) *structpb.Struct {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	s := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for _, k := range keys[:min(len(keys), maxParameters)] {
		if v := m[k]; v != "" {
			s.Fields[clip(k, maxUntrusted)] = structpb.NewStringValue(clip(v, maxUntrusted))
		}
	}
	return s
}
