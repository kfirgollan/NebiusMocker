package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// K8sWebhookPath is the Kubernetes webhook token authentication endpoint of
// one Managed Kubernetes cluster: the cluster's API server posts a TokenReview
// here (--authentication-token-webhook-config-file).
const K8sWebhookPath = "/mk8s/v1/clusters/{cluster_id}/tokenreview"

// maxTokenReviewBytes bounds the TokenReview request body.
const maxTokenReviewBytes = 1 << 20

// K8sReviewService maps a Nebius token to a Kubernetes identity.
type K8sReviewService interface {
	Review(ctx context.Context, clusterID, token string) (domain.KubeIdentity, error)
}

// K8sWebhookHandler serves Kubernetes TokenReview requests (authentication.k8s.io).
type K8sWebhookHandler struct {
	svc K8sReviewService
	log *slog.Logger
}

var _ http.Handler = (*K8sWebhookHandler)(nil)

// NewK8sWebhookHandler returns a handler backed by svc.
func NewK8sWebhookHandler(svc K8sReviewService, log *slog.Logger) *K8sWebhookHandler {
	return &K8sWebhookHandler{svc: svc, log: log}
}

type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec"`
	Status     *tokenReviewState `json:"status,omitempty"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewState struct {
	Authenticated bool            `json:"authenticated"`
	User          *tokenReviewUsr `json:"user,omitempty"`
	Audiences     []string        `json:"audiences,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type tokenReviewUsr struct {
	Username string   `json:"username"`
	UID      string   `json:"uid"`
	Groups   []string `json:"groups,omitempty"`
}

// ServeHTTP handles POST /mk8s/v1/clusters/{cluster_id}/tokenreview.
func (h *K8sWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req tokenReview
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTokenReviewBytes)).Decode(&req); err != nil {
		http.Error(w, "malformed TokenReview", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	id, err := h.svc.Review(ctx, r.PathValue("cluster_id"), req.Spec.Token)
	var status *tokenReviewState
	switch {
	case err == nil:
		status = &tokenReviewState{
			Authenticated: true, Audiences: req.Spec.Audiences,
			User: &tokenReviewUsr{Username: id.Username, UID: id.UID, Groups: id.Groups},
		}
	case errs.IsKind(err, errs.KindUnauthenticated):
		status = &tokenReviewState{Error: err.Error()}
	case errs.IsKind(err, errs.KindBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	default:
		h.log.ErrorContext(ctx, "k8s token review failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(tokenReview{APIVersion: req.APIVersion, Kind: "TokenReview", Status: status}); err != nil {
		h.log.WarnContext(ctx, "k8s token review: write response", "error", err)
	}
}
