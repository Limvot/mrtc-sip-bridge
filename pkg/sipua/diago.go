package sipua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// UA is one registered SIP account (the interface itself lives in pkg/leg and is re-exported
// from alias.go, ADR-014; the SIP semantics are: Start binds the SIP socket and registers,
// keeping the registration alive until Stop, which unregisters OUR Contact only; Dial sends
// the INVITE on the call's own goroutine and returns ErrNotRegistered when not registered).

// SetLibraryLogger routes sipgo's package-level logging through log, capped at Warn (D21), so raw SIP
// never reaches stderr. Call once at startup.
func SetLibraryLogger(log zerolog.Logger) {
	sip.SetDefaultLogger(slog.New(zerolog.NewSlogHandler(log.With().Str("component", "sipgo").Logger().Level(zerolog.WarnLevel))))
	// diago's media package logs through its own default logger (slog.Default otherwise); it warns once
	// per packet for an unsupported payload type, which any stray sender can trigger, so drop that one.
	mlog := log.With().Str("component", "diago").Logger().Level(zerolog.WarnLevel)
	media.SetDefaultLogger(slog.New(quietHandler{zerolog.NewSlogHandler(mlog)}))
}

// quietHandler drops diago's per-packet "unsupported payload_type" warning (attacker-triggerable spam).
type quietHandler struct{ slog.Handler }

func (h quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "Received RTP with unsupported payload_type") {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h quietHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return quietHandler{h.Handler.WithAttrs(a)}
}
func (h quietHandler) WithGroup(n string) slog.Handler { return quietHandler{h.Handler.WithGroup(n)} }

// New validates cfg (no I/O) and returns an unstarted UA. Validation errors name the field.
func New(cfg Config) (UA, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if _, err := buildTargets(cfg); err != nil {
		return nil, err
	}
	return &diagoUA{cfg: cfg, tracker: &stateTracker{cb: cfg.OnStateChange}}, nil
}

type diagoUA struct {
	cfg     Config
	tracker *stateTracker

	mu      sync.Mutex
	started bool
	stopped bool

	ua          *sipgo.UserAgent
	serveCancel context.CancelFunc
	loopCancel  context.CancelFunc
	loop        *registerLoop
	done        chan struct{}
	dg          *diago.Diago
	aorHost     string

	// dest is the registrar "ip:port" the register loop currently pins (D9); outbound INVITEs go there.
	dest atomic.Pointer[string]

	stopOnce sync.Once
	stopErr  error
}

func (u *diagoUA) State() StateEvent { return u.tracker.state() }

// Dial places an outbound call to the pinned registrar from the UA's bound SIP socket (D9).
func (u *diagoUA) Dial(req DialRequest) (OutboundCall, error) {
	u.mu.Lock()
	err := errUANotRunning(u.started, u.stopped)
	u.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !phone.IsE164(req.Target) {
		return nil, errInvalidTarget
	}
	dest := ""
	if p := u.dest.Load(); p != nil {
		dest = *p
	}
	if dest == "" || u.tracker.state().State != StateRegistered {
		return nil, ErrNotRegistered
	}
	return u.dialTo(req, dest)
}

var errInvalidTarget = errors.New("sipua: dial target is not an E.164 number")

// errUANotRunning returns an error unless the UA is started and not stopped.
func errUANotRunning(started, stopped bool) error {
	switch {
	case stopped:
		return errors.New("sipua: stopped")
	case !started:
		return errors.New("sipua: not started")
	}
	return nil
}

