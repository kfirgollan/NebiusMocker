package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// createErr runs BucketService.Create and returns its (synchronous) error.
func createErr(t *testing.T, f *fixture, name string, spec *storagepb.BucketSpec) error {
	t.Helper()
	_, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}, Spec: spec,
	})
	return err
}

func hasViolation(err error, field string) bool {
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		return false
	}
	return slices.ContainsFunc(e.Violations, func(v errs.Violation) bool { return v.Field == field })
}

func TestValidateSpecThroughCreate(t *testing.T) {
	t.Parallel()
	transition := rule("t", func(r *storagepb.LifecycleRule) {
		r.Transition = &storagepb.LifecycleTransition{
			TransitedWith: &storagepb.LifecycleTransition_Days{Days: 1}, StorageClass: storagepb.StorageClass_ENHANCED_THROUGHPUT,
		}
	})
	tests := []struct {
		name      string
		spec      *storagepb.BucketSpec
		wantField string // empty means accepted
	}{
		{"audit logging NONE", &storagepb.BucketSpec{ObjectAuditLogging: storagepb.BucketSpec_NONE}, ""},
		{"audit logging ALL", &storagepb.BucketSpec{ObjectAuditLogging: storagepb.BucketSpec_ALL}, "spec.object_audit_logging"},
		{"audit logging MUTATE_ONLY", &storagepb.BucketSpec{ObjectAuditLogging: storagepb.BucketSpec_MUTATE_ONLY}, "spec.object_audit_logging"},
		{"last access with a condition", &storagepb.BucketSpec{LastAccess: &storagepb.LastAccessConfiguration{
			Conditions: []*storagepb.LastAccessConfiguration_Condition{{}},
		}}, "spec.last_access"},
		{"last access without conditions", &storagepb.BucketSpec{LastAccess: &storagepb.LastAccessConfiguration{}}, ""},
		{"default class INTELLIGENT", &storagepb.BucketSpec{DefaultStorageClass: storagepb.StorageClass_INTELLIGENT}, "spec.default_storage_class"},
		{"default class FILESYSTEM", &storagepb.BucketSpec{DefaultStorageClass: storagepb.StorageClass_FILESYSTEM}, "spec.default_storage_class"},
		{"default class ENHANCED_THROUGHPUT", &storagepb.BucketSpec{DefaultStorageClass: storagepb.StorageClass_ENHANCED_THROUGHPUT}, ""},
		{
			"lifecycle transition", &storagepb.BucketSpec{LifecycleConfiguration: cfgOf(transition)},
			"spec.lifecycle_configuration.rules[0].transition",
		},
		{"lifecycle expiration", &storagepb.BucketSpec{LifecycleConfiguration: cfgOf(rule("e", func(r *storagepb.LifecycleRule) { r.Expiration = expDays(3) }))}, ""},
		{
			"invalid CORS", &storagepb.BucketSpec{Cors: corsOf(corsRule([]string{"*"}, []string{"PATCH"}, nil))},
			"spec.cors.rules[0].allowed_methods[0]",
		},
		{"valid CORS", &storagepb.BucketSpec{Cors: corsOf(corsRule([]string{"*"}, []string{"GET"}, nil))}, ""},
		{
			"invalid policy", &storagepb.BucketSpec{BucketPolicy: policyOf(groupRule("g", []string{"*"}, []string{"bogus"}))},
			"spec.bucket_policy.rules[0].roles[0]",
		},
		{"valid policy", &storagepb.BucketSpec{BucketPolicy: policyOf(groupRule("g", []string{"*"}, []string{"storage.viewer"}))}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			err := createErr(t, f, "spec-bucket", tt.spec)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Create = %v, want success", err)
				}
				return
			}
			if !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, tt.wantField) {
				t.Fatalf("Create = %v, want a bad request naming %s", err, tt.wantField)
			}
		})
	}
}

func TestAnonymousAccessStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	anon := policyOf(anonRule([]string{"*"}, []string{"storage.object-viewer"}))
	b := f.create(t, "public", &storagepb.BucketSpec{BucketPolicy: anon})
	if !b.GetStatus().GetAnonymousAccessEnabled() {
		t.Fatal("anonymous_access_enabled = false after create with an anonymous rule")
	}
	if g := f.create(t, "private", &storagepb.BucketSpec{
		BucketPolicy: policyOf(groupRule("g", []string{"*"}, []string{"storage.viewer"})),
	}); g.GetStatus().GetAnonymousAccessEnabled() {
		t.Error("anonymous_access_enabled = true for a group-only policy")
	}

	if _, err := f.svc.Update(t.Context(), domain.UpdateBucketCmd{
		Metadata: &commonpb.ResourceMetadata{Id: b.GetMetadata().GetId()}, Spec: &storagepb.BucketSpec{},
		ResetMask: mask.ParseMust("spec.bucket_policy"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := f.svc.Get(t.Context(), b.GetMetadata().GetId())
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetBucketPolicy() != nil || got.GetStatus().GetAnonymousAccessEnabled() {
		t.Errorf("after removing the policy: policy = %v, anonymous = %v", got.GetSpec().GetBucketPolicy(), got.GetStatus().GetAnonymousAccessEnabled())
	}
}

func TestBucketCounters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.content.versioned = true
	f.content.
		put("simple", "s1", baseTime, 10, "etag", "STANDARD").
		put("multi", "m1", baseTime, 30, "etag-2", "STANDARD").
		put("old", "o1", baseTime, 5, "etag", "STANDARD").
		put("old", "o2", baseTime, 8, "etag", "STANDARD").
		put("fast", "f1", baseTime, 7, "etag", "REDUCED_REDUNDANCY")
	b := f.create(t, "counted", nil)

	want := []*storagepb.BucketCounters{
		counters(
			&storagepb.CurrentBucketCounters{SimpleObjectsQuantity: 2, SimpleObjectsSize: 18, MultipartObjectsQuantity: 1, MultipartObjectsSize: 30},
			&storagepb.NonCurrentBucketCounters{SimpleObjectsQuantity: 1, SimpleObjectsSize: 5},
			storagepb.StorageClass_STANDARD),
		counters(&storagepb.CurrentBucketCounters{SimpleObjectsQuantity: 1, SimpleObjectsSize: 7}, nil, storagepb.StorageClass_ENHANCED_THROUGHPUT),
	}
	check := func(name string, got *storagepb.Bucket) {
		t.Helper()
		if diff := cmp.Diff(want, got.GetStatus().GetCounters(), protocmp.Transform()); diff != "" {
			t.Errorf("%s counters mismatch (-want +got):\n%s", name, diff)
		}
	}
	got, err := f.svc.Get(t.Context(), b.GetMetadata().GetId())
	if err != nil {
		t.Fatal(err)
	}
	check("Get", got)
	byName, err := f.svc.GetByName(t.Context(), projectID, "counted")
	if err != nil {
		t.Fatal(err)
	}
	check("GetByName", byName)
	page, err := f.svc.List(t.Context(), projectID, 0, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("List = %v, %v; want one bucket", page, err)
	}
	check("List", page.Items[0])
}
