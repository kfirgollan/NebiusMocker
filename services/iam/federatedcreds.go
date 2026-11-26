package iam

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/grpc"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/services/iam/api"
	"github.com/kfirgollan/NebiusMocker/services/iam/backend/oidc"
	"github.com/kfirgollan/NebiusMocker/services/iam/dal"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// federated is what the token exchange needs from the federated credentials
// slice: the credentials of a service account and the OIDC key fetcher.
type federated struct {
	creds *dal.FederatedCredentialsRepo
	oidc  *oidc.Client
}

// registerFederatedCredentials serves FederatedCredentialsService, registers
// its resource kind and, unless disabled, the fake OIDC issuer on the edge
// HTTP mux.
func registerFederatedCredentials(reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) (federated, error) {
	creds := dal.NewFederatedCredentialsRepo(deps.Store)
	svc, err := domain.NewFederatedCredentialsService(domain.FederatedCredentialsDeps{
		Credentials: creds, ServiceAccounts: r.sas, Projects: r.projects, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Revoker: deps.Tokens,
	})
	if err != nil {
		return federated{}, fmt.Errorf("iam: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindFederatedCredentials, func(ctx context.Context, id string) (string, error) {
		c, err := creds.Get(ctx, id)
		return c.GetMetadata().GetParentId(), err
	})
	iampb.RegisterFederatedCredentialsServiceServer(reg, api.NewFederatedCredentialsHandler(svc))
	api.RegisterFederatedCredentialsRules(deps.Rules)

	roots, err := ownCertificates(deps.TLS)
	if err != nil {
		return federated{}, err
	}
	client, err := oidc.New(deps.Clock, oidc.Options{ExtraRoots: roots})
	if err != nil {
		return federated{}, fmt.Errorf("iam: %w", err)
	}
	if deps.HTTP != nil && deps.Config.Auth.FakeOIDC {
		issuers, err := domain.NewFakeOIDCIssuers(deps.Clock)
		if err != nil {
			return federated{}, fmt.Errorf("iam: %w", err)
		}
		api.NewFakeOIDCHandler(issuers, deps.Log).Register(deps.HTTP)
	}
	return federated{creds: creds, oidc: client}, nil
}

// ownCertificates is the mocker's own TLS certificate chain, trusted when it
// fetches an issuer's keys: the fake issuer is served on the TLS edge port.
func ownCertificates(cfg *tls.Config) ([]*x509.Certificate, error) {
	if cfg == nil {
		return nil, nil
	}
	var out []*x509.Certificate
	for _, cert := range cfg.Certificates {
		for _, der := range cert.Certificate {
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, fmt.Errorf("iam: parse the TLS certificate: %w", err)
			}
			out = append(out, c)
		}
	}
	return out, nil
}