// dialTo places an outbound call to dest without the registration check (test seam; Dial checks it).
func (u *diagoUA) dialTo(req DialRequest, dest string) (OutboundCall, error) {
	if !phone.IsE164(req.Target) {
		return nil, errInvalidTarget
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := errUANotRunning(u.started, u.stopped); err != nil {
		return nil, err
	}
	dlg, err := newDiagoClient(u.dg, u.cfg, u.aorHost, req, dest)
	if err != nil {
		return nil, err
	}
	c := newOutboundCall(dlg, u.cfg.Log.With().Str("component", "sipua").Logger(), clock.Real(), req)
	go c.run()
	return c, nil
}

func (u *diagoUA) Start(ctx context.Context) error {
	_, span := obs.StartChild(ctx, "sip.ua.start", trace.WithAttributes(
		obs.AttrNetworkTransport.String(string(u.cfg.Transport)),
		attribute.Int("network.local.port", u.cfg.BindPort)))
	defer span.End()
	err := u.start(ctx)
	obs.Fail(span, err)
	return err
}

func (u *diagoUA) start(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped {
		return errors.New("sipua: stopped")
	}
	if u.started {
		return errors.New("sipua: already started")
	}
	cfg := u.cfg
	t, err := buildTargets(cfg)
	if err != nil {
		return err
	}
	// Library logs are capped at Warn (D21) and routed through zerolog so raw SIP never reaches stderr.
	slogger := slog.New(zerolog.NewSlogHandler(cfg.Log.With().Str("component", "diago").Logger().Level(zerolog.WarnLevel)))
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.Username),
		sipgo.WithUserAgentHostname(t.AORHost),
		sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(slogger)),
		sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(slogger)),
	)
	if err != nil {
		return fmt.Errorf("create SIP user agent: %w", err)
	}
	dg := diago.NewDiago(ua,
		diago.WithLogger(slogger),
		diago.WithTransport(diago.Transport{
			Transport:       string(cfg.Transport) + "4",
			BindHost:        cfg.BindIP,
			BindPort:        cfg.BindPort,
			ExternalHost:    cfg.ExternalIP,
			ExternalPort:    cfg.ExternalPort,
			MediaExternalIP: net.ParseIP(cfg.ExternalIP),
			RewriteContact:  true,
		}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000}}),
	)

	serveCtx, serveCancel := context.WithCancel(context.Background())
	if err := dg.ServeBackground(serveCtx, u.serveInvite); err != nil {
		serveCancel()
		_ = ua.Close()
		return fmt.Errorf("sip listen %s:%d/%s: %w", cfg.BindIP, cfg.BindPort, cfg.Transport, err)
	}
	rt, err := dg.RegisterTransaction(serveCtx, t.RequestURI, diago.RegisterOptions{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Expiry:        cfg.Expiry,
		RetryInterval: cfg.RefreshInterval,
	})
	if err != nil {
		serveCancel()
		_ = ua.Close()
		return fmt.Errorf("create register transaction: %w", err)
	}
	ensureAORTo(rt.Origin, t.AORUser, t.AORHost)

	pin, err := newPinner(cfg.Registrar, cfg.Resolver)
	if err != nil {
		serveCancel()
		_ = ua.Close()
		return err
	}
	loop := &registerLoop{
		cfg:     cfg,
		startSC: trace.SpanContextFromContext(ctx),
		txn:     &diagoTxn{rt: rt, onDest: func(hostport string) { u.dest.Store(&hostport) }},
		pin:     pin,
		log:     cfg.Log.With().Str("component", "sipua").Logger(),
		emit:    u.tracker.emit,
	}
	loopCtx, loopCancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	u.started = true
	u.ua = ua
	u.serveCancel = serveCancel
	u.loopCancel = loopCancel
	u.loop = loop
	u.done = done
	u.dg = dg
	u.aorHost = t.AORHost
	go func() {
		defer close(done)
		loop.run(loopCtx)
	}()
	return nil
}

func (u *diagoUA) Stop(ctx context.Context) error {
	u.stopOnce.Do(func() {
		u.mu.Lock()
		started := u.started
		u.stopped = true
		u.mu.Unlock()
		if !started {
			return
		}
		u.loopCancel()
		<-u.done
		last := u.tracker.state().State
		u.stopErr = u.loop.shutdown(ctx, last)
		u.serveCancel()
		_ = u.ua.Close()
		u.tracker.emit(StateEvent{State: StateStopped})
	})
	return u.stopErr
}

// serveInvite runs on the diago handler goroutine and blocks until the call ended.
func (u *diagoUA) serveInvite(d *diago.DialogServerSession) {
	if u.cfg.OnInvite == nil {
		u.rejectInbound(d)
		return
	}
	c := newInboundCall(newDiagoDialog(d), u.cfg.Log.With().Str("component", "sipua").Logger(), clock.Real())
	c.serve(u.cfg.OnInvite)
}

// rejectInbound answers an inbound INVITE with 480 (used when no OnInvite handler is configured).
func (u *diagoUA) rejectInbound(d *diago.DialogServerSession) {
	u.cfg.Log.Info().
		Str("component", "sipua").
		Str("from", d.FromUser()).
		Str("call_id", d.InviteRequest.CallID().Value()).
		Msg("Rejecting inbound call: call handling is not implemented yet")
	_ = d.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
}

// diagoTxn adapts *diago.RegisterTransaction to regTxn.
type diagoTxn struct {
	rt     *diago.RegisterTransaction
	onDest func(hostport string) // also told every pinned destination (outbound INVITEs use it, D9)
}

func (t *diagoTxn) Register(ctx context.Context) error {
	// diago's register() does not drop Via, so a second Register would reuse the old branch (D10).
	t.rt.Origin.RemoveHeader("Via")
	return mapErr(t.rt.Register(ctx))
}

func (t *diagoTxn) Refresh(ctx context.Context) error {
	// A full Register (fresh Via) rather than Qualify, so Contact keeps following the response's
	// Via rport/received after a NAT rebinding.
	t.rt.Origin.RemoveHeader("Via")
	return mapErr(t.rt.Register(ctx))
}

func (t *diagoTxn) Unregister(ctx context.Context) error {
	prepareUnregister(t.rt.Origin)
	return mapErr(t.rt.Qualify(ctx))
}

func (t *diagoTxn) SetDestination(hostport string) {
	t.rt.Origin.SetDestination(hostport)
	if t.onDest != nil {
		t.onDest(hostport)
	}
}

func (t *diagoTxn) Contact() string {
	c := t.rt.Origin.Contact()
	if c == nil {
		return ""
	}
	return net.JoinHostPort(c.Address.Host, strconv.Itoa(c.Address.Port))
}

func mapErr(err error) error {
	var rr *diago.RegisterResponseError
	if errors.As(err, &rr) {
		return &ResponseError{StatusCode: rr.RegisterRes.StatusCode, Reason: rr.RegisterRes.Reason}
	}
	return err
}
