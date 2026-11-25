package domain

import (
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func pfx(tb testing.TB, s string) netip.Prefix {
	tb.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		tb.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}

func pfxs(tb testing.TB, ss ...string) []netip.Prefix {
	tb.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, pfx(tb, s))
	}
	return out
}

func TestParseBlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		in           string
		wantKind     errs.Kind
		wantExplicit bool
		wantPrefix   string
		wantBits     int
	}{
		{name: "explicit cidr", in: "10.1.2.0/24", wantExplicit: true, wantPrefix: "10.1.2.0/24", wantBits: 24},
		{name: "bare address", in: "10.1.2.3", wantExplicit: true, wantPrefix: "10.1.2.3/32", wantBits: 32},
		{name: "prefix length", in: "/24", wantBits: 24},
		{name: "zero prefix length", in: "/0", wantBits: 0},
		{name: "empty uses default", in: "", wantBits: 28},
		{name: "host bits set", in: "10.1.2.3/24", wantKind: errs.KindBadRequest},
		{name: "ipv6 cidr", in: "fd00::/64", wantKind: errs.KindBadRequest},
		{name: "ipv6 address", in: "fd00::1", wantKind: errs.KindBadRequest},
		{name: "garbage", in: "banana", wantKind: errs.KindBadRequest},
		{name: "garbage cidr", in: "10.0.0.0/x", wantKind: errs.KindBadRequest},
		{name: "too long prefix length", in: "/33", wantKind: errs.KindBadRequest},
		{name: "too long cidr", in: "10.0.0.0/33", wantKind: errs.KindBadRequest},
		{name: "negative prefix length", in: "/-1", wantKind: errs.KindBadRequest},
		{name: "non numeric prefix length", in: "/abc", wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBlock("f", tt.in, 28)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("parseBlock(%q) kind = %v, want %v (err: %v)", tt.in, k, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			if got.explicit() != tt.wantExplicit {
				t.Errorf("parseBlock(%q).explicit() = %v, want %v", tt.in, got.explicit(), tt.wantExplicit)
			}
			if tt.wantExplicit && got.prefix.String() != tt.wantPrefix {
				t.Errorf("parseBlock(%q).prefix = %v, want %v", tt.in, got.prefix, tt.wantPrefix)
			}
			if got.bits != tt.wantBits {
				t.Errorf("parseBlock(%q).bits = %d, want %d", tt.in, got.bits, tt.wantBits)
			}
		})
	}
}

func TestParseCIDR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"10.0.0.0/8", false},
		{"0.0.0.0/0", false},
		{"10.0.0.1/32", false},
		{"10.0.0.1/8", true},
		{"10.0.0.0", true},
		{"::/0", true},
		{"", true},
		{"10.0.0.0/33", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			p, err := parseCIDR(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCIDR(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err == nil && p.String() != tt.in {
				t.Errorf("parseCIDR(%q) = %v", tt.in, p)
			}
		})
	}
}

// hosts returns the first n /32 addresses of 10.0.0.0.
func hosts(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, "10.0.0."+strconv.Itoa(i)+"/32")
	}
	return out
}

