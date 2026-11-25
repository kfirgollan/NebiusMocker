package domain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const rulesField = "spec.lifecycle_configuration.rules"

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func rule(id string, f func(*storagepb.LifecycleRule)) *storagepb.LifecycleRule {
	r := &storagepb.LifecycleRule{Id: id, Status: storagepb.LifecycleRule_ENABLED}
	if f != nil {
		f(r)
	}
	return r
}

func cfgOf(rs ...*storagepb.LifecycleRule) *storagepb.LifecycleConfiguration {
	return &storagepb.LifecycleConfiguration{Rules: rs}
}

func expDays(n int32) *storagepb.LifecycleExpiration {
	return &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: n}}
}

func expDate(t time.Time) *storagepb.LifecycleExpiration {
	return &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Date{Date: timestamppb.New(t)}}
}

func expMarker() *storagepb.LifecycleExpiration {
	return &storagepb.LifecycleExpiration{ExpiredObjectDeleteMarker: true}
}

func nonCurrent(days int32, newer *int32) *storagepb.LifecycleNoncurrentVersionExpiration {
	return &storagepb.LifecycleNoncurrentVersionExpiration{NoncurrentDays: days, NewerNoncurrentVersions: newer}
}

func abortAfter(days int32) *storagepb.LifecycleAbortIncompleteMultipartUpload {
	return &storagepb.LifecycleAbortIncompleteMultipartUpload{DaysAfterInitiation: days}
}

func ptr[T any](v T) *T { return &v }

