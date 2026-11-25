// Package nid generates Nebius IDs (NIDs) of the form <type>-<routing code><unique>.
//
// Generated IDs are validated with github.com/nebius/gosdk/nid so they are
// always parseable by official clients.
package nid

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"

	gosdknid "github.com/nebius/gosdk/nid"
)

const (
	alphabet  = "0123456789abcdefghijklmnopqrstuvwxyz"
	uniqueLen = 17
)

// Generator creates new IDs for a resource type prefix (e.g. "group").
type Generator interface {
	New(typ string) (string, error)
}

// Mode selects how unique parts are produced.
type Mode int

// Generation modes.
const (
	// Random produces realistic random IDs.
	Random Mode = iota
	// Sequential produces predictable IDs (…mock00000000000001) for golden tests.
	Sequential
)

// NewGenerator returns a Generator for the given routing code (e.g. "e00").
// seed is used only in Random mode; pass 0 for a non-deterministic seed.
func NewGenerator(mode Mode, routingCode string, seed uint64) (Generator, error) {
	rc, err := gosdknid.NewRoutingCode(routingCode)
	if err != nil {
		return nil, fmt.Errorf("nid: routing code %q: %w", routingCode, err)
	}
	g := &generator{mode: mode, routing: rc, counters: map[string]uint64{}}
	if mode == Random {
		if seed == 0 {
			seed = rand.Uint64() //nolint:gosec // IDs are identifiers, not secrets
		}
		g.rng = rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // IDs are identifiers, not secrets
	}
	return g, nil
}

type generator struct {
	mode     Mode
	routing  gosdknid.RoutingCode
	mu       sync.Mutex
	rng      *rand.Rand
	counters map[string]uint64
}

func (g *generator) New(typ string) (string, error) {
	t, err := gosdknid.NewType(typ)
	if err != nil {
		return "", fmt.Errorf("nid: type %q: %w", typ, err)
	}
	g.mu.Lock()
	unique := g.uniqueLocked(typ)
	g.mu.Unlock()
	weak, err := gosdknid.NewWeakID(unique)
	if err != nil {
		return "", fmt.Errorf("nid: weak id %q: %w", unique, err)
	}
	id, err := gosdknid.NewWithRoutingCode(t, g.routing, weak)
	if err != nil {
		return "", fmt.Errorf("nid: build: %w", err)
	}
	return id.String(), nil
}

func (g *generator) uniqueLocked(typ string) string {
	if g.mode == Sequential {
		g.counters[typ]++
		return fmt.Sprintf("mock%013d", g.counters[typ])
	}
	var b strings.Builder
	b.Grow(uniqueLen)
	for range uniqueLen {
		b.WriteByte(alphabet[g.rng.IntN(len(alphabet))])
	}
	return b.String()
}