func TestLowestFree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		space []string
		used  []string
		bits  int
		want  string // "" = none
	}{
		{name: "nothing used", space: []string{"10.0.0.0/16"}, bits: 24, want: "10.0.0.0/24"},
		{name: "skips used same size", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/24"}, bits: 24, want: "10.0.1.0/24"},
		{name: "skips two used", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/24", "10.0.1.0/24"}, bits: 24, want: "10.0.2.0/24"},
		{name: "fills gap", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/24", "10.0.2.0/24"}, bits: 24, want: "10.0.1.0/24"},
		{name: "skips larger used block", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/23"}, bits: 24, want: "10.0.2.0/24"},
		{name: "skips smaller used inside", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.5/32"}, bits: 24, want: "10.0.1.0/24"},
		{name: "alignment after small used", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/30"}, bits: 28, want: "10.0.0.16/28"},
		{name: "aligned to size", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/24"}, bits: 23, want: "10.0.2.0/23"},
		{name: "larger block skips misaligned gap", space: []string{"10.0.0.0/16"}, used: []string{"10.0.0.0/24", "10.0.2.0/24"}, bits: 23, want: "10.0.4.0/23"},
		{name: "multiple spaces sorted", space: []string{"192.168.0.0/24", "10.0.0.0/24"}, bits: 25, want: "10.0.0.0/25"},
		{name: "first space full goes to second", space: []string{"10.0.0.0/24", "10.1.0.0/24"}, used: []string{"10.0.0.0/24"}, bits: 24, want: "10.1.0.0/24"},
		{name: "exhausted", space: []string{"10.0.0.0/24"}, used: []string{"10.0.0.0/25", "10.0.0.128/25"}, bits: 25},
		{name: "exhausted by one big block", space: []string{"10.0.0.0/24"}, used: []string{"10.0.0.0/16"}, bits: 28},
		{name: "no space", bits: 24},
		{name: "host in /30", space: []string{"10.0.0.0/30"}, used: []string{"10.0.0.0/32", "10.0.0.1/32", "10.0.0.3/32"}, bits: 32, want: "10.0.0.2/32"},
		{name: "/30 full of hosts", space: []string{"10.0.0.0/30"}, used: hosts(4), bits: 32},
		{name: "bits smaller than space prefix skipped", space: []string{"10.0.0.0/24"}, bits: 16},
		{name: "bits smaller skips only that space", space: []string{"10.0.0.0/24", "10.8.0.0/16"}, bits: 16, want: "10.8.0.0/16"},
		{name: "whole space", space: []string{"10.0.0.0/24"}, bits: 24, want: "10.0.0.0/24"},
		{name: "last block", space: []string{"10.0.0.0/24"}, used: []string{"10.0.0.0/25"}, bits: 25, want: "10.0.0.128/25"},
		{name: "top of address space", space: []string{"255.255.255.0/24"}, used: []string{"255.255.255.0/25"}, bits: 25, want: "255.255.255.128/25"},
		{name: "top of address space full", space: []string{"255.255.255.0/24"}, used: []string{"255.255.255.0/24"}, bits: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := lowestFree(pfxs(t, tt.space...), pfxs(t, tt.used...), tt.bits)
			if tt.want == "" {
				if ok {
					t.Fatalf("lowestFree(%v, %v, %d) = %v, want none", tt.space, tt.used, tt.bits, got)
				}
				return
			}
			if !ok || got.String() != tt.want {
				t.Fatalf("lowestFree(%v, %v, %d) = %v, %v, want %s", tt.space, tt.used, tt.bits, got, ok, tt.want)
			}
		})
	}
}

func TestLowestFreeDoesNotMutateInputs(t *testing.T) {
	t.Parallel()
	space := pfxs(t, "192.168.0.0/24", "10.0.0.0/24")
	orig := slices.Clone(space)
	if _, ok := lowestFree(space, nil, 25); !ok {
		t.Fatal("lowestFree found nothing")
	}
	if !slices.Equal(space, orig) {
		t.Errorf("space mutated: %v, want %v", space, orig)
	}
}

func FuzzLowestFree(f *testing.F) {
	f.Add(uint32(0x0a000000), uint8(16), uint32(0x0a000000), uint8(24), uint32(0x0a000500), uint8(32), uint8(24))
	f.Add(uint32(0x0a000000), uint8(30), uint32(0x0a000001), uint8(32), uint32(0x0a000003), uint8(32), uint8(32))
	f.Add(uint32(0xffffff00), uint8(24), uint32(0xffffff00), uint8(25), uint32(0), uint8(0), uint8(25))
	f.Fuzz(func(t *testing.T, sAddr uint32, sBits uint8, u1Addr uint32, u1Bits uint8, u2Addr uint32, u2Bits, bits uint8) {
		mk := func(a uint32, b uint8) netip.Prefix {
			return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)}), int(b%33)).Masked()
		}
		space := []netip.Prefix{mk(sAddr, sBits)}
		used := []netip.Prefix{mk(u1Addr, u1Bits), mk(u2Addr, u2Bits)}
		want := int(bits % 33)
		got, ok := lowestFree(space, used, want)
		if !ok {
			return
		}
		if got.Bits() != want {
			t.Fatalf("got %v, want a /%d", got, want)
		}
		if got.Masked() != got {
			t.Errorf("got %v is not aligned", got)
		}
		if space[0].Bits() > got.Bits() || !space[0].Contains(got.Addr()) {
			t.Errorf("got %v is outside space %v", got, space[0])
		}
		for _, u := range used {
			if u.Overlaps(got) {
				t.Errorf("got %v overlaps used %v", got, u)
			}
		}
	})
}

func TestReservedAddrs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		block string
		want  []string
	}{
		{"10.0.0.0/24", []string{"10.0.0.0/32", "10.0.0.1/32", "10.0.0.255/32"}},
		{"10.0.1.0/30", []string{"10.0.1.0/32", "10.0.1.1/32", "10.0.1.3/32"}},
		{"10.0.0.0/31", nil},
		{"10.0.0.7/32", nil},
	}
	for _, tt := range tests {
		t.Run(tt.block, func(t *testing.T) {
			t.Parallel()
			got := cidrStrings(reservedAddrs(pfx(t, tt.block)))
			if !slices.Equal(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Fatalf("reservedAddrs(%s) = %v, want %v", tt.block, got, tt.want)
			}
		})
	}
}

func TestAnyOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want bool
	}{
		{"none", []string{"10.0.0.0/24", "10.0.1.0/24"}, false},
		{"empty", nil, false},
		{"single", []string{"10.0.0.0/24"}, false},
		{"nested", []string{"10.0.0.0/16", "10.0.5.0/24"}, true},
		{"identical", []string{"10.0.0.0/24", "10.0.0.0/24"}, true},
		{"non adjacent", []string{"10.0.0.0/24", "10.1.0.0/24", "10.0.0.128/25"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, b, ok := anyOverlap(pfxs(t, tt.in...))
			if ok != tt.want {
				t.Fatalf("anyOverlap(%v) = %v, %v, %v, want %v", tt.in, a, b, ok, tt.want)
			}
			if ok && !a.Overlaps(b) {
				t.Errorf("reported pair %v, %v does not overlap", a, b)
			}
		})
	}
}

func TestBlockPlanResolve(t *testing.T) {
	t.Parallel()
	field := func(i int) string { return "cidrs[" + strconv.Itoa(i) + "]" }
	tests := []struct {
		name     string
		plan     blockPlan
		specs    []string
		want     []string
		wantKind errs.Kind
	}{
		{
			name:  "auto allocates lowest",
			plan:  blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16"), used: pfxs(t, "10.0.0.0/24")},
			specs: []string{"/24"}, want: []string{"10.0.1.0/24"},
		},
		{
			name:  "explicit inside space",
			plan:  blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")},
			specs: []string{"10.0.7.0/24"}, want: []string{"10.0.7.0/24"},
		},
		{
			name:  "two auto requests do not collide",
			plan:  blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")},
			specs: []string{"/24", "/24", "/25"}, want: []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.2.0/25"},
		},
		{
			name:  "auto avoids explicit in same call",
			plan:  blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")},
			specs: []string{"/24", "10.0.0.0/24"}, want: []string{"10.0.1.0/24", "10.0.0.0/24"},
		},
		{
			name:     "explicit outside space",
			plan:     blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")},
			specs:    []string{"192.168.0.0/24"},
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "explicit larger than space",
			plan:     blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/24")},
			specs:    []string{"10.0.0.0/16"},
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "explicit overlaps used",
			plan:     blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16"), used: pfxs(t, "10.0.1.0/24")},
			specs:    []string{"10.0.1.128/25"},
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "explicit overlap in one call",
			plan:     blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")},
			specs:    []string{"10.0.0.0/24", "10.0.0.0/25"},
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "exhaustion",
			plan:     blockPlan{what: "pool", space: pfxs(t, "10.0.0.0/24"), used: pfxs(t, "10.0.0.0/25")},
			specs:    []string{"/25", "/25"},
			wantKind: errs.KindBadState,
		},
		{
			name:     "exhaustion with no space",
			plan:     blockPlan{what: "pool"},
			specs:    []string{"/24"},
			wantKind: errs.KindBadState,
		},
		{
			name:     "malformed spec",
			plan:     blockPlan{what: "pool", space: pfxs(t, "10.0.0.0/16")},
			specs:    []string{"nope"},
			wantKind: errs.KindBadRequest,
		},
		{
			name: "prev keeps resolved block",
			plan: blockPlan{
				what: "subnet", space: pfxs(t, "10.0.0.0/16"), used: pfxs(t, "10.0.0.0/24"),
				prev: newPrev([]string{"/24"}, []string{"10.0.5.0/24"}),
			},
			specs: []string{"/24"}, want: []string{"10.0.5.0/24"},
		},
		{
			name: "prev block outside space is replaced",
			plan: blockPlan{
				what: "subnet", space: pfxs(t, "10.0.0.0/16"),
				prev: newPrev([]string{"/24"}, []string{"172.16.0.0/24"}),
			},
			specs: []string{"/24"}, want: []string{"10.0.0.0/24"},
		},
		{
			name: "prev is consumed once per request",
			plan: blockPlan{
				what: "subnet", space: pfxs(t, "10.0.0.0/16"),
				prev: newPrev([]string{"/24"}, []string{"10.0.5.0/24"}),
			},
			specs: []string{"/24", "/24"}, want: []string{"10.0.5.0/24", "10.0.0.0/24"},
		},
		{
			name: "prev two identical specs keep both blocks in order",
			plan: blockPlan{
				what: "subnet", space: pfxs(t, "10.0.0.0/16"),
				prev: newPrev([]string{"/24", "/24"}, []string{"10.0.5.0/24", "10.0.3.0/24"}),
			},
			specs: []string{"/24", "/24"}, want: []string{"10.0.5.0/24", "10.0.3.0/24"},
		},
		{
			name: "new request avoids kept prev block",
			plan: blockPlan{
				what: "subnet", space: pfxs(t, "10.0.0.0/16"),
				prev: newPrev([]string{"/24"}, []string{"10.0.0.0/24"}),
			},
			specs: []string{"/24", "/24"}, want: []string{"10.0.0.0/24", "10.0.1.0/24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.plan.resolve(field, tt.specs, ipv4Bits)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("resolve(%v) kind = %v, want %v (err: %v)", tt.specs, k, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			if g := cidrStrings(got); !slices.Equal(g, tt.want) {
				t.Fatalf("resolve(%v) = %v, want %v", tt.specs, g, tt.want)
			}
		})
	}
}

