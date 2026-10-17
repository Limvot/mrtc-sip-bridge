package sipaccount

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// fakeUA is a sipua.UA whose state the test sets and whose OnStateChange the test drives.
type fakeUA struct {
	mu       sync.Mutex
	cfg      sipua.Config
	startErr error
	stopped  bool
	stopErr  error // ctx error seen by Stop
	stopDL   bool  // Stop's ctx had a deadline
	state    sipua.StateEvent
	// onStart, if set, runs in a goroutine after a successful Start (e.g. to emit a result).
	onStart func(f *fakeUA)
}

func (f *fakeUA) Start(context.Context) error {
	if f.startErr == nil && f.onStart != nil {
		go f.onStart(f)
	}
	return f.startErr
}

func (f *fakeUA) Stop(ctx context.Context) error {
	f.mu.Lock()
	f.stopped = true
	f.stopErr = ctx.Err()
	_, f.stopDL = ctx.Deadline()
	f.mu.Unlock()
	return nil
}

func (f *fakeUA) State() sipua.StateEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeUA) Dial(sipua.DialRequest) (sipua.OutboundCall, error) {
	return nil, errors.New("fakeUA: Dial not implemented")
}

func (f *fakeUA) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// emit sets the UA state and reports it through OnStateChange (like the real UA goroutine).
func (f *fakeUA) emit(ev sipua.StateEvent) {
	f.mu.Lock()
	f.state = ev
	cb := f.cfg.OnStateChange
	f.mu.Unlock()
	cb(ev)
}

// uaFactory records every UA it creates.
type uaFactory struct {
	mu    sync.Mutex
	uas   []*fakeUA
	tried []int
	// mk customises each new UA (nil = a UA that starts fine).
	mk func(cfg sipua.Config) (*fakeUA, error)
}

func (u *uaFactory) New(cfg sipua.Config) (sipua.UA, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tried = append(u.tried, cfg.BindPort)
	f := &fakeUA{}
	if u.mk != nil {
		var err error
		f, err = u.mk(cfg)
		if err != nil {
			return nil, err
		}
	}
	f.cfg = cfg
	u.uas = append(u.uas, f)
	return f, nil
}

func (u *uaFactory) last() *fakeUA {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.uas[len(u.uas)-1]
}

