package sipua

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"
)

type fakeTxn struct {
	results  []error // per Register/Refresh call; beyond the end: nil
	calls    []string
	dests    []string
	unreg    int
	unregErr error
}

func (f *fakeTxn) next(name string) error {
	i := len(f.calls)
	f.calls = append(f.calls, name)
	if i < len(f.results) {
		return f.results[i]
	}
	return nil
}
func (f *fakeTxn) Register(context.Context) error   { return f.next("Register") }
func (f *fakeTxn) Refresh(context.Context) error    { return f.next("Refresh") }
func (f *fakeTxn) Unregister(context.Context) error { f.unreg++; return f.unregErr }
func (f *fakeTxn) SetDestination(hp string)         { f.dests = append(f.dests, hp) }
func (f *fakeTxn) Contact() string                  { return "203.0.113.5:40123" }

type loopHarness struct {
	l      *registerLoop
	txn    *fakeTxn
	states []State
	events []StateEvent
	sleeps []time.Duration
	cancel context.CancelFunc
	ctx    context.Context
}

func newHarness(t *testing.T, registrar string, res Resolver, results []error, maxSleeps int) *loopHarness {
	t.Helper()
	h := &loopHarness{txn: &fakeTxn{results: results}}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	cfg := Config{Username: "12345678", Password: "secret", Registrar: registrar, RefreshInterval: 25 * time.Second}
	pin, err := newPinner(registrar, res)
	if err != nil {
		t.Fatal(err)
	}
	tr := &stateTracker{cb: func(ev StateEvent) {
		h.states = append(h.states, ev.State)
		h.events = append(h.events, ev)
	}}
	h.l = &registerLoop{
		cfg: cfg, txn: h.txn, pin: pin,
		sleep: func(ctx context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			if len(h.sleeps) >= maxSleeps {
				h.cancel()
				return context.Canceled
			}
			return nil
		},
		rand: func() float64 { return 0.5 },
		emit: tr.emit,
	}
	return h
}

func twoIPs() *fakeResolver {
	return &fakeResolver{results: [][]net.IP{ips("1.1.1.1", "2.2.2.2")}}
}

func eq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestLoopSuccessThenRefresh(t *testing.T) {
	h := newHarness(t, "reg.example.com:5008", twoIPs(), nil, 2)
	h.l.run(h.ctx)
	eq(t, "states", h.states, []State{StateRegistering, StateRegistered})
	eq(t, "calls", h.txn.calls, []string{"Register", "Refresh"})
	eq(t, "sleeps", h.sleeps, []time.Duration{25 * time.Second, 25 * time.Second})
	if h.events[1].Contact != "203.0.113.5:40123" || h.events[1].RegistrarAddr != "1.1.1.1:5008" {
		t.Errorf("registered event: %+v", h.events[1])
	}
}

func TestLoopAuthFailure(t *testing.T) {
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{&ResponseError{StatusCode: 401, Reason: "Unauthorized"}}, 1)
	h.l.run(h.ctx)
	eq(t, "states", h.states, []State{StateRegistering, StateAuthFailed})
	eq(t, "sleeps", h.sleeps, []time.Duration{5 * time.Minute})
	if h.events[1].StatusCode != 401 || h.events[1].NextRetry != 5*time.Minute {
		t.Errorf("event %+v", h.events[1])
	}

	h = newHarness(t, "reg.example.com:5008", twoIPs(), []error{&ResponseError{StatusCode: 403}}, 2)
	h.l.run(h.ctx)
	eq(t, "calls after auth", h.txn.calls, []string{"Register", "Register"})
}

func TestLoopTransientIPSwitch(t *testing.T) {
	boom := errors.New("timeout")
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{boom, boom, boom}, 3)
	h.l.run(h.ctx)
	eq(t, "sleeps", h.sleeps, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second})
	eq(t, "dests", h.txn.dests, []string{"1.1.1.1:5008", "1.1.1.1:5008", "2.2.2.2:5008"})
	eq(t, "states", h.states, []State{StateRegistering, StateTransientError})
}

