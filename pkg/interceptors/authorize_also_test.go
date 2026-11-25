package interceptors_test

import (
	"context"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/interceptors"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// staticBindings grants each principal one role on every resource.
type staticBindings map[string]string

func (b staticBindings) RolesOn(_ context.Context, principalID, _ string) ([]string, error) {
	if r, ok := b[principalID]; ok {
		return []string{r}, nil
	}
	return nil, nil
}

type secretReq struct{ secret bool }

func TestAuthorizeAlsoPermission(t *testing.T) {
	t.Parallel()
	engine := authz.NewEngine(authz.DefaultCatalog())
	engine.RegisterKind("thing", func(context.Context, string) (string, error) { return "", nil })
	engine.SetBindings(staticBindings{"viewer-1": authz.RoleViewer, "admin-1": authz.RoleAdmin})
	rules := authz.NewRules()
	const method = "/test.Things/Get"
	rule := authz.Rule{
		Permission: authz.Perm("test", "things", authz.VerbGet),
		Target:     func(context.Context, any) (string, error) { return "thing-1", nil },
		Also: func(req any) authz.Permission {
			if r, ok := req.(secretReq); ok && r.secret {
				return authz.Perm("test", "things", "getSensitiveData")
			}
			return ""
		},
	}
	rules.Add(method, rule)
	intercept := interceptors.Authorize(engine, rules, authz.ModeEnforce, slog.New(slog.DiscardHandler))
	ok := func(context.Context, any) (any, error) { return "ok", nil }

	tests := []struct {
		name      string
		principal string
		req       secretReq
		want      codes.Code
	}{
		{"viewer plain read", "viewer-1", secretReq{}, codes.OK},
		{"viewer secret read", "viewer-1", secretReq{secret: true}, codes.PermissionDenied},
		{"admin secret read", "admin-1", secretReq{secret: true}, codes.OK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := reqctx.WithInfo(t.Context(), &reqctx.Info{Principal: reqctx.Principal{ID: tc.principal}})
			_, err := intercept(ctx, tc.req, &grpc.UnaryServerInfo{FullMethod: method}, ok)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}
