// Package clock is the only place in NebiusMocker that touches real time.
//
// Everything else receives a Clock, so lifecycles, operation timing and token
// expiry can be driven deterministically in tests with a Manual clock.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the time source injected into every component that needs time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// Real is a Clock backed by the system clock.
type Real struct{}

var _ Clock = Real{}

// Now returns the current system time in UTC.
func (Real) Now() time.Time { return time.Now().UTC() }

// After wraps time.After.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Manual is a Clock that only moves when Advance or Set is called.
// It is safe for concurrent use.
type Manual struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

var _ Clock = (*Manual)(nil)

// NewManual returns a Manual clock starting at start.
func NewManual(start time.Time) *Manual {
	return &Manual{now: start.UTC()}
}

// Now returns the manual clock's current time.
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// After returns a channel that fires when the clock is advanced past now+d.
// A non-positive d fires immediately.
func (m *Manual) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := m.now.Add(d)
	if d <= 0 {
		ch <- m.now
		return ch
	}
	m.waiters = append(m.waiters, waiter{at: at, ch: ch})
	return ch
}

// Advance moves the clock forward by d and fires every waiter that became due.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	now := m.now
	due, pending := splitDue(m.waiters, now)
	m.waiters = pending
	m.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

func splitDue(ws []waiter, now time.Time) (due, pending []waiter) {
	sort.Slice(ws, func(i, j int) bool { return ws[i].at.Before(ws[j].at) })
	for _, w := range ws {
		if w.at.After(now) {
			pending = append(pending, w)
			continue
		}
		due = append(due, w)
	}
	return due, pending
}
