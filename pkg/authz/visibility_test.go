package authz_test

import (
	"context"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

func as(t *testing.T, principal string) context.Context {
	t.Helper()
	return reqctx.WithInfo(t.Context(), &reqctx.Info{Principal: reqctx.Principal{ID: principal}})
}

func TestVisibilityEnforce(t *testing.T) {
	t.Parallel()
	v := authz.NewVisibility(newEngine(), authz.ModeEnforce)
	tests := []struct {
		name, principal, target string
		want                    bool
	}{
		{"viewer on the tenant sees a child", "alice", "group-a", true},
		{"grant on the item itself", "bob", "group-a", true},
		{"grant below does not reach the parent", "bob", "tenant-t", false},
		{"outsider", "carol", "group-a", false},
		{"anonymous", "", "group-a", false},
		{"deleted item is hidden, not an error", "alice", "group-gone", false},
	}
	for _, tt := range tests {
		got, err := v.CanSee(as(t, tt.principal), "iam.groups.get", tt.target)
		if err != nil {
			t.Fatalf("%s: CanSee: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: CanSee = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestVisibilityAuditAndOffHideNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []authz.Mode{authz.ModeAudit, authz.ModeOff} {
		v := authz.NewVisibility(newEngine(), mode)
		got, err := v.CanSee(as(t, "carol"), "iam.groups.get", "group-a")
		if err != nil || !got {
			t.Errorf("mode %d: CanSee = %v, %v; want true", mode, got, err)
		}
	}
}
