package jingleleg

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/leg"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// Config configures one UA (one XMPP session's call capability).
type Config struct {
	// LocalBare is our own bare JID (identity checks for carbons/other-resource JMI). Required.
	LocalBare string
	// Transport sends our stanzas. Required.
	Transport Transport
	// PeerDomain is appended to "+E.164" when PeerJID is nil (e.g. "cheogram.com"). Required
	// unless PeerJID is set.
	PeerDomain string
	// PeerJID maps a dialled E.164 to the peer JID. nil = "+"+e164+"@"+PeerDomain.
	PeerJID func(e164 string) (string, error)
	// NewEngine builds the per-call media engine. Required for calls to succeed.
	NewEngine EngineFactory
	// ProposeFirst (default true) sends XEP-0353 <propose/> before session-initiate on
	// outbound calls and expects <proceed/>. false sends session-initiate directly.
	ProposeFirst *bool
	// PeerInitiateGrace is how long we wait for the peer's session-initiate (XEP-0353 says the
	// caller initiates; Cheogram behaves as the initiator on its side of both directions) before
	// initiating ourselves. 0 = 1.5s. Glare (both initiate) is answered by taking the peer's
	// offer as authoritative.
	PeerInitiateGrace time.Duration

	Log zerolog.Logger
	// OnStateChange is called on every state change (Start/Stop). Must not block.
	OnStateChange func(leg.StateEvent)
	// OnInvite receives every new inbound call. It runs on the stanza handler goroutine and
	// MUST return quickly. nil means every inbound call is rejected (480).
	OnInvite leg.InviteHandler
}

func (c *Config) proposeFirst() bool {
	return c.ProposeFirst == nil || *c.ProposeFirst
}

// UA is a Jingle call leg over one XMPP session. It satisfies leg.UA. It does not own the
// session: the host feeds stanzas in via HandleIQ/HandleMessage and sends via Config.Transport.
type UA struct {
	cfg       Config
	transport Transport

	mu       sync.Mutex
	started  bool
	stopped  bool
	state    leg.StateEvent
	sessions map[string]*callCore
	log      zerolog.Logger
}

var _ leg.UA = (*UA)(nil)

// New validates the config and returns a UA. The leg is usable after Start.
func New(cfg Config) (*UA, error) {
	if cfg.LocalBare == "" {
		return nil, fmt.Errorf("jingleleg: LocalBare is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("jingleleg: Transport is required")
	}
	if cfg.PeerJID == nil && cfg.PeerDomain == "" {
		return nil, fmt.Errorf("jingleleg: PeerDomain or PeerJID is required")
	}
	if cfg.NewEngine == nil {
		cfg.NewEngine = func() (Engine, error) { return nil, ErrNoEngine }
	}
	if cfg.PeerInitiateGrace <= 0 {
		cfg.PeerInitiateGrace = 1500 * time.Millisecond
	}
	if cfg.PeerJID == nil {
		domain := cfg.PeerDomain
		cfg.PeerJID = func(e164 string) (string, error) {
			if !phone.IsE164(e164) {
				return "", fmt.Errorf("jingleleg: not an E.164 number")
			}
			return e164 + "@" + domain, nil
		}
	}
	return &UA{
		cfg:       cfg,
		transport: cfg.Transport,
		sessions:  map[string]*callCore{},
		log:       cfg.Log.With().Str("component", "jingleleg").Logger(),
	}, nil
}

// Start marks the leg usable. The host's XMPP session is the transport; there is nothing to
// register (ADR-014). Calling Start twice, or after Stop, returns an error.
func (u *UA) Start(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.started || u.stopped {
		return fmt.Errorf("jingleleg: UA already started or stopped")
	}
	u.started = true
	u.setState(leg.StateEvent{State: leg.StateRegistered})
	return nil
}

// Stop is idempotent: it ends every live call and emits StateStopped.
func (u *UA) Stop(ctx context.Context) error {
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		return nil
	}
	u.stopped = true
	sessions := make([]*callCore, 0, len(u.sessions))
	for _, c := range u.sessions {
		sessions = append(sessions, c)
	}
	u.mu.Unlock()
	for _, c := range sessions {
		c.end(leg.EndFailed, 0, "leg stopped", nil)
	}
	u.setState(leg.StateEvent{State: leg.StateStopped})
	return nil
}

