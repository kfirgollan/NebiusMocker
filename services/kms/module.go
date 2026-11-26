// Package kms is the NebiusMocker module for Nebius KMS (cpl.kms).
//
// Implemented: SymmetricKeyService, AsymmetricKeyService,
// SymmetricCryptoService, AsymmetricCryptoService, and the authorization
// bindings they define.
package kms

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/kms/api"
	"github.com/kfirgollan/NebiusMocker/services/kms/dal"
	"github.com/kfirgollan/NebiusMocker/services/kms/domain"
)

// Module is the KMS service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the KMS module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "kms" }

// Register installs authorization bindings and registers the KMS gRPC services.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	symRepo := dal.NewSymmetricKeyRepo(deps.Store)
	asymRepo := dal.NewAsymmetricKeyRepo(deps.Store)

	// Raw repo Get: keys scheduled for deletion must still resolve for authz.
	deps.Authz.RegisterKind(domain.KindSymmetricKey, func(ctx context.Context, id string) (string, error) {
		k, err := symRepo.Get(ctx, id)
		return k.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindAsymmetricKey, func(ctx context.Context, id string) (string, error) {
		k, err := asymRepo.Get(ctx, id)
		return k.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)

	gate := peer.QuotaGateFor(deps.Peers)
	if err := peer.Provide(deps.Peers, peer.QuotaUsageKey("kms"), dal.NewQuotaUsage(deps.Store)); err != nil {
		return fmt.Errorf("kms: %w", err)
	}

	sym, err := domain.NewSymmetricKeyService(domain.KeyDeps[*kmspb.SymmetricKey]{
		Keys: symRepo, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Quota: gate,
	})
	if err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	asym, err := domain.NewAsymmetricKeyService(domain.KeyDeps[*kmspb.AsymmetricKey]{
		Keys: asymRepo, Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock, Quota: gate,
	})
	if err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	kmspb.RegisterSymmetricKeyServiceServer(reg, api.NewSymmetricKeyHandler(sym))
	kmspb.RegisterAsymmetricKeyServiceServer(reg, api.NewAsymmetricKeyHandler(asym))
	kmspb.RegisterSymmetricCryptoServiceServer(reg, api.NewSymmetricCryptoHandler(sym))
	kmspb.RegisterAsymmetricCryptoServiceServer(reg, api.NewAsymmetricCryptoHandler(asym))
	return nil
}
