// Package interceptors implements the cross-cutting gRPC request pipeline:
// request IDs, panic recovery, logging, authentication and protovalidate.
// See docs/architecture/04-request-pipeline.md for the order and contracts.
package interceptors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

const (
	requestIDHeader = "x-request-id"
	authzHeader     = "authorization"
	bearerPrefix    = "Bearer "
)

// exemptFromAuthn lists method prefixes that never require credentials.
var exemptFromAuthn = []string{
	"/grpc.health.v1.Health/",
	"/grpc.reflection.",
	"/nebius.iam.v1.TokenExchangeService/",
}

// RequestID assigns a request ID, stores it in the context and returns it in
// the x-request-id response header.
func RequestID() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		info := &reqctx.Info{RequestID: newRequestID()}
		ctx = reqctx.WithInfo(ctx, info)
		_ = grpc.SetHeader(ctx, metadata.Pairs(requestIDHeader, info.RequestID)) // best-effort: header may already be sent
		return next(ctx, req)
	}
}

// Recover turns a handler panic into an INTERNAL error and logs the stack.
func Recover(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "handler panic", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = serviceerror.FromDomain(ctx, errs.Internal("panic"))
			}
		}()
		return next(ctx, req)
	}
}

// Log logs every request with its outcome and latency.
func Log(log *slog.Logger, clk clock.Clock) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		start := clk.Now()
		resp, err := next(ctx, req)
		code := status.Code(err)
		attrs := []any{
			"method", info.FullMethod,
			"code", code.String(),
			"duration", clk.Now().Sub(start).Round(time.Microsecond),
			"principal", reqctx.PrincipalFrom(ctx).ID,
			"request_id", reqctx.RequestID(ctx),
		}
		if err != nil {
			attrs = append(attrs, "error", status.Convert(err).Message())
		}
		log.InfoContext(ctx, "rpc", attrs...)
		return resp, err
	}
}

// TokenAuthenticator resolves a bearer token to a principal ID (satisfied by
// *auth.Authenticator: static dev tokens and mocker-issued access tokens).
type TokenAuthenticator interface {
	Authenticate(token string) (string, error)
}

// Authenticate resolves the bearer token to a principal. Unknown, expired and
// revoked tokens are always rejected; missing tokens are rejected only in
// AuthnRequired mode.
func Authenticate(mode config.AuthnMode, tokens TokenAuthenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if isExempt(info.FullMethod) {
			return next(ctx, req)
		}
		token, present := bearerToken(ctx)
		if !present {
			if mode == config.AuthnRequired {
				return nil, serviceerror.FromDomain(ctx, errs.Unauthenticated("missing bearer token"))
			}
			return next(ctx, req) // anonymous
		}
		principal, err := tokens.Authenticate(token)
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, errs.Unauthenticated("invalid bearer token: "+err.Error()))
		}
		rinfo := reqctx.InfoFrom(ctx)
		if rinfo == nil {
			rinfo = &reqctx.Info{}
			ctx = reqctx.WithInfo(ctx, rinfo)
		}
		rinfo.Principal = reqctx.Principal{ID: principal}
		return next(ctx, req)
	}
}

// Validate applies the buf.validate rules embedded in the Nebius protos.
//
// Update RPCs are partial: the official CLI's default "patch" mode sends only
// the changed fields (e.g. metadata.id + spec.description) with an empty
// X-ResetMask, meaning "leave everything else as it is". Presence rules
// ("required", required oneofs) therefore do not apply to Update requests;
// the module checks the merged resource instead. Value rules (lengths,
// patterns, ranges) still apply.
func Validate() (grpc.UnaryServerInterceptor, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("protovalidate: %w", err)
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		msg, ok := req.(proto.Message)
		if !ok {
			return next(ctx, req)
		}
		if err := v.Validate(msg); err != nil {
			if bad := toBadRequest(err, ruleFilter(info.FullMethod)); bad != nil {
				return nil, serviceerror.FromDomain(ctx, bad)
			}
		}
		return next(ctx, req)
	}, nil
}

// ruleFilter returns which violations to ignore for method: presence rules
// for standard (partial) Update RPCs, nothing otherwise.
func ruleFilter(method string) func(ruleID string) bool {
	if strings.HasSuffix(method, "/Update") {
		return isPresenceRule
	}
	return func(string) bool { return false }
}

// toBadRequest converts a validation error, dropping violations whose rule ID
// skip matches; it returns nil if none remain.
func toBadRequest(err error, skip func(ruleID string) bool) error {
	ve, ok := err.(*protovalidate.ValidationError) //nolint:errorlint // protovalidate returns this type unwrapped
	if !ok {
		return errs.BadRequest(errs.Violation{Message: err.Error()})
	}
	violations := make([]errs.Violation, 0, len(ve.Violations))
	for _, v := range ve.Violations {
		if skip(v.Proto.GetRuleId()) {
			continue
		}
		violations = append(violations, errs.Violation{
			Field:   protovalidate.FieldPathString(v.Proto.GetField()),
			Message: v.Proto.GetMessage(),
		})
	}
	if len(violations) == 0 {
		return nil
	}
	return errs.BadRequest(violations...)
}

// isPresenceRule matches protovalidate's field "required" and oneof
// "required" rule IDs.
func isPresenceRule(ruleID string) bool {
	return ruleID == "required" || strings.HasSuffix(ruleID, ".required")
}

func bearerToken(ctx context.Context) (string, bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(authzHeader)
	if len(values) == 0 {
		return "", false
	}
	token, ok := strings.CutPrefix(values[0], bearerPrefix)
	if !ok {
		return values[0], true // present but malformed → rejected as unknown
	}
	return strings.TrimSpace(token), true
}

func isExempt(method string) bool {
	for _, p := range exemptFromAuthn {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}