// State returns the latest StateEvent.
func (u *UA) State() leg.StateEvent {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *UA) setState(ev leg.StateEvent) {
	u.state = ev
	if u.cfg.OnStateChange != nil {
		u.cfg.OnStateChange(ev)
	}
}

// Dial starts one outbound call (leg.UA). It returns immediately; the call runs on its own
// events channel and is driven by incoming stanzas.
func (u *UA) Dial(req leg.DialRequest) (leg.OutboundCall, error) {
	u.mu.Lock()
	started, stopped := u.started, u.stopped
	u.mu.Unlock()
	if !started || stopped {
		return nil, leg.ErrNotRegistered
	}
	if !phone.IsE164(req.Target) {
		return nil, fmt.Errorf("jingleleg: invalid target")
	}
	peerJID, err := u.cfg.PeerJID(req.Target)
	if err != nil {
		return nil, err
	}
	sid := newSID()
	now := time.Now()
	core := &callCore{
		u:        u,
		sid:      sid,
		outbound: true,
		peerBare: bareOf(peerJID),
		peerFull: peerJID,
		dial:     req,
		dialInfo: leg.DialInfo{CallID: sid, Target: req.Target, RequestUser: peerJID, StartedAt: now},
		events:   make(chan leg.CallEvent, 16),
		log:      u.log.With().Str("call_id", sid).Logger(),
	}
	u.mu.Lock()
	u.sessions[sid] = core
	u.mu.Unlock()

	if u.cfg.proposeFirst() {
		desc := Description{Media: "audio"}
		payload, err := mustJMIErr(JMI{
			Action:       JMIPropose,
			ID:           sid,
			Descriptions: []Description{desc},
		})
		if err != nil {
			return nil, err
		}
		if err := u.transport.SendMessage(context.Background(), peerJID, "chat", sid, payload); err != nil {
			u.dropSession(sid)
			return nil, err
		}
		core.mu.Lock()
		core.state = stProposed
		core.mu.Unlock()
	} else {
		core.startOffer()
	}
	return &Outbound{core}, nil
}

func (u *UA) dropSession(sid string) {
	u.mu.Lock()
	delete(u.sessions, sid)
	u.mu.Unlock()
}

// HandleMessage feeds one incoming <message/> child payload (a JMI action) into the leg.
// handled=false means the payload was not a JMI action (or not ours).
func (u *UA) HandleMessage(ctx context.Context, from, msgType, id string, payload []byte) (handled bool) {
	u.trace(ctx, "in", from, payload)
	j, ok, err := ParseJMI(payload)
	if err != nil || !ok {
		return false
	}
	return u.HandleJMI(ctx, from, false, j)
}

// HandleJMI feeds one already-parsed JMI action into the leg (hosts with their own JMI parser
// use this). from is the stanza sender (bare or full); fromSelf marks actions carbon-copied from
// one of our own resources (XEP-0280), which drive the "answered/declined on another device"
// paths. handled=false means the action is not ours.
func (u *UA) HandleJMI(ctx context.Context, from string, fromSelf bool, j JMI) (handled bool) {
	switch j.Action {
	case JMIPropose, JMIAccept, JMIProceed, JMIReject, JMIRetract, JMIFinish:
	default:
		return false
	}
	fromOwn := fromSelf || bareOf(from) == bareOf(u.cfg.LocalBare)
	u.mu.Lock()
	core := u.sessions[j.ID]
	u.mu.Unlock()

	switch j.Action {
	case JMIPropose:
		if fromOwn {
			// Carbon of our own outbound propose.
			return core != nil
		}
		if core != nil {
			return true // duplicate propose
		}
		u.onPropose(j, from)
		return true
	case JMIAccept, JMIProceed, JMIReject, JMIRetract, JMIFinish:
		if core == nil {
			// Propose/accept pairing may race our own map; only "propose-less" actions we
			// care about are for sessions we track. Accept the stanza to avoid double notices.
			return true
		}
		core.onJMI(j, fromOwn)
		return true
	}
	return false
}

