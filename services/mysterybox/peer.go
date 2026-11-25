package mysterybox

import (
	"context"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// payloadPeer provides peer.SecretPayloads to other modules. It is read-only
// and in-process: no RPC, so no RPC authorization; the consumer authorizes
// its own request. It never logs payloads.
type payloadPeer struct{ svc *domain.Service }

var _ peer.SecretPayloads = payloadPeer{}

// SecretPayload implements peer.SecretPayloads.
func (p payloadPeer) SecretPayload(ctx context.Context, ref peer.SecretRef) (peer.SecretPayload, error) {
	r, err := p.svc.ResolvePayload(ctx, domain.PayloadRef{SecretID: ref.SecretID, VersionID: ref.VersionID})
	if err != nil {
		return peer.SecretPayload{}, err
	}
	out := peer.SecretPayload{SecretID: r.SecretID, VersionID: r.VersionID, Entries: make([]peer.PayloadEntry, 0, len(r.Entries))}
	for _, e := range r.Entries {
		out.Entries = append(out.Entries, toPeerEntry(e))
	}
	return out, nil
}

func toPeerEntry(e *mysteryboxpb.Payload) peer.PayloadEntry {
	if b, ok := e.GetPayload().(*mysteryboxpb.Payload_BinaryValue); ok {
		return peer.PayloadEntry{Key: e.GetKey(), BinaryValue: b.BinaryValue, Binary: true}
	}
	return peer.PayloadEntry{Key: e.GetKey(), StringValue: e.GetStringValue()}
}

// systemSecretsPeer provides peer.SystemSecrets to other modules: secrets
// created on a caller's behalf (e.g. IAM access keys delivered to MysteryBox).
// The consumer authorizes its own request; reading the payload later goes
// through PayloadService and its usual permissions.
type systemSecretsPeer struct{ svc *domain.Service }

var _ peer.SystemSecrets = systemSecretsPeer{}

// CreateSystemSecret implements peer.SystemSecrets.
func (p systemSecretsPeer) CreateSystemSecret(ctx context.Context, spec peer.SystemSecretSpec) (peer.SystemSecret, error) {
	entries := make([]*mysteryboxpb.Payload, 0, len(spec.Entries))
	for _, e := range spec.Entries {
		entries = append(entries, fromPeerEntry(e))
	}
	sec, err := p.svc.CreateSystemSecret(ctx, domain.SystemSecretCmd{
		ParentID: spec.ParentID, Name: spec.Name, Description: spec.Description, Labels: spec.Labels, Payload: entries,
	})
	if err != nil {
		return peer.SystemSecret{}, err
	}
	return peer.SystemSecret{SecretID: sec.GetMetadata().GetId(), VersionID: sec.GetSpec().GetPrimaryVersionId()}, nil
}

// DiscardSystemSecret implements peer.SystemSecrets.
func (p systemSecretsPeer) DiscardSystemSecret(ctx context.Context, secretID string) error {
	return p.svc.DiscardSystemSecret(ctx, secretID)
}

func fromPeerEntry(e peer.PayloadEntry) *mysteryboxpb.Payload {
	if e.Binary {
		return &mysteryboxpb.Payload{Key: e.Key, Payload: &mysteryboxpb.Payload_BinaryValue{BinaryValue: e.BinaryValue}}
	}
	return &mysteryboxpb.Payload{Key: e.Key, Payload: &mysteryboxpb.Payload_StringValue{StringValue: e.StringValue}}
}
