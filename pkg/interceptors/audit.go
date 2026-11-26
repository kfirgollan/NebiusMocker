package interceptors

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	userAgentHeader   = "user-agent"
	idempotencyHeader = "x-idempotency-key"
)

// Hierarchy resolves a resource's parent ("" for a root). *authz.Engine
// implements it.
type Hierarchy = audit.Hierarchy

// AuditConfig configures the Audit interceptor.
type AuditConfig struct {
	Recorder  *audit.Recorder
	Hierarchy Hierarchy
	// Rules classify methods (by their permission verb) and locate the
	// target resource of a request.
	Rules *authz.Rules
	// Reads also records Get/List calls. Off by default: Nebius audits
	// control-plane changes.
	Reads bool
	// DefaultTenant owns events whose resource hierarchy cannot be resolved.
	DefaultTenant string
	// States reads resource.state: previous before a mutating call, current
	// after it. Nil leaves resource.state empty.
	States audit.StateReader
}

// Audit records one audit event per mutating RPC and per authorization
// denial, after the call completes. It must run after Authenticate (it reads
// the principal) and outside Authorize (it sees denials). Recording is
// synchronous, bounded and never changes the RPC's result.
func Audit(cfg AuditConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if isExempt(info.FullMethod) {
			return next(ctx, req)
		}
		read := isRead(cfg.Rules, info.FullMethod)
		var target subject
		var previous *structpb.Struct
		if !read || cfg.Reads {
			target = resolveSubject(ctx, cfg, info.FullMethod, req)
		}
		if !read {
			previous = readState(ctx, cfg, info.FullMethod, target.id)
		}
		resp, err := next(ctx, req)
		denied := reqctx.InfoFrom(ctx) != nil && reqctx.InfoFrom(ctx).Denied
		if read && !cfg.Reads && !denied {
			return resp, err
		}
		if read && !cfg.Reads {
			target = resolveSubject(ctx, cfg, info.FullMethod, req)
		}
		ev, runningOp := buildEvent(ctx, cfg, call{method: info.FullMethod, req: req, resp: resp, err: err, target: target})
		if !read && err == nil && !denied {
			setState(ctx, cfg, ev, info.FullMethod, previous, runningOp)
		}
		cfg.Recorder.Record(ctx, ev, runningOp)
		return resp, err
	}
}

// readState reads the state of resource id through the service of method,
// or nil when there is no reader, no ID or nothing to read.
func readState(ctx context.Context, cfg AuditConfig, method, id string) *structpb.Struct {
	if cfg.States == nil || id == "" {
		return nil
	}
	st, _ := cfg.States.ResourceState(ctx, method, id)
	return st
}

// setState fills resource.state of a successful mutating call, with Nebius
// semantics: previous is the resource before the call (absent for a Create),
// current the resource after it (absent once it is deleted). A running
// operation gets its current state when it finishes (audit.Recorder).
func setState(ctx context.Context, cfg AuditConfig, ev *auditv2pb.AuditEvent, method string, previous *structpb.Struct, runningOp string) {
	var current *structpb.Struct
	if runningOp == "" && ev.GetStatus() == auditv2pb.Status_DONE {
		current = readState(ctx, cfg, method, ev.GetResource().GetMetadata().GetId())
	}
	if previous == nil && current == nil {
		return
	}
	ev.Resource.State = &auditv2pb.ResourceState{Previous: previous, Current: current}
}

// call is one completed RPC.
type call struct {
	method string
	req    any
	resp   any
	err    error
	target subject
}

// subject is what an event is about, resolved before the handler runs
// (a Delete removes the resource and with it its ancestry).
type subject struct {
	id        string   // resource ID, when the request names it
	name      string   // from request metadata
	hierarchy []string // ancestors of the resource, root first
}

type (
	idGetter       interface{ GetId() string }
	parentGetter   interface{ GetParentId() string }
	metadataGetter interface {
		GetMetadata() *commonpb.ResourceMetadata
	}
)

