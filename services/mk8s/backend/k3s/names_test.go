package k3s

import "testing"

func TestServerAliasIsADNSLabel(t *testing.T) {
	t.Parallel()
	// A cluster NID is "mk8scluster-" + 3 routing + 17 characters.
	const id = "mk8scluster-e00mock0000000000001"
	if a := serverAlias(id); len(a) > 63 {
		t.Errorf("server alias %q is %d characters, longer than a DNS label (63)", a, len(a))
	}
	b1, b2 := &Backend{webhookURL: "https://host.docker.internal:4566"}, &Backend{webhookURL: "https://host.docker.internal:4567"}
	if b1.serverName(id) == b2.serverName(id) || b1.networkName(id) == b2.networkName(id) {
		t.Error("two mockers name the same cluster's objects alike")
	}
}
