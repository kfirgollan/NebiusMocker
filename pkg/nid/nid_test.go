package nid

import (
	"strings"
	"testing"

	gosdknid "github.com/nebius/gosdk/nid"
)

func TestGeneratedIDsParseWithGosdk(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{Random, Sequential} {
		g, err := NewGenerator(mode, "e00", 42)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for range 100 {
			id, err := g.New("group")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := gosdknid.Parse(id); err != nil {
				t.Fatalf("gosdk cannot parse %q: %v", id, err)
			}
			if !strings.HasPrefix(id, "group-e00") || seen[id] {
				t.Fatalf("bad or duplicate id %q", id)
			}
			seen[id] = true
		}
	}
}

func TestSequentialIsPredictable(t *testing.T) {
	t.Parallel()
	g, _ := NewGenerator(Sequential, "e00", 0)
	a, _ := g.New("group")
	b, _ := g.New("group")
	c, _ := g.New("operation")
	if a != "group-e00mock0000000000001" || b != "group-e00mock0000000000002" || c != "operation-e00mock0000000000001" {
		t.Fatalf("got %s %s %s", a, b, c)
	}
}

func TestRejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := NewGenerator(Random, "E0", 0); err == nil {
		t.Error("bad routing code accepted")
	}
	g, _ := NewGenerator(Random, "e00", 1)
	if _, err := g.New("x"); err == nil {
		t.Error("too-short type accepted")
	}
}