// resolveSubject finds the resource a request is about and its ancestry.
func resolveSubject(ctx context.Context, cfg AuditConfig, method string, req any) subject {
	var s subject
	var parent string
	if g, ok := req.(metadataGetter); ok {
		s.id, s.name, parent = g.GetMetadata().GetId(), g.GetMetadata().GetName(), g.GetMetadata().GetParentId()
	}
	if g, ok := req.(idGetter); ok && s.id == "" {
		s.id = g.GetId()
	}
	if g, ok := req.(parentGetter); ok && parent == "" {
		parent = g.GetParentId()
	}
	if rule, ok := cfg.Rules.Lookup(method); ok && rule.Target != nil && s.id == "" && parent == "" {
		// The authz target names the resource of custom verbs that carry
		// their own field (KMS Encrypt's key_id), or a container.
		if t, err := rule.Target(ctx, req); err == nil && isContainer(t) {
			parent = t
		} else if err == nil {
			s.id = t
		}
	}
	switch {
	case s.id != "":
		s.hierarchy = ancestors(ctx, cfg.Hierarchy, s.id)
	case parent != "":
		s.hierarchy = append(ancestors(ctx, cfg.Hierarchy, parent), parent)
	}
	if len(s.hierarchy) == 0 && !strings.HasPrefix(s.id, "tenant-") && cfg.DefaultTenant != "" {
		s.hierarchy = []string{cfg.DefaultTenant}
	}
	return s
}

// isContainer reports whether id is a tenant or project, which hold
// resources rather than being the subject of a call.
func isContainer(id string) bool {
	return strings.HasPrefix(id, "tenant-") || strings.HasPrefix(id, "project-")
}

// ancestors returns the ancestors of id, root first; it stops at the first
// unresolvable link.
func ancestors(ctx context.Context, h Hierarchy, id string) []string {
	return audit.Ancestry(ctx, h, id)
}

// buildEvent maps a completed call to an event. runningOp is the ID of an
// operation that is still running, whose completion is recorded later.
func buildEvent(ctx context.Context, cfg AuditConfig, c call) (ev *auditv2pb.AuditEvent, runningOp string) {
	m := parseMethod(c.method)
	resourceID := c.target.id
	op, _ := c.resp.(*commonpb.Operation)
	if op.GetResourceId() != "" {
		resourceID = op.GetResourceId()
	}
	if g, ok := c.resp.(metadataGetter); ok && resourceID == "" && op == nil {
		resourceID = g.GetMetadata().GetId()
	}
	resType := m.resourceType
	if kind, _, ok := strings.Cut(resourceID, "-"); ok {
		resType = kind
	}
	ev = &auditv2pb.AuditEvent{
		Source:         strings.TrimPrefix(c.method, "/"),
		Type:           "ai.nebius." + m.area + "." + resType + "." + strings.ToLower(m.action),
		Service:        &auditv2pb.Service{Name: m.serviceName},
		Action:         m.action,
		Authentication: authentication(ctx),
		Resource:       resource(resourceID, c.target, resType),
		Request:        request(ctx, c.req),
		Response:       response(c.resp, c.err),
	}
	ev.Status, runningOp = eventStatus(ev.GetResponse(), op, c.err)
	if op != nil && runningOp == "" {
		cfg.Recorder.Forget(op.GetId())
	}
	return ev, runningOp
}

func eventStatus(resp *auditv2pb.Response, op *commonpb.Operation, err error) (st auditv2pb.Status, runningOp string) {
	switch {
	case err != nil:
		return auditv2pb.Status_ERROR, ""
	case op == nil:
		return auditv2pb.Status_DONE, ""
	case op.GetFinishedAt() == nil:
		return auditv2pb.Status_STARTED, op.GetId()
	case op.GetStatus().GetCode() != int32(code.Code_OK):
		resp.StatusCode = code.Code(op.GetStatus().GetCode())
		resp.ErrorMessage = op.GetStatus().GetMessage()
		return auditv2pb.Status_ERROR, ""
	default:
		return auditv2pb.Status_DONE, ""
	}
}

