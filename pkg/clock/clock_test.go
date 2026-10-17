package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func recv(c <-chan time.Time) (time.Time, bool) {
	select {
	case v := <-c:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestTimerFiresOnceAtDeadline(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(10 * time.Second)
	f.Advance(9 * time.Second)
	if _, ok := recv(tm.C()); ok {
		t.Fatal("fired early")
	}
	f.Advance(time.Second)
	v, ok := recv(tm.C())
	if !ok || !v.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("got %v %v", v, ok)
	}
	f.Advance(time.Hour)
	if _, ok := recv(tm.C()); ok {
		t.Fatal("fired twice")
	}
	if !f.Now().Equal(t0.Add(10*time.Second + time.Hour)) {
		t.Fatalf("now = %v", f.Now())
	}
}

func TestTimerStop(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop should report it was active")
	}
	if tm.Stop() {
		t.Fatal("second Stop should report inactive")
	}
	f.Advance(time.Minute)
	if _, ok := recv(tm.C()); ok {
		t.Fatal("stopped timer fired")
	}
}

func TestTimerReset(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(5 * time.Second)
	f.Advance(3 * time.Second)
	if !tm.Reset(5 * time.Second) {
		t.Fatal("Reset of active timer returns true")
	}
	f.Advance(4 * time.Second)
	if _, ok := recv(tm.C()); ok {
		t.Fatal("fired at old deadline")
	}
	f.Advance(time.Second)
	if _, ok := recv(tm.C()); !ok {
		t.Fatal("did not fire at new deadline")
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset of fired timer returns false")
	}
	f.Advance(time.Second)
	if _, ok := recv(tm.C()); !ok {
		t.Fatal("did not fire after Reset")
	}
}

func TestTickerDropsUnreadTicks(t *testing.T) {
	f := NewFake(t0)
	tk := f.NewTicker(time.Second)
	f.Advance(5 * time.Second)
	n := 0
	for {
		if _, ok := recv(tk.C()); !ok {
			break
		}
		n++
	}
	if n != 1 {
		t.Fatalf("got %d buffered ticks, want 1", n)
	}
	f.Advance(time.Second)
	if _, ok := recv(tk.C()); !ok {
		t.Fatal("no tick after consuming")
	}
	tk.Stop()
	f.Advance(time.Minute)
	if _, ok := recv(tk.C()); ok {
		t.Fatal("stopped ticker ticked")
	}
}

func TestAdvanceFiresInOrder(t *testing.T) {
	f := NewFake(t0)
	a := f.NewTimer(3 * time.Second)
	b := f.NewTimer(1 * time.Second)
	c := f.NewTimer(2 * time.Second)
	f.Advance(10 * time.Second)
	var got []time.Time
	for _, tm := range []Timer{b, c, a} {
		v, ok := recv(tm.C())
		if !ok {
			t.Fatal("timer did not fire")
		}
		got = append(got, v)
	}
	// Each timer is stamped with its own deadline, which proves they fired in time order.
	for i, want := range []time.Duration{1, 2, 3} {
		if !got[i].Equal(t0.Add(want * time.Second)) {
			t.Fatalf("timer %d stamped %v", i, got[i])
		}
	}
}

func TestWaitForWaiters(t *testing.T) {
	f := NewFake(t0)
	done := make(chan bool)
	go func() { done <- f.WaitForWaiters(2) }()
	f.NewTimer(time.Second)
	select {
	case <-done:
		t.Fatal("returned with one waiter")
	case <-time.After(20 * time.Millisecond):
	}
	f.NewTicker(time.Second)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("timed out")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return")
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	tm := c.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real timer did not fire")
	}
	tk := c.NewTicker(time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker did not tick")
	}
	if c.Now().IsZero() {
		t.Fatal("zero now")
	}
}
