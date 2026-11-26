package domain_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const testCluster = "mk8scluster-e00abc"

type fakeTokens map[string]string

func (f fakeTokens) Authenticate(token string) (string, error) {
	if p, ok := f[token]; ok {
		return p, nil
	}
	return "", errors.New("unknown token")
}

// fakePerms allows (principal, permission) pairs on testCluster; any other
// target is not found.
type fakePerms struct {
	allow map[string][]authz.Permission
	err   error
}

func (f fakePerms) Check(_ context.Context, principal string, perm authz.Permission, target string) (authz.Decision, error) {
	if f.err != nil {
		return authz.Decision{}, f.err
	}
	if target != testCluster {
		return authz.Decision{}, errs.NotFound(target)
	}
	return authz.Decision{Allowed: slices.Contains(f.allow[principal], perm), Permission: perm, Target: target}, nil
}

func TestK8sReviewerReview(t *testing.T) {
	viewerPerms := []authz.Permission{authz.Perm("mk8s", "clusters", "get")}
	editorPerms := append([]authz.Permission{authz.Perm("mk8s", "clusters", "update")}, viewerPerms...)
	adminPerms := append([]authz.Permission{authz.Perm("iam", "accessPermits", "create")}, editorPerms...)
	perms := fakePerms{allow: map[string][]authz.Permission{
		"viewer-1": viewerPerms, "editor-1": editorPerms, "admin-1": adminPerms,
	}}
	tokens := fakeTokens{"tv": "viewer-1", "te": "editor-1", "ta": "admin-1", "tn": "nobody-1"}

	tests := []struct {
		name    string
		mode    authz.Mode
		cluster string
		token   string
		perms   fakePerms
		want    []string
		wantErr bool
		kind    errs.Kind
	}{
		{name: "viewer", mode: authz.ModeEnforce, cluster: testCluster, token: "tv", perms: perms, want: []string{domain.KubeGroupViewer}},
		{name: "editor", mode: authz.ModeEnforce, cluster: testCluster, token: "te", perms: perms, want: []string{domain.KubeGroupEditor}},
		{name: "admin", mode: authz.ModeEnforce, cluster: testCluster, token: "ta", perms: perms, want: []string{domain.KubeGroupAdmin}},
		{name: "no role", mode: authz.ModeEnforce, cluster: testCluster, token: "tn", perms: perms},
		{name: "bad token", mode: authz.ModeEnforce, cluster: testCluster, token: "x", perms: perms, wantErr: true, kind: errs.KindUnauthenticated},
		{name: "unknown cluster", mode: authz.ModeEnforce, cluster: "mk8scluster-nope", token: "ta", perms: perms, wantErr: true, kind: errs.KindUnauthenticated},
		{name: "bad cluster id", mode: authz.ModeEnforce, cluster: "project-x", token: "ta", perms: perms, wantErr: true, kind: errs.KindBadRequest},
		{name: "check failure", mode: authz.ModeEnforce, cluster: testCluster, token: "ta", perms: fakePerms{err: errors.New("boom")}, wantErr: true, kind: errs.KindUnknown},
		{name: "audit grants admin", mode: authz.ModeAudit, cluster: testCluster, token: "tv", perms: perms, want: []string{domain.KubeGroupAdmin}},
		{name: "audit without role grants admin", mode: authz.ModeAudit, cluster: testCluster, token: "tn", perms: perms, want: []string{domain.KubeGroupAdmin}},
		{name: "off grants admin without checks", mode: authz.ModeOff, cluster: testCluster, token: "tn", perms: fakePerms{err: errors.New("unused")}, want: []string{domain.KubeGroupAdmin}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			r, err := domain.NewK8sReviewer(tokens, tc.perms, tc.mode, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Review(t.Context(), tc.cluster, tc.token)
			if tc.wantErr {
				if err == nil || errs.KindOf(err) != tc.kind {
					t.Fatalf("err = %v (kind %v), want kind %v", err, errs.KindOf(err), tc.kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if !slices.Equal(got.Groups, tc.want) {
				t.Errorf("groups = %v, want %v", got.Groups, tc.want)
			}
			if got.Username == "" || got.Username != got.UID {
				t.Errorf("identity = %+v, want username == uid == principal", got)
			}
			if tc.mode == authz.ModeAudit && !bytes.Contains(logs.Bytes(), []byte("enforced=false")) {
				t.Errorf("audit mode did not log the decision: %s", logs.String())
			}
		})
	}
}

func TestK8sReviewerDoesNotLeakToken(t *testing.T) {
	r, err := domain.NewK8sReviewer(fakeTokens{}, fakePerms{}, authz.ModeEnforce, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Review(t.Context(), testCluster, "secret-token-value")
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("secret-token-value")) {
		t.Fatalf("err = %v, want an error without the token", err)
	}
}

func TestNewK8sReviewerRequiresDeps(t *testing.T) {
	if _, err := domain.NewK8sReviewer(nil, fakePerms{}, authz.ModeEnforce, slog.Default()); err == nil {
		t.Error("nil authenticator accepted")
	}
	if _, err := domain.NewK8sReviewer(fakeTokens{}, nil, authz.ModeEnforce, slog.Default()); err == nil {
		t.Error("nil permission checker accepted")
	}
}
