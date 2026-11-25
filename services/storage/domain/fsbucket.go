package domain

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Filesystem buckets (spec.filesystem_bucket) expose a compute shared
// filesystem through the S3 API
// (https://docs.nebius.com/object-storage/buckets/filesystem-buckets). The
// files live in the filesystem's own object-store bucket, owned by the
// compute module and reached through peer.Filesystems; the Nebius bucket is a
// view of it:
//   - deleting the bucket never deletes the files, and it is immediate
//     (no delayed deletion or restore for filesystem buckets);
//   - a new filesystem bucket on the same filesystem sees the same files;
//   - writes are limited by the filesystem size (and by max_size_bytes);
//   - versioning, lifecycle rules, object tagging and anonymous access are not
//     supported for filesystem buckets (S3 API compatibility page);
//   - the filesystem must be READY and in the bucket's project.
//
// Only the filesystem root with default ownership and permissions can be
// mounted: a sub-directory, uid/gid and file/directory modes need POSIX
// semantics the mocker does not emulate, so other values are rejected by name.

// PermFilesystemViaObjectStorage is checked on the filesystem when a
// filesystem bucket is created (Nebius: the editor role or
// compute.use-via-object-storage).
var PermFilesystemViaObjectStorage = authz.Perm("compute", "filesystems", authz.VerbUseViaObjectStorage)

// fsField names a filesystem_bucket field in violations.
func fsField(name string) string { return "spec.filesystem_bucket." + name }

// IsFilesystemBucket reports whether b exposes a compute filesystem.
func IsFilesystemBucket(b *storagepb.Bucket) bool { return b.GetSpec().GetFilesystemBucket() != nil }

// checkFilesystemSpec validates the settings of a filesystem bucket's spec
// that differ from regular buckets. It is a no-op for regular buckets.
func checkFilesystemSpec(spec *storagepb.BucketSpec) error {
	fb := spec.GetFilesystemBucket()
	if fb == nil {
		return nil
	}
	bad := func(field, msg string) error { return errs.BadRequest(errs.Violation{Field: field, Message: msg}) }
	switch {
	case fb.GetFilesystemId() == "":
		return bad(fsField("filesystem_id"), "required")
	case fb.GetDirectory() != "" && fb.GetDirectory() != "/":
		return bad(fsField("directory"), `not supported by NebiusMocker: only the filesystem root ("" or "/") can be exposed`)
	case fb.GetUid() != 0:
		return bad(fsField("uid"), "not supported by NebiusMocker: files are written as root (uid 0); POSIX ownership is not emulated")
	case fb.GetGid() != 0:
		return bad(fsField("gid"), "not supported by NebiusMocker: files are written as root (gid 0); POSIX ownership is not emulated")
	case !defaultMode(fb.GetFileMode(), 0o644):
		return bad(fsField("file_mode"), `not supported by NebiusMocker: only the default "644" (or empty) is accepted; POSIX permissions are not emulated`)
	case !defaultMode(fb.GetDirectoryMode(), 0o755):
		return bad(fsField("directory_mode"), `not supported by NebiusMocker: only the default "755" (or empty) is accepted; POSIX permissions are not emulated`)
	case spec.GetVersioningPolicy() == storagepb.VersioningPolicy_ENABLED || spec.GetVersioningPolicy() == storagepb.VersioningPolicy_SUSPENDED:
		return bad("spec.versioning_policy", "versioning is not supported for filesystem buckets")
	case len(spec.GetLifecycleConfiguration().GetRules()) > 0:
		return bad("spec.lifecycle_configuration", "lifecycle rules are not supported for filesystem buckets")
	case AnonymousAccess(spec.GetBucketPolicy()):
		return bad("spec.bucket_policy", "bucket policies of filesystem buckets cannot grant anonymous access")
	}
	if c := spec.GetDefaultStorageClass(); c != storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED && c != storagepb.StorageClass_FILESYSTEM {
		return bad("spec.default_storage_class", fmt.Sprintf("filesystem buckets store objects in the FILESYSTEM class, not %s", c))
	}
	return nil
}

// defaultMode reports whether an octal permission string is empty or equals want.
func defaultMode(s string, want uint64) bool {
	if s == "" {
		return true
	}
	n, err := strconv.ParseUint(s, 8, 32)
	return err == nil && n == want
}

// filesystem resolves a filesystem through the compute module.
func (s *BucketService) filesystem(ctx context.Context, id string) (peer.Filesystem, error) {
	if s.filesystems == nil {
		return peer.Filesystem{}, errs.BadRequest(errs.Violation{
			Field: fsField("filesystem_id"), Message: "filesystem buckets need the compute module, which is not enabled",
		})
	}
	return s.filesystems.Filesystem(ctx, id)
}

// checkNewFilesystemBucket checks, at bucket creation, that the filesystem
// exists, is READY, is in the bucket's project and that the caller may expose
// it through Object Storage.
func (s *BucketService) checkNewFilesystemBucket(ctx context.Context, projectID string, spec *storagepb.BucketSpec) error {
	id := spec.GetFilesystemBucket().GetFilesystemId()
	fs, err := s.filesystem(ctx, id)
	if err != nil {
		return fmt.Errorf("%s: %w", fsField("filesystem_id"), err)
	}
	if fs.ParentID != projectID {
		return errs.BadRequest(errs.Violation{
			Field: fsField("filesystem_id"), Message: fmt.Sprintf("filesystem %s is not in project %s", id, projectID),
		})
	}
	if !fs.Ready {
		return errs.BadState(id, fmt.Sprintf("filesystem %s is not READY", id))
	}
	return s.authorizeFilesystem(ctx, id)
}