func TestValidateLifecycle(t *testing.T) {
	longID := strings.Repeat("x", 256)
	withExp := func(e *storagepb.LifecycleExpiration) func(*storagepb.LifecycleRule) {
		return func(r *storagepb.LifecycleRule) { r.Expiration = e }
	}
	tests := []struct {
		name string
		cfg  *storagepb.LifecycleConfiguration
		want []string // violation fields, nil means valid
	}{
		{name: "nil", cfg: nil},
		{name: "valid mixed", cfg: cfgOf(
			rule("a", withExp(expDays(30))),
			rule("b", func(r *storagepb.LifecycleRule) {
				r.NoncurrentVersionExpiration = nonCurrent(7, ptr(int32(3)))
				r.AbortIncompleteMultipartUpload = abortAfter(1)
			}),
			rule("c", withExp(expDate(day(2027, 1, 1)))),
			rule("d", withExp(expMarker())),
		)},
		{name: "transition rejected by name", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Expiration = expDays(1)
			r.Transition = &storagepb.LifecycleTransition{}
		})), want: []string{rulesField + "[0].transition"}},
		{name: "noncurrent transition rejected by name", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.NoncurrentVersionTransition = &storagepb.LifecycleNoncurrentVersionTransition{}
		})), want: []string{rulesField + "[0].noncurrent_version_transition"}},
		{name: "last access filter rejected by name", cfg: &storagepb.LifecycleConfiguration{
			Rules:            []*storagepb.LifecycleRule{rule("a", withExp(expDays(1)))},
			LastAccessFilter: &storagepb.LifecycleAccessFilter{},
		}, want: []string{"spec.lifecycle_configuration.last_access_filter"}},
		{name: "id required", cfg: cfgOf(rule("", withExp(expDays(1)))), want: []string{rulesField + "[0].id"}},
		{name: "id too long", cfg: cfgOf(rule(longID, withExp(expDays(1)))), want: []string{rulesField + "[0].id"}},
		{
			name: "id duplicate", cfg: cfgOf(rule("a", withExp(expDays(1))), rule("b", withExp(expDays(1))), rule("a", withExp(expDays(1)))),
			want: []string{rulesField + "[2].id"},
		},
		{name: "status required", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Status = storagepb.LifecycleRule_STATUS_UNSPECIFIED
			r.Expiration = expDays(1)
		})), want: []string{rulesField + "[0].status"}},
		{name: "no action", cfg: cfgOf(rule("a", nil)), want: []string{rulesField + "[0]"}},
		{name: "expiration empty", cfg: cfgOf(rule("a", withExp(&storagepb.LifecycleExpiration{}))), want: []string{rulesField + "[0].expiration"}},
		{name: "days with marker", cfg: cfgOf(rule("a", withExp(&storagepb.LifecycleExpiration{
			ExpiredWith:               &storagepb.LifecycleExpiration_Days{Days: 1},
			ExpiredObjectDeleteMarker: true,
		}))), want: []string{rulesField + "[0].expiration"}},
		{name: "date with marker", cfg: cfgOf(rule("a", withExp(&storagepb.LifecycleExpiration{
			ExpiredWith:               &storagepb.LifecycleExpiration_Date{Date: timestamppb.New(day(2027, 1, 1))},
			ExpiredObjectDeleteMarker: true,
		}))), want: []string{rulesField + "[0].expiration"}},
		{
			name: "date not midnight", cfg: cfgOf(rule("a", withExp(expDate(day(2027, 1, 1).Add(time.Hour))))),
			want: []string{rulesField + "[0].expiration.date"},
		},
		{name: "days zero", cfg: cfgOf(rule("a", withExp(expDays(0)))), want: []string{rulesField + "[0].expiration.days"}},
		{name: "days too large", cfg: cfgOf(rule("a", withExp(expDays(18251)))), want: []string{rulesField + "[0].expiration.days"}},
		{name: "days max ok", cfg: cfgOf(rule("a", withExp(expDays(18250))))},
		{
			name: "noncurrent days zero", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) { r.NoncurrentVersionExpiration = nonCurrent(0, nil) })),
			want: []string{rulesField + "[0].noncurrent_version_expiration.noncurrent_days"},
		},
		{
			name: "newer noncurrent zero", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) { r.NoncurrentVersionExpiration = nonCurrent(1, ptr(int32(0))) })),
			want: []string{rulesField + "[0].noncurrent_version_expiration.newer_noncurrent_versions"},
		},
		{
			name: "newer noncurrent too many", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) { r.NoncurrentVersionExpiration = nonCurrent(1, ptr(int32(101))) })),
			want: []string{rulesField + "[0].noncurrent_version_expiration.newer_noncurrent_versions"},
		},
		{
			name: "abort days zero", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) { r.AbortIncompleteMultipartUpload = abortAfter(0) })),
			want: []string{rulesField + "[0].abort_incomplete_multipart_upload.days_after_initiation"},
		},
		{name: "abort with tags", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.AbortIncompleteMultipartUpload = abortAfter(1)
			r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: "k"}}}
		})), want: []string{rulesField + "[0].abort_incomplete_multipart_upload"}},
		{name: "marker with tags", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Expiration = expMarker()
			r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: "k"}}}
		})), want: []string{rulesField + "[0].expiration"}},
		{name: "negative sizes", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Expiration = expDays(1)
			r.Filter = &storagepb.LifecycleFilter{ObjectSizeGreaterThanBytes: -1, ObjectSizeLessThanBytes: -2}
		})), want: []string{rulesField + "[0].filter.object_size_greater_than_bytes", rulesField + "[0].filter.object_size_less_than_bytes"}},
		{name: "size bounds inverted", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Expiration = expDays(1)
			r.Filter = &storagepb.LifecycleFilter{ObjectSizeGreaterThanBytes: 10, ObjectSizeLessThanBytes: 10}
		})), want: []string{rulesField + "[0].filter"}},
		{name: "tag keys empty and duplicate", cfg: cfgOf(rule("a", func(r *storagepb.LifecycleRule) {
			r.Expiration = expDays(1)
			r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: ""}, {Key: "k"}, {Key: "k"}}}
		})), want: []string{rulesField + "[0].filter.tags[0].key", rulesField + "[0].filter.tags[2].key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.ValidateLifecycle(tt.cfg)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidateLifecycle = %v, want nil", err)
				}
				return
			}
			e, ok := errors.AsType[*errs.Error](err)
			if !ok || e.Kind != errs.KindBadRequest {
				t.Fatalf("ValidateLifecycle = %v, want a bad request", err)
			}
			var got []string
			for _, v := range e.Violations {
				got = append(got, v.Field)
			}
			slices.Sort(got)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("violation fields mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestValidateLifecycleMessages(t *testing.T) {
	err := domain.ValidateLifecycle(&storagepb.LifecycleConfiguration{
		Rules: []*storagepb.LifecycleRule{rule("a", func(r *storagepb.LifecycleRule) {
			r.Transition = &storagepb.LifecycleTransition{}
		})},
		LastAccessFilter: &storagepb.LifecycleAccessFilter{},
	})
	for _, want := range []string{
		"not supported by NebiusMocker: storage-class transitions are not emulated",
		"not supported by NebiusMocker: last-access tracking is not emulated",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not contain %q", err, want)
		}
	}
}

