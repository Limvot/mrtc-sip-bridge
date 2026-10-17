package clock

import (
	"sync"
	"time"
)

// Fake is a deterministic clock. Timer channels have capacity 1; ticker channels have capacity 1 and
// drop ticks the receiver has not consumed (like time.Ticker). Safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
	changed chan struct{} // closed and replaced whenever the waiter set changes
}

type fakeWaiter struct {
	f      *Fake
	c      chan time.Time
	when   time.Time
	period time.Duration // 0 for timers
	active bool
}

// NewFake returns a Fake clock reading start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, changed: make(chan struct{})}
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// notifyLocked wakes WaitForWaiters callers. f.mu must be held.
func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *Fake) addLocked(w *fakeWaiter) {
	w.active = true
	f.waiters = append(f.waiters, w)
	f.notifyLocked()
}

func (f *Fake) removeLocked(w *fakeWaiter) bool {
	if !w.active {
		return false
	}
	w.active = false
	for i, x := range f.waiters {
		if x == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			break
		}
	}
	f.notifyLocked()
	return true
}

// NewTimer returns a timer firing once after d of fake time.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{f: f, c: make(chan time.Time, 1), when: f.now.Add(d)}
	f.addLocked(w)
	return (*fakeTimer)(w)
}

// NewTicker returns a ticker firing every d of fake time. d must be positive.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{f: f, c: make(chan time.Time, 1), when: f.now.Add(d), period: d}
	f.addLocked(w)
	return (*fakeTicker)(w)
}

// Advance moves time forward by d, firing due timers and ticks in time order.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		var next *fakeWaiter
		for _, w := range f.waiters {
			if !w.when.After(target) && (next == nil || w.when.Before(next.when)) {
				next = w
			}
		}
		if next == nil {
			break
		}
		f.now = next.when
		select {
		case next.c <- f.now:
		default: // receiver has not consumed the previous tick
		}
		if next.period > 0 {
			next.when = next.when.Add(next.period)
		} else {
			f.removeLocked(next)
		}
	}
	f.now = target
}

// WaitForWaiters blocks until at least n timers/tickers are pending (test synchronisation), or 5 s.
// Returns false on timeout.
func (f *Fake) WaitForWaiters(n int) bool {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		ok := len(f.waiters) >= n
		ch := f.changed
		f.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-deadline.C:
			return false
		}
	}
}

type fakeTimer fakeWaiter

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.f.removeLocked((*fakeWaiter)(t))
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	w := (*fakeWaiter)(t)
	was := t.f.removeLocked(w)
	select { // like Go 1.23+ timers: Reset discards a stale unread value
	case <-w.c:
	default:
	}
	w.when = t.f.now.Add(d)
	t.f.addLocked(w)
	return was
}

type fakeTicker fakeWaiter

func (t *fakeTicker) C() <-chan time.Time { return t.c }

func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.removeLocked((*fakeWaiter)(t))
}
