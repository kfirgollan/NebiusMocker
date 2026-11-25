package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// violationFields returns the sorted violation fields of a bad request.
func violationFields(t *testing.T, err error) []string {
	t.Helper()
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || e.Kind != errs.KindBadRequest {
		t.Fatalf("error = %v, want a bad request", err)
	}
	var got []string
	for _, v := range e.Violations {
		got = append(got, v.Field)
	}
	slices.Sort(got)
	return got
}

func groupRule(group string, paths, roles []string) *storagepb.BucketPolicy_Rule {
	return &storagepb.BucketPolicy_Rule{
		Paths: paths, Roles: roles,
		Subject: &storagepb.BucketPolicy_Rule_GroupId{GroupId: group},
	}
}

func anonRule(paths, roles []string) *storagepb.BucketPolicy_Rule {
	return &storagepb.BucketPolicy_Rule{
		Paths: paths, Roles: roles,
		Subject: &storagepb.BucketPolicy_Rule_Anonymous{Anonymous: &storagepb.BucketPolicy_Rule_AnonymousAccess{}},
	}
}

func policyOf(rs ...*storagepb.BucketPolicy_Rule) *storagepb.BucketPolicy {
	return &storagepb.BucketPolicy{Rules: rs}
}

func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	all := []string{"*"}
	tests := []struct {
		name   string
		policy *storagepb.BucketPolicy
		want   []string // violation fields; nil means valid
	}{
		{"nil policy", nil, nil},
		{"group rule", policyOf(groupRule("group-1", []string{"logs/*", "a/b.txt", "img?.png"}, []string{"storage.editor", "storage.viewer"})), nil},
		{"anonymous object-lister", policyOf(anonRule(all, []string{"storage.object-lister"})), nil},
		{"anonymous viewer and object-viewer", policyOf(anonRule(all, []string{"storage.viewer", "storage.object-viewer"})), nil},
		{
			"empty paths", policyOf(groupRule("g", nil, []string{"storage.viewer"})),
			[]string{"spec.bucket_policy.rules[0].paths"},
		},
		{
			"empty path string", policyOf(groupRule("g", []string{""}, []string{"storage.viewer"})),
			[]string{"spec.bucket_policy.rules[0].paths[0]"},
		},
		{
			"wildcard in the middle", policyOf(groupRule("g", []string{"a*b"}, []string{"storage.viewer"})),
			[]string{"spec.bucket_policy.rules[0].paths[0]"},
		},
		{
			"double wildcard", policyOf(groupRule("g", []string{"ok/*", "**"}, []string{"storage.viewer"})),
			[]string{"spec.bucket_policy.rules[0].paths[1]"},
		},
		{
			"no roles", policyOf(groupRule("g", all, nil)),
			[]string{"spec.bucket_policy.rules[0].roles"},
		},
		{
			"unknown role", policyOf(groupRule("g", all, []string{"storage.viewer", "storage.superuser"})),
			[]string{"spec.bucket_policy.rules[0].roles[1]"},
		},
		{
			"anonymous editor", policyOf(anonRule(all, []string{"storage.object-lister", "storage.editor"})),
			[]string{"spec.bucket_policy.rules[0].roles[1]"},
		},
		{
			"anonymous unknown role", policyOf(anonRule(all, []string{"nope"})),
			[]string{"spec.bucket_policy.rules[0].roles[0]"},
		},
		{
			"missing subject", policyOf(&storagepb.BucketPolicy_Rule{Paths: all, Roles: []string{"storage.viewer"}}),
			[]string{"spec.bucket_policy.rules[0]"},
		},
		{"second rule indexed", policyOf(
			groupRule("g", all, []string{"storage.viewer"}),
			groupRule("g", all, []string{"bad"}),
		), []string{"spec.bucket_policy.rules[1].roles[0]"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := domain.ValidatePolicy(tt.policy)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidatePolicy = %v, want nil", err)
				}
				return
			}
			if diff := cmp.Diff(tt.want, violationFields(t, err)); diff != "" {
				t.Errorf("violation fields mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAnonymousAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy *storagepb.BucketPolicy
		want   bool
	}{
		{"nil", nil, false},
		{"empty", policyOf(), false},
		{"group only", policyOf(groupRule("g", []string{"*"}, []string{"storage.viewer"})), false},
		{"anonymous among groups", policyOf(
			groupRule("g", []string{"*"}, []string{"storage.viewer"}),
			anonRule([]string{"*"}, []string{"storage.object-viewer"}),
		), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.AnonymousAccess(tt.policy); got != tt.want {
				t.Errorf("AnonymousAccess = %v, want %v", got, tt.want)
			}
		})
	}
}
