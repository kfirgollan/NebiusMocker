package config

import (
	"fmt"
	"net/netip"
)

// VPCConfig is the seeded VPC address space (NEBIUS_MOCKER_VPC_*).
type VPCConfig struct {
	// DefaultPrivateCIDR is the CIDR of each project's default-network-pool.
	// The default is the eu-north1 block Nebius documents.
	DefaultPrivateCIDR string
	// PublicCIDR is the CIDR of the default-public-pool. The default is
	// TEST-NET-3, so mock public IPs can never collide with real ones.
	PublicCIDR string
}

func defaultVPC() VPCConfig {
	return VPCConfig{DefaultPrivateCIDR: "10.0.0.0/13", PublicCIDR: "203.0.113.0/24"}
}

func vpcParsers(v *VPCConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"VPC_DEFAULT_PRIVATE_CIDR", func(s string) error { return parseIPv4CIDR(s, &v.DefaultPrivateCIDR) }},
		{"VPC_PUBLIC_CIDR", func(s string) error { return parseIPv4CIDR(s, &v.PublicCIDR) }},
	}
}

func parseIPv4CIDR(s string, out *string) error {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return fmt.Errorf("want a canonical IPv4 CIDR such as 10.0.0.0/13, got %q", s)
	}
	*out = p.String()
	return nil
}
