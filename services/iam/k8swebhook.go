package iam

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// registerK8sWebhook serves the Kubernetes token review endpoint that each
// Managed Kubernetes cluster's API server calls to authenticate Nebius
// tokens. It is a plain HTTP endpoint on the edge port, outside the gRPC
// interceptors: the request carries the token to review, not a Nebius caller.
func registerK8sWebhook(deps mocker.Deps) error {
	reviewer, err := domain.NewK8sReviewer(deps.Authenticator, deps.Authz, deps.Config.Authz, deps.Log)
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	if deps.HTTP != nil {
		deps.HTTP.HandleHTTP("POST "+api.K8sWebhookPath, api.NewK8sWebhookHandler(reviewer, deps.Log))
	}
	return nil
}
