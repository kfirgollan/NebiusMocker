package mysterybox

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

func TestToPeerEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   *mysteryboxpb.Payload
		want peer.PayloadEntry
	}{
		{
			&mysteryboxpb.Payload{Key: "s", Payload: &mysteryboxpb.Payload_StringValue{StringValue: "v"}},
			peer.PayloadEntry{Key: "s", StringValue: "v"},
		},
		{
			&mysteryboxpb.Payload{Key: "b", Payload: &mysteryboxpb.Payload_BinaryValue{BinaryValue: []byte{0, 0xff}}},
			peer.PayloadEntry{Key: "b", BinaryValue: []byte{0, 0xff}, Binary: true},
		},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, toPeerEntry(tt.in)); diff != "" {
			t.Errorf("toPeerEntry(%s) (-want +got):\n%s", tt.in.GetKey(), diff)
		}
	}
}

func TestFromPeerEntryRoundTrip(t *testing.T) {
	t.Parallel()
	for _, e := range []peer.PayloadEntry{
		{Key: "s", StringValue: "v"},
		{Key: "b", BinaryValue: []byte{0, 0xff}, Binary: true},
	} {
		if diff := cmp.Diff(e, toPeerEntry(fromPeerEntry(e))); diff != "" {
			t.Errorf("round trip of %s (-want +got):\n%s", e.Key, diff)
		}
	}
}