func testRuntime(t *testing.T, min, max int) *Runtime {
	t.Helper()
	rt, err := NewRuntime(context.Background(), NetConfig{
		SIPBindIP: "127.0.0.1", ExternalIP: "127.0.0.1",
		SIPPortMin: min, SIPPortMax: max, RTPPortMin: 40000, RTPPortMax: 40100,
		RegisterExpiry: 60 * time.Second, RegisterRefresh: 25 * time.Second,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

var testCreds = Credentials{Username: "12345678", Password: "pw", Registrar: "reg.example.com:5060", Transport: "udp", DisplayNumber: "+15551234567"}

func testAccount(rt *Runtime, f *uaFactory) *Account {
	return rt.NewAccount(AccountConfig{Creds: testCreds, NewUA: f.New})
}

func TestNewRuntimeIPs(t *testing.T) {
	rt := testRuntime(t, 5060, 5061)
	if rt.BindIP().String() != "127.0.0.1" || rt.ExternalIP().String() != "127.0.0.1" {
		t.Fatalf("bind %v external %v", rt.BindIP(), rt.ExternalIP())
	}
	rt.Close()
	rt.Close() // idempotent
	var nilRT *Runtime
	nilRT.Close()
	if err := nilRT.NewAccount(AccountConfig{}).Start(context.Background()); err == nil {
		t.Fatal("account of a nil runtime started")
	}
	if _, err := NewRuntime(context.Background(), NetConfig{SIPBindIP: "nope"}, zerolog.Nop()); err == nil {
		t.Fatal("invalid bind IP accepted")
	}
}

func TestNewRuntimeKeyPrefix(t *testing.T) {
	for _, tc := range []struct {
		prefix, want string
		cfg          NetConfig
	}{
		{"", "network.sip_bind_ip:", NetConfig{SIPBindIP: "nope"}},
		{"network.calls.", "network.calls.sip_bind_ip:", NetConfig{SIPBindIP: "nope"}},
		{"", "network.external_ip:", NetConfig{SIPBindIP: "127.0.0.1", ExternalIP: "bad"}},
		{"network.calls.", "network.calls.external_ip:", NetConfig{SIPBindIP: "127.0.0.1", ExternalIP: "bad"}},
	} {
		tc.cfg.KeyPrefix = tc.prefix
		_, err := NewRuntime(context.Background(), tc.cfg, zerolog.Nop())
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("prefix %q: error %v, want prefix %q", tc.prefix, err, tc.want)
		}
	}
}

func TestStartSkipsBusyPorts(t *testing.T) {
	f := &uaFactory{mk: func(cfg sipua.Config) (*fakeUA, error) {
		u := &fakeUA{}
		if cfg.BindPort < 5062 {
			u.startErr = errors.New("address already in use")
		}
		return u, nil
	}}
	rt := testRuntime(t, 5060, 5064)
	a := testAccount(rt, f)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.tried) != 3 || f.tried[2] != 5062 || a.port != 5062 {
		t.Fatalf("tried %v, port %d", f.tried, a.port)
	}
	cfg := f.last().cfg
	if cfg.Username != "12345678" || cfg.Registrar != "reg.example.com:5060" || cfg.BindIP != "127.0.0.1" ||
		cfg.ExternalIP != "127.0.0.1" || cfg.Expiry != 60*time.Second || cfg.RefreshInterval != 25*time.Second {
		t.Fatalf("UA config = %+v", cfg)
	}
	// Skipped ports are released again; only 5062 stays used.
	if p, _ := rt.ports.Acquire(); p != 5060 {
		t.Fatalf("5060 not released: got %d", p)
	}
	if p, _ := rt.ports.Acquire(); p != 5061 {
		t.Fatalf("5061 not released: got %d", p)
	}
	if p, _ := rt.ports.Acquire(); p != 5063 {
		t.Fatalf("expected 5063 after 5062 in use, got %d", p)
	}
}

func TestStartAllPortsFail(t *testing.T) {
	want := errors.New("address already in use")
	f := &uaFactory{mk: func(sipua.Config) (*fakeUA, error) { return &fakeUA{startErr: want}, nil }}
	rt := testRuntime(t, 5060, 5062)
	a := testAccount(rt, f)
	if err := a.Start(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if len(f.tried) != 3 {
		t.Fatalf("tries = %d", len(f.tried))
	}
	if a.Running() {
		t.Fatal("running after failure")
	}
	for i := 0; i < 3; i++ {
		if _, err := rt.ports.Acquire(); err != nil {
			t.Fatalf("port %d leaked: %v", i, err)
		}
	}
}

func TestStartConfigErrorReleasesPort(t *testing.T) {
	want := errors.New("bad config")
	f := &uaFactory{mk: func(sipua.Config) (*fakeUA, error) { return nil, want }}
	rt := testRuntime(t, 5060, 5062)
	a := testAccount(rt, f)
	if err := a.Start(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if len(f.tried) != 1 {
		t.Fatalf("config error must not try other ports: %v", f.tried)
	}
	if p, _ := rt.ports.Acquire(); p != 5060 {
		t.Fatalf("port not released: got %d", p)
	}
}

func TestStopIdempotentAndDetached(t *testing.T) {
	f := &uaFactory{}
	rt := testRuntime(t, 5060, 5060)
	a := testAccount(rt, f)
	a.Stop(context.Background()) // no UA: no-op
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil || len(f.uas) != 1 {
		t.Fatalf("second Start must be a no-op: %v, %d UAs", err, len(f.uas))
	}
	ua := f.last()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Stop(ctx)
	if !ua.isStopped() || ua.stopErr != nil || !ua.stopDL {
		t.Fatalf("stopped=%v ctxErr=%v deadline=%v: unregister must run under a detached timeout", ua.stopped, ua.stopErr, ua.stopDL)
	}
	a.Stop(context.Background()) // idempotent
	if a.Running() {
		t.Fatal("running after Stop")
	}
	if p, err := rt.ports.Acquire(); err != nil || p != 5060 {
		t.Fatalf("port not released: %d %v", p, err)
	}
	rt.ports.Release(5060)
	if _, err := a.Dial(sipua.DialRequest{}); !errors.Is(err, sipua.ErrNotRegistered) {
		t.Fatalf("Dial after Stop: %v", err)
	}
}

func TestOnStateIgnoresOldUA(t *testing.T) {
	f := &uaFactory{}
	rt := testRuntime(t, 5060, 5062)
	var mu sync.Mutex
	var seen []sipua.State
	a := rt.NewAccount(AccountConfig{Creds: testCreds, NewUA: f.New, OnState: func(ev sipua.StateEvent) {
		mu.Lock()
		seen = append(seen, ev.State)
		mu.Unlock()
	}})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := f.last()
	a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur := f.last()
	old.emit(sipua.StateEvent{State: sipua.StateRegistered})
	if a.State().State != sipua.StateIdle {
		t.Fatal("event from an older UA was applied")
	}
	cur.emit(sipua.StateEvent{State: sipua.StateRegistering})
	if a.State().State != sipua.StateRegistering {
		t.Fatal("event from the current UA was dropped")
	}
	a.Stop(context.Background())
	cur.emit(sipua.StateEvent{State: sipua.StateRegistered})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != sipua.StateRegistering {
		t.Fatalf("OnState saw %v, want only the current UA's registering", seen)
	}
}

func TestWaitFirstResultLatchPerStart(t *testing.T) {
	f := &uaFactory{}
	rt := testRuntime(t, 5060, 5062)
	a := testAccount(rt, f)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.last().emit(sipua.StateEvent{State: sipua.StateRegistering}) // not a result
	f.last().emit(sipua.StateEvent{State: sipua.StateAuthFailed, StatusCode: 403})
	f.last().emit(sipua.StateEvent{State: sipua.StateRegistered}) // latch already fired
	ev, err := a.WaitFirstResult(context.Background(), time.Second)
	if err != nil || ev.State != sipua.StateAuthFailed {
		t.Fatalf("first result %+v %v", ev, err)
	}
	if _, err = a.WaitFirstResult(context.Background(), 10*time.Millisecond); !errors.Is(err, ErrRegisterTimeout) {
		t.Fatalf("second wait without a new Start: %v", err)
	}
	a.Stop(context.Background())
	if err = a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.last().emit(sipua.StateEvent{State: sipua.StateRegistered})
	if ev, err = a.WaitFirstResult(context.Background(), time.Second); err != nil || ev.State != sipua.StateRegistered {
		t.Fatalf("after restart %+v %v", ev, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.WaitFirstResult(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}

func emitting(ev sipua.StateEvent) *uaFactory {
	return &uaFactory{mk: func(sipua.Config) (*fakeUA, error) {
		return &fakeUA{onStart: func(f *fakeUA) { f.emit(ev) }}, nil
	}}
}

func TestRegister(t *testing.T) {
	tests := []struct {
		name    string
		f       *uaFactory
		timeout time.Duration
		cancel  bool
		state   sipua.State
		errIs   error
		running bool
		outcome string
	}{
		{"registered", emitting(sipua.StateEvent{State: sipua.StateRegistered}), time.Second, false, sipua.StateRegistered, nil, true, "ok"},
		{"auth failed", emitting(sipua.StateEvent{State: sipua.StateAuthFailed, StatusCode: 403}), time.Second, false, sipua.StateAuthFailed, nil, false, "auth_failed"},
		{"transient", emitting(sipua.StateEvent{State: sipua.StateTransientError, Reason: "timeout"}), time.Second, false, sipua.StateTransientError, nil, false, "transient_error"},
		{"timeout", &uaFactory{}, 20 * time.Millisecond, false, sipua.StateIdle, ErrRegisterTimeout, false, "timeout"},
		{"ctx cancel", &uaFactory{}, time.Second, true, sipua.StateIdle, context.Canceled, false, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := testRuntime(t, 5060, 5060)
			a := testAccount(rt, tt.f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
			}
			ev, err := Register(ctx, a, tt.timeout)
			if ev.State != tt.state || (tt.errIs == nil) != (err == nil) || (tt.errIs != nil && !errors.Is(err, tt.errIs)) {
				t.Fatalf("Register = %+v, %v", ev, err)
			}
			if a.Running() != tt.running {
				t.Fatalf("running = %v, want %v", a.Running(), tt.running)
			}
			if !tt.running && !tt.f.last().isStopped() {
				t.Fatal("UA not stopped")
			}
			if got := Outcome(ev, err); got != tt.outcome {
				t.Fatalf("Outcome = %q, want %q", got, tt.outcome)
			}
		})
	}
	// A start error is returned as is.
	rt := testRuntime(t, 5060, 5060)
	boom := errors.New("bad config")
	a := testAccount(rt, &uaFactory{mk: func(sipua.Config) (*fakeUA, error) { return nil, boom }})
	if _, err := Register(context.Background(), a, time.Second); !errors.Is(err, boom) {
		t.Fatalf("start error: %v", err)
	}
}

func TestOutcome(t *testing.T) {
	for _, tt := range []struct {
		ev   sipua.StateEvent
		err  error
		want string
	}{
		{sipua.StateEvent{State: sipua.StateRegistered}, nil, "ok"},
		{sipua.StateEvent{State: sipua.StateAuthFailed}, nil, "auth_failed"},
		{sipua.StateEvent{State: sipua.StateTransientError}, nil, "transient_error"},
		{sipua.StateEvent{}, ErrRegisterTimeout, "timeout"},
		{sipua.StateEvent{}, errors.New("x"), "error"},
		{sipua.StateEvent{}, context.Canceled, "error"},
		{sipua.StateEvent{State: sipua.StateRegistering}, nil, "error"},
	} {
		if got := Outcome(tt.ev, tt.err); got != tt.want {
			t.Errorf("Outcome(%v, %v) = %q, want %q", tt.ev.State, tt.err, got, tt.want)
		}
	}
}

func TestRegistrationsGauge(t *testing.T) {
	env := obstest.Setup(t)
	rt := testRuntime(t, 5060, 5062) // after obs.Use: the gauge callback is registered on the test meter
	f := &uaFactory{}
	a1, a2 := testAccount(rt, f), testAccount(rt, f)
	if err := a1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	u1 := f.last()
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	u2 := f.last()
	u1.emit(sipua.StateEvent{State: sipua.StateRegistered})
	u2.emit(sipua.StateEvent{State: sipua.StateAuthFailed})
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 1 {
		t.Errorf("registered = %v", got)
	}
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "auth_failed"); got != 1 {
		t.Errorf("auth_failed = %v", got)
	}
	u2.emit(sipua.StateEvent{State: sipua.StateRegistered})
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 2 {
		t.Errorf("registered (two accounts) = %v", got)
	}
	a1.Stop(context.Background())
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 1 {
		t.Errorf("registered after Stop = %v", got)
	}
	a2.Stop(context.Background())
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 0 {
		t.Errorf("registered after both stopped = %v", got)
	}
}

type rejectCall struct {
	sipua.InboundCall
	code int
}

func (r *rejectCall) Reject(code int, _ string) { r.code = code }

func TestOnInviteDefault480(t *testing.T) {
	f := &uaFactory{}
	rt := testRuntime(t, 5060, 5060)
	a := testAccount(rt, f)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc := &rejectCall{}
	f.last().cfg.OnInvite(rc)
	if rc.code != 480 {
		t.Fatalf("reject code = %d, want 480", rc.code)
	}
	var got sipua.InboundCall
	b := rt.NewAccount(AccountConfig{Creds: testCreds, NewUA: f.New, OnInvite: func(c sipua.InboundCall) { got = c }})
	a.Stop(context.Background())
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc2 := &rejectCall{}
	f.last().cfg.OnInvite(rc2)
	if got != rc2 || rc2.code != 0 {
		t.Fatal("invite not handed to OnInvite")
	}
	if b.Creds() != testCreds {
		t.Fatal("Creds mismatch")
	}
}
