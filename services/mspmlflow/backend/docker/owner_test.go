package docker

import (
	"strings"
	"testing"
)

func TestNamesCarryTheOwner(t *testing.T) {
	t.Parallel()
	a, b := &Backend{owner: ownerOf("127.0.0.1:4566")}, &Backend{owner: ownerOf("127.0.0.1:4567")}
	if a.owner != "127.0.0.1:4566" {
		t.Errorf("owner = %q, want the edge address", a.owner)
	}
	if a.containerName("mlflow-1") == b.containerName("mlflow-1") {
		t.Errorf("two mockers name cluster mlflow-1 alike: %s", a.containerName("mlflow-1"))
	}
	if x, y := ownerOf("127.0.0.1:0"), ownerOf(":0"); x == y || !strings.HasPrefix(x, "unbound-") {
		t.Errorf("owners of unbound edges = %q, %q, want distinct random owners", x, y)
	}
}
