package domain

import (
	"context"
	"errors"
	"fmt"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// S3Gate makes the access decisions of the S3 front: it authenticates Nebius
// access keys and authorizes requests against Nebius buckets, IAM and bucket
// policies, so the mocker's metadata — not the object store — is the source
// of truth.
type S3Gate struct {
	buckets BucketRepository
	keys    KeyResolver
	checker AccessChecker
	members func() GroupMembers
	parents Hierarchy
	mode    authz.Mode
}

// GateDeps are the dependencies of an S3Gate. Members is looked up per
// request (the iam module may be absent; then group policy rules never match).
type GateDeps struct {
	Buckets   BucketRepository
	Keys      KeyResolver
	Checker   AccessChecker
	Hierarchy Hierarchy
	Members   func() GroupMembers
	// Mode is the process authorization mode.
	Mode authz.Mode
}

// NewS3Gate returns a gate.
func NewS3Gate(d GateDeps) (*S3Gate, error) {
	if d.Buckets == nil || d.Keys == nil || d.Checker == nil || d.Hierarchy == nil {
		return nil, errors.New("storage: s3 gate dependencies must not be nil")
	}
	members := d.Members
	if members == nil {
		members = func() GroupMembers { return nil }
	}
	return &S3Gate{buckets: d.Buckets, keys: d.Keys, checker: d.Checker, members: members, parents: d.Hierarchy, mode: d.Mode}, nil
}

// Access is the outcome of an authorized request.
type Access struct {
	Bucket *storagepb.Bucket
	// Decision is the authz decision. In audit mode a denied decision is
	// returned with a nil error, so the caller can log it.
	Decision authz.Decision
}

// ResolveKey returns the key's secret and principal, or errs.Unauthenticated.
func (g *S3Gate) ResolveKey(ctx context.Context, accessKeyID string) (peer.AccessKey, error) {
	return g.keys.ResolveAccessKey(ctx, accessKeyID)
}

// S3Request is one S3 request to authorize.
type S3Request struct {
	// Principal is the access key's account; "" for an anonymous (unsigned) request.
	Principal string
	Bucket    string
	Action    S3Action
	// Target is the object key for object actions and the prefix for listings.
	Target string
}

// Authorize checks that the bucket is an ACTIVE Nebius bucket and that the
// request is allowed: by an IAM permit of the principal (the storage.*
// permission of the action), or by a bucket policy rule for one of the
// principal's groups or for anonymous access. A missing or soft-deleted
// bucket is errs.NotFound (S3 NoSuchBucket). An anonymous request without a
// policy grant is always errs.PermissionDenied; a signed one only under
// enforce (audit reports the denial in the returned decision).
func (g *S3Gate) Authorize(ctx context.Context, req S3Request) (Access, error) {
	b, err := g.buckets.GetByBucketName(ctx, req.Bucket)
	if err != nil {
		return Access{}, err
	}
	if !usable(b) {
		return Access{}, errs.NotFound(req.Bucket)
	}
	perm, id := req.Action.Permission(), b.GetMetadata().GetId()
	d := authz.Decision{Permission: perm, Target: id, Reason: "anonymous request"}
	if req.Principal != "" {
		if d, err = g.check(ctx, req.Principal, perm, id); err != nil {
			return Access{}, err
		}
		if d.Allowed {
			return Access{Bucket: b, Decision: d}, nil
		}
	}
	ok, reason, err := policyGrant(ctx, b.GetSpec().GetBucketPolicy(), g.members(), req.Principal, req.Action, req.Target)
	if err != nil {
		return Access{}, err
	}
	if ok {
		return Access{Bucket: b, Decision: authz.Decision{Allowed: true, Permission: perm, Target: id, Reason: reason}}, nil
	}
	if req.Principal == "" || g.mode == authz.ModeEnforce {
		return Access{}, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: id, Message: string(req.Action) + " denied: " + d.Reason}
	}
	return Access{Bucket: b, Decision: d}, nil
}

// Bucket returns a bucket S3 serves (ACTIVE or UPDATING) by name, without an
// access check (CORS preflight requests are unauthenticated).
func (g *S3Gate) Bucket(ctx context.Context, name string) (*storagepb.Bucket, error) {
	b, err := g.buckets.GetByBucketName(ctx, name)
	if err != nil {
		return nil, err
	}
	if !usable(b) {
		return nil, errs.NotFound(name)
	}
	return b, nil
}

// AuthorizeProject checks perm of principalID on the project that owns the
// principal (a service account's parent), for S3 CreateBucket. It returns
// the project ID and the decision (modes as in Authorize).
func (g *S3Gate) AuthorizeProject(ctx context.Context, principalID string, perm authz.Permission) (string, authz.Decision, error) {
	project, err := g.parents.Parent(ctx, principalID)
	if err != nil {
		return "", authz.Decision{}, fmt.Errorf("project of %s: %w", principalID, err)
	}
	if kindOf(project) != KindProject {
		return "", authz.Decision{}, errs.PermissionDenied(principalID)
	}
	d, err := g.decide(ctx, principalID, perm, project)
	return project, d, err
}

// VisibleBuckets returns the ACTIVE buckets principalID may list (S3
// ListBuckets), in creation order.
func (g *S3Gate) VisibleBuckets(ctx context.Context, principalID string) ([]*storagepb.Bucket, error) {
	all, err := g.buckets.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	var out []*storagepb.Bucket
	for _, b := range all {
		if !usable(b) {
			continue
		}
		d, err := g.decide(ctx, principalID, PermBucketList, b.GetMetadata().GetParentId())
		if errs.IsKind(err, errs.KindPermissionDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d.Allowed || g.mode != authz.ModeEnforce {
			out = append(out, b)
		}
	}
	return out, nil
}

// usable reports whether S3 serves the bucket.
func usable(b *storagepb.Bucket) bool {
	st := b.GetStatus().GetState()
	return st == storagepb.BucketStatus_ACTIVE || st == storagepb.BucketStatus_UPDATING
}

// check evaluates perm without applying the mode (off allows everything).
func (g *S3Gate) check(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error) {
	if g.mode == authz.ModeOff {
		return authz.Decision{Allowed: true, Permission: perm, Target: target, Reason: "authorization is off"}, nil
	}
	d, err := g.checker.Check(ctx, principalID, perm, target)
	if err != nil {
		return d, fmt.Errorf("authorize %s on %s: %w", perm, target, err)
	}
	return d, nil
}

// decide evaluates perm per the mode: off allows, audit reports, enforce denies.
func (g *S3Gate) decide(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error) {
	d, err := g.check(ctx, principalID, perm, target)
	if err != nil {
		return d, err
	}
	if !d.Allowed && g.mode == authz.ModeEnforce {
		return d, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: target, Message: string(perm) + " denied: " + d.Reason}
	}
	return d, nil
}
