// Package mysterybox is the NebiusMocker module for Nebius MysteryBox
// (SecretStash), the secrets manager: SecretService and SecretVersionService
// on cpl.mysterybox, PayloadService on dpl.mysterybox.
//
// Payloads are sealed at rest with AES-GCM under an instance key and are
// returned only by PayloadService. See docs/services/mysterybox.md.
package mysterybox

import (
	"context"
	"encoding/base64"
	"fmt"

	"google.golang.org/grpc"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/api"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/backend/aesgcm"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/dal"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// Module is the MysteryBox service module.
type Module struct {
	svc *domain.Service
}

var _ mocker.Module = (*Module)(nil)

// New returns the MysteryBox module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "mysterybox" }

// Service returns the domain service once Register has run (nil before). It
// is the in-process entry point for other modules wired in a composition root,
// e.g. CreateSystemSecret.
func (m *Module) Service() *domain.Service { return m.svc }

// Register wires the repositories, the payload sealer and the gRPC services.
func (m *Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	sealer, err := newSealer(deps.Config.MysteryBox.Key)
	if err != nil {
		return fmt.Errorf("mysterybox: %w", err)
	}
	secrets := dal.NewSecretRepo(deps.Store)
	versions := dal.NewVersionRepo(deps.Store)
	cfg := deps.Config.MysteryBox
	svc, err := domain.NewService(domain.Deps{
		Secrets: secrets, Versions: versions, Payloads: dal.NewPayloadRepo(deps.Store), Sealer: sealer,
		Hierarchy: deps.Authz, Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Limits: domain.Limits{
			MaxEntriesPerVersion: cfg.MaxEntries, MaxValueBytes: cfg.MaxValueBytes,
			MaxVersionsPerSecret: cfg.MaxVersions, PurgeDelay: cfg.PurgeDelay,
		},
	})
	if err != nil {
		return fmt.Errorf("mysterybox: %w", err)
	}
	m.svc = svc

	// Parent lookups see soft-deleted resources too, so authz can be decided
	// (e.g. for Undelete) before the domain hides them.
	deps.Authz.RegisterKind(domain.KindSecret, func(ctx context.Context, id string) (string, error) {
		s, err := secrets.Get(ctx, id)
		return s.GetMetadata().GetParentId(), err
	})
	deps.Authz.RegisterKind(domain.KindSecretVersion, func(ctx context.Context, id string) (string, error) {
		v, err := versions.Get(ctx, id)
		return v.GetMetadata().GetParentId(), err
	})
	api.RegisterAuthzRules(deps.Rules)
	if err := peer.Provide[peer.SecretPayloads](deps.Peers, peer.SecretPayloadsKey, payloadPeer{svc: svc}); err != nil {
		return fmt.Errorf("mysterybox: %w", err)
	}

	mysteryboxpb.RegisterSecretServiceServer(reg, api.NewSecretHandler(svc))
	mysteryboxpb.RegisterSecretVersionServiceServer(reg, api.NewVersionHandler(svc))
	mysteryboxpb.RegisterPayloadServiceServer(reg, api.NewPayloadHandler(svc))
	return nil
}

// newSealer builds the AES-GCM sealer from a base64 key, or a random one.
func newSealer(key string) (*aesgcm.Sealer, error) {
	if key == "" {
		return aesgcm.NewRandom()
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("NEBIUS_MOCKER_MYSTERYBOX_KEY: not base64: %w", err)
	}
	return aesgcm.New(raw)
}
