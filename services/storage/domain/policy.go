package domain

import (
	"context"
	"fmt"
	"slices"
	"strings"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Bucket policy roles (Nebius "supported actions" table). A policy rule
// grants its roles' actions to a group, or to everyone when anonymous.
var (
	objectViewerActions = []S3Action{ActGetObject, ActHeadObject, ActGetObjectTagging}
	objectListerActions = []S3Action{ActListObjects, ActListObjectVersions}
	bucketReadActions   = []S3Action{ActHeadBucket, ActGetBucketLifecycle, ActGetBucketVersioning, ActGetBucketLocation, ActGetBucketCors}
	uploadActions       = []S3Action{
		ActPutObject, ActCopyObject, ActCreateMultipartUpload, ActUploadPart, ActUploadPartCopy,
		ActCompleteMultipartUpload, ActAbortMultipartUpload, ActListMultipartUploads, ActListParts, ActPutObjectTagging,
	}

	policyRoles = map[string][]S3Action{
		"storage.object-viewer": objectViewerActions,
		"storage.object-lister": objectListerActions,
		"storage.viewer":        concat(objectViewerActions, objectListerActions, bucketReadActions, []S3Action{ActListMultipartUploads, ActListParts}),
		"storage.uploader":      concat(uploadActions, bucketReadActions),
		"storage.object-editor": concat(objectViewerActions, objectListerActions, uploadActions,
			[]S3Action{ActDeleteObjectTagging, ActDeleteObject, ActDeleteObjects}),
		"storage.editor": concat(objectViewerActions, objectListerActions, uploadActions, bucketReadActions,
			[]S3Action{
				ActDeleteObjectTagging, ActDeleteObject, ActDeleteObjects, ActPutBucketLifecycle,
				ActDeleteBucketLifecycle, ActPutBucketCors, ActDeleteBucketCors, ActPutBucketVersioning, ActGetBucketOther,
			}),
	}

	// anonymousRoles are the read-only roles anonymous rules may grant.
	anonymousRoles = []string{"storage.viewer", "storage.object-viewer", "storage.object-lister"}
)

func concat(lists ...[]S3Action) []S3Action {
	var out []S3Action
	for _, l := range lists {
		for _, a := range l {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// ValidatePolicy checks a bucket policy against the documented model: paths
// are full keys or prefixes with a single trailing "*" ("?" matches one
// character, "*" alone is the whole bucket), roles are storage.* roles, each
// rule has a subject, and anonymous rules grant read-only roles only.
func ValidatePolicy(p *storagepb.BucketPolicy) error {
	var vs []errs.Violation
	for i, r := range p.GetRules() {
		field := fmt.Sprintf("spec.bucket_policy.rules[%d]", i)
		if len(r.GetPaths()) == 0 {
			vs = append(vs, errs.Violation{Field: field + ".paths", Message: "at least one path is required"})
		}
		for j, path := range r.GetPaths() {
			if !validPolicyPath(path) {
				vs = append(vs, errs.Violation{
					Field:   fmt.Sprintf("%s.paths[%d]", field, j),
					Message: fmt.Sprintf("invalid path %q: a full key, or a prefix ending with a single \"*\"", path),
				})
			}
		}
		if len(r.GetRoles()) == 0 {
			vs = append(vs, errs.Violation{Field: field + ".roles", Message: "at least one role is required"})
		}
		anonymous := r.GetAnonymous() != nil
		for j, role := range r.GetRoles() {
			switch _, known := policyRoles[role]; {
			case !known:
				vs = append(vs, errs.Violation{Field: fmt.Sprintf("%s.roles[%d]", field, j), Message: fmt.Sprintf("unknown role %q; supported: %s", role, strings.Join(roleNames(), ", "))})
			case anonymous && !slices.Contains(anonymousRoles, role):
				vs = append(vs, errs.Violation{Field: fmt.Sprintf("%s.roles[%d]", field, j), Message: fmt.Sprintf("anonymous rules allow only read-only roles (%s)", strings.Join(anonymousRoles, ", "))})
			}
		}
		if !anonymous && r.GetGroupId() == "" {
			vs = append(vs, errs.Violation{Field: field, Message: "a subject (group_id or anonymous) is required"})
		}
	}
	if len(vs) > 0 {
		return errs.BadRequest(vs...)
	}
	return nil
}

func roleNames() []string {
	names := make([]string, 0, len(policyRoles))
	for r := range policyRoles {
		names = append(names, r)
	}
	slices.Sort(names)
	return names
}

func validPolicyPath(p string) bool {
	if p == "" {
		return false
	}
	i := strings.IndexByte(p, '*')
	return i < 0 || i == len(p)-1
}

// matchPath reports whether a policy path matches an object key.
func matchPath(pattern, key string) bool {
	prefix, wildcard := strings.CutSuffix(pattern, "*")
	if !wildcard {
		return len(pattern) == len(key) && matchChars(pattern, key)
	}
	return len(key) >= len(prefix) && matchChars(prefix, key[:len(prefix)])
}

// coversPrefix reports whether a policy path covers every key under a
// listing prefix: "*", or a wildcard path whose prefix the listing is within.
func coversPrefix(pattern, listPrefix string) bool {
	prefix, wildcard := strings.CutSuffix(pattern, "*")
	return wildcard && len(listPrefix) >= len(prefix) && matchChars(prefix, listPrefix[:len(prefix)])
}

// matchChars compares equal-length strings where "?" in pattern matches any byte.
func matchChars(pattern, s string) bool {
	for i := range len(pattern) {
		if pattern[i] != '?' && pattern[i] != s[i] {
			return false
		}
	}
	return true
}

// AnonymousAccess reports whether any rule grants anonymous access
// (status.anonymous_access_enabled).
func AnonymousAccess(p *storagepb.BucketPolicy) bool {
	for _, r := range p.GetRules() {
		if r.GetAnonymous() != nil {
			return true
		}
	}
	return false
}

// policyTarget says what a request touches, for path matching.
func policyTargetMatches(pattern string, act S3Action, target string) bool {
	switch {
	case pattern == "*":
		return true
	case act.ObjectScoped():
		return matchPath(pattern, target)
	case act.Listing():
		return coversPrefix(pattern, target)
	}
	return false
}

// policyGrant evaluates a bucket policy for one request. principalID is ""
// for anonymous requests. It returns the reason of the first granting rule.
func policyGrant(ctx context.Context, p *storagepb.BucketPolicy, members GroupMembers, principalID string, act S3Action, target string) (granted bool, reason string, err error) {
	for i, r := range p.GetRules() {
		if !ruleGrantsAction(r, act) || !slices.ContainsFunc(r.GetPaths(), func(path string) bool {
			return policyTargetMatches(path, act, target)
		}) {
			continue
		}
		if r.GetAnonymous() != nil {
			return true, fmt.Sprintf("bucket policy rule %d (anonymous)", i), nil
		}
		if principalID == "" || members == nil {
			continue
		}
		ok, err := members.IsMember(ctx, principalID, r.GetGroupId())
		if err != nil {
			return false, "", fmt.Errorf("bucket policy rule %d: membership of %s: %w", i, r.GetGroupId(), err)
		}
		if ok {
			return true, fmt.Sprintf("bucket policy rule %d (group %s)", i, r.GetGroupId()), nil
		}
	}
	return false, "", nil
}

func ruleGrantsAction(r *storagepb.BucketPolicy_Rule, act S3Action) bool {
	for _, role := range r.GetRoles() {
		if slices.Contains(policyRoles[role], act) {
			return true
		}
	}
	return false
}
