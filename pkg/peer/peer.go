// Package peer is the in-process capability registry that lets one service
// area use another without importing it (services/A never imports services/B).
//
// A capability is a small interface declared in this package together with a
// typed Key. The providing module registers its implementation in Register:
//
//	err := peer.Provide(deps.Peers, peer.SecretPayloadsKey, impl)
//
// and a consuming module looks it up at call time, not at Register time, since
// module order is not guaranteed and the provider may be disabled:
//
//	sp, ok := peer.Lookup(deps.Peers, peer.SecretPayloadsKey)
//
// The registry is transport-free: capabilities speak domain types and return
// pkg/errs kinds.
package peer

import (
	"fmt"
	"sync"
)

// Key names a capability of type T. Keys are declared once, next to the
// capability interface, so a provider and a consumer cannot disagree on T.
type Key[T any] struct {
	name string
}

// NewKey returns the key for a capability called name (e.g.
// "mysterybox.secret-payloads").
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

// Name returns the capability name.
func (k Key[T]) Name() string { return k.name }

// Directory holds the capabilities provided in one process. It is safe for
// concurrent use. Use NewDirectory; a nil *Directory provides nothing.
type Directory struct {
	mu   sync.RWMutex
	caps map[string]any
}

// NewDirectory returns an empty directory.
func NewDirectory() *Directory { return &Directory{caps: map[string]any{}} }

// Provide registers impl as the capability k. Each capability has one
// provider: a second Provide for the same key is an error.
func Provide[T any](d *Directory, k Key[T], impl T) error {
	if d == nil {
		return fmt.Errorf("peer: provide %s: nil directory", k.name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.caps[k.name]; dup {
		return fmt.Errorf("peer: capability %s already provided", k.name)
	}
	d.caps[k.name] = impl
	return nil
}

// Lookup returns the capability k, or false when no enabled module provides
// it. A nil directory provides nothing.
func Lookup[T any](d *Directory, k Key[T]) (T, bool) {
	var zero T
	if d == nil {
		return zero, false
	}
	d.mu.RLock()
	v, ok := d.caps[k.name]
	d.mu.RUnlock()
	if !ok {
		return zero, false
	}
	impl, ok := v.(T)
	return impl, ok
}
