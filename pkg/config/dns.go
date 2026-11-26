package config

import (
	"fmt"
	"net"
	"strconv"
)

// DNSOff disables the DNS responder (NEBIUS_MOCKER_DNS_LISTEN=off).
const DNSOff = "off"

// DNSConfig configures the DNS module (NEBIUS_MOCKER_DNS_*).
type DNSConfig struct {
	// Listen is the UDP and TCP address of the DNS responder that answers
	// queries from the mock's zones, e.g. ":4653" or "127.0.0.1:0"; "off"
	// disables it.
	Listen string
}

func defaultDNS() DNSConfig { return DNSConfig{Listen: ":4653"} }

func dnsParsers(d *DNSConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"DNS_LISTEN", func(s string) error { return parseDNSListen(s, &d.Listen) }},
	}
}

func parseDNSListen(s string, out *string) error {
	if s == DNSOff {
		*out = s
		return nil
	}
	_, port, err := net.SplitHostPort(s)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		return fmt.Errorf("want host:port (e.g. :4653 or 127.0.0.1:4653) or %q, got %q", DNSOff, s)
	}
	*out = s
	return nil
}
