package interceptors

import (
	"context"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// AuditAuthnFailures records one audit event for every request whose
// credentials Authenticate rejects: an unknown, malformed, expired or revoked
// bearer token, or a missing one when authentication is required. Every
// method qualifies, reads included: the point is to see bad credentials. It
// must run outside Authenticate.
//
// The caller is unauthenticated, so the event omits request.parameters and
// clips what it does carry (see audit.AuthnFailureEvent); it lands in the
// same bounded log as every other event.
func AuditAuthnFailures(cfg AuditConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if isExempt(info.FullMethod) {
			return next(ctx, req)
		}
		rinfo := reqctx.InfoFrom(ctx)
		if rinfo == nil {
			rinfo = &reqctx.Info{}
			ctx = reqctx.WithInfo(ctx, rinfo)
		}
		resp, err := next(ctx, req)
		if rinfo.AuthnFailed {
			cfg.Recorder.Record(ctx, audit.AuthnFailureEvent(edgeAuthnFailure(ctx, cfg, info.FullMethod, req, err)), "")
		}
		return resp, err
	}
}

// edgeAuthnFailure describes a request Authenticate rejected. The resource
// and its hierarchy are resolved as for any event, so the tenant's List finds
// it; the claimed subject comes from the claims of a mocker-issued token.
func edgeAuthnFailure(ctx context.Context, cfg AuditConfig, method string, req any, err error) audit.AuthnFailure {
	m := parseMethod(method)
	target := resolveSubject(ctx, cfg, method, req)
	token, _ := bearerToken(ctx)
	st := status.Convert(err)
	f := audit.AuthnFailure{
		Source: strings.TrimPrefix(method, "/"), Area: m.area, ServiceName: m.serviceName, Action: m.action,
		ResourceID: target.id, ResourceType: m.resourceType, Hierarchy: target.hierarchy,
		Token:     token,
		RequestID: reqctx.RequestID(ctx),
		Code:      code.Code(st.Proto().GetCode()),
		Message:   st.Message(),
	}
	if c, ok := auth.UnverifiedClaims(token); ok {
		f.Subject = c.Principal
		f.Parameters = map[string]string{"token_id": c.TokenID, "token_key_id": c.KeyID}
	}
	md, _ := metadata.FromIncomingContext(ctx)
	f.UserAgent = first(md.Get(userAgentHeader))
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		f.ClientIP = hostOf(p.Addr.String())
	}
	return f
}
