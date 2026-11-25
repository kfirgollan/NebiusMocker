package domain_test

import (
	"context"
	"testing"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	saID    = "serviceaccount-1"
	groupID = "group-e00readers00000000001"
)

// fakeMembers knows that saID is in groupID.
type fakeMembers struct{}

func (fakeMembers) IsMember(_ context.Context, principal, group string) (bool, error) {
	return principal == saID && group == groupID, nil
}

// saHierarchy places saID in projectID.
type saHierarchy struct{}

func (saHierarchy) Parent(_ context.Context, id string) (string, error) {
	switch id {
	case saID:
		return projectID, nil
	case projectID:
		return "tenant-e00test0000000000001", nil
	}
	return "", errs.NotFound(id)
}

func newGate(t *testing.T, f *fixture, checker fakeChecker, mode authz.Mode) *domain.S3Gate {
	t.Helper()
	g, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: f.buckets, Keys: fakeKeys{key: peer.AccessKey{AccessKeyID: "NMK1", Secret: "s", PrincipalID: saID}},
		Checker: checker, Hierarchy: saHierarchy{}, Members: func() domain.GroupMembers { return fakeMembers{} }, Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestS3GateAuthorize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	active := f.create(t, "active-bucket", nil)
	gone := f.create(t, "gone-bucket", nil)
	if _, err := f.svc.Delete(t.Context(), domain.DeleteBucketCmd{ID: gone.GetMetadata().GetId()}); err != nil {
		t.Fatal(err)
	}
	checker := fakeChecker{allowed: map[authz.Permission]bool{domain.PermObjectGet: true}}
	tests := []struct {
		name        string
		mode        authz.Mode
		bucket      string
		action      domain.S3Action
		wantKind    errs.Kind
		wantAllowed bool
	}{
		{"enforce allowed", authz.ModeEnforce, "active-bucket", domain.ActGetObject, errs.KindUnknown, true},
		{"enforce denied", authz.ModeEnforce, "active-bucket", domain.ActPutObject, errs.KindPermissionDenied, false},
		{"audit denied is reported, not enforced", authz.ModeAudit, "active-bucket", domain.ActPutObject, errs.KindUnknown, false},
		{"off allows", authz.ModeOff, "active-bucket", domain.ActDeleteObject, errs.KindUnknown, true},
		{"unknown bucket", authz.ModeEnforce, "no-such-bucket", domain.ActGetObject, errs.KindNotFound, false},
		{"soft-deleted bucket", authz.ModeOff, "gone-bucket", domain.ActGetObject, errs.KindNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newGate(t, f, checker, tt.mode)
			acc, err := g.Authorize(t.Context(), domain.S3Request{Principal: saID, Bucket: tt.bucket, Action: tt.action, Target: "k"})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Authorize kind = %v, want %v (err %v)", got, tt.wantKind, err)
			}
			if err == nil && (acc.Decision.Allowed != tt.wantAllowed || acc.Bucket.GetMetadata().GetId() != active.GetMetadata().GetId()) {
				t.Fatalf("Authorize = %+v, want allowed=%v on %s", acc.Decision, tt.wantAllowed, active.GetMetadata().GetId())
			}
		})
	}
}