// onPropose creates the inbound call for a peer propose and hands it to OnInvite.
func (u *UA) onPropose(j JMI, from string) {
	now := time.Now()
	sid := j.ID
	var o leg.Offer
	var codecs []string
	if len(j.Descriptions) > 0 {
		d := j.Descriptions[0]
		for _, pt := range d.PayloadTypes {
			codecs = append(codecs, fmt.Sprintf("%s/%d", pt.Name, pt.ID))
		}
		o = leg.Offer{
			// A XEP-0353 propose IS the call offer (usually a bare <description media="audio"/>
			// with no payload types): codecs are negotiated later on session-initiate. So HasSDP
			// is true here and Opus defaults to true when nothing is listed.
			HasSDP:           true,
			Opus:             hasCodec(&d, "opus") || len(d.PayloadTypes) == 0,
			PCMU:             hasCodec(&d, "PCMU"),
			TelephoneEvent:   hasCodec(&d, "telephone-event"),
			TelephoneEventPT: uint8(tePT(&d)),
			Codecs:           codecs,
		}
	} else {
		o = leg.Offer{HasSDP: true, Opus: true}
	}
	core := &callCore{
		u:        u,
		sid:      sid,
		outbound: false,
		peerBare: bareOf(from),
		peerFull: from,
		info: leg.InviteInfo{
			CallID:     sid,
			FromUser:   e164FromJID(from),
			ToUser:     e164FromJID(u.cfg.LocalBare),
			RemoteAddr: from,
			ReceivedAt: now,
			Offer:      o,
		},
		events: make(chan leg.CallEvent, 4),
		log:    u.log.With().Str("call_id", sid).Logger(),
	}
	core.state = stProposed
	u.mu.Lock()
	u.sessions[sid] = core
	u.mu.Unlock()

	if u.cfg.OnInvite == nil {
		(&Inbound{core}).Reject(480, "Temporarily Unavailable")
		return
	}
	u.cfg.OnInvite(&Inbound{core})
}

// HandleIQ feeds one incoming <iq/> child payload (<jingle .../>) into the leg.
// handled=false means the payload was not a Jingle action (or not ours).
func (u *UA) HandleIQ(ctx context.Context, from, iqType, id string, payload []byte) (handled bool) {
	u.trace(ctx, "in", from, payload)
	if iqType == "error" {
		return u.handleIQError(ctx, from, id, payload)
	}
	if iqType != "set" {
		return false
	}
	j, err := ParseJingle(payload)
	if err != nil {
		return false
	}
	fromOwn := bareOf(from) == bareOf(u.cfg.LocalBare)
	u.mu.Lock()
	core := u.sessions[j.SID]
	u.mu.Unlock()

	switch j.Action {
	case ActionSessionInitiate:
		if fromOwn {
			return core != nil
		}
		if core == nil {
			// Direct session-initiate with no JMI propose: create the inbound call.
			u.onDirectInitiate(j, from)
			return true
		}
		core.onSessionInitiate(j, from)
		return true
	case ActionSessionAccept:
		if core == nil {
			return true
		}
		core.onSessionAccept(j)
		return true
	case ActionSessionTerminate:
		if core == nil {
			return true
		}
		core.onTerminate(j)
		u.dropSession(j.SID)
		return true
	case ActionSessionInfo:
		if core == nil {
			return true
		}
		core.onSessionInfo(j)
		return true
	}
	return false
}

