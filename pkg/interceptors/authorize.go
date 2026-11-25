package interceptors

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

// Authorize checks every request against its method's authz.Rule.
//
//   - ModeOff: no checks.
//   - ModeAudit: decisions are evaluated and denials are logged, never enforced.
//   - ModeEnforce: denials fail the request. A denied read returns NOT_FOUND
//     (like real IAM, existence is not leaked); other denials return
//     PERMISSION_DENIED; anonymous callers get UNAUTHENTICATED.
//
// It must run after Authenticate (needs the principal) and Validate (targets
// are read from well-formed requests).
func Authorize(engine *authz.Engine, rules *authz.Rules, mode authz.Mode, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if mode == authz.ModeOff || isExempt(info.FullMethod) {
			return next(ctx, req)
		}
		if reason, err := authorize(ctx, engine, rules, info.FullMethod, req); err != nil {
			log.WarnContext(ctx, "authz denied",
				"method", info.FullMethod, "principal", reqctx.PrincipalFrom(ctx).ID,
				"enforced", mode == authz.ModeEnforce, "reason", reason)
			if mode == authz.ModeEnforce {
				if info := reqctx.InfoFrom(ctx); info != nil {
					info.Denied = true
				}
				return nil, serviceerror.FromDomain(ctx, err)
			}
		}
		return next(ctx, req)
	}
}

// authorize returns a nil error when allowed. Otherwise it returns the error to
// send to the client and the real reason, which is only logged.
func authorize(ctx context.Context, engine *authz.Engine, rules *authz.Rules, method string, req any) (reason string, err error) {
	rule, ok := rules.Lookup(method)
	if !ok {
		reason = "no authorization rule for " + method
		return reason, &errs.Error{Kind: errs.KindPermissionDenied, Message: reason}
	}
	principal := reqctx.PrincipalFrom(ctx)
	if principal.Anonymous() {
		return "anonymous caller", errs.Unauthenticated("authorization requires credentials")
	}
	if rule.AllowIf != nil && rule.AllowIf(ctx, principal.ID, req) {
		return "", nil
	}
	target, terr := rule.Target(ctx, req)
	if terr != nil {
		return "cannot resolve target: " + terr.Error(), terr
	}
	d, cerr := engine.Check(ctx, principal.ID, rule.Permission, target)
	if cerr != nil {
		return "cannot evaluate: " + cerr.Error(), cerr
	}
	if d.Allowed {
		return checkAlso(ctx, engine, rule, principal.ID, target, req)
	}
	reason = "permission " + string(rule.Permission) + " denied on " + target + ": " + d.Reason
	if rule.Permission.Verb() == authz.VerbGet {
		return reason, errs.NotFound(target) // do not leak existence
	}
	return reason, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: target, Message: reason}
}

// checkAlso evaluates the rule's request-dependent extra permission. The
// caller can already read the target, so a denial is PERMISSION_DENIED.
func checkAlso(ctx context.Context, engine *authz.Engine, rule authz.Rule, principalID, target string, req any) (reason string, err error) {
	if rule.Also == nil {
		return "", nil
	}
	perm := rule.Also(req)
	if perm == "" {
		return "", nil
	}
	d, cerr := engine.Check(ctx, principalID, perm, target)
	if cerr != nil {
		return "cannot evaluate: " + cerr.Error(), cerr
	}
	if d.Allowed {
		return "", nil
	}
	reason = "permission " + string(perm) + " denied on " + target + ": " + d.Reason
	return reason, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: target, Message: reason}
}