func TestCeilMidnightUTC(t *testing.T) {
	tests := []struct {
		name     string
		in, want time.Time
	}{
		{"exact midnight", day(2026, 1, 3), day(2026, 1, 3)},
		{"morning", time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC), day(2026, 1, 3)},
		{"one nanosecond past", day(2026, 1, 2).Add(time.Nanosecond), day(2026, 1, 3)},
		{"other zone", time.Date(2026, 1, 2, 23, 0, 0, 0, time.FixedZone("x", -2*3600)), day(2026, 1, 4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.CeilMidnightUTC(tt.in); !got.Equal(tt.want) {
				t.Errorf("CeilMidnightUTC(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestApplyLifecycle(t *testing.T) {
	// Objects are written at baseTime = 2026-01-01 10:00 UTC, so days=1 is due at
	// the midnight starting 2026-01-03.
	oneObject := func() *contentFake { return newContentFake(true).put("a", "v1", baseTime, 10, "e", "") }
	tests := []struct {
		name      string
		backend   func() *contentFake
		cfg       *storagepb.LifecycleConfiguration
		now       time.Time
		want      domain.LifecycleResult
		wantCalls []string
	}{
		{
			name:    "days rounds up to the next midnight: not due the evening before",
			backend: oneObject,
			cfg:     cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(1) })),
			now:     time.Date(2026, 1, 2, 23, 59, 59, 0, time.UTC),
		},
		{
			name:      "days due at the midnight two days later",
			backend:   oneObject,
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(1) })),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete a"},
		},
		{
			name:    "date rule not yet due",
			backend: oneObject,
			cfg:     cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDate(day(2026, 1, 5)) })),
			now:     day(2026, 1, 4),
		},
		{
			name:      "date rule due",
			backend:   oneObject,
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDate(day(2026, 1, 5)) })),
			now:       day(2026, 1, 5),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete a"},
		},
		{
			name: "unversioned bucket removes the object",
			backend: func() *contentFake {
				return newContentFake(false).put("a", objstore.NullVersion, baseTime, 10, "e", "")
			},
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(1) })),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete a"},
		},
		{
			name: "prefix filter",
			backend: func() *contentFake {
				return newContentFake(true).put("data/y", "v", baseTime, 1, "e", "").put("logs/x", "v", baseTime, 1, "e", "")
			},
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) {
				r.Expiration = expDays(1)
				r.Filter = &storagepb.LifecycleFilter{Prefix: "logs/"}
			})),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete logs/x"},
		},
		{
			name: "size filter is strict on both bounds",
			backend: func() *contentFake {
				return newContentFake(true).
					put("s10", "v", baseTime, 10, "e", "").put("s50", "v", baseTime, 50, "e", "").put("s100", "v", baseTime, 100, "e", "")
			},
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) {
				r.Expiration = expDays(1)
				r.Filter = &storagepb.LifecycleFilter{ObjectSizeGreaterThanBytes: 10, ObjectSizeLessThanBytes: 100}
			})),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete s50"},
		},
		{
			name: "tag filter needs every tag to match",
			backend: func() *contentFake {
				return newContentFake(true).
					put("dev", "v", baseTime, 1, "e", "").tag("dev", "v", map[string]string{"env": "dev", "x": "1"}).
					put("prod", "v", baseTime, 1, "e", "").tag("prod", "v", map[string]string{"env": "prod", "x": "1"}).
					put("untagged", "v", baseTime, 1, "e", "")
			},
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) {
				r.Expiration = expDays(1)
				r.Filter = &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: "env", Value: "dev"}, {Key: "x", Value: "1"}}}
			})),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{Expired: 1},
			wantCalls: []string{"Delete dev"},
		},
		{
			name:    "disabled rule is ignored",
			backend: oneObject,
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) {
				r.Status = storagepb.LifecycleRule_DISABLED
				r.Expiration = expDays(1)
			})),
			now: day(2026, 2, 1),
		},
		{
			name: "expiration does not touch a key whose latest entry is a delete marker",
			backend: func() *contentFake {
				return newContentFake(true).put("a", "v1", baseTime, 1, "e", "").putMarker("a", "m", baseTime)
			},
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(1) })),
			now: day(2026, 2, 1),
		},
		{
			name: "expired delete marker cleanup only for lone markers",
			backend: func() *contentFake {
				return newContentFake(true).
					putMarker("lone", "m1", baseTime).
					put("kept", "v1", baseTime, 1, "e", "").putMarker("kept", "m2", baseTime)
			},
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.Expiration = expMarker() })),
			now:       day(2026, 1, 2),
			want:      domain.LifecycleResult{MarkersRemoved: 1},
			wantCalls: []string{"DeleteVersion lone m1"},
		},
		{
			name: "noncurrent days counts from when the newer version arrived",
			backend: func() *contentFake {
				return newContentFake(true).
					put("k", "v1", baseTime, 1, "e", "").
					put("k", "v2", baseTime.AddDate(0, 0, 5), 1, "e", "").
					put("k", "v3", baseTime.AddDate(0, 0, 10), 1, "e", "")
			},
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.NoncurrentVersionExpiration = nonCurrent(1, nil) })),
			now:       day(2026, 1, 10), // v1 noncurrent since 01-06 10:00, due 01-08; v2 since 01-11 10:00, due 01-13
			want:      domain.LifecycleResult{NoncurrentExpired: 1},
			wantCalls: []string{"DeleteVersion k v1"},
		},
		{
			name: "newer noncurrent versions are kept",
			backend: func() *contentFake {
				return newContentFake(true).
					put("k", "v1", baseTime, 1, "e", "").
					put("k", "v2", baseTime.AddDate(0, 0, 5), 1, "e", "").
					put("k", "v3", baseTime.AddDate(0, 0, 10), 1, "e", "")
			},
			cfg:       cfgOf(rule("r", func(r *storagepb.LifecycleRule) { r.NoncurrentVersionExpiration = nonCurrent(1, ptr(int32(1))) })),
			now:       day(2026, 2, 1),
			want:      domain.LifecycleResult{NoncurrentExpired: 1},
			wantCalls: []string{"DeleteVersion k v1"},
		},
		{
			name: "abort incomplete uploads by prefix and age",
			backend: func() *contentFake {
				f := newContentFake(true)
				f.uploads = []objstore.Upload{
					{Key: "tmp/a", UploadID: "1", Initiated: baseTime},
					{Key: "tmp/b", UploadID: "2", Initiated: baseTime.AddDate(0, 0, 1)},
					{Key: "keep/c", UploadID: "3", Initiated: baseTime},
				}
				return f
			},
			cfg: cfgOf(rule("r", func(r *storagepb.LifecycleRule) {
				r.AbortIncompleteMultipartUpload = abortAfter(1)
				r.Filter = &storagepb.LifecycleFilter{Prefix: "tmp/"}
			})),
			now:       day(2026, 1, 3),
			want:      domain.LifecycleResult{UploadsAborted: 1},
			wantCalls: []string{"AbortUpload tmp/a 1"},
		},
		{
			name: "overlapping rules act once per version and upload",
			backend: func() *contentFake {
				f := newContentFake(true).
					put("k", "v1", baseTime, 1, "e", "").
					put("k", "v2", baseTime, 1, "e", "").
					put("k", "v3", baseTime, 1, "e", "")
				f.uploads = []objstore.Upload{{Key: "k", UploadID: "1", Initiated: baseTime}}
				return f
			},
			cfg: cfgOf(
				rule("days", func(r *storagepb.LifecycleRule) {
					r.Expiration = expDays(1)
					r.NoncurrentVersionExpiration = nonCurrent(1, nil)
					r.AbortIncompleteMultipartUpload = abortAfter(1)
				}),
				rule("date", func(r *storagepb.LifecycleRule) {
					r.Expiration = expDate(day(2026, 1, 2))
					r.NoncurrentVersionExpiration = nonCurrent(1, nil)
					r.AbortIncompleteMultipartUpload = abortAfter(1)
				}),
			),
			now:  day(2026, 1, 3),
			want: domain.LifecycleResult{Expired: 1, NoncurrentExpired: 2, UploadsAborted: 1},
			wantCalls: []string{
				"Delete k", "DeleteVersion k v2", "DeleteVersion k v1", "AbortUpload k 1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.backend()
			got, err := domain.ApplyLifecycle(t.Context(), f, "bkt", tt.cfg, tt.now)
			if err != nil {
				t.Fatalf("ApplyLifecycle: %v", err)
			}
			if got != tt.want {
				t.Errorf("result = %+v, want %+v", got, tt.want)
			}
			if diff := cmp.Diff(sortedCalls(tt.wantCalls), sortedCalls(f.calls)); diff != "" {
				t.Errorf("backend calls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func sortedCalls(calls []string) []string {
	if calls == nil {
		return nil
	}
	return slices.Sorted(slices.Values(calls))
}
