package sipua

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// regTxn is the minimal REGISTER transaction surface. diagoTxn implements it; tests use a fake.
// Implementations must return *ResponseError for non-200 final responses.
type regTxn interface {
	Register(ctx context.Context) error   // full REGISTER: fresh Via, digest, Contact re-derived from rport/received
	Refresh(ctx context.Context) error    // periodic refresh; also a full Register so a NAT-rewritten Contact stays current
	Unregister(ctx context.Context) error // prepareUnregister + send (diago Qualify)
	SetDestination(hostport string)
	Contact() string
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeAuth
	outcomeTransient
)

// classify maps a transaction error to an outcome and the SIP status (0 if none).
func classify(err error) (outcome, int) {
	if err == nil {
		return outcomeOK, 0
	}
	var rr *ResponseError
	if errors.As(err, &rr) {
		switch rr.StatusCode {
		case 401, 403, 404, 407:
			return outcomeAuth, rr.StatusCode
		}
		return outcomeTransient, rr.StatusCode
	}
	return outcomeTransient, 0
}

type registerLoop struct {
	cfg   Config
	txn   regTxn
	pin   *pinner
	log   zerolog.Logger
	sleep func(ctx context.Context, d time.Duration) error // default: timer + ctx.Done; returns ctx.Err()
	rand  func() float64                                   // default math/rand/v2 Float64
	emit  func(StateEvent)                                 // stores current state; calls cfg.OnStateChange on State change only

	// registered is set once the registrar has accepted a REGISTER. A later transient error does not
	// mean the binding is gone, so shutdown still unregisters. Written by run, read after run returns.
	registered bool

	startSC   trace.SpanContext // span context of the UA.Start caller; parents the first sip.register
	prevState State             // last state passed to emitState (run goroutine only)
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// reasonFor returns the status code and credential-free reason text for err.
func reasonFor(err error) (int, string) {
	var rr *ResponseError
	if errors.As(err, &rr) {
		return rr.StatusCode, rr.Reason
	}
	return 0, truncate(err.Error(), 200)
}

// logFor returns the loop's logger enriched with the trace fields of the span in ctx.
func (l *registerLoop) logFor(ctx context.Context) *zerolog.Logger { return obs.Logger(ctx, &l.log) }

// emitState records a sip.registration.state event on span when the state changes, then emits.
func (l *registerLoop) emitState(span trace.Span, ev StateEvent) {
	if ev.State != l.prevState {
		span.AddEvent("sip.registration.state", trace.WithAttributes(
			obs.AttrState.String(ev.State.String()),
			attribute.String("previous_state", l.prevState.String())))
		l.prevState = ev.State
	}
	l.emit(ev)
}

// resolve runs the registrar lookup under a child-only sip.register.resolve span.
func (l *registerLoop) resolve(ctx context.Context) error {
	sctx, span := obs.StartChild(ctx, "sip.register.resolve",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(obs.AttrServerAddress.String(l.pin.host)))
	defer span.End()
	err := l.pin.resolve(sctx)
	if err != nil {
		obs.Fail(span, err)
		return err
	}
	span.SetAttributes(attribute.Int("sip.registrar.ip_count", len(l.pin.ips)))
	return nil
}

// recordAttempt counts one REGISTER transaction result and its duration.
func recordAttempt(ctx context.Context, kind, outcome string, d time.Duration) {
	opt := metric.WithAttributes(obs.AttrSIPRegisterKind.String(kind), obs.AttrOutcome.String(outcome))
	m := obs.M()
	m.RegisterAttempts.Add(ctx, 1, opt)
	m.RegisterDuration.Record(ctx, d.Seconds(), opt)
}

func outcomeName(oc outcome) string {
	switch oc {
	case outcomeOK:
		return "ok"
	case outcomeAuth:
		return "auth"
	}
	return "transient"
}

func (l *registerLoop) run(ctx context.Context) {
	if l.sleep == nil {
		l.sleep = defaultSleep
	}
	if l.rand == nil {
		l.rand = rand.Float64
	}
	user := MaskUsername(l.cfg.Username)
	// The first sip.register is a child of the Start caller's span context; later ones are roots.
	firstSC := l.startSC
	first := true
	noSpan := trace.SpanFromContext(context.Background())

	l.emitState(noSpan, StateEvent{State: StateRegistering})
	needFull := true
	attempt := 0
	failsOnIP := 0
	wasRegistered := false

	// transientFailure records a transient failure and returns the wait before the next attempt.
	transientFailure := func(lctx context.Context, span trace.Span, status int, reason string, err error, addr string) time.Duration {
		needFull = true
		attempt++
		failsOnIP++
		if failsOnIP >= ipSwitchAfter {
			old := l.pin.current()
			if aerr := l.pin.advance(lctx); aerr != nil {
				l.logFor(lctx).Warn().Err(aerr).Msg("Registrar re-resolve failed while switching IP")
			}
			if cur := l.pin.current(); cur != old {
				span.AddEvent("sip.registrar.switch", trace.WithAttributes(
					attribute.String("old", old), attribute.String("new", cur)))
			}
			failsOnIP = 0
		}
		wait := jitter(backoffDelay(attempt), l.rand())
		l.emitState(span, StateEvent{State: StateTransientError, StatusCode: status, Reason: reason, Err: err, RegistrarAddr: addr, NextRetry: wait})
		l.logFor(lctx).Warn().Str("registrar_addr", addr).Int("status", status).Str("reason", reason).
			Dur("next_retry", wait).Str("username", user).Msg("SIP registration attempt failed")
		return wait
	}

	if err := l.resolve(obs.WithParent(ctx, firstSC)); err != nil {
		first = false
		wait := transientFailure(ctx, noSpan, 0, "resolve registrar: "+truncate(err.Error(), 200), err, "")
		if l.sleep(ctx, wait) != nil {
			return
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if l.pin.current() == "" {
			if err := l.resolve(ctx); err != nil {
				first = false
				wait := transientFailure(ctx, noSpan, 0, "resolve registrar: "+truncate(err.Error(), 200), err, "")
				if l.sleep(ctx, wait) != nil {
					return
				}
				continue
			}
		}
		addr := l.pin.current()
		l.txn.SetDestination(addr)
		kind := "refresh"
		if needFull {
			kind = "full"
		}
		lctx, span := ctx, noSpan
		if needFull {
			base := ctx
			if first {
				base = obs.WithParent(ctx, firstSC)
			}
			lctx, span = obs.Start(base, "sip.register", trace.WithSpanKind(trace.SpanKindClient))
		}
		first = false
		t0 := time.Now()
		actx, cancel := context.WithTimeout(lctx, attemptTimeout)
		var err error
		if needFull {
			err = l.txn.Register(actx)
		} else {
			err = l.txn.Refresh(actx)
		}
		cancel()
		if ctx.Err() != nil {
			span.End()
			return
		}

		oc, status := classify(err)
		recordAttempt(ctx, kind, outcomeName(oc), time.Since(t0))
		if oc != outcomeOK {
			wasRegistered = false
			if !needFull {
				// A failed refresh is worth a span, recorded after the fact (successful refreshes get none).
				lctx, span = obs.Start(ctx, "sip.register",
					trace.WithSpanKind(trace.SpanKindClient), trace.WithTimestamp(t0))
			}
			obs.Fail(span, err)
		}
		if span.IsRecording() {
			span.SetAttributes(
				obs.AttrSIPRegisterKind.String(kind),
				obs.AttrSIPRegisterAttempt.Int(attempt+1),
				obs.AttrSIPRegistrarAddress.String(addr),
				obs.AttrSIPUserMasked.String(user),
				obs.AttrOutcome.String(outcomeName(oc)))
			if status != 0 {
				span.SetAttributes(obs.AttrSIPStatusCode.Int(status))
			}
		}

		var wait time.Duration
		switch oc {
		case outcomeOK:
			attempt, failsOnIP, needFull = 0, 0, false
			wait = l.cfg.RefreshInterval
			contact := l.txn.Contact()
			span.SetAttributes(obs.AttrSIPContact.String(contact))
			l.emitState(span, StateEvent{State: StateRegistered, RegistrarAddr: addr, Contact: contact})
			ev := l.logFor(lctx).Debug()
			if !wasRegistered {
				ev = l.logFor(lctx).Info()
			}
			ev.Str("registrar_addr", addr).Str("contact", contact).Str("username", user).Msg("SIP registered")
			wasRegistered = true
			l.registered = true
		case outcomeAuth:
			needFull, attempt, failsOnIP = true, 0, 0
			wait = authRetryInterval
			_, reason := reasonFor(err)
			l.emitState(span, StateEvent{State: StateAuthFailed, StatusCode: status, Reason: reason, Err: err, RegistrarAddr: addr, NextRetry: wait})
			l.logFor(lctx).Warn().Str("registrar_addr", addr).Int("status", status).Str("reason", reason).
				Dur("next_retry", wait).Str("username", user).Msg("SIP registration rejected, check credentials")
		default:
			st, reason := reasonFor(err)
			wait = transientFailure(lctx, span, st, reason, err, addr)
		}
		if oc != outcomeOK {
			span.SetAttributes(obs.AttrSIPRegisterNextRetry.Int64(wait.Milliseconds()))
		}
		span.End()
		if l.sleep(ctx, wait) != nil {
			return
		}
	}
}

// shutdown unregisters our Contact iff the last state was StateRegistered, or the UA was registered at
// any point earlier (the binding may still exist after a TransientError). In the latter case the
// unregister is best effort and its error is ignored.
func (l *registerLoop) shutdown(ctx context.Context, last State) error {
	if last == StateRegistered {
		return l.unregister(ctx)
	}
	if l.registered {
		_ = l.unregister(ctx)
	}
	return nil
}

// unregister sends the un-REGISTER under a sip.unregister span (child of the caller's span, root if none).
func (l *registerLoop) unregister(ctx context.Context) error {
	sctx, span := obs.Start(ctx, "sip.unregister", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(obs.AttrSIPUserMasked.String(MaskUsername(l.cfg.Username))))
	defer span.End()
	t0 := time.Now()
	err := l.txn.Unregister(sctx)
	oc, status := classify(err)
	recordAttempt(ctx, "unregister", outcomeName(oc), time.Since(t0))
	span.SetAttributes(obs.AttrOutcome.String(outcomeName(oc)))
	if status != 0 {
		span.SetAttributes(obs.AttrSIPStatusCode.Int(status))
	}
	obs.Fail(span, err)
	lg := l.logFor(sctx)
	user := MaskUsername(l.cfg.Username)
	if err != nil {
		st, reason := reasonFor(err)
		lg.Warn().Str("username", user).Int("status", st).Str("reason", obs.Scrub(reason)).
			Str("error", obs.Scrub(err.Error())).Msg("SIP unregister failed")
		return err
	}
	lg.Info().Str("username", user).Str("contact", l.txn.Contact()).
		Str("registrar_addr", l.pin.current()).Msg("SIP unregistered")
	return nil
}

// stateTracker stores the latest StateEvent and fires the callback only when State changes.
type stateTracker struct {
	mu  sync.Mutex
	cur StateEvent
	cb  func(StateEvent)
}

func (t *stateTracker) emit(ev StateEvent) {
	t.mu.Lock()
	changed := t.cur.State != ev.State
	t.cur = ev
	t.mu.Unlock()
	if changed && t.cb != nil {
		t.cb(ev)
	}
}

func (t *stateTracker) state() StateEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}
