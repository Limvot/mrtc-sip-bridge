package sipaccount

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// AccountConfig configures one SIP account.
type AccountConfig struct {
	Creds Credentials
	// Log is a clean logger WITHOUT a component key (sipua adds its own). Account logs with component=sip.
	Log zerolog.Logger
	// OnState is called for every state change of the current UA (stale UAs are filtered), on the UA
	// goroutine. Must not block. Optional.
	OnState func(sipua.StateEvent)
	// OnInvite receives inbound calls (diago handler goroutine, must return quickly). nil = 480.
	OnInvite sipua.InviteHandler
	// NewUA is a test seam; nil = sipua.New.
	NewUA func(sipua.Config) (sipua.UA, error)
}

// Account is one SIP registration. All methods are safe for concurrent use.
type Account struct {
	rt    *Runtime
	cfg   AccountConfig
	log   zerolog.Logger // cfg.Log with component=sip (clean: never annotated)
	newUA func(sipua.Config) (sipua.UA, error)

	mu        sync.Mutex
	ua        sipua.UA
	port      int
	last      sipua.StateEvent
	firstCh   chan sipua.StateEvent // buffered 1; replaced by every Start that creates a UA
	firstDone bool
	uaGen     int // bumped for every UA created and on Stop; onState ignores events from older UAs
}

// errNotStarted is returned by Start on an account created from a nil Runtime.
var errNotStarted = errors.New("SIP runtime is not started")

// NewAccount creates a stopped account. It is nil-safe on rt: Start of such an account fails.
func (rt *Runtime) NewAccount(cfg AccountConfig) *Account {
	newUA := cfg.NewUA
	if newUA == nil {
		newUA = sipua.New
	}
	return &Account{
		rt:      rt,
		cfg:     cfg,
		log:     cfg.Log.With().Str("component", "sip").Logger(),
		newUA:   newUA,
		firstCh: make(chan sipua.StateEvent, 1),
	}
}

// Start creates and starts a UA (no-op if one is running): acquires a SIP port, retries the next port
// when Start fails (bind), returns config errors immediately, releases skipped ports at the end. Resets
// the first-result latch.
func (a *Account) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ua != nil {
		return nil
	}
	if a.rt == nil || a.rt.ports == nil {
		return errNotStarted
	}
	rt := a.rt
	a.firstCh = make(chan sipua.StateEvent, 1)
	a.firstDone = false
	// Ports that failed to bind stay marked used while we try the next ones, and are released at the end.
	var skipped []int
	defer func() {
		for _, p := range skipped {
			rt.ports.Release(p)
		}
	}()
	tries := rt.ports.Size()
	var lastErr error
	for i := 0; i < tries; i++ {
		port, err := rt.ports.Acquire()
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		a.uaGen++
		gen := a.uaGen
		var ua sipua.UA
		ua, err = a.newUA(sipua.Config{
			Username:        a.cfg.Creds.Username,
			Password:        a.cfg.Creds.Password,
			Registrar:       a.cfg.Creds.Registrar,
			Domain:          a.cfg.Creds.Domain,
			Transport:       sipua.Transport(a.cfg.Creds.Transport),
			BindIP:          rt.bindIP.String(),
			BindPort:        port,
			ExternalIP:      rt.externalIP.String(),
			Expiry:          rt.cfg.RegisterExpiry,
			RefreshInterval: rt.cfg.RegisterRefresh,
			Log:             a.cfg.Log,
			OnStateChange:   func(ev sipua.StateEvent) { a.onState(gen, ev) },
			OnInvite:        a.onInvite,
		})
		if err != nil {
			// Invalid config: another port will not help.
			rt.ports.Release(port)
			return err
		}
		if err = ua.Start(ctx); err != nil {
			// Most likely the port is in use: keep it marked used while trying the next one.
			lastErr = err
			skipped = append(skipped, port)
			continue
		}
		a.ua = ua
		a.port = port
		a.last = sipua.StateEvent{}
		rt.trackUA(ua)
		return nil
	}
	return lastErr
}

// Stop stops the UA (no-op if none): bumps the generation, unregisters our Contact under a 5 s timeout
// detached from ctx's cancellation (ctx only carries the caller's span), releases the port, logs
// "SIP unregister failed" on error. The host calls calls.Manager.EndLogin BEFORE Stop.
func (a *Account) Stop(ctx context.Context) {
	a.mu.Lock()
	a.uaGen++
	ua, port := a.ua, a.port
	a.ua, a.port = nil, 0
	a.mu.Unlock()
	if ua == nil {
		return
	}
	a.rt.untrackUA(ua)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := ua.Stop(sctx); err != nil {
		obs.Logger(ctx, &a.log).Warn().Err(err).Msg("SIP unregister failed")
	}
	a.rt.ports.Release(port)
}

// Running reports whether a UA is currently started.
func (a *Account) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ua != nil
}

// State returns the latest StateEvent of the current UA (zero value when none).
func (a *Account) State() sipua.StateEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// WaitFirstResult waits for the first Registered/AuthFailed/TransientError since the latest Start. It
// returns ErrRegisterTimeout after timeout and ctx.Err() when ctx ends first.
func (a *Account) WaitFirstResult(ctx context.Context, timeout time.Duration) (sipua.StateEvent, error) {
	a.mu.Lock()
	ch := a.firstCh
	a.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ev := <-ch:
		return ev, nil
	case <-timer.C:
		return sipua.StateEvent{}, ErrRegisterTimeout
	case <-ctx.Done():
		return sipua.StateEvent{}, ctx.Err()
	}
}

// Dial places an outbound call; sipua.ErrNotRegistered when no UA is running.
func (a *Account) Dial(req sipua.DialRequest) (sipua.OutboundCall, error) {
	a.mu.Lock()
	ua := a.ua
	a.mu.Unlock()
	if ua == nil {
		return nil, sipua.ErrNotRegistered
	}
	return ua.Dial(req)
}

// Creds returns a copy of the account's credentials.
func (a *Account) Creds() Credentials { return a.cfg.Creds }

// onState runs on the UA goroutine and must not block.
func (a *Account) onState(gen int, ev sipua.StateEvent) {
	a.mu.Lock()
	if gen != a.uaGen {
		a.mu.Unlock()
		return
	}
	a.last = ev
	if !a.firstDone && (ev.State == sipua.StateRegistered || ev.State == sipua.StateAuthFailed || ev.State == sipua.StateTransientError) {
		a.firstDone = true
		select {
		case a.firstCh <- ev:
		default:
		}
	}
	a.mu.Unlock()
	if a.cfg.OnState != nil {
		a.cfg.OnState(ev)
	}
}

// onInvite hands an inbound call to the host (diago handler goroutine); 480 without a handler.
func (a *Account) onInvite(call sipua.InboundCall) {
	if a.cfg.OnInvite == nil {
		call.Reject(480, "Temporarily Unavailable")
		return
	}
	a.cfg.OnInvite(call)
}
