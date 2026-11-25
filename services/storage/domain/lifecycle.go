package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

const (
	lifecycleField   = "spec.lifecycle_configuration"
	maxLifecycleDays = 18250
	maxRuleIDLength  = 255
	maxNewerVersions = 100
	day              = 24 * time.Hour

	transitionMsg = "not supported by NebiusMocker: storage-class transitions are not emulated"
	lastAccessMsg = "not supported by NebiusMocker: last-access tracking is not emulated"
)

// ValidateLifecycle checks a bucket lifecycle configuration and returns an
// errs.BadRequest with one violation per problem. Transitions and last-access
// filters are rejected by name: the mocker does not emulate them.
func ValidateLifecycle(cfg *storagepb.LifecycleConfiguration) error {
	if cfg == nil {
		return nil
	}
	var vs []errs.Violation
	add := func(field, msg string) {
		vs = append(vs, errs.Violation{Field: field, Message: msg})
	}
	if cfg.GetLastAccessFilter() != nil {
		add(lifecycleField+".last_access_filter", lastAccessMsg)
	}
	seen := map[string]bool{}
	for i, r := range cfg.GetRules() {
		base := fmt.Sprintf("%s.rules[%d]", lifecycleField, i)
		validateRule(base, r, seen, add)
	}
	if len(vs) == 0 {
		return nil
	}
	return errs.BadRequest(vs...)
}

func validateRule(base string, r *storagepb.LifecycleRule, seen map[string]bool, add func(field, msg string)) {
	validateRuleID(base, r, seen, add)
	if r.GetTransition() != nil {
		add(base+".transition", transitionMsg)
	}
	if r.GetNoncurrentVersionTransition() != nil {
		add(base+".noncurrent_version_transition", transitionMsg)
	}
	if r.GetExpiration() == nil && r.GetNoncurrentVersionExpiration() == nil &&
		r.GetAbortIncompleteMultipartUpload() == nil &&
		r.GetTransition() == nil && r.GetNoncurrentVersionTransition() == nil {
		add(base, "at least one of expiration, noncurrent_version_expiration or abort_incomplete_multipart_upload is required")
	}
	hasTags := len(r.GetFilter().GetTags()) > 0
	validateExpiration(base+".expiration", r.GetExpiration(), add)
	if hasTags && r.GetExpiration().GetExpiredObjectDeleteMarker() {
		add(base+".expiration", "expired_object_delete_marker cannot be combined with a tags filter")
	}
	if n := r.GetNoncurrentVersionExpiration(); n != nil {
		f := base + ".noncurrent_version_expiration"
		checkDays(f+".noncurrent_days", n.GetNoncurrentDays(), add)
		if n.NewerNoncurrentVersions != nil && (n.GetNewerNoncurrentVersions() < 1 || n.GetNewerNoncurrentVersions() > maxNewerVersions) {
			add(f+".newer_noncurrent_versions", fmt.Sprintf("must be between 1 and %d", maxNewerVersions))
		}
	}
	if a := r.GetAbortIncompleteMultipartUpload(); a != nil {
		f := base + ".abort_incomplete_multipart_upload"
		checkDays(f+".days_after_initiation", a.GetDaysAfterInitiation(), add)
		if hasTags {
			add(f, "cannot be combined with a tags filter")
		}
	}
	validateFilter(base+".filter", r.GetFilter(), add)
}

func validateRuleID(base string, r *storagepb.LifecycleRule, seen map[string]bool, add func(field, msg string)) {
	switch id := r.GetId(); {
	case id == "":
		add(base+".id", "required")
	case len(id) > maxRuleIDLength:
		add(base+".id", fmt.Sprintf("must be at most %d characters", maxRuleIDLength))
	case seen[id]:
		add(base+".id", "must be unique within the configuration")
	default:
		seen[id] = true
	}
	if st := r.GetStatus(); st != storagepb.LifecycleRule_ENABLED && st != storagepb.LifecycleRule_DISABLED {
		add(base+".status", "must be ENABLED or DISABLED")
	}
}

func validateExpiration(field string, e *storagepb.LifecycleExpiration, add func(field, msg string)) {
	if e == nil {
		return
	}
	var date, days bool
	switch e.GetExpiredWith().(type) {
	case *storagepb.LifecycleExpiration_Date:
		date = true
	case *storagepb.LifecycleExpiration_Days:
		days = true
	}
	marker := e.GetExpiredObjectDeleteMarker()
	switch {
	case (date || days) && marker:
		add(field, "days or date cannot be combined with expired_object_delete_marker")
	case !date && !days && !marker:
		add(field, "exactly one of date, days or expired_object_delete_marker is required")
	}
	if date {
		if t := e.GetDate().AsTime(); !t.Equal(t.Truncate(day)) {
			add(field+".date", "must be midnight UTC")
		}
	}
	if days {
		checkDays(field+".days", e.GetDays(), add)
	}
}