func TestS3GateBucketPolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "policy-bucket", &storagepb.BucketSpec{BucketPolicy: &storagepb.BucketPolicy{Rules: []*storagepb.BucketPolicy_Rule{
		{Paths: []string{"public/*"}, Roles: []string{"storage.viewer"}, Subject: &storagepb.BucketPolicy_Rule_Anonymous{Anonymous: &storagepb.BucketPolicy_Rule_AnonymousAccess{}}},
		{Paths: []string{"team?/*", "exact.txt"}, Roles: []string{"storage.object-editor"}, Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: groupID}},
	}}})
	g := newGate(t, f, fakeChecker{}, authz.ModeEnforce) // no IAM grants at all
	tests := []struct {
		name      string
		principal string
		action    domain.S3Action
		target    string
		allowed   bool
	}{
		{"anonymous get under public/", "", domain.ActGetObject, "public/a.txt", true},
		{"anonymous list of public/", "", domain.ActListObjects, "public/", true},
		{"anonymous list of whole bucket", "", domain.ActListObjects, "", false},
		{"anonymous get outside public/", "", domain.ActGetObject, "private/a.txt", false},
		{"anonymous write denied (read-only role)", "", domain.ActPutObject, "public/a.txt", false},
		{"group member writes team1/", saID, domain.ActPutObject, "team1/x", true},
		{"group member deletes exact key", saID, domain.ActDeleteObject, "exact.txt", true},
		{"? matches exactly one character", saID, domain.ActPutObject, "team10/x", false},
		{"exact path is not a prefix", saID, domain.ActGetObject, "exact.txt.bak", false},
		{"object-editor has no bucket config", saID, domain.ActPutBucketCors, "", false},
		{"non-member gets nothing from group rule", "serviceaccount-2", domain.ActPutObject, "team1/x", false},
		{"signed requests also get anonymous grants", "serviceaccount-2", domain.ActGetObject, "public/a", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			acc, err := g.Authorize(t.Context(), domain.S3Request{Principal: tt.principal, Bucket: "policy-bucket", Action: tt.action, Target: tt.target})
			if tt.allowed && (err != nil || !acc.Decision.Allowed) {
				t.Fatalf("Authorize = %+v, %v; want allowed", acc.Decision, err)
			}
			if !tt.allowed && !errs.IsKind(err, errs.KindPermissionDenied) {
				t.Fatalf("Authorize = %+v, %v; want PermissionDenied", acc.Decision, err)
			}
		})
	}
	// Anonymous requests are denied without a policy grant even when authz is off.
	off := newGate(t, f, fakeChecker{}, authz.ModeOff)
	if _, err := off.Authorize(t.Context(), domain.S3Request{Bucket: "policy-bucket", Action: domain.ActGetObject, Target: "private/x"}); !errs.IsKind(err, errs.KindPermissionDenied) {
		t.Fatalf("anonymous under authz off: %v, want PermissionDenied", err)
	}
}

func TestS3GateAuthorizeProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	g := newGate(t, f, fakeChecker{allowed: map[authz.Permission]bool{domain.PermBucketCreate: true}}, authz.ModeEnforce)
	if p, d, err := g.AuthorizeProject(t.Context(), saID, domain.PermBucketCreate); err != nil || p != projectID || !d.Allowed {
		t.Fatalf("AuthorizeProject = %q, %+v, %v", p, d, err)
	}
	if _, _, err := g.AuthorizeProject(t.Context(), saID, domain.PermBucketDelete); !errs.IsKind(err, errs.KindPermissionDenied) {
		t.Fatalf("AuthorizeProject without permission: %v", err)
	}
	if _, _, err := g.AuthorizeProject(t.Context(), "serviceaccount-unknown", domain.PermBucketCreate); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("AuthorizeProject unknown principal: %v", err)
	}
}

func TestS3GateKeysAndListing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.create(t, "listed-bucket", nil)
	deny := newGate(t, f, fakeChecker{}, authz.ModeEnforce)
	if k, err := deny.ResolveKey(t.Context(), "NMK1"); err != nil || k.PrincipalID != saID {
		t.Fatalf("ResolveKey = %+v, %v", k, err)
	}
	if _, err := deny.ResolveKey(t.Context(), "NMK2"); !errs.IsKind(err, errs.KindUnauthenticated) {
		t.Fatalf("ResolveKey unknown: %v, want Unauthenticated", err)
	}
	if got, err := deny.VisibleBuckets(t.Context(), saID); err != nil || len(got) != 0 {
		t.Fatalf("VisibleBuckets without list permission = %v, %v; want none", got, err)
	}
	allow := newGate(t, f, fakeChecker{allowed: map[authz.Permission]bool{domain.PermBucketList: true}}, authz.ModeEnforce)
	got, err := allow.VisibleBuckets(t.Context(), saID)
	if err != nil || len(got) != 1 || got[0].GetMetadata().GetName() != "listed-bucket" {
		t.Fatalf("VisibleBuckets = %v, %v", got, err)
	}
	if _, err := domain.NewS3Gate(domain.GateDeps{}); err == nil {
		t.Error("NewS3Gate(nil deps) succeeded")
	}
}
