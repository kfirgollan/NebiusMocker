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