func checkDays(field string, n int32, add func(field, msg string)) {
	if n < 1 || n > maxLifecycleDays {
		add(field, fmt.Sprintf("must be between 1 and %d", maxLifecycleDays))
	}
}

func validateFilter(field string, f *storagepb.LifecycleFilter, add func(field, msg string)) {
	if f == nil {
		return
	}
	gt, lt := f.GetObjectSizeGreaterThanBytes(), f.GetObjectSizeLessThanBytes()
	if gt < 0 {
		add(field+".object_size_greater_than_bytes", "must not be negative")
	}
	if lt < 0 {
		add(field+".object_size_less_than_bytes", "must not be negative")
	}
	if gt > 0 && lt > 0 && gt >= lt {
		add(field, "object_size_greater_than_bytes must be less than object_size_less_than_bytes")
	}
	keys := map[string]bool{}
	for i, t := range f.GetTags() {
		tf := fmt.Sprintf("%s.tags[%d].key", field, i)
		switch {
		case t.GetKey() == "":
			add(tf, "required")
		case keys[t.GetKey()]:
			add(tf, "must be unique")
		}
		keys[t.GetKey()] = true
	}
}

// LifecycleResult counts what one ApplyLifecycle pass did.
type LifecycleResult struct {
	Expired           int
	NoncurrentExpired int
	MarkersRemoved    int
	UploadsAborted    int
}

// CeilMidnightUTC rounds t up to the next 00:00 UTC, or returns t (in UTC)
// when it already is midnight. Lifecycle actions fire at midnight UTC.
func CeilMidnightUTC(t time.Time) time.Time {
	t = t.UTC()
	d := t.Truncate(day)
	if d.Equal(t) {
		return t
	}
	return d.Add(day)
}

