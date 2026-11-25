package domain_test

import (
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestS3GateAuthorize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	active := f.create(t, "active-bucket", nil)
	gone := f.create(t, "gone-bucket", nil)
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: gone.GetMetadata().GetId()}); err != nil {
		t.Fatal(err)
	}
	checker := fakeChecker{allowed: map[authz.Permission]bool{domain.PermObjectGet: true}}
	keys := fakeKeys{key: peer.AccessKey{AccessKeyID: "NMK1", Secret: "s", PrincipalID: "serviceaccount-1"}}
	tests := []struct {
		name        string
		mode        authz.Mode
		bucket      string
		perm        authz.Permission
		wantKind    errs.Kind
		wantAllowed bool
	}{
		{"enforce allowed", authz.ModeEnforce, "active-bucket", domain.PermObjectGet, errs.KindUnknown, true},
		{"enforce denied", authz.ModeEnforce, "active-bucket", domain.PermObjectWrite, errs.KindPermissionDenied, false},
		{"audit denied is reported, not enforced", authz.ModeAudit, "active-bucket", domain.PermObjectWrite, errs.KindUnknown, false},
		{"off allows", authz.ModeOff, "active-bucket", domain.PermObjectDelete, errs.KindUnknown, true},
		{"unknown bucket", authz.ModeEnforce, "no-such-bucket", domain.PermObjectGet, errs.KindNotFound, false},
		{"soft-deleted bucket", authz.ModeOff, "gone-bucket", domain.PermObjectGet, errs.KindNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, err := domain.NewS3Gate(f.buckets, keys, checker, tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			acc, err := g.Authorize(t.Context(), "serviceaccount-1", tt.bucket, tt.perm)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Authorize kind = %v, want %v (err %v)", got, tt.wantKind, err)
			}
			if err == nil && (acc.Decision.Allowed != tt.wantAllowed || acc.Bucket.GetMetadata().GetId() != active.GetMetadata().GetId()) {
				t.Fatalf("Authorize = %+v, want allowed=%v on %s", acc.Decision, tt.wantAllowed, active.GetMetadata().GetId())
			}
		})
	}
}

func TestS3GateKeysAndListing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "listed-bucket", nil)
	keys := fakeKeys{key: peer.AccessKey{AccessKeyID: "NMK1", Secret: "s", PrincipalID: "serviceaccount-1"}}
	deny, err := domain.NewS3Gate(f.buckets, keys, fakeChecker{}, authz.ModeEnforce)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := deny.ResolveKey(t.Context(), "NMK1"); err != nil || k.PrincipalID != "serviceaccount-1" {
		t.Fatalf("ResolveKey = %+v, %v", k, err)
	}
	if _, err := deny.ResolveKey(t.Context(), "NMK2"); !errs.IsKind(err, errs.KindUnauthenticated) {
		t.Fatalf("ResolveKey unknown: %v, want Unauthenticated", err)
	}
	if got, err := deny.VisibleBuckets(t.Context(), "serviceaccount-1"); err != nil || len(got) != 0 {
		t.Fatalf("VisibleBuckets without list permission = %v, %v; want none", got, err)
	}
	allow, _ := domain.NewS3Gate(f.buckets, keys, fakeChecker{allowed: map[authz.Permission]bool{domain.PermBucketList: true}}, authz.ModeEnforce)
	got, err := allow.VisibleBuckets(t.Context(), "serviceaccount-1")
	if err != nil || len(got) != 1 || got[0].GetMetadata().GetName() != "listed-bucket" {
		t.Fatalf("VisibleBuckets = %v, %v", got, err)
	}
	if _, err := domain.NewS3Gate(nil, keys, fakeChecker{}, authz.ModeOff); err == nil {
		t.Error("NewS3Gate(nil repo) succeeded")
	}
}