// authorizeFilesystem checks PermFilesystemViaObjectStorage on the
// filesystem for the caller, per the authorization mode.
func (s *BucketService) authorizeFilesystem(ctx context.Context, fsID string) error {
	if s.access == nil || s.mode == authz.ModeOff {
		return nil
	}
	principal := reqctx.PrincipalFrom(ctx).ID
	d, err := s.access.Check(ctx, principal, PermFilesystemViaObjectStorage, fsID)
	if err != nil {
		return fmt.Errorf("authorize %s on %s: %w", PermFilesystemViaObjectStorage, fsID, err)
	}
	if d.Allowed {
		return nil
	}
	if s.mode == authz.ModeEnforce {
		return &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: fsID, Message: string(PermFilesystemViaObjectStorage) + " denied: " + d.Reason}
	}
	s.log.WarnContext(ctx, "authz denied", "method", "storage.filesystem_bucket", "principal", principal,
		"permission", string(PermFilesystemViaObjectStorage), "target", fsID, "enforced", false, "reason", d.Reason)
	return nil
}

// Backend returns the object-store bucket that holds b's objects: the
// bucket's own name, or for a filesystem bucket the filesystem's bucket. A
// filesystem bucket whose filesystem is gone or not usable is errs.BadState.
func (s *BucketService) Backend(ctx context.Context, b *storagepb.Bucket) (string, error) {
	fb := b.GetSpec().GetFilesystemBucket()
	if fb == nil {
		return b.GetMetadata().GetName(), nil
	}
	fs, err := s.usableFilesystem(ctx, b)
	if err != nil {
		return "", err
	}
	return fs.BackendBucket, nil
}

// usableFilesystem returns the filesystem behind a filesystem bucket.
func (s *BucketService) usableFilesystem(ctx context.Context, b *storagepb.Bucket) (peer.Filesystem, error) {
	id, fsID := b.GetMetadata().GetId(), b.GetSpec().GetFilesystemBucket().GetFilesystemId()
	fs, err := s.filesystem(ctx, fsID)
	if errs.IsKind(err, errs.KindNotFound) {
		return peer.Filesystem{}, errs.BadState(id, fmt.Sprintf("the filesystem %s of this filesystem bucket no longer exists", fsID))
	}
	if err != nil {
		return peer.Filesystem{}, err
	}
	if !fs.Ready {
		return peer.Filesystem{}, errs.BadState(id, fmt.Sprintf("the filesystem %s of this filesystem bucket is not READY", fsID))
	}
	return fs, nil
}

// checkFilesystemCapacity refuses a write of incoming bytes that would make
// the filesystem's files exceed its size. Every filesystem bucket of the
// filesystem shares the capacity, since they all show the same files.
func (s *BucketService) checkFilesystemCapacity(ctx context.Context, b *storagepb.Bucket, incoming int64) error {
	fs, err := s.usableFilesystem(ctx, b)
	if err != nil {
		return err
	}
	cs, err := ComputeCounters(ctx, s.content, fs.BackendBucket)
	if err != nil {
		return fmt.Errorf("filesystem %s usage: %w", fs.ID, err)
	}
	if used := UsedBytes(cs); used+incoming > fs.CapacityBytes {
		return &errs.Error{Kind: errs.KindQuotaExceeded, ResourceID: fs.ID, Message: fmt.Sprintf(
			"no space left on filesystem %s: its size is %d bytes; %d bytes are stored and the request adds %d", fs.ID, fs.CapacityBytes, used, incoming)}
	}
	return nil
}

// BucketsOf implements peer.FilesystemBuckets: the names of the buckets that
// expose filesystemID, in any state.
func (s *BucketService) BucketsOf(ctx context.Context, filesystemID string) ([]string, error) {
	all, err := s.buckets.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, b := range all {
		if b.GetSpec().GetFilesystemBucket().GetFilesystemId() == filesystemID {
			names = append(names, b.GetMetadata().GetName())
		}
	}
	slices.Sort(names)
	return names, nil
}

// checkFilesystemBucketDelete refuses a delayed deletion: filesystem buckets
// are deleted at once and cannot be restored.
func checkFilesystemBucketDelete(cmd DeleteBucketCmd, now time.Time) error {
	if cmd.TTL != nil && *cmd.TTL != 0 {
		return errs.BadRequest(errs.Violation{Field: "ttl", Message: "delayed deletion is not supported for filesystem buckets: omit ttl or set it to 0"})
	}
	if cmd.PurgeAt != nil && cmd.PurgeAt.After(now) {
		return errs.BadRequest(errs.Violation{Field: "purge_at", Message: "delayed deletion is not supported for filesystem buckets: omit purge_at"})
	}
	return nil
}

// matchesFilter reports whether b passes a ListWithFilter filter.
func matchesFilter(b *storagepb.Bucket, f *storagepb.ListBucketsWithFilterRequest_Filters) bool {
	if t := f.GetBucketType(); t != storagepb.BucketStatus_BUCKET_TYPE_UNSPECIFIED && b.GetStatus().GetBucketType() != t {
		return false
	}
	if id := f.GetFilesystemId(); id != "" && b.GetSpec().GetFilesystemBucket().GetFilesystemId() != id {
		return false
	}
	return true
}
