// Package ratelimit is a token bucket per caller.
//
// It exists because the cheapest denial-of-service against a message bus is not a
// flood of huge messages — body size is capped — it is a flood of small ones: each
// one costs the homeserver a write and every other agent a read.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter hands out tokens per key at a fixed refill rate.
type Limiter struct {
	// PerMinute is the sustained rate. Zero or negative disables the limiter
	// entirely, which is the right default for a single-user deployment.
	PerMinute int

	mu     sync.Mutex
	bucket map[string]*state
}

type state struct {
	tokens float64
	last   time.Time
}

// New returns a limiter at perMinute, or nil when the rate is disabled.
//
// Returning nil rather than a disabled struct is deliberate: nil is the value
// that makes every call site's happy path just work, so nobody has to remember
// to check a flag.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	return &Limiter{PerMinute: perMinute, bucket: map[string]*state{}}
}

// Allow consumes one token for key, or reports how long to wait.
//
// Burst equals one minute's quota: an agent that has been quiet may spend it all
// at once, which is what makes "read a batch, then answer each" work without
// tripping the limit.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.PerMinute <= 0 {
		return true, 0
	}
	now := time.Now()
	capacity := float64(l.PerMinute)

	l.mu.Lock()
	defer l.mu.Unlock()

	s, ok := l.bucket[key]
	if !ok {
		s = &state{tokens: capacity, last: now}
		l.bucket[key] = s
	} else {
		elapsed := now.Sub(s.last).Minutes()
		s.tokens += elapsed * capacity
		if s.tokens > capacity {
			s.tokens = capacity
		}
		s.last = now
	}

	if s.tokens >= 1 {
		s.tokens--
		return true, 0
	}
	// 一个 token 要多久：60s / 每分钟速率。
	wait := time.Duration(float64(time.Minute) / capacity)
	return false, wait
}
