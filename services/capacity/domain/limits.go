package domain

import (
	"math"
	"slices"
	"strconv"
	"time"
)

// limitAt is the block group's quota at t: the sum of the quantities of the
// intervals active at t (start <= t < end).
func (g *BlockGroup) limitAt(t time.Time) uint64 {
	var sum uint64
	for _, iv := range g.Intervals {
		if iv.activeAt(t) {
			sum += iv.Quantity
		}
	}
	return sum
}

func (iv Interval) activeAt(t time.Time) bool { return !t.Before(iv.Start) && t.Before(iv.End) }

// nextChange returns the first time after now at which the group's limit
// changes, and the limit from then on.
func (g *BlockGroup) nextChange(now time.Time) (time.Time, uint64, bool) {
	current := g.limitAt(now)
	for _, b := range g.boundaries() {
		if !b.After(now) {
			continue
		}
		if l := g.limitAt(b); l != current {
			return b, l, true
		}
	}
	return time.Time{}, 0, false
}

// boundaries returns every interval start and end, sorted and unique.
func (g *BlockGroup) boundaries() []time.Time {
	var out []time.Time
	for _, iv := range g.Intervals {
		out = append(out, iv.Start, iv.End)
	}
	slices.SortFunc(out, time.Time.Compare)
	return slices.CompactFunc(out, time.Time.Equal)
}

// continuityGap is the largest break between two intervals that still makes
// them one continuous interval: Nebius intervals end at 23:59 UTC and the
// next one starts at 00:00 UTC.
const continuityGap = time.Minute

// segment is a continuous run of non-zero intervals.
type segment struct{ start, end time.Time }

// segments merges the non-zero intervals that overlap or follow each other
// without a break.
func (g *BlockGroup) segments() []segment {
	var out []segment
	for _, iv := range g.Intervals { // sorted by start
		if iv.Quantity == 0 {
			continue
		}
		if n := len(out); n > 0 && !iv.Start.After(out[n-1].end.Add(continuityGap)) {
			if iv.End.After(out[n-1].end) {
				out[n-1].end = iv.End
			}
			continue
		}
		out = append(out, segment{iv.Start, iv.End})
	}
	return out
}

// ContinuousState is the state of a continuous interval or of an interval.
type ContinuousState int

// Interval states relative to now.
const (
	StateScheduled ContinuousState = iota + 1
	StateActive
	StateExpired
)

// continuous is the block group's current continuous interval: the one
// active now, else the first scheduled one, else the last expired one.
type continuous struct {
	segment
	quantity uint64
	state    ContinuousState
}

func (g *BlockGroup) continuousAt(now time.Time) (continuous, bool) {
	segs := g.segments()
	if len(segs) == 0 {
		return continuous{}, false
	}
	for _, s := range segs {
		switch {
		case !now.Before(s.start) && now.Before(s.end):
			return continuous{segment: s, quantity: g.limitAt(now), state: StateActive}, true
		case now.Before(s.start):
			return continuous{segment: s, quantity: g.limitAt(s.start), state: StateScheduled}, true
		}
	}
	last := segs[len(segs)-1]
	return continuous{segment: last, quantity: g.limitAt(last.end.Add(-time.Nanosecond)), state: StateExpired}, true
}

func (iv Interval) stateAt(now time.Time) ContinuousState {
	switch {
	case now.Before(iv.Start):
		return StateScheduled
	case now.Before(iv.End):
		return StateActive
	default:
		return StateExpired
	}
}

// effectiveLimit is what a project may use of a group whose current limit is
// groupLimit: the allowance limit capped at the group's size, or the group's
// size without a limit (docs: project limits).
func effectiveLimit(allowance *uint64, groupLimit uint64) uint64 {
	if allowance == nil {
		return groupLimit
	}
	return min(*allowance, groupLimit)
}

// percentage formats usage as a share of limit, e.g. "37.5%".
func percentage(usage, limit uint64) string {
	if limit == 0 {
		if usage == 0 {
			return "0%"
		}
		return "100%"
	}
	p := math.Round(float64(usage)/float64(limit)*10000) / 100
	return strconv.FormatFloat(p, 'f', -1, 64) + "%"
}