func TestNewPrev(t *testing.T) {
	t.Parallel()
	got := newPrev([]string{"/24", "10.0.0.0/24", "/24", "extra"}, []string{"10.0.1.0/24", "10.0.0.0/24", "10.0.2.0/24"})
	if g := cidrStrings(got["/24"]); !slices.Equal(g, []string{"10.0.1.0/24", "10.0.2.0/24"}) {
		t.Errorf("newPrev[/24] = %v", g)
	}
	if g := cidrStrings(got["10.0.0.0/24"]); !slices.Equal(g, []string{"10.0.0.0/24"}) {
		t.Errorf("newPrev[explicit] = %v", g)
	}
	if _, ok := got["extra"]; ok {
		t.Errorf("newPrev has an entry for a spec without a resolution")
	}
}

func TestBlockPlanResolveDefaultBits(t *testing.T) {
	t.Parallel()
	plan := blockPlan{what: "subnet", space: pfxs(t, "10.0.0.0/16")}
	got, err := plan.resolve(func(int) string { return "f" }, []string{"", "/24"}, 28)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if g := cidrStrings(got); !slices.Equal(g, []string{"10.0.0.0/28", "10.0.1.0/24"}) {
		t.Fatalf("resolve = %v, want [10.0.0.0/28 10.0.1.0/24]", g)
	}
}