func authentication(ctx context.Context) *auditv2pb.Authentication {
	p := reqctx.PrincipalFrom(ctx)
	if p.Anonymous() {
		return &auditv2pb.Authentication{Authenticated: false}
	}
	a := &auditv2pb.Authentication{
		Authenticated:      true,
		Subject:            audit.SubjectOf(p.ID),
		AuthenticationType: auditv2pb.AuthenticationType_ACCESS_TOKEN,
	}
	if tok, ok := bearerToken(ctx); ok {
		a.Credential = &auditv2pb.Authentication_TokenCredential{
			TokenCredential: &auditv2pb.AccessToken{MaskedToken: audit.MaskToken(tok)},
		}
	}
	return a
}

func resource(id string, s subject, resType string) *auditv2pb.Resource {
	r := &auditv2pb.Resource{Metadata: &auditv2pb.ResourceMetadata{Id: id, Name: s.name, Type: resType}}
	for _, h := range s.hierarchy {
		kind, _, _ := strings.Cut(h, "-")
		r.Hierarchy = append(r.Hierarchy, &auditv2pb.ResourceMetadata{Id: h, Type: kind})
	}
	return r
}

func request(ctx context.Context, req any) *auditv2pb.Request {
	md, _ := metadata.FromIncomingContext(ctx)
	r := &auditv2pb.Request{
		UserAgent:     first(md.Get(userAgentHeader)),
		RequestId:     reqctx.RequestID(ctx),
		TraceId:       reqctx.RequestID(ctx),
		IdempotencyId: first(md.Get(idempotencyHeader)),
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		r.ClientIp = hostOf(p.Addr.String())
	}
	if m, ok := req.(proto.Message); ok {
		r.Parameters = audit.ToStruct(audit.Redact(m))
	}
	return r
}

func response(resp any, err error) *auditv2pb.Response {
	st := status.Convert(err)
	r := &auditv2pb.Response{StatusCode: code.Code(st.Proto().GetCode()), ErrorMessage: st.Message()}
	if m, ok := resp.(proto.Message); ok && err == nil {
		r.Payload = audit.ToStruct(audit.Redact(m))
	}
	return r
}

// method is the audit view of a gRPC method name.
type method struct {
	area         string // "iam", "vpc", "msp.postgresql"
	serviceName  string // service.name: "IAM", "VIRTUAL_NETWORKS"
	action       string // "CREATE", "LIST_MEMBER_OF"
	resourceType string // fallback type when no resource ID is known: "group"
}

var versionSegment = regexp.MustCompile(`^v\d+((alpha|beta)\d*)?$`)

// serviceNames maps API areas to the service.name values Nebius documents
// (docs.nebius.com/audit-logs/services). Other areas use their upper-cased name.
var serviceNames = map[string]string{
	"vpc":            "VIRTUAL_NETWORKS",
	"mysterybox":     "MBOX",
	"msp.postgresql": "MSP_POSTGRESQL",
	"msp.mlflow":     "MSP_MLFLOW",
	"msp.serverless": "MSP_SERVERLESS",
}

// parseMethod splits "/nebius.iam.v1.GroupService/Create".
func parseMethod(full string) method {
	svc, name, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
	parts := strings.Split(strings.TrimPrefix(svc, "nebius."), ".")
	var area []string
	for _, p := range parts[:max(len(parts)-1, 0)] {
		if versionSegment.MatchString(p) {
			break
		}
		area = append(area, p)
	}
	m := method{area: strings.Join(area, "."), action: upperSnake(name)}
	m.serviceName = serviceNames[m.area]
	if m.serviceName == "" {
		m.serviceName = strings.ToUpper(strings.ReplaceAll(m.area, ".", "_"))
	}
	m.resourceType = strings.ToLower(strings.TrimSuffix(parts[len(parts)-1], "Service"))
	return m
}

// upperSnake turns "ListMemberOf" into "LIST_MEMBER_OF".
func upperSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// isRead classifies a method by the verb of its permission, falling back to
// its name for methods without a rule.
func isRead(rules *authz.Rules, full string) bool {
	if rule, ok := rules.Lookup(full); ok {
		v := rule.Permission.Verb()
		return v == authz.VerbGet || v == authz.VerbList
	}
	_, name, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
	return strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List")
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// hostOf strips the port from "ip:port" (and brackets from IPv6).
func hostOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}
