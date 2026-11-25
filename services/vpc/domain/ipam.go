package domain

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// IPAM primitives over net/netip. Only IPv4 is supported, like Nebius VPC.

const ipv4Bits = 32

// blockRequest is a requested address block: an explicit prefix, or just a
// size (prefix length) to auto-allocate.
type blockRequest struct {
	prefix netip.Prefix // valid when explicit
	bits   int
}

func (b blockRequest) explicit() bool { return b.prefix.IsValid() }

// parseBlock parses "10.1.2.0/24", "10.1.2.3" (a /32) or "/24". An empty
// value requests a block of defaultBits.
func parseBlock(field, s string, defaultBits int) (blockRequest, error) {
	bad := func(msg string) (blockRequest, error) {
		return blockRequest{}, errs.BadRequest(errs.Violation{Field: field, Message: msg})
	}
	switch {
	case s == "":
		return blockRequest{bits: defaultBits}, nil
	case strings.HasPrefix(s, "/"):
		n, err := strconv.Atoi(s[1:])
		if err != nil || n < 0 {
			return bad(fmt.Sprintf("%q is not a prefix length", s))
		}
		if n > ipv4Bits {
			return bad("only IPv4 is supported")
		}
		return blockRequest{bits: n}, nil
	case !strings.Contains(s, "/"):
		a, err := netip.ParseAddr(s)
		if err != nil {
			return bad(fmt.Sprintf("%q is not an IP address or CIDR", s))
		}
		if !a.Is4() {
			return bad("only IPv4 is supported")
		}
		return blockRequest{prefix: netip.PrefixFrom(a, ipv4Bits), bits: ipv4Bits}, nil
	}
	p, err := parseCIDR(s)
	if err != nil {
		return bad(err.Error())
	}
	return blockRequest{prefix: p, bits: p.Bits()}, nil
}

// parseCIDR parses a canonical IPv4 CIDR (host bits must be zero).
func parseCIDR(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not a CIDR", s)
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%q: only IPv4 is supported", s)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("%q has host bits set (did you mean %s?)", s, p.Masked())
	}
	return p, nil
}

