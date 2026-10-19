package widget

import (
	"math"
	"sync"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

const (
	bucketIdle    = 30 * time.Minute // buckets idle this long are evicted (WIDGET-SPEC §5.6)
	maxBuckets    = 20000            // per limiter; when full (after eviction) new keys are refused
	failLockout   = 15 * time.Minute // failed-session lockout per IP
	fullRetryWait = time.Minute      // Retry-After when a limiter is full
)

// bucket is one token bucket. lockedUntil is used only by lockout limiters.
type bucket struct {
	tokens      float64
	last        time.Time
	lockedUntil time.Time
}

// limiter is an in-memory token-bucket rate limiter keyed by string (IP, MXID or session hash).
type limiter struct {
	mu      sync.Mutex
	clk     clock.Clock
	rate    float64 // tokens per second
	burst   float64
	lockout time.Duration // >0: exhausting the bucket locks the key out for this long (failure limiter)
	buckets map[string]*bucket
	inserts int
}

func newLimiter(clk clock.Clock, perPeriod int, period time.Duration, burst int) *limiter {
	return &limiter{
		clk:     clk,
		rate:    float64(perPeriod) / period.Seconds(),
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
	}
}

// get returns the refilled bucket of key, creating it (nil when the limiter is full). l.mu held.
func (l *limiter) get(key string, now time.Time) *bucket {
	b := l.buckets[key]
	if b == nil {
		l.inserts++
		if l.inserts%256 == 0 || len(l.buckets) >= maxBuckets {
			l.evictLocked(now)
		}
		if len(l.buckets) >= maxBuckets {
			return nil
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		return b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el*l.rate)
		b.last = now
	}
	return b
}

// retryAfter returns how long until b has one token.
func (l *limiter) retryAfter(b *bucket) time.Duration {
	need := 1 - b.tokens
	if need <= 0 {
		return 0
	}
	return time.Duration(need / l.rate * float64(time.Second))
}

// allow consumes one token of key. When refused it returns the wait until the next token (or the
// remaining lockout).
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	b := l.get(key, now)
	if b == nil {
		return false, fullRetryWait
	}
	if now.Before(b.lockedUntil) {
		return false, b.lockedUntil.Sub(now)
	}
	if b.tokens < 1 {
		return false, l.retryAfter(b)
	}
	b.tokens--
	return true, 0
}

// locked reports whether key is locked out (lockout limiters) and for how long, without consuming.
func (l *limiter) locked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	b, ok := l.buckets[key]
	if !ok || !now.Before(b.lockedUntil) {
		return false, 0
	}
	return true, b.lockedUntil.Sub(now)
}

// exhausted reports whether key has no token left (or is locked out), without consuming. Unknown keys
// are not exhausted.
func (l *limiter) exhausted(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	b, ok := l.buckets[key]
	if !ok {
		return false, 0
	}
	b = l.get(key, now) // refill
	if now.Before(b.lockedUntil) {
		return true, b.lockedUntil.Sub(now)
	}
	if b.tokens < 1 {
		return true, l.retryAfter(b)
	}
	return false, 0
}

// fail consumes one token of key; when that exhausts the bucket the key is locked out for l.lockout.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	b := l.get(key, now)
	if b == nil {
		return
	}
	b.tokens--
	if b.tokens < 1 && l.lockout > 0 {
		b.lockedUntil = now.Add(l.lockout)
		b.tokens = 0
	}
}

// evict drops buckets idle for bucketIdle and not locked out.
func (l *limiter) evict() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictLocked(l.clk.Now())
}

func (l *limiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) >= bucketIdle && !now.Before(b.lockedUntil) {
			delete(l.buckets, k)
		}
	}
}

// size returns the number of tracked keys (tests).
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// limits are the rate limiters of WIDGET-SPEC §5.6.
type limits struct {
	sessionIP   *limiter // session per client IP: 10/min
	sessionFail *limiter // failed session per IP: 5/min, then 15 min lockout
	sessionUser *limiter // session per MXID: 20/min
	call        *limiter // call per MXID: 5/min
	normalize   *limiter // normalize per MXID: 60/min
	dtmf        *limiter // dtmf per MXID: 10/s, burst 32
	hangup      *limiter // hangup per MXID: 30/min
	state       *limiter // state per session: 4/s
	chat        *limiter // chat per MXID: 5/min
	contact     *limiter // contact per MXID: 10/min
	prefs       *limiter // prefs per MXID: 10/min
	contacts    *limiter // contacts per MXID: 5/s, burst 10

	// Dialpad PWA (PWA-SPEC §7.1). Failed enroll/device-session requests count in sessionFail.
	enrollIP     *limiter // device/enroll per client IP: 5/min
	devSessionIP *limiter // device/session per client IP: 10/min
	devSession   *limiter // device/session per device: 30/h
	devConfirm   *limiter // device/confirm per device: 60/h
}

func newLimits(clk clock.Clock) limits {
	l := limits{
		sessionIP:   newLimiter(clk, 10, time.Minute, 10),
		sessionFail: newLimiter(clk, 5, time.Minute, 5),
		sessionUser: newLimiter(clk, 20, time.Minute, 20),
		call:        newLimiter(clk, 5, time.Minute, 5),
		normalize:   newLimiter(clk, 60, time.Minute, 60),
		dtmf:        newLimiter(clk, 10, time.Second, 32),
		hangup:      newLimiter(clk, 30, time.Minute, 30),
		state:       newLimiter(clk, 4, time.Second, 4),
		chat:        newLimiter(clk, 5, time.Minute, 5),
		contact:     newLimiter(clk, 10, time.Minute, 10),
		prefs:       newLimiter(clk, 10, time.Minute, 10),
		contacts:    newLimiter(clk, 5, time.Second, 10),

		enrollIP:     newLimiter(clk, 5, time.Minute, 5),
		devSessionIP: newLimiter(clk, 10, time.Minute, 10),
		devSession:   newLimiter(clk, 30, time.Hour, 30),
		devConfirm:   newLimiter(clk, 60, time.Hour, 60),
	}
	l.sessionFail.lockout = failLockout
	return l
}

func (l limits) all() []*limiter {
	return []*limiter{l.sessionIP, l.sessionFail, l.sessionUser, l.call, l.normalize, l.dtmf, l.hangup, l.state,
		l.chat, l.contact, l.prefs, l.contacts,
		l.enrollIP, l.devSessionIP, l.devSession, l.devConfirm}
}

// evict drops idle buckets of every limiter.
func (l limits) evict() {
	for _, x := range l.all() {
		x.evict()
	}
}
