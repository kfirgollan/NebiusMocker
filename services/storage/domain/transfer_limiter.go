package domain

import (
	"context"
	"io"
	"math"
	"math/bits"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// tokenBucket is a token bucket of capacity rate that refills rate tokens per
// second on an injected clock. It starts full. A nil bucket is unlimited.
//
// Upstream: docs.nebius.com/object-storage/transfer/overview, "Limiters".
type tokenBucket struct {
	clk  clock.Clock
	rate uint64 // tokens per second, also the capacity

	mu     sync.Mutex
	tokens uint64
	last   time.Time
}

// newTokenBucket returns a full bucket, or nil (unlimited) when rate is zero.
func newTokenBucket(clk clock.Clock, rate uint64) *tokenBucket {
	if rate == 0 {
		return nil
	}
	return &tokenBucket{clk: clk, rate: rate, tokens: rate, last: clk.Now()}
}

// Take consumes n tokens, waiting on the clock for refills while the bucket
// is empty. It returns ctx.Err() when ctx is cancelled while waiting.
func (b *tokenBucket) Take(ctx context.Context, n uint64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.refill(b.clk.Now())
		if b.tokens > 0 {
			got := min(n, b.tokens)
			b.tokens -= got
			n -= got
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.clk.After(b.waitFor(min(n, b.rate))):
		}
	}
	return nil
}

// refill adds the tokens earned since last, capped at the capacity. Integer
// arithmetic keeps waits exact: after waitFor(d) at least d tokens are earned.
func (b *tokenBucket) refill(now time.Time) {
	if !now.After(b.last) {
		return
	}
	elapsed := nanos(now.Sub(b.last))
	hi, lo := bits.Mul64(elapsed, b.rate)
	if hi >= uint64(time.Second) {
		b.tokens, b.last = b.rate, now
		return
	}
	gained, rem := bits.Div64(hi, lo, uint64(time.Second))
	if gained >= b.rate-b.tokens {
		b.tokens, b.last = b.rate, now
		return
	}
	b.tokens += gained
	// Keep the unspent fraction of a token for the next refill.
	b.last = now.Add(-durationOf(rem / b.rate))
}

// waitFor returns how long it takes to earn d tokens (d <= rate), rounded up.
func (b *tokenBucket) waitFor(d uint64) time.Duration {
	hi, lo := bits.Mul64(d, uint64(time.Second))
	ns, rem := bits.Div64(hi, lo, b.rate)
	if rem != 0 {
		ns++
	}
	return durationOf(ns)
}

func nanos(d time.Duration) uint64 {
	if d < 0 {
		return 0
	}
	return uint64(d)
}

func durationOf(ns uint64) time.Duration {
	if ns > math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(ns)
}

// limitedReader charges every byte read from r to a bandwidth bucket. Reads
// are capped at the bucket capacity so one read never outruns a second's
// budget.
type limitedReader struct {
	r    io.Reader
	take func(n uint64) error
	max  int
}

func newLimitedReader(ctx context.Context, r io.Reader, b *tokenBucket) io.Reader {
	if b == nil {
		return r
	}
	return &limitedReader{
		r:    r,
		take: func(n uint64) error { return b.Take(ctx, n) },
		max:  int(min(b.rate, uint64(1<<30))),
	}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if len(p) > l.max {
		p = p[:l.max]
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if terr := l.take(uint64(n)); terr != nil {
			return 0, terr
		}
	}
	return n, err
}