// mustCIDRs parses CIDRs the service itself stored; invalid entries are skipped.
func mustCIDRs(ss []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func cidrStrings(ps []netip.Prefix) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func u32(a netip.Addr) uint64 {
	b := a.As4()
	return uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
}

func addrOf(v uint64) netip.Addr {
	b := binary.BigEndian.AppendUint64(nil, v)
	return netip.AddrFrom4([4]byte(b[4:]))
}

func blockSize(bits int) uint64 { return uint64(1) << (ipv4Bits - bits) }

// end returns the first address after p (exclusive), as a number.
func end(p netip.Prefix) uint64 { return u32(p.Masked().Addr()) + blockSize(p.Bits()) }

// firstOverlap returns an element of used that overlaps p.
func firstOverlap(p netip.Prefix, used []netip.Prefix) (netip.Prefix, bool) {
	for _, u := range used {
		if u.Overlaps(p) {
			return u, true
		}
	}
	return netip.Prefix{}, false
}

// containerOf returns the element of space that fully contains p.
func containerOf(p netip.Prefix, space []netip.Prefix) (netip.Prefix, bool) {
	for _, s := range space {
		if s.Bits() <= p.Bits() && s.Contains(p.Addr()) {
			return s, true
		}
	}
	return netip.Prefix{}, false
}

// contained reports whether an element of space fully contains p.
func contained(p netip.Prefix, space []netip.Prefix) bool {
	_, ok := containerOf(p, space)
	return ok
}

// lowestFree returns the lowest block of the given prefix length inside space
// that overlaps nothing in used. Blocks are aligned to their size.
func lowestFree(space, used []netip.Prefix, bits int) (netip.Prefix, bool) {
	sorted := slices.Clone(space)
	slices.SortFunc(sorted, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	size := blockSize(bits)
	for _, s := range sorted {
		if s.Bits() > bits {
			continue
		}
		limit := end(s)
		for c := u32(s.Masked().Addr()); c+size <= limit; {
			cand := netip.PrefixFrom(addrOf(c), bits)
			o, hit := firstOverlap(cand, used)
			if !hit {
				return cand, true
			}
			next := max(end(o), c+size)
			c = (next + size - 1) / size * size
		}
	}
	return netip.Prefix{}, false
}

// reservedAddrs are the addresses of a subnet block that are never allocated:
// the network address, the gateway (first host) and the broadcast address.
func reservedAddrs(block netip.Prefix) []netip.Prefix {
	if block.Bits() > ipv4Bits-2 {
		return nil
	}
	first := u32(block.Masked().Addr())
	return []netip.Prefix{
		netip.PrefixFrom(addrOf(first), ipv4Bits),
		netip.PrefixFrom(addrOf(first+1), ipv4Bits),
		netip.PrefixFrom(addrOf(end(block)-1), ipv4Bits),
	}
}

// anyOverlap reports the first pair of overlapping prefixes within ps.
func anyOverlap(ps []netip.Prefix) (a, b netip.Prefix, ok bool) {
	for i := range ps {
		for j := i + 1; j < len(ps); j++ {
			if ps[i].Overlaps(ps[j]) {
				return ps[i], ps[j], true
			}
		}
	}
	return netip.Prefix{}, netip.Prefix{}, false
}

// blockPlan resolves the address blocks one resource requests.
type blockPlan struct {
	// what is the resource kind, for messages ("subnet", "pool").
	what string
	// space is where blocks may be placed.
	space []netip.Prefix
	// used is already occupied by other resources.
	used []netip.Prefix
	// prev maps each previous request string to its resolution, in order, so
	// that an update keeps the blocks it already holds.
	prev map[string][]netip.Prefix
	// maxMask limits the prefix length carved from a space block
	// (max_mask_length); blocks without an entry allow up to /32.
	maxMask map[netip.Prefix]int64
}

// allows reports whether a block of the given prefix length may come from s.
func (p blockPlan) allows(s netip.Prefix, bits int) bool {
	limit, ok := p.maxMask[s]
	return !ok || int64(bits) <= limit
}

// newPrev pairs previous request strings with their resolutions.
func newPrev(specs, resolved []string) map[string][]netip.Prefix {
	out := map[string][]netip.Prefix{}
	ps := mustCIDRs(resolved)
	for i, s := range specs {
		if i < len(ps) {
			out[s] = append(out[s], ps[i])
		}
	}
	return out
}

// resolve returns one prefix per request, in order. field formats the
// violation path of request i.
func (p blockPlan) resolve(field func(i int) string, specs []string, defaultBits int) ([]netip.Prefix, error) {
	reqs := make([]blockRequest, len(specs))
	out := make([]netip.Prefix, len(specs))
	for i, s := range specs {
		r, err := parseBlock(field(i), s, defaultBits)
		if err != nil {
			return nil, err
		}
		reqs[i] = r
		if held := p.prev[s]; len(held) > 0 {
			if contained(held[0], p.space) {
				out[i], p.prev[s] = held[0], held[1:]
			}
		}
	}
	// Explicit blocks are placed before auto-sized ones, so a "/24" listed
	// first cannot take the block a later explicit CIDR asks for.
	order := make([]int, 0, len(reqs))
	for _, explicitPass := range []bool{true, false} {
		for i, r := range reqs {
			if r.explicit() == explicitPass {
				order = append(order, i)
			}
		}
	}
	for _, i := range order {
		r := reqs[i]
		if out[i].IsValid() {
			continue
		}
		taken := append(slices.Clone(p.used), compact(out)...)
		got, err := p.place(field(i), r, taken)
		if err != nil {
			return nil, err
		}
		out[i] = got
	}
	if a, b, bad := anyOverlap(out); bad {
		return nil, errs.BadRequest(errs.Violation{Field: field(0), Message: fmt.Sprintf("blocks %s and %s overlap", a, b)})
	}
	return out, nil
}

func (p blockPlan) place(field string, r blockRequest, taken []netip.Prefix) (netip.Prefix, error) {
	if !r.explicit() {
		space := slices.DeleteFunc(slices.Clone(p.space), func(s netip.Prefix) bool { return !p.allows(s, r.bits) })
		if len(space) == 0 && len(p.space) > 0 {
			return netip.Prefix{}, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
				"/%d is longer than the max_mask_length of every block in %v", r.bits, p.space)})
		}
		got, ok := lowestFree(space, taken, r.bits)
		if !ok {
			return netip.Prefix{}, errs.BadState("", fmt.Sprintf("no free /%d block left for the %s in %v", r.bits, p.what, p.space))
		}
		return got, nil
	}
	s, ok := containerOf(r.prefix, p.space)
	if !ok {
		return netip.Prefix{}, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("%s is not inside the available address space %v", r.prefix, p.space)})
	}
	if !p.allows(s, r.bits) {
		return netip.Prefix{}, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"/%d is longer than the max_mask_length /%d of %s", r.bits, p.maxMask[s], s)})
	}
	if o, hit := firstOverlap(r.prefix, taken); hit {
		return netip.Prefix{}, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("%s overlaps %s, which is already in use", r.prefix, o)})
	}
	return r.prefix, nil
}

func compact(ps []netip.Prefix) []netip.Prefix {
	return slices.DeleteFunc(slices.Clone(ps), func(p netip.Prefix) bool { return !p.IsValid() })
}
