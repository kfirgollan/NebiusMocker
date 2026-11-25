package peer_test

import (
	"context"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

type greeter interface{ Greet() string }

type english struct{}

func (english) Greet() string { return "hello" }

var greeterKey = peer.NewKey[greeter]("test.greeter")

func TestProvideLookup(t *testing.T) {
	t.Parallel()
	d := peer.NewDirectory()
	if _, ok := peer.Lookup(d, greeterKey); ok {
		t.Fatal("Lookup before Provide = ok")
	}
	if err := peer.Provide[greeter](d, greeterKey, english{}); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	g, ok := peer.Lookup(d, greeterKey)
	if !ok || g.Greet() != "hello" {
		t.Fatalf("Lookup = %v, %v", g, ok)
	}
	if err := peer.Provide[greeter](d, greeterKey, english{}); err == nil {
		t.Fatal("second Provide succeeded, want error")
	}
	if greeterKey.Name() != "test.greeter" {
		t.Fatalf("Name = %q", greeterKey.Name())
	}
}

func TestNilDirectory(t *testing.T) {
	t.Parallel()
	if _, ok := peer.Lookup[greeter](nil, greeterKey); ok {
		t.Fatal("nil directory provided a capability")
	}
	if err := peer.Provide[greeter](nil, greeterKey, english{}); err == nil {
		t.Fatal("Provide on nil directory succeeded")
	}
}

type fakePayloads struct{}

func (fakePayloads) SecretPayload(_ context.Context, ref peer.SecretRef) (peer.SecretPayload, error) {
	return peer.SecretPayload{SecretID: ref.SecretID, VersionID: "mbsecver-1", Entries: []peer.PayloadEntry{
		{Key: "AUTH_TOKEN", StringValue: "t0k"},
	}}, nil
}

func TestSecretPayloadsCapability(t *testing.T) {
	t.Parallel()
	d := peer.NewDirectory()
	if err := peer.Provide[peer.SecretPayloads](d, peer.SecretPayloadsKey, fakePayloads{}); err != nil {
		t.Fatal(err)
	}
	sp, ok := peer.Lookup(d, peer.SecretPayloadsKey)
	if !ok {
		t.Fatal("SecretPayloads not found")
	}
	p, err := sp.SecretPayload(t.Context(), peer.SecretRef{SecretID: "mbsec-1"})
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := p.Entry("AUTH_TOKEN"); !ok || e.StringValue != "t0k" {
		t.Fatalf("Entry(AUTH_TOKEN) = %+v, %v", e, ok)
	}
	if _, ok := p.Entry("MISSING"); ok {
		t.Fatal("Entry(MISSING) found")
	}
}
