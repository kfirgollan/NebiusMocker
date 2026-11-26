package config

import "testing"

func TestFromLookupDNS(t *testing.T) {
	t.Parallel()
	lookup := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == "NEBIUS_MOCKER_DNS_LISTEN" {
				return v, true
			}
			return "", false
		}
	}
	if got := Default().DNS.Listen; got != ":4653" {
		t.Fatalf("default DNS listen = %q, want :4653", got)
	}
	if c, err := FromLookup(func(string) (string, bool) { return "", false }); err != nil || c.DNS.Listen != ":4653" {
		t.Fatalf("FromLookup(no env) = %q, %v, want :4653", c.DNS.Listen, err)
	}

	for _, ok := range []string{"127.0.0.1:0", "127.0.0.1:5353", ":53", "[::1]:4653", DNSOff} {
		c, err := FromLookup(lookup(ok))
		if err != nil {
			t.Errorf("NEBIUS_MOCKER_DNS_LISTEN=%q rejected: %v", ok, err)
			continue
		}
		if c.DNS.Listen != ok {
			t.Errorf("NEBIUS_MOCKER_DNS_LISTEN=%q gave listen %q", ok, c.DNS.Listen)
		}
	}
	for _, bad := range []string{"nope", "4653", "127.0.0.1", "127.0.0.1:", "127.0.0.1:abc", "127.0.0.1:70000", "127.0.0.1:-1", "OFF"} {
		if _, err := FromLookup(lookup(bad)); err == nil {
			t.Errorf("NEBIUS_MOCKER_DNS_LISTEN=%q accepted", bad)
		}
	}
}
