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

// S3 data-plane permissions. Object operations use the standard verbs, so the
// seeded viewer/editor/admin roles apply to objects as they do to buckets.
var (
	PermObjectGet    = authz.Perm("storage", "objects", authz.VerbGet)
	PermObjectList   = authz.Perm("storage", "objects", authz.VerbList)
	PermObjectWrite  = authz.Perm("storage", "objects", authz.VerbCreate)
	PermObjectDelete = authz.Perm("storage", "objects", authz.VerbDelete)
	PermBucketGet    = authz.Perm("storage", "buckets", authz.VerbGet)
	PermBucketList   = authz.Perm("storage", "buckets", authz.VerbList)
)

// S3Gate makes the access decisions of the S3 front: it authenticates Nebius
// access keys and authorizes requests against Nebius buckets and IAM, so the
// mocker's metadata — not the object store — is the source of truth.
type S3Gate struct {
	buckets BucketRepository
	keys    KeyResolver
	checker AccessChecker
	mode    authz.Mode
}

// NewS3Gate returns a gate; mode is the process authorization mode.
func NewS3Gate(buckets BucketRepository, keys KeyResolver, checker AccessChecker, mode authz.Mode) (*S3Gate, error) {
	if buckets == nil || keys == nil || checker == nil {
		return nil, errors.New("storage: s3 gate dependencies must not be nil")
	}
	return &S3Gate{buckets: buckets, keys: keys, checker: checker, mode: mode}, nil
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

// Authorize checks that bucketName is an ACTIVE Nebius bucket and that
// principalID holds perm on it. A missing or soft-deleted bucket is
// errs.NotFound (S3 NoSuchBucket); a denial under enforce is
// errs.PermissionDenied.
func (g *S3Gate) Authorize(ctx context.Context, principalID, bucketName string, perm authz.Permission) (Access, error) {
	b, err := g.buckets.GetByBucketName(ctx, bucketName)
	if err != nil {
		return Access{}, err
	}
	if b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE && b.GetStatus().GetState() != storagepb.BucketStatus_UPDATING {
		return Access{}, errs.NotFound(bucketName)
	}
	d, err := g.decide(ctx, principalID, perm, b.GetMetadata().GetId())
	if err != nil {
		return Access{}, err
	}
	return Access{Bucket: b, Decision: d}, nil
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
		if b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE && b.GetStatus().GetState() != storagepb.BucketStatus_UPDATING {
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

// decide evaluates perm per the mode: off allows, audit reports, enforce denies.
func (g *S3Gate) decide(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error) {
	if g.mode == authz.ModeOff {
		return authz.Decision{Allowed: true, Permission: perm, Target: target, Reason: "authorization is off"}, nil
	}
	d, err := g.checker.Check(ctx, principalID, perm, target)
	if err != nil {
		return d, fmt.Errorf("authorize %s on %s: %w", perm, target, err)
	}
	if !d.Allowed && g.mode == authz.ModeEnforce {
		return d, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: target, Message: string(perm) + " denied: " + d.Reason}
	}
	return d, nil
}