// ApplyLifecycle runs the ENABLED rules of cfg against the bucket once, as of
// now, working on a single snapshot of the bucket's versions and uploads. No
// version or upload is acted on twice. Individual failures do not stop the
// pass; they are joined into the returned error next to the partial result.
func ApplyLifecycle(ctx context.Context, backend ContentBackend, bucket string, cfg *storagepb.LifecycleConfiguration, now time.Time) (LifecycleResult, error) {
	var rules []*storagepb.LifecycleRule
	for _, r := range cfg.GetRules() {
		if r.GetStatus() == storagepb.LifecycleRule_ENABLED {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return LifecycleResult{}, nil
	}
	x := &lifecycleRun{backend: backend, bucket: bucket, now: now.UTC(), done: map[string]bool{}}
	if err := x.snapshot(ctx); err != nil {
		return LifecycleResult{}, err
	}
	for _, r := range rules {
		x.applyRule(ctx, r)
	}
	return x.res, errors.Join(x.errs...)
}

// lifecycleRun is the state of one ApplyLifecycle pass.
type lifecycleRun struct {
	backend ContentBackend
	bucket  string
	now     time.Time

	keys    []string
	byKey   map[string][]objstore.Version
	uploads []objstore.Upload

	done map[string]bool // acted-on versions and uploads
	res  LifecycleResult
	errs []error
}

func (x *lifecycleRun) snapshot(ctx context.Context) error {
	x.byKey = map[string][]objstore.Version{}
	for v, err := range x.backend.ListVersions(ctx, x.bucket, "") {
		if err != nil {
			return fmt.Errorf("list versions of bucket %q: %w", x.bucket, err)
		}
		if _, ok := x.byKey[v.Key]; !ok {
			x.keys = append(x.keys, v.Key)
		}
		x.byKey[v.Key] = append(x.byKey[v.Key], v)
	}
	for u, err := range x.backend.ListUploads(ctx, x.bucket) {
		if err != nil {
			return fmt.Errorf("list uploads of bucket %q: %w", x.bucket, err)
		}
		x.uploads = append(x.uploads, u)
	}
	return nil
}

func (x *lifecycleRun) applyRule(ctx context.Context, r *storagepb.LifecycleRule) {
	if e := r.GetExpiration(); e != nil {
		if e.GetExpiredObjectDeleteMarker() {
			x.removeMarkers(ctx, r)
		} else {
			x.expire(ctx, r)
		}
	}
	if r.GetNoncurrentVersionExpiration() != nil {
		x.expireNoncurrent(ctx, r)
	}
	if r.GetAbortIncompleteMultipartUpload() != nil {
		x.abortUploads(ctx, r)
	}
}

// claim marks an item acted on and reports whether it was still free.
func (x *lifecycleRun) claim(id string) bool {
	if x.done[id] {
		return false
	}
	x.done[id] = true
	return true
}

func versionID(v objstore.Version) string { return "v\x00" + v.Key + "\x00" + v.VersionID }

// matches reports whether the filter selects the version. Size filters apply
// to real versions only; tags are fetched only when the filter has any.
func (x *lifecycleRun) matches(ctx context.Context, f *storagepb.LifecycleFilter, v objstore.Version) (bool, error) {
	if !strings.HasPrefix(v.Key, f.GetPrefix()) {
		return false, nil
	}
	if !v.DeleteMarker {
		if gt := f.GetObjectSizeGreaterThanBytes(); gt > 0 && v.Size <= gt {
			return false, nil
		}
		if lt := f.GetObjectSizeLessThanBytes(); lt > 0 && v.Size >= lt {
			return false, nil
		}
	}
	if len(f.GetTags()) == 0 {
		return true, nil
	}
	have, err := x.backend.ObjectTags(ctx, x.bucket, v.Key, v.VersionID)
	if err != nil {
		return false, fmt.Errorf("tags of %s/%s: %w", v.Key, v.VersionID, err)
	}
	for _, t := range f.GetTags() {
		if got, ok := have[t.GetKey()]; !ok || got != t.GetValue() {
			return false, nil
		}
	}
	return true, nil
}

func (x *lifecycleRun) fail(err error) { x.errs = append(x.errs, err) }

func (x *lifecycleRun) expire(ctx context.Context, r *storagepb.LifecycleRule) {
	e := r.GetExpiration()
	for _, key := range x.keys {
		latest := x.byKey[key][0]
		if latest.DeleteMarker {
			continue
		}
		var due time.Time
		if _, ok := e.GetExpiredWith().(*storagepb.LifecycleExpiration_Date); ok {
			due = e.GetDate().AsTime()
		} else {
			due = CeilMidnightUTC(latest.LastModified.Add(time.Duration(e.GetDays()) * day))
		}
		if x.now.Before(due) || x.done[versionID(latest)] {
			continue
		}
		ok, err := x.matches(ctx, r.GetFilter(), latest)
		if err != nil {
			x.fail(err)
			continue
		}
		if !ok || !x.claim(versionID(latest)) {
			continue
		}
		if err := x.backend.Delete(ctx, x.bucket, key); err != nil {
			x.fail(fmt.Errorf("expire %q: %w", key, err))
			continue
		}
		x.res.Expired++
	}
}

func (x *lifecycleRun) removeMarkers(ctx context.Context, r *storagepb.LifecycleRule) {
	for _, key := range x.keys {
		vs := x.byKey[key]
		if len(vs) != 1 || !vs[0].DeleteMarker || !vs[0].IsLatest {
			continue
		}
		if ok, err := x.matches(ctx, r.GetFilter(), vs[0]); err != nil {
			x.fail(err)
			continue
		} else if !ok || !x.claim(versionID(vs[0])) {
			continue
		}
		if err := x.backend.DeleteVersion(ctx, x.bucket, key, vs[0].VersionID); err != nil {
			x.fail(fmt.Errorf("remove delete marker of %q: %w", key, err))
			continue
		}
		x.res.MarkersRemoved++
	}
}

func (x *lifecycleRun) expireNoncurrent(ctx context.Context, r *storagepb.LifecycleRule) {
	n := r.GetNoncurrentVersionExpiration()
	keep := int(n.GetNewerNoncurrentVersions())
	age := time.Duration(n.GetNoncurrentDays()) * day
	for _, key := range x.keys {
		vs := x.byKey[key]
		seen := 0 // noncurrent real versions passed so far
		for i := 1; i < len(vs); i++ {
			if vs[i].DeleteMarker {
				continue
			}
			seen++
			// vs[i] became noncurrent when its successor vs[i-1] was written.
			if seen > keep && !x.now.Before(CeilMidnightUTC(vs[i-1].LastModified.Add(age))) {
				x.deleteNoncurrent(ctx, r, vs[i])
			}
		}
	}
}

func (x *lifecycleRun) deleteNoncurrent(ctx context.Context, r *storagepb.LifecycleRule, v objstore.Version) {
	if x.done[versionID(v)] {
		return
	}
	ok, err := x.matches(ctx, r.GetFilter(), v)
	if err != nil {
		x.fail(err)
		return
	}
	if !ok || !x.claim(versionID(v)) {
		return
	}
	if err := x.backend.DeleteVersion(ctx, x.bucket, v.Key, v.VersionID); err != nil {
		x.fail(fmt.Errorf("expire noncurrent %q/%s: %w", v.Key, v.VersionID, err))
		return
	}
	x.res.NoncurrentExpired++
}

func (x *lifecycleRun) abortUploads(ctx context.Context, r *storagepb.LifecycleRule) {
	days := time.Duration(r.GetAbortIncompleteMultipartUpload().GetDaysAfterInitiation()) * day
	prefix := r.GetFilter().GetPrefix()
	for _, u := range x.uploads {
		if !strings.HasPrefix(u.Key, prefix) || x.now.Before(CeilMidnightUTC(u.Initiated.Add(days))) {
			continue
		}
		if !x.claim("u\x00" + u.Key + "\x00" + u.UploadID) {
			continue
		}
		if err := x.backend.AbortUpload(ctx, x.bucket, u.Key, u.UploadID); err != nil {
			x.fail(fmt.Errorf("abort upload %q/%s: %w", u.Key, u.UploadID, err))
			continue
		}
		x.res.UploadsAborted++
	}
}
