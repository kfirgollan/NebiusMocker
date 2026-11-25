package domain

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultFabricCatalog(t *testing.T) {
	t.Parallel()
	c, err := DefaultFabricCatalog()
	if err != nil {
		t.Fatal(err)
	}
	f, ok := c.Lookup("fabric-3")
	if !ok || f.Region != "eu-north1" || !f.Allows("gpu-h100-sxm") || f.Allows("gpu-h200-sxm") {
		t.Fatalf("fabric-3 = %+v, %v", f, ok)
	}
	if _, ok := c.Lookup("fabric-1"); ok {
		t.Fatal("fabric-1 is not a documented fabric")
	}
	if got := c.InRegion("us-central1"); !slices.Equal(got, []string{"us-central1-a", "us-central1-b"}) {
		t.Fatalf("us-central1 fabrics = %v", got)
	}
}

func TestParseFabricCatalogRejects(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"unknown field":  "fabrics: [{name: a, region: r, platforms: [p], zone: z}]",
		"no name":        "fabrics: [{region: r, platforms: [p]}]",
		"duplicate":      "fabrics: [{name: a, region: r, platforms: [p]}, {name: a, region: r, platforms: [p]}]",
		"no region":      "fabrics: [{name: a, platforms: [p]}]",
		"no platforms":   "fabrics: [{name: a, region: r}]",
		"empty platform": "fabrics: [{name: a, region: r, platforms: ['']}]",
	} {
		if _, err := ParseFabricCatalog([]byte(doc)); err == nil || !strings.Contains(err.Error(), "fabric catalog") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
