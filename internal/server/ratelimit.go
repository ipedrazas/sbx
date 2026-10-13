package server

import (
	"math"
	"sync"
	"time"
)

// BindingKind keeps dataset and action buckets in separate namespaces.
type BindingKind int

const (
	BindingDataset BindingKind = iota
	BindingAction
)

// RateDecision is the outcome of RateLimiter.Check.
type RateDecision struct {
	Allowed bool
	Limit   uint32
	// Remaining and ResetSeconds are set when Allowed.
	Remaining    uint32
	ResetSeconds uint32
	// RetryAfter is set when denied (whole seconds, minimum 1).
	RetryAfter uint32
}

type bucket struct {
	capacity     float64
	refillPerSec float64
	tokens       float64
	lastRefill   time.Time
}

func refillRate(count, period uint32) float64 {
	if count == 0 || period == 0 {
		return 0
	}
	return float64(count) / float64(period)
}

func ceilU32(f float64) uint32 {
	c := math.Ceil(f)
	if c >= math.MaxUint32 || math.IsInf(c, 1) {
		return math.MaxUint32
	}
	if c < 0 {
		return 0
	}
	return uint32(c)
}

func (b *bucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens = math.Min(b.tokens+elapsed*b.refillPerSec, b.capacity)
	b.lastRefill = now
}

func (b *bucket) tryTake(now time.Time) RateDecision {
	b.refill(now)
	limit := uint32(b.capacity)
	if b.tokens >= 1 {
		b.tokens--
		reset := uint32(math.MaxUint32)
		if b.refillPerSec > 0 {
			reset = ceilU32((b.capacity - b.tokens) / b.refillPerSec)
		}
		return RateDecision{Allowed: true, Limit: limit, Remaining: uint32(math.Floor(b.tokens)), ResetSeconds: reset}
	}
	retry := uint32(math.MaxUint32)
	if b.refillPerSec > 0 {
		retry = ceilU32((1 - b.tokens) / b.refillPerSec)
	}
	return RateDecision{Limit: limit, RetryAfter: max(retry, 1)}
}

func (b *bucket) resize(count, period uint32, now time.Time) {
	b.refill(now)
	newCap := float64(count)
	if b.capacity > 0 {
		ratio := math.Max(0, math.Min(1, b.tokens/b.capacity))
		b.tokens = ratio * newCap
	} else {
		b.tokens = newCap
	}
	b.capacity = newCap
	b.refillPerSec = refillRate(count, period)
}

type bucketKey struct {
	agentID   int64
	kind      BindingKind
	bindingID int64
}

// RateLimiter is an in-memory token-bucket limiter keyed on
// (agent, binding kind, binding id). It resets on restart.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[bucketKey]*bucket
	now     func() time.Time
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[bucketKey]*bucket{}, now: time.Now}
}

// Check consumes one slot against (count, periodSeconds).
func (rl *RateLimiter) Check(agentID int64, kind BindingKind, bindingID int64, count, periodSeconds uint32) RateDecision {
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	k := bucketKey{agentID, kind, bindingID}
	b, ok := rl.buckets[k]
	if !ok {
		c := float64(count)
		b = &bucket{capacity: c, refillPerSec: refillRate(count, periodSeconds), tokens: c, lastRefill: now}
		rl.buckets[k] = b
	}
	if uint32(b.capacity) != count || math.Abs(b.refillPerSec-refillRate(count, periodSeconds)) > 2.220446049250313e-16 {
		b.resize(count, periodSeconds, now)
	}
	return b.tryTake(now)
}

// Forget drops the bucket for a binding.
func (rl *RateLimiter) Forget(agentID int64, kind BindingKind, bindingID int64) {
	rl.mu.Lock()
	delete(rl.buckets, bucketKey{agentID, kind, bindingID})
	rl.mu.Unlock()
}
