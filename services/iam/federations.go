package iam

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// registerFederations serves FederationService and FederationCertificateService.
func registerFederations(reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) error {
	feds := dal.NewFederationRepo(deps.Store)
	certs := dal.NewFederationCertificateRepo(deps.Store)

	deps.Authz.RegisterKind(domain.KindFederation, func(ctx context.Context, id string) (string, error) {
		f, err := feds.Get(ctx, id)
		return f.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindFederationCertificate, func(ctx context.Context, id string) (string, error) {
		c, err := certs.Get(ctx, id)
		return c.GetMetadata().GetParentId(), err
	})

	fedSvc, err := domain.NewFederationService(domain.FederationDeps{
		Federations: feds, Certificates: certs, Tenants: r.tenants, Users: r.users,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	certSvc, err := domain.NewFederationCertificateService(domain.FederationCertificateDeps{
		Certificates: certs, Federations: feds, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
	})
	if err != nil {
		return fmt.Errorf("iam: %w", err)
	}
	iampb.RegisterFederationServiceServer(reg, api.NewFederationHandler(fedSvc))
	iampb.RegisterFederationCertificateServiceServer(reg, api.NewFederationCertificateHandler(certSvc))
	api.RegisterFederationRules(deps.Rules)
	return nil
}
