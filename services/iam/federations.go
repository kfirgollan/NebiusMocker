package iam

import (
	"google.golang.org/grpc"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
)

// registerFederations serves FederationService and FederationCertificateService.
func registerFederations(reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) error {
	return nil
}
