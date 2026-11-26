package domain_test

import (
	"encoding/base64"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// pager lists one page of some listing.
type pager func(env *env, size int64, token string) (ids []string, next string, err error)

func blockGroupPager(t *testing.T) pager {
	t.Helper()
	return func(e *env, size int64, token string) ([]string, string, error) {
		p, err := e.svc.ListBlockGroups(t.Context(), tenantID, size, token)
		var ids []string
		for _, g := range p.Items {
			ids = append(ids, g.GetMetadata().GetId())
		}
		return ids, p.NextPageToken, err
	}
}

func intervalPager(t *testing.T) pager {
	t.Helper()
	return func(e *env, size int64, token string) ([]string, string, error) {
		p, err := e.svc.ListIntervals(t.Context(), gid(t, "gap"), size, token)
		var ids []string
		for _, iv := range p.Items {
			ids = append(ids, iv.GetMetadata().GetId())
		}
		return ids, p.NextPageToken, err
	}
}

func allowancePager(t *testing.T) pager {
	t.Helper()
	return func(e *env, size int64, token string) ([]string, string, error) {
		p, err := e.svc.ListAllowances(t.Context(), projectID, size, token)
		var ids []string
		for _, a := range p.Items {
			ids = append(ids, a.GetSpec().GetCapacityBlockGroupId())
		}
		return ids, p.NextPageToken, err
	}
}

func advicePager(t *testing.T) pager {
	t.Helper()
	return func(e *env, size int64, token string) ([]string, string, error) {
		p, err := e.svc.ListResourceAdvice(t.Context(), tenantID, size, token)
		var ids []string
		for _, a := range p.Items {
			ids = append(ids, a.GetSpec().GetFabric()+"/"+a.GetSpec().GetComputeInstance().GetPreset().GetName())
		}
		return ids, p.NextPageToken, err
	}
}

func TestPaging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pager   func(*testing.T) pager
		total   int
		maxSize int64
	}{
		{"block groups", blockGroupPager, 5, 200},
		{"intervals", intervalPager, 3, 200},
		{"allowances", allowancePager, 5, 1000},
		{"resource advice", advicePager, 5, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, t0)
			list := tt.pager(t)

			all, next, err := list(e, 0, "")
			if err != nil || len(all) != tt.total || next != "" {
				t.Fatalf("default page = %d items, token %q, %v; want %d, none", len(all), next, err, tt.total)
			}

			// Walking the pages of size 2 yields the same items in order.
			var walked []string
			token := ""
			for range tt.total {
				ids, n, err := list(e, 2, token)
				if err != nil {
					t.Fatalf("page %q error: %v", token, err)
				}
				if len(ids) > 2 {
					t.Fatalf("page %q has %d items, want at most 2", token, len(ids))
				}
				walked = append(walked, ids...)
				if token = n; token == "" {
					break
				}
			}
			if token != "" {
				t.Errorf("still a next page token %q after %d items", token, len(walked))
			}
			if diff := cmp.Diff(all, walked); diff != "" {
				t.Errorf("paged items differ from the full listing (-all +walked):\n%s", diff)
			}

			if ids, n, err := list(e, tt.maxSize, ""); err != nil || len(ids) != tt.total || n != "" {
				t.Errorf("max page size = %d items, token %q, %v; want %d, none", len(ids), n, err, tt.total)
			}
			for _, size := range []int64{-1, tt.maxSize + 1} {
				if _, _, err := list(e, size, ""); !errs.IsKind(err, errs.KindBadRequest) {
					t.Errorf("page_size %d error = %v, want bad_request", size, err)
				}
			}

			past := base64.RawURLEncoding.EncodeToString([]byte("100"))
			if ids, n, err := list(e, 2, past); err != nil || len(ids) != 0 || n != "" {
				t.Errorf("page past the end = %v, token %q, %v; want empty", ids, n, err)
			}
			malformed := []string{
				"%%%",
				"!not base64!",
				base64.RawURLEncoding.EncodeToString([]byte("abc")),
				base64.RawURLEncoding.EncodeToString([]byte("-3")),
			}
			for _, bad := range malformed {
				if _, _, err := list(e, 2, bad); !errs.IsKind(err, errs.KindBadRequest) {
					t.Errorf("page_token %q error = %v, want bad_request", bad, err)
				}
			}
		})
	}
}

func TestPageSizeOfListByGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, t0)
	e.newAllowance(t, projectID, gid(t, "a"), u64(1))
	if _, err := e.svc.ListAllowancesByGroup(t.Context(), gid(t, "a"), 1001, ""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("ListAllowancesByGroup(page_size 1001) error = %v, want bad_request", err)
	}
	if _, err := e.svc.ListAllowancesByGroup(t.Context(), gid(t, "a"), 1000, ""); err != nil {
		t.Errorf("ListAllowancesByGroup(page_size 1000) error: %v", err)
	}
	if _, err := e.svc.ListAllowancesByGroup(t.Context(), gid(t, "a"), 1, "@@"); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("ListAllowancesByGroup(malformed token) error = %v, want bad_request", err)
	}
}
