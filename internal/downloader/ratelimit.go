package downloader

import (
	"context"
	"io"
	"sync"
	"time"
)

// defaultRateBytesPerSec seeds a fresh RateLimiter with a non-zero rate so
// SetEnabled(true) before any SetBytesPerSec call cannot trigger division by
// zero in the wait loop. One MiB/s is a benign default; callers virtually
// always configure their own rate before enabling.
const defaultRateBytesPerSec = 1 << 20

// maxSleepBetweenChecks bounds how long Wait sleeps before re-evaluating the
// limiter's state. A short cap lets a UI toggle Enabled or change the rate
// and have an already-blocked Wait notice the change quickly.
const maxSleepBetweenChecks = 50 * time.Millisecond

// RateLimiter is a token-bucket throttle shared between the queue runner
// (which calls Wait) and a UI/controller (which calls Set* to mutate the
// rate live). The zero value is unusable — construct via [NewRateLimiter].
//
// All methods are safe for concurrent use.
type RateLimiter struct {
	mu          sync.Mutex
	enabled     bool
	bytesPerSec float64
	tokens      float64
	last        time.Time
}

// NewRateLimiter returns a disabled limiter with a non-zero default rate so
// toggling on without a prior [RateLimiter.SetBytesPerSec] doesn't divide by
// zero.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		bytesPerSec: defaultRateBytesPerSec,
		last:        time.Now(),
	}
}

// Enabled reports whether [RateLimiter.Wait] currently throttles.
func (r *RateLimiter) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

// SetEnabled toggles whether [RateLimiter.Wait] throttles. Flipping off
// while a Wait is blocked unblocks it on the next polling iteration.
func (r *RateLimiter) SetEnabled(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on && !r.enabled {
		r.last = time.Now()
		r.tokens = 0
	}
	r.enabled = on
}

// BytesPerSec returns the current throttle cap in bytes per second.
func (r *RateLimiter) BytesPerSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytesPerSec
}

// SetBytesPerSec updates the cap. Values below 1 are clamped to 1 byte per
// second to avoid division by zero in the wait loop.
func (r *RateLimiter) SetBytesPerSec(bps float64) {
	if bps < 1 {
		bps = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytesPerSec = bps
	if r.tokens > bps {
		r.tokens = bps
	}
}

// Wait blocks until n bytes' worth of tokens have accrued, or ctx is done.
// When disabled, returns nil immediately. Bucket capacity is one second's
// worth of bytes (burst == bytesPerSec).
func (r *RateLimiter) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		r.mu.Lock()
		if !r.enabled {
			r.mu.Unlock()
			return nil
		}
		now := time.Now()
		elapsed := now.Sub(r.last).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		r.tokens += elapsed * r.bytesPerSec
		if r.tokens > r.bytesPerSec {
			r.tokens = r.bytesPerSec
		}
		r.last = now

		need := float64(n) - r.tokens
		if need <= 0 {
			r.tokens -= float64(n)
			r.mu.Unlock()
			return nil
		}
		sleep := time.Duration(need / r.bytesPerSec * float64(time.Second))
		r.mu.Unlock()

		if sleep > maxSleepBetweenChecks {
			sleep = maxSleepBetweenChecks
		}
		if sleep <= 0 {
			sleep = time.Millisecond
		}

		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// limitedReader wraps an [io.Reader] and calls [RateLimiter.Wait] after each
// successful read so the resulting throughput matches the limiter's cap.
// Wait happens post-read (rather than pre-read) so that ctx cancellation
// during the throttle still preserves the bytes already pulled — the
// downloader's [io.Copy] receives the read bytes plus the ctx error and
// flushes them to the .part file before unwinding.
type limitedReader struct {
	r   io.Reader
	lim *RateLimiter
	ctx context.Context
}

// Read implements [io.Reader].
func (l *limitedReader) Read(b []byte) (int, error) {
	n, err := l.r.Read(b)
	if n > 0 {
		if werr := l.lim.Wait(l.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
