package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/jiin-yang/notification-dispatcher/internal/domain"
)

// ChannelLimiter holds one token-bucket limiter per channel. The zero value
// is not usable; construct with New.
type ChannelLimiter struct {
	mu       sync.Mutex
	limiters map[domain.Channel]*rate.Limiter
	rps      rate.Limit
	burst    int
}

// New returns a ChannelLimiter that allows rps tokens per second with the
// given burst for each channel. Limiters are created lazily on first use.
func New(rps float64, burst int) *ChannelLimiter {
	return &ChannelLimiter{
		limiters: make(map[domain.Channel]*rate.Limiter),
		rps:      rate.Limit(rps),
		burst:    burst,
	}
}

// limiterFor returns the per-channel limiter, creating it on first access.
func (c *ChannelLimiter) limiterFor(ch domain.Channel) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limiters[ch]
	if !ok {
		l = rate.NewLimiter(c.rps, c.burst)
		c.limiters[ch] = l
	}
	return l
}

// AllowChannel checks whether n tokens are available for ch right now.
// If ok is false, retryAfter is the minimum wait before n tokens will be
// available again. When ok is true, the tokens are consumed.
func (c *ChannelLimiter) AllowChannel(ch domain.Channel, n int) (ok bool, retryAfter time.Duration) {
	l := c.limiterFor(ch)
	now := time.Now()
	r := l.ReserveN(now, n)
	if !r.OK() {
		// n exceeds burst — never satisfiable.
		return false, time.Duration(float64(n)/float64(c.rps)*float64(time.Second)) + time.Second
	}
	delay := r.DelayFrom(now)
	if delay <= 0 {
		return true, 0
	}
	// Not immediately available — cancel the reservation and report how long.
	r.Cancel()
	return false, delay
}