// onDirectInitiate creates the inbound call for a session-initiate with no JMI propose.
func (u *UA) onDirectInitiate(j *Jingle, from string) {
	now := time.Now()
	core := &callCore{
		u:        u,
		sid:      j.SID,
		outbound: false,
		peerBare: bareOf(from),
		peerFull: from,
		offer:    j,
		info: leg.InviteInfo{
			CallID:     j.SID,
			FromUser:   e164FromJID(from),
			ToUser:     e164FromJID(u.cfg.LocalBare),
			RemoteAddr: from,
			ReceivedAt: now,
		},
		events: make(chan leg.CallEvent, 4),
		log:    u.log.With().Str("call_id", j.SID).Logger(),
	}
	if d := j.AudioDescription(); d != nil {
		var codecs []string
		for _, pt := range d.PayloadTypes {
			codecs = append(codecs, fmt.Sprintf("%s/%d", pt.Name, pt.ID))
		}
		core.info.Offer = leg.Offer{
			HasSDP:           true,
			Opus:             hasCodec(d, "opus"),
			PCMU:             hasCodec(d, "PCMU"),
			TelephoneEvent:   hasCodec(d, "telephone-event"),
			TelephoneEventPT: uint8(tePT(d)),
			Codecs:           codecs,
		}
	}
	core.state = stOffered
	u.mu.Lock()
	u.sessions[j.SID] = core
	u.mu.Unlock()

	if u.cfg.OnInvite == nil {
		(&Inbound{core}).Reject(480, "Temporarily Unavailable")
		return
	}
	u.cfg.OnInvite(&Inbound{core})
}

func mustJMIErr(j JMI) ([]byte, error) { return j.Marshal() }

// trace logs one raw Jingle/JMI payload at debug level (call signalling carries no secrets).
func (u *UA) trace(ctx context.Context, dir, from string, payload []byte) {
	u.log.Debug().Str("dir", dir).Str("from", from).Str("payload", string(payload)).Msg("Jingle stanza")
}

// handleIQError processes <iq type='error'/> answers to our session stanzas (the gateway
// rejecting a session-initiate is otherwise invisible: no terminate, no event, dead air).
func (u *UA) handleIQError(ctx context.Context, from, id string, payload []byte) bool {
	cond, text := parseStanzaError(payload)
	u.mu.Lock()
	core := u.sessions[id]
	u.mu.Unlock()
	reason := cond
	if text != "" {
		reason = cond + ": " + text
	}
	u.log.Warn().Str("from", from).Str("sid", id).Str("condition", cond).Str("text", text).
		Msg("Peer answered our Jingle stanza with an IQ error")
	if core == nil {
		return true // ours (matches a session id space) but the call is already gone
	}
	core.end(leg.EndFailed, 500, "gateway error "+reason, nil)
	return true
}

// parseStanzaError extracts the XMPP stanza error condition and text from an <error/> payload.
func parseStanzaError(payload []byte) (condition, text string) {
	var probe struct {
		XMLName xml.Name `xml:"error"`
		Text    string   `xml:"text"`
		Any     []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal(payload, &probe); err != nil {
		return "malformed-error", string(payload)
	}
	for _, e := range probe.Any {
		if e.XMLName.Space == "urn:ietf:params:xml:ns:xmpp-stanzas" {
			condition = e.XMLName.Local
		}
	}
	if condition == "" {
		condition = "unknown"
	}
	return condition, probe.Text
}

// PeerJIDFor is the default Cheogram mapping helper hosts can reuse: "+E.164@domain".
func PeerJIDFor(domain string) func(string) (string, error) {
	return func(e164 string) (string, error) {
		if !phone.IsE164(e164) {
			return "", fmt.Errorf("jingleleg: not an E.164 number")
		}
		return e164 + "@" + strings.TrimSpace(domain), nil
	}
}

// unused keeps the compiler honest about time in this file if configs change.
var _ = time.Now
