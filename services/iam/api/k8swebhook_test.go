package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

type fakeReviewer struct {
	id  domain.KubeIdentity
	err error
}

func (f fakeReviewer) Review(_ context.Context, clusterID, token string) (domain.KubeIdentity, error) {
	if clusterID != "mk8scluster-1" || token != "tok" {
		return domain.KubeIdentity{}, errs.Unauthenticated("bad input")
	}
	return f.id, f.err
}

func postReview(t *testing.T, rv fakeReviewer, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST "+api.K8sWebhookPath, api.NewK8sWebhookHandler(rv, slog.Default()))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mk8s/v1/clusters/mk8scluster-1/tokenreview", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type reviewResp struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Authenticated bool     `json:"authenticated"`
		Audiences     []string `json:"audiences"`
		Error         string   `json:"error"`
		User          struct {
			Username string   `json:"username"`
			UID      string   `json:"uid"`
			Groups   []string `json:"groups"`
		} `json:"user"`
	} `json:"status"`
}

func TestK8sWebhook(t *testing.T) {
	ok := fakeReviewer{id: domain.KubeIdentity{Username: "u-1", UID: "u-1", Groups: []string{domain.KubeGroupEditor}}}
	tests := []struct {
		name     string
		rv       fakeReviewer
		body     string
		wantCode int
		check    func(t *testing.T, r reviewResp)
	}{
		{
			name: "success", rv: ok, wantCode: 200,
			body: `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"tok","audiences":["a"]}}`,
			check: func(t *testing.T, r reviewResp) {
				t.Helper()
				if r.APIVersion != "authentication.k8s.io/v1" || r.Kind != "TokenReview" || !r.Status.Authenticated {
					t.Errorf("resp = %+v", r)
				}
				if r.Status.User.Username != "u-1" || r.Status.User.UID != "u-1" || !slices.Equal(r.Status.User.Groups, []string{"nebius:editor"}) {
					t.Errorf("user = %+v", r.Status.User)
				}
				if !slices.Equal(r.Status.Audiences, []string{"a"}) {
					t.Errorf("audiences = %v", r.Status.Audiences)
				}
			},
		},
		{
			name: "v1beta1 echoed", rv: ok, wantCode: 200,
			body: `{"apiVersion":"authentication.k8s.io/v1beta1","kind":"TokenReview","spec":{"token":"tok"}}`,
			check: func(t *testing.T, r reviewResp) {
				t.Helper()
				if r.APIVersion != "authentication.k8s.io/v1beta1" || !r.Status.Authenticated {
					t.Errorf("resp = %+v", r)
				}
			},
		},
		{
			name: "unauthenticated", rv: ok, wantCode: 200,
			body: `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"wrong"}}`,
			check: func(t *testing.T, r reviewResp) {
				t.Helper()
				if r.Status.Authenticated || r.Status.Error == "" || r.Status.User.Username != "" {
					t.Errorf("status = %+v", r.Status)
				}
			},
		},
		{name: "malformed", rv: ok, wantCode: 400, body: `{not json`},
		{
			name: "bad request", wantCode: 400,
			rv:   fakeReviewer{err: errs.BadRequest(errs.Violation{Field: "cluster_id", Message: "bad"})},
			body: `{"apiVersion":"authentication.k8s.io/v1","spec":{"token":"tok"}}`,
		},
		{
			name: "internal", wantCode: 500,
			rv:   fakeReviewer{err: errors.New("db down")},
			body: `{"apiVersion":"authentication.k8s.io/v1","spec":{"token":"tok"}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postReview(t, tc.rv, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body)
			}
			if tc.wantCode == 500 && strings.Contains(rec.Body.String(), "db down") {
				t.Errorf("internal error leaked: %s", rec.Body)
			}
			if tc.check == nil {
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type = %q", ct)
			}
			var got reviewResp
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			tc.check(t, got)
		})
	}
}
