package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Kubernetes groups Nebius reserves for users that access a cluster with
// Nebius credentials; the cluster pre-binds them to view, edit and
// cluster-admin.
const (
	KubeGroupViewer = "nebius:viewer"
	KubeGroupEditor = "nebius:editor"
	KubeGroupAdmin  = "nebius:admin"
)

const clusterIDPrefix = "mk8scluster-"

// KubeIdentity is the Kubernetes user a Nebius token maps to.
type KubeIdentity struct {
	Username string
	UID      string
	Groups   []string
}

// kubeProbe pairs a Kubernetes group with the permission that earns it.
type kubeProbe struct {
	group string
	perm  authz.Permission
}

// kubeProbes are checked highest role first. Only admins may manage access
// (editors are excluded from it), so it tells admin from editor.
var kubeProbes = []kubeProbe{
	{KubeGroupAdmin, authz.Perm("iam", "accessPermits", "create")},
	{KubeGroupEditor, authz.Perm("mk8s", "clusters", "update")},
	{KubeGroupViewer, authz.Perm("mk8s", "clusters", "get")},
}

// K8sReviewer answers Kubernetes TokenReviews: it maps a Nebius IAM token to
// a Kubernetes identity on one Managed Kubernetes cluster.
type K8sReviewer struct {
	authn TokenAuthenticator
	perms PermissionChecker
	mode  authz.Mode
	log   *slog.Logger
}

// NewK8sReviewer returns a reviewer.
func NewK8sReviewer(authn TokenAuthenticator, perms PermissionChecker, mode authz.Mode, log *slog.Logger) (*K8sReviewer, error) {
	if authn == nil || perms == nil || log == nil {
		return nil, errors.New("k8s reviewer: authenticator, permission checker and logger are required")
	}
	return &K8sReviewer{authn: authn, perms: perms, mode: mode, log: log}, nil
}

// Review authenticates token and returns its identity on cluster clusterID.
// A valid token without any role on the cluster gets no groups, so RBAC
// denies everything. Under ModeAudit the real group is only logged and
// nebius:admin is granted; under ModeOff nothing is checked.
func (r *K8sReviewer) Review(ctx context.Context, clusterID, token string) (KubeIdentity, error) {
	if !strings.HasPrefix(clusterID, clusterIDPrefix) {
		return KubeIdentity{}, errs.BadRequest(errs.Violation{Field: "cluster_id", Message: "must be a Managed Kubernetes cluster id"})
	}
	principal, err := r.authn.Authenticate(token)
	if err != nil {
		return KubeIdentity{}, errs.Unauthenticated(fmt.Sprintf("invalid token: %v", err))
	}
	id := KubeIdentity{Username: principal, UID: principal}
	if r.mode == authz.ModeOff {
		id.Groups = []string{KubeGroupAdmin}
		return id, nil
	}
	group, err := r.groupOf(ctx, principal, clusterID)
	if err != nil {
		return KubeIdentity{}, err
	}
	if r.mode == authz.ModeAudit {
		r.log.InfoContext(ctx, "k8s token review", "principal", principal, "cluster", clusterID, "group", group, "enforced", false)
		group = KubeGroupAdmin
	}
	if group != "" {
		id.Groups = []string{group}
	}
	return id, nil
}

// groupOf returns the highest Kubernetes group the principal earns, or "".
func (r *K8sReviewer) groupOf(ctx context.Context, principal, clusterID string) (string, error) {
	for _, p := range kubeProbes {
		d, err := r.perms.Check(ctx, principal, p.perm, clusterID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return "", errs.Unauthenticated("unknown cluster " + clusterID + ": token is not valid for it")
			}
			return "", fmt.Errorf("check %s on %s: %w", p.perm, clusterID, err)
		}
		if d.Allowed {
			return p.group, nil
		}
	}
	return "", nil
}
