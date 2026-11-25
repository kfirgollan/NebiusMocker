package domain_test

import (
	"errors"
	"testing"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestCreateFromS3(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b, err := f.svc.CreateFromS3(t.Context(), projectID, "s3-made")
	if err != nil {
		t.Fatalf("CreateFromS3: %v", err)
	}
	if b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE || b.GetMetadata().GetParentId() != projectID ||
		b.GetSpec().GetVersioningPolicy() != storagepb.VersioningPolicy_DISABLED || b.GetMetadata().GetName() != "s3-made" {
		t.Fatalf("bucket = %v", b)
	}
	if _, ok := f.backend.versioning("s3-made"); !ok {
		t.Error("backing bucket not created")
	}
	if got, err := f.svc.GetByName(t.Context(), projectID, "s3-made"); err != nil || got.GetMetadata().GetId() != b.GetMetadata().GetId() {
		t.Errorf("GetByName = %v, %v; want the S3-created bucket", got, err)
	}

	tests := []struct {
		name   string
		bucket string
		want   errs.Kind
	}{
		{"name taken", "s3-made", errs.KindAlreadyExists},
		{"invalid name", "Not_Valid", errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.svc.CreateFromS3(t.Context(), projectID, tt.bucket); errs.KindOf(err) != tt.want {
				t.Fatalf("CreateFromS3(%q) = %v, want kind %v", tt.bucket, err, tt.want)
			}
		})
	}
}

func TestDeleteFromS3(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		objects int
		wantErr bool
	}{
		{"empty bucket is scheduled", 0, false},
		{"non-empty bucket is refused", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			b := f.create(t, "to-delete", nil)
			id := b.GetMetadata().GetId()
			f.backend.objects["to-delete"] = tt.objects
			err := f.svc.DeleteFromS3(t.Context(), id)
			got, gerr := f.svc.Get(t.Context(), id)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if tt.wantErr {
				if !errs.IsKind(err, errs.KindBadState) {
					t.Fatalf("DeleteFromS3 = %v, want BadState", err)
				}
				if got.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE {
					t.Errorf("state = %v, want ACTIVE", got.GetStatus().GetState())
				}
				return
			}
			if err != nil {
				t.Fatalf("DeleteFromS3: %v", err)
			}
			st := got.GetStatus()
			if st.GetState() != storagepb.BucketStatus_SCHEDULED_FOR_DELETION || !st.GetPurgeAt().AsTime().Equal(t0.Add(grace)) ||
				!st.GetDeletedAt().AsTime().Equal(t0) {
				t.Errorf("status = %v, want scheduled with purge_at = now+grace", st)
			}
		})
	}
}

func TestUpdateSpecFromS3(t *testing.T) {
	t.Parallel()
	cors := corsOf(corsRule([]string{"*"}, []string{"GET"}, nil))

	t.Run("sets cors and bumps the version", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "upd", nil)
		f.clk.Advance(time.Hour)
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) { s.Cors = cors }); err != nil {
			t.Fatalf("UpdateSpecFromS3: %v", err)
		}
		got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId())
		if len(got.GetSpec().GetCors().GetRules()) != 1 ||
			got.GetMetadata().GetResourceVersion() != b.GetMetadata().GetResourceVersion()+1 ||
			!got.GetMetadata().GetUpdatedAt().AsTime().Equal(t0.Add(time.Hour)) {
			t.Fatalf("bucket after update = %v", got)
		}
	})

	t.Run("clearing a setting removes it and keeps the others", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "clear", &storagepb.BucketSpec{Cors: cors, MaxSizeBytes: 1 << 20})
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) { s.Cors = nil }); err != nil {
			t.Fatalf("UpdateSpecFromS3: %v", err)
		}
		got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId())
		if got.GetSpec().GetCors() != nil || got.GetSpec().GetMaxSizeBytes() != 1<<20 ||
			got.GetSpec().GetVersioningPolicy() != storagepb.VersioningPolicy_DISABLED {
			t.Fatalf("spec after clearing cors = %v; want no cors, max size and versioning kept", got.GetSpec())
		}
	})

	t.Run("no-op mutate keeps the version", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "noop", nil)
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(*storagepb.BucketSpec) {}); err != nil {
			t.Fatal(err)
		}
		got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId())
		if got.GetMetadata().GetResourceVersion() != b.GetMetadata().GetResourceVersion() {
			t.Errorf("resource_version = %d, want %d", got.GetMetadata().GetResourceVersion(), b.GetMetadata().GetResourceVersion())
		}
	})

	t.Run("validation errors propagate", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "invalid", nil)
		err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) {
			s.Cors = corsOf(corsRule([]string{"*"}, []string{"PATCH"}, nil))
		})
		if !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, "spec.cors.rules[0].allowed_methods[0]") {
			t.Fatalf("UpdateSpecFromS3 = %v, want a bad request naming the method", err)
		}
		got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId())
		if got.GetSpec().GetCors() != nil {
			t.Error("invalid CORS was stored")
		}
	})

	t.Run("versioning cannot be disabled again", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "keep-versions", &storagepb.BucketSpec{VersioningPolicy: storagepb.VersioningPolicy_ENABLED})
		err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) {
			s.VersioningPolicy = storagepb.VersioningPolicy_DISABLED
		})
		if !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, "spec.versioning_policy") {
			t.Fatalf("UpdateSpecFromS3 = %v, want a bad request on spec.versioning_policy", err)
		}
	})

	t.Run("enabling versioning reaches the backend", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "enable-versions", nil)
		before := f.backend.setCalls
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) {
			s.VersioningPolicy = storagepb.VersioningPolicy_ENABLED
		}); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.backend.versioning("enable-versions"); got != objstore.VersioningEnabled || f.backend.setCalls != before+1 {
			t.Errorf("backend versioning = %v after %d new calls, want enabled after 1", got, f.backend.setCalls-before)
		}
	})

	t.Run("anonymous policy sets the status", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "policy", nil)
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), func(s *storagepb.BucketSpec) {
			s.BucketPolicy = policyOf(anonRule([]string{"*"}, []string{"storage.object-viewer"}))
		}); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.svc.Get(t.Context(), b.GetMetadata().GetId()); !got.GetStatus().GetAnonymousAccessEnabled() {
			t.Error("anonymous_access_enabled = false")
		}
	})

	t.Run("unknown and deleted buckets are not found", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		noop := func(*storagepb.BucketSpec) {}
		if err := f.svc.UpdateSpecFromS3(t.Context(), "storagebucket-e00missing", noop); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("missing bucket: %v, want NotFound", err)
		}
		b := f.create(t, "gone", nil)
		if err := f.svc.DeleteFromS3(t.Context(), b.GetMetadata().GetId()); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.UpdateSpecFromS3(t.Context(), b.GetMetadata().GetId(), noop); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("scheduled bucket: %v, want NotFound", err)
		}
	})
}

func TestCheckCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		limit    int64
		stored   int64
		incoming int64
		wantErr  bool
	}{
		{"no limit", 0, 1000, 1000, false},
		{"under the limit", 100, 60, 39, false},
		{"exactly at the limit", 100, 60, 40, false},
		{"over the limit", 100, 60, 41, true},
		{"already full", 100, 100, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			b := f.create(t, "capped", &storagepb.BucketSpec{MaxSizeBytes: tt.limit})
			if tt.stored > 0 {
				f.content.put("data", "v1", baseTime, tt.stored, "etag", "STANDARD")
			}
			err := f.svc.CheckCapacity(t.Context(), b, tt.incoming)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("CheckCapacity = %v, want nil", err)
				}
				return
			}
			if !errs.IsKind(err, errs.KindQuotaExceeded) {
				t.Fatalf("CheckCapacity = %v, want QuotaExceeded", err)
			}
		})
	}
}

func TestObjectSize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.content.versioned = true
	f.content.
		put("doc", "v1", baseTime, 10, "etag", "STANDARD").
		put("doc", "v2", baseTime, 20, "etag", "STANDARD").
		put("other", "o1", baseTime, 99, "etag", "STANDARD")
	f.content.putMarker("gone", "m1", baseTime)

	tests := []struct {
		name    string
		key     string
		version string
		want    int64
		wantErr error
	}{
		{"current version", "doc", "", 20, nil},
		{"specific version", "doc", "v1", 10, nil},
		{"latest by id", "doc", "v2", 20, nil},
		{"unknown version", "doc", "v9", 0, objstore.ErrNotFound},
		{"missing key", "nothing", "", 0, objstore.ErrNotFound},
		{"delete marker", "gone", "", 0, objstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.svc.ObjectSize(t.Context(), "bkt", tt.key, tt.version)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("ObjectSize = %d, %v; want %d, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestApplyLifecycles(t *testing.T) {
	t.Parallel()
	expire := cfgOf(rule("expire", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(1) }))

	t.Run("expires old objects of a bucket with rules", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.create(t, "with-rules", &storagepb.BucketSpec{LifecycleConfiguration: expire})
		f.content.put("old", "", baseTime, 5, "etag", "STANDARD")

		if r, err := f.svc.ApplyLifecycles(t.Context()); err != nil || r != (domain.LifecycleResult{}) {
			t.Fatalf("before the object is due = %+v, %v; want nothing", r, err)
		}
		f.clk.Advance(72 * time.Hour)
		r, err := f.svc.ApplyLifecycles(t.Context())
		if err != nil {
			t.Fatalf("ApplyLifecycles: %v", err)
		}
		if r.Expired != 1 {
			t.Fatalf("result = %+v, want Expired 1", r)
		}
		if len(f.content.versions["old"]) != 0 {
			t.Errorf("object survived: %v", f.content.versions["old"])
		}
	})

	t.Run("leaves buckets without rules alone", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.create(t, "no-rules", nil)
		f.content.put("old", "", baseTime, 5, "etag", "STANDARD")
		f.clk.Advance(72 * time.Hour)
		r, err := f.svc.ApplyLifecycles(t.Context())
		if err != nil || r != (domain.LifecycleResult{}) {
			t.Fatalf("ApplyLifecycles = %+v, %v; want nothing", r, err)
		}
		if len(f.content.calls) != 0 || len(f.content.versions["old"]) != 1 {
			t.Errorf("backend touched: calls %v", f.content.calls)
		}
	})

	t.Run("skips soft-deleted buckets", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		b := f.create(t, "deleted", &storagepb.BucketSpec{LifecycleConfiguration: expire})
		if err := f.svc.DeleteFromS3(t.Context(), b.GetMetadata().GetId()); err != nil {
			t.Fatal(err)
		}
		f.content.put("old", "", baseTime, 5, "etag", "STANDARD")
		f.clk.Advance(72 * time.Hour)
		if r, err := f.svc.ApplyLifecycles(t.Context()); err != nil || r.Expired != 0 {
			t.Fatalf("ApplyLifecycles = %+v, %v; want nothing", r, err)
		}
	})
}
