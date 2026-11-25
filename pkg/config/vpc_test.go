package config

import "testing"

func TestFromLookupVPC(t *testing.T) {
	t.Parallel()
	def := Default().VPC
	if def.DefaultPrivateCIDR != "10.0.0.0/13" || def.PublicCIDR != "203.0.113.0/24" {
		t.Fatalf("default VPC config = %+v", def)
	}
	env := map[string]string{
		"NEBIUS_MOCKER_VPC_DEFAULT_PRIVATE_CIDR": "10.24.0.0/13",
		"NEBIUS_MOCKER_VPC_PUBLIC_CIDR":          "198.51.100.0/24",
	}
	c, err := FromLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.VPC.DefaultPrivateCIDR != "10.24.0.0/13" || c.VPC.PublicCIDR != "198.51.100.0/24" {
		t.Fatalf("VPC config = %+v", c.VPC)
	}
	for _, bad := range []string{"10.0.0.1/13", "fd00::/8", "nope"} {
		_, err := FromLookup(func(k string) (string, bool) {
			if k == "NEBIUS_MOCKER_VPC_PUBLIC_CIDR" {
				return bad, true
			}
			return "", false
		})
		if err == nil {
			t.Errorf("NEBIUS_MOCKER_VPC_PUBLIC_CIDR=%q accepted", bad)
		}
	}
}