func TestLoopTransientThenSuccess(t *testing.T) {
	boom := errors.New("timeout")
	// fail, ok, fail, ok...
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{boom, nil, boom}, 4)
	h.l.run(h.ctx)
	eq(t, "calls", h.txn.calls, []string{"Register", "Register", "Refresh", "Register"})
	eq(t, "states", h.states, []State{StateRegistering, StateTransientError, StateRegistered, StateTransientError, StateRegistered})
	// Sleep after the second failure restarts at 2s (attempt reset).
	eq(t, "sleeps", h.sleeps, []time.Duration{2 * time.Second, 25 * time.Second, 2 * time.Second, 25 * time.Second})
}

func TestLoopRefreshFailureForcesFullRegister(t *testing.T) {
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{nil, &ResponseError{StatusCode: 503, Reason: "x"}}, 3)
	h.l.run(h.ctx)
	eq(t, "calls", h.txn.calls, []string{"Register", "Refresh", "Register"})
}

func TestLoopDNSFailureAtStart(t *testing.T) {
	res := &fakeResolver{results: [][]net.IP{nil}, errs: []error{errors.New("no such host")}}
	h := newHarness(t, "reg.example.com:5008", res, nil, 1)
	h.l.run(h.ctx)
	eq(t, "states", h.states, []State{StateRegistering, StateTransientError})
	if r := h.events[1].Reason; len(r) < 7 || r[:7] != "resolve" {
		t.Errorf("reason %q", r)
	}
	if len(h.txn.calls) != 0 {
		t.Errorf("txn called: %v", h.txn.calls)
	}
}

func TestLoopShutdown(t *testing.T) {
	h := newHarness(t, "reg.example.com:5008", twoIPs(), nil, 1)
	for _, s := range []State{StateIdle, StateRegistering, StateAuthFailed, StateTransientError, StateStopped} {
		if err := h.l.shutdown(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if h.txn.unreg != 0 {
		t.Errorf("unregister called %d times for non-registered states", h.txn.unreg)
	}
	_ = h.l.shutdown(context.Background(), StateRegistered)
	if h.txn.unreg != 1 {
		t.Errorf("unregister calls = %d", h.txn.unreg)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err    error
		oc     outcome
		status int
	}{
		{nil, outcomeOK, 0},
		{&ResponseError{StatusCode: 401}, outcomeAuth, 401},
		{&ResponseError{StatusCode: 403}, outcomeAuth, 403},
		{&ResponseError{StatusCode: 404}, outcomeAuth, 404},
		{&ResponseError{StatusCode: 407}, outcomeAuth, 407},
		{&ResponseError{StatusCode: 408}, outcomeTransient, 408},
		{&ResponseError{StatusCode: 480}, outcomeTransient, 480},
		{&ResponseError{StatusCode: 500}, outcomeTransient, 500},
		{&ResponseError{StatusCode: 503}, outcomeTransient, 503},
		{context.DeadlineExceeded, outcomeTransient, 0},
		{errors.New("x"), outcomeTransient, 0},
		{fmt.Errorf("wrap: %w", &ResponseError{StatusCode: 401}), outcomeAuth, 401},
	}
	for _, tc := range cases {
		oc, st := classify(tc.err)
		if oc != tc.oc || st != tc.status {
			t.Errorf("classify(%v) = %v,%d want %v,%d", tc.err, oc, st, tc.oc, tc.status)
		}
	}
}

func TestLoopShutdownAfterTransientError(t *testing.T) {
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{nil, &ResponseError{StatusCode: 503, Reason: "x"}}, 2)
	h.l.run(h.ctx)
	if !h.l.registered {
		t.Fatal("loop did not record that it was registered")
	}
	if err := h.l.shutdown(context.Background(), StateTransientError); err != nil {
		t.Fatal(err)
	}
	if h.txn.unreg != 1 {
		t.Errorf("unregister calls = %d, want 1 after a transient error", h.txn.unreg)
	}
}

func TestStartAfterStop(t *testing.T) {
	u, err := New(Config{Username: "12345678", Password: "pw", Registrar: "reg.example.com:5060", BindIP: "127.0.0.1", BindPort: 5060})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := u.Start(context.Background()); err == nil || err.Error() != "sipua: stopped" {
		t.Fatalf("Start after Stop: %v", err)
	}
}
