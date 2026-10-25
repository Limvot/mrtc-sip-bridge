package jingleleg

// Per-call state machines: map XEP-0166 session actions and XEP-0353 JMI actions onto
// leg.CallEvents. Inbound and Outbound wrap one callCore and satisfy leg.InboundCall /
// leg.OutboundCall respectively (the two interfaces need different Info()/Hangup()).
//
// Flow (XEP-0353 §2 + XEP-0166), both directions:
//
//	inbound, JMI first:  peer propose ──> OnInvite ──(Answer)──> proceed to peer
//	                     peer session-initiate ──> engine.Accept ──(answered already: now)──>
//	                     session-accept ──> EventAnswered
//	inbound, direct:     peer session-initiate ──> OnInvite ──(Answer)──> session-accept ──>
//	                     EventAnswered
//	outbound:            propose to peer ──> peer proceed ──> session-initiate ──>
//	                     peer session-accept ──> EventAnswered
//	                     (Config.ProposeFirst=false skips straight to session-initiate)

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/leg"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// ErrNoEngine is the EventAnswerFailed error when Config.NewEngine is nil or fails.
var ErrNoEngine = errors.New("jingleleg: no media engine")

// callState is the per-call state.
type callState int

const (
	stProposed   callState = iota // JMI propose seen/sent, no Jingle session yet
	stProceeding                  // inbound: we sent proceed, waiting for session-initiate
	stOffered                     // session-initiate seen (inbound) / sent (outbound)
	stAccepted                    // answered, media up
	stEnded
)

// callCore is shared by Inbound and Outbound.
type callCore struct {
	u        *UA
	sid      string
	outbound bool

	mu       sync.Mutex
	state    callState
	peerBare string
	peerFull string

	// inbound
	info  leg.InviteInfo
	offer *Jingle

	// outbound
	dial     leg.DialRequest
	dialInfo leg.DialInfo

	// sentOffer is set once we sent session-initiate (outbound dial, or inbound callee-initiate).
	sentOffer bool
	// proceedSent is set once we sent JMI proceed (inbound answer / outbound proceed): a peer
	// session-initiate arriving afterwards is answered instead of starting a competing session.
	proceedSent bool

	engine Engine
	events chan leg.CallEvent
	ended  bool
	log    zerolog.Logger
}

// Inbound is one inbound Jingle call (leg.InboundCall).
type Inbound struct{ *callCore }

// Outbound is one outbound Jingle call (leg.OutboundCall).
type Outbound struct{ *callCore }

var (
	_ leg.InboundCall  = (*Inbound)(nil)
	_ leg.OutboundCall = (*Outbound)(nil)
)

// Info implements leg.InboundCall.
func (c *Inbound) Info() leg.InviteInfo { return c.info }

// Info implements leg.OutboundCall.
func (c *Outbound) Info() leg.DialInfo { return c.dialInfo }

// Events implements leg.InboundCall and leg.OutboundCall.
func (c *callCore) Events() <-chan leg.CallEvent { return c.events }

func (c *callCore) emit(ev leg.CallEvent) {
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return
	}
	if ev.Kind == leg.EventEnded {
		c.ended = true
	}
	prov := ev.Kind == leg.EventProvisional
	drop := prov && len(c.events) >= cap(c.events)-1
	c.mu.Unlock()
	if drop {
		return
	}
	select {
	case c.events <- ev:
	default:
		if prov {
			return
		}
		c.events <- ev // contract: only provisional events may be dropped
	}
	if ev.Kind == leg.EventEnded {
		close(c.events)
		if c.engine != nil {
			_ = c.engine.Close()
		}
	}
}

// end emits exactly one EventEnded and closes the dialog.
func (c *callCore) end(cause leg.EndCause, status int, reason string, err error) {
	c.mu.Lock()
	done := c.ended
	c.mu.Unlock()
	if done {
		return
	}
	c.emit(leg.CallEvent{Kind: leg.EventEnded, At: time.Now(), Cause: cause, StatusCode: status, Reason: reason, Err: err})
}

// --- inbound commands (leg.InboundCall) ---

// Ringing signals ringing towards the caller (session-info ringing), once a session exists.
func (c *Inbound) Ringing() {
	c.mu.Lock()
	offered := c.state == stOffered && c.offer != nil
	c.mu.Unlock()
	if !offered {
		return
	}
	c.sendSessionInfo(c.sid, "ringing")
}

// Answer accepts the call: JMI proceed plus a callee-initiated Jingle session when only a
// propose exists (XEP-0353 callers initiate after proceed in theory; Cheogram's gateway does
// not, so we initiate ourselves and answer a competing session-initiate if one arrives),
// session-accept when a session-initiate is already pending.
func (c *Inbound) Answer() {
	c.mu.Lock()
	switch c.state {
	case stEnded:
		c.mu.Unlock()
		return
	case stProposed:
		c.proceedSent = true
		sid, peer := c.sid, c.peerFull
		c.mu.Unlock()
		_ = c.u.transport.SendMessage(context.Background(), peer, "chat", sid, mustJMI(JMI{Action: JMIProceed, ID: sid}))
		c.initiateAfterGrace()
	case stOffered:
		initiated := c.sentOffer
		c.mu.Unlock()
		if !initiated {
			c.acceptNow()
		}
	default: // stProceeding/stAccepted: the session handler finishes it
		c.mu.Unlock()
	}
}

// Reject sends the final rejection and ends the call (EndRejected).
func (c *Inbound) Reject(code int, reason string) {
	c.mu.Lock()
	if c.state == stEnded {
		c.mu.Unlock()
		return
	}
	offered := c.offer != nil
	sid, peer := c.sid, c.peerFull
	c.mu.Unlock()
	if offered {
		c.sendTerminate(peer, sid, ReasonDeclined, reason)
	} else {
		r := &Reason{Condition: ReasonDeclined}
		if reason != "" {
			r.Text = reason
		}
		_ = c.u.transport.SendMessage(context.Background(), peer, "chat", sid,
			mustJMI(JMI{Action: JMIReject, ID: sid, Reason: r}))
	}
	c.end(leg.EndRejected, rejectStatus(code), reason, nil)
}

// Hangup ends an answered call; before answer it behaves like Reject(480, ...).
func (c *Inbound) Hangup() {
	c.mu.Lock()
	answered := c.state == stAccepted
	c.mu.Unlock()
	if !answered {
		c.Reject(480, "Temporarily Unavailable")
		return
	}
	c.sendTerminate(c.peerFull, c.sid, ReasonSuccess, "")
	_ = c.u.transport.SendMessage(context.Background(), c.peerFull, "chat", c.sid,
		mustJMI(JMI{Action: JMIFinish, ID: c.sid}))
	c.end(leg.EndLocalHangup, 200, "", nil)
}

// --- outbound commands (leg.OutboundCall) ---

// Hangup ends the call: retract+cancel before answer, terminate+finish after.
func (c *Outbound) Hangup() {
	c.mu.Lock()
	st, sid, peer := c.state, c.sid, c.peerFull
	c.mu.Unlock()
	switch st {
	case stEnded:
		return
	case stAccepted:
		c.sendTerminate(peer, sid, ReasonSuccess, "")
		_ = c.u.transport.SendMessage(context.Background(), peer, "chat", sid,
			mustJMI(JMI{Action: JMIFinish, ID: sid}))
		c.end(leg.EndLocalHangup, 200, "", nil)
	case stProposed:
		r := &Reason{Condition: ReasonCancel}
		_ = c.u.transport.SendMessage(context.Background(), peer, "chat", sid,
			mustJMI(JMI{Action: JMIRetract, ID: sid, Reason: r}))
		c.end(leg.EndLocalCancel, 487, "", nil)
	default: // stOffered/stProceeding
		c.sendTerminate(peer, sid, ReasonCancel, "")
		c.end(leg.EndLocalCancel, 487, "", nil)
	}
}

// --- state machine inputs (called from UA dispatch) ---

// onSessionInitiate handles an inbound <jingle action='session-initiate'>.
func (c *callCore) onSessionInitiate(j *Jingle, peerFull string) {
	c.mu.Lock()
	if c.state == stEnded {
		c.mu.Unlock()
		return
	}
	proceeding := c.state == stProceeding
	c.offer = j
	if peerFull != "" {
		c.peerFull = peerFull
	}
	if proceeding {
		c.state = stOffered // acceptNow transitions to stAccepted
	}
	wasProposed := c.state == stProposed
	if wasProposed {
		c.state = stOffered
	}
	d := j.AudioDescription()
	if d != nil {
		var codecs []string
		for _, pt := range d.PayloadTypes {
			codecs = append(codecs, fmt.Sprintf("%s/%d", pt.Name, pt.ID))
		}
		c.info.Offer = leg.Offer{
			HasSDP:           true,
			Opus:             hasCodec(d, "opus"),
			PCMU:             hasCodec(d, "PCMU"),
			TelephoneEvent:   hasCodec(d, "telephone-event"),
			TelephoneEventPT: uint8(tePT(d)),
			Codecs:           codecs,
		}
	}
	glare := c.sentOffer
	proceeded := c.proceedSent
	c.info.RemoteAddr = c.peerFull
	c.mu.Unlock()

	if proceeding || glare || proceeded {
		// Answer the peer's session-initiate: theirs is the authoritative offer (glare
		// resolution when both sides initiated; the plain case when we waited for it).
		c.acceptNow()
	}
}

// onSessionAccept handles the peer's <jingle action='session-accept'> (outbound answer).
func (c *callCore) onSessionAccept(j *Jingle) {
	c.mu.Lock()
	if c.state != stOffered || c.engine == nil {
		c.mu.Unlock()
		return
	}
	eng := c.engine
	c.mu.Unlock()
	if err := eng.SetRemoteAnswer(j); err != nil {
		c.end(leg.EndFailed, 488, "engine", err)
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.state = stAccepted
	c.mu.Unlock()
	te, teOK := eng.TelephoneEventPT()
	local, remote := eng.SelectedPair()
	c.emit(leg.CallEvent{
		Kind:  leg.EventAnswered,
		At:    now,
		Media: eng,
		Answer: leg.AnswerInfo{
			Codec:            c.engine.NegotiatedCodec(),
			PtimeMS:          20,
			TelephoneEvent:   teOK,
			TelephoneEventPT: te,
			LocalRTP:         local,
			RemoteRTP:        remote,
			OKReceivedAt:     now,
			AckSentAt:        now,
		},
	})
}

// onTerminate handles <jingle action='session-terminate'>.
func (c *callCore) onTerminate(j *Jingle) {
	cond := ReasonGeneralError
	if j.Reason != nil && j.Reason.Condition != "" {
		cond = j.Reason.Condition
	}
	text := ""
	if j.Reason != nil {
		text = j.Reason.Text
	}
	if text == "" {
		text = cond // keep the real XEP-0166 condition visible in logs and notices
	}
	c.mu.Lock()
	answered := c.state == stAccepted
	c.mu.Unlock()

	var cause leg.EndCause
	switch cond {
	case ReasonSuccess:
		if answered {
			cause = leg.EndRemoteBye
		} else if c.outbound {
			cause = leg.EndRemoteRejected
		} else {
			cause = leg.EndCancelled
		}
	case ReasonCancel, ReasonTimeout, ReasonExpired:
		cause = leg.EndCancelled
	case ReasonBusy, ReasonDeclined, ReasonGone:
		if answered {
			cause = leg.EndRemoteBye
		} else if c.outbound {
			cause = leg.EndRemoteRejected
		} else {
			cause = leg.EndCancelled
		}
	case ReasonAlternativeSession:
		cause = leg.EndRemoteRejected
	default:
		// general-error, failed-application/transport, unsupported-*, incompatible-parameters,
		// connectivity/media/security-error: the peer could not run the session.
		cause = leg.EndFailed
	}
	c.end(cause, terminateStatus(cond, answered), text, nil)
}

// onTransportInfo handles <jingle action='transport-info'>: trickled ICE candidates.
func (c *callCore) onTransportInfo(j *Jingle) {
	tr := j.AudioTransport()
	if tr == nil {
		return
	}
	c.mu.Lock()
	eng := c.engine
	c.mu.Unlock()
	if eng == nil {
		return
	}
	if err := eng.AddRemoteTransport(tr); err != nil {
		c.log.Warn().Err(err).Msg("jingleleg: failed to apply trickled transport")
	}
}

// onSessionInfo handles <jingle action='session-info'> (ringing, hold, ...).
func (c *callCore) onSessionInfo(j *Jingle) {
	if j.InfoName == "ringing" && c.outbound {
		c.emit(leg.CallEvent{Kind: leg.EventProvisional, At: time.Now(), StatusCode: 180})
	}
}

// onJMI handles one JMI action for this call's sid. Actions from our own other resources are
// ADVISORY (logged, never fatal): the bridge is the designated call endpoint for this account,
// and some other device/gateway path was auto-rejecting calls within ~50ms of the propose,
// sending callers to voicemail before the Matrix user could answer. If the call truly leaves
// for another device, the gateway sends session-terminate/gone and we follow that instead.
func (c *callCore) onJMI(j JMI, fromOwnBare bool) {
	switch j.Action {
	case JMIProceed, JMIAccept:
		if fromOwnBare {
			c.log.Info().Str("action", j.Action).Str("sid", j.ID).
				Msg("jingleleg: ignoring own-device JMI (bridge is the call endpoint)")
			return
		}
		if !c.outbound {
			return
		}
		c.mu.Lock()
		state := c.state
		c.mu.Unlock()
		if state != stProposed {
			return
		}
		c.mu.Lock()
		c.proceedSent = true
		c.mu.Unlock()
		c.initiateAfterGrace()
		c.emit(leg.CallEvent{Kind: leg.EventProvisional, At: time.Now(), StatusCode: 183})
	case JMIReject:
		if fromOwnBare {
			c.log.Info().Str("action", j.Action).Str("sid", j.ID).
				Msg("jingleleg: ignoring own-device JMI reject (bridge is the call endpoint)")
			return
		}
		if c.outbound {
			reason := ""
			if j.Reason != nil {
				reason = j.Reason.Text
			}
			c.end(leg.EndRemoteRejected, 603, reason, nil)
		}
	case JMIRetract:
		if !fromOwnBare {
			c.end(leg.EndCancelled, 487, "caller cancelled", nil)
		} else {
			c.log.Info().Str("sid", j.ID).Msg("jingleleg: ignoring own-device JMI retract")
		}
	case JMIFinish:
		if fromOwnBare {
			c.log.Info().Str("sid", j.ID).Msg("jingleleg: ignoring own-device JMI finish")
			return
		}
		c.mu.Lock()
		answered := c.state == stAccepted
		c.mu.Unlock()
		if answered {
			c.end(leg.EndRemoteBye, 200, "", nil)
		} else {
			c.end(leg.EndCancelled, 487, "caller cancelled", nil)
		}
	}
}

// initiateAfterGrace waits PeerInitiateGrace for the peer's session-initiate before sending our
// own: XEP-0353 has the caller initiate, but gateways differ on who does, and simultaneous
// initiation is resolved in onSessionInitiate by taking the peer's offer.
func (c *callCore) initiateAfterGrace() {
	time.AfterFunc(c.u.cfg.PeerInitiateGrace, func() {
		c.mu.Lock()
		st, offered, sent := c.state, c.offer != nil, c.sentOffer
		c.mu.Unlock()
		if st == stEnded || sent || offered {
			return
		}
		c.startOffer()
	})
}

// startOffer builds the engine offer and sends session-initiate (outbound, after proceed).
func (c *callCore) startOffer() {
	if c.engine == nil {
		eng, err := c.u.cfg.NewEngine()
		if err != nil {
			c.failWithTerminate("engine", err)
			return
		}
		c.engine = eng
	}
	desc, transport, err := c.engine.Offer()
	if err != nil {
		c.failWithTerminate("engine", err)
		return
	}
	offer := &Jingle{
		Action:    ActionSessionInitiate,
		SID:       c.sid,
		Initiator: c.u.cfg.LocalBare,
		Contents: []Content{{
			Creator:     "initiator",
			Name:        "audio", // XEP-0166 content names must match the peer's (Cheogram uses "audio")
			Senders:     "both",
			Description: desc,
			Transport:   transport,
		}},
	}
	payload, err := MarshalJingle(offer)
	if err != nil {
		c.failWithTerminate("marshal", err)
		return
	}
	c.mu.Lock()
	c.sentOffer = true
	if c.state == stProposed {
		c.state = stOffered
	}
	c.mu.Unlock()
	if err := c.u.transport.SendIQ(context.Background(), c.peerFull, "set", c.sid, payload); err != nil {
		c.failWithTerminate("send", err)
		return
	}
}

// acceptNow answers a pending session-initiate and emits EventAnswered.
func (c *callCore) acceptNow() {
	c.mu.Lock()
	if c.state == stEnded || c.state == stAccepted {
		c.mu.Unlock()
		return
	}
	offer := c.offer
	c.mu.Unlock()

	if c.engine == nil {
		eng, err := c.u.cfg.NewEngine()
		if err != nil {
			c.emit(leg.CallEvent{Kind: leg.EventAnswerFailed, At: time.Now(), Err: err})
			c.failWithTerminate("engine", err)
			return
		}
		c.engine = eng
	}
	desc, transport, err := c.engine.Accept(offer)
	if err != nil {
		c.emit(leg.CallEvent{Kind: leg.EventAnswerFailed, At: time.Now(), Err: err})
		c.failWithTerminate("engine", err)
		return
	}
	answer := &Jingle{
		Action:    ActionSessionAccept,
		SID:       c.sid,
		Responder: c.u.cfg.LocalBare,
		Contents: []Content{{
			Creator:     "initiator",
			Name:        contentName(offer),
			Senders:     "both",
			Description: desc,
			Transport:   transport,
		}},
	}
	payload, err := MarshalJingle(answer)
	if err != nil {
		c.failWithTerminate("marshal", err)
		return
	}
	if err := c.u.transport.SendIQ(context.Background(), c.peerFull, "set", c.sid, payload); err != nil {
		c.failWithTerminate("send", err)
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.state = stAccepted
	c.mu.Unlock()
	te, teOK := c.engine.TelephoneEventPT()
	local, remote := c.engine.SelectedPair()
	c.emit(leg.CallEvent{
		Kind:  leg.EventAnswered,
		At:    now,
		Media: c.engine,
		Answer: leg.AnswerInfo{
			Codec:            c.engine.NegotiatedCodec(),
			PtimeMS:          20,
			TelephoneEvent:   teOK,
			TelephoneEventPT: te,
			LocalRTP:         local,
			RemoteRTP:        remote,
			OKSentAt:         now,
			AckAt:            now,
		},
	})
}

// --- shared helpers ---

func (c *callCore) sendSessionInfo(sid, name string) {
	payload, err := MarshalJingle(&Jingle{Action: ActionSessionInfo, SID: sid, InfoName: name})
	if err != nil {
		return
	}
	_ = c.u.transport.SendIQ(context.Background(), c.peerFull, "set", sid, payload)
}

func (c *callCore) sendTerminate(peer, sid, condition, text string) {
	payload, err := MarshalJingle(&Jingle{
		Action: ActionSessionTerminate,
		SID:    sid,
		Reason: &Reason{Condition: condition, Text: text},
	})
	if err != nil {
		return
	}
	_ = c.u.transport.SendIQ(context.Background(), peer, "set", sid, payload)
}

func mustJMI(j JMI) []byte {
	b, err := j.Marshal()
	if err != nil {
		return nil
	}
	return b
}

func contentName(j *Jingle) string {
	if j != nil && len(j.Contents) > 0 && j.Contents[0].Name != "" {
		return j.Contents[0].Name
	}
	return "voice"
}

func hasCodec(d *Description, name string) bool {
	for _, pt := range d.PayloadTypes {
		if strings.EqualFold(pt.Name, name) {
			return true
		}
	}
	return false
}

func tePT(d *Description) int {
	for _, pt := range d.PayloadTypes {
		if strings.EqualFold(pt.Name, "telephone-event") {
			return int(pt.ID)
		}
	}
	return 0
}

// rejectStatus maps the Reject code onto a SIP-like status for CallEvent.StatusCode.
func rejectStatus(code int) int {
	if code < 300 || code > 699 {
		return 603
	}
	return code
}

// failWithTerminate ends the call with a local error, telling the peer with a
// session-terminate so its side stops ringing.
func (c *callCore) failWithTerminate(reason string, err error) {
	c.sendTerminate(c.peerFull, c.sid, ReasonGeneralError, reason)
	c.end(leg.EndFailed, 500, reason, err)
}

// terminateStatus maps a Jingle reason condition onto a SIP-like status.
func terminateStatus(condition string, answered bool) int {
	switch condition {
	case ReasonSuccess:
		return 200
	case ReasonBusy:
		return 486
	case ReasonCancel:
		return 487
	case ReasonTimeout:
		return 408
	case ReasonDeclined, ReasonGone:
		return 603
	default:
		if answered {
			return 200
		}
		return 500
	}
}

// newSID returns a random session id.
func newSID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sid%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// e164FromJID returns the E.164 number a Cheogram-style "+E.164@domain" JID stands for, or "".
func e164FromJID(jid string) string {
	user := bareOf(jid)
	if i := strings.IndexByte(user, '@'); i >= 0 {
		user = user[:i]
	}
	if !strings.HasPrefix(user, "+") {
		return ""
	}
	if i := strings.IndexByte(user, ';'); i >= 0 {
		// ";phone-context=..." suffixes are context-dependent short codes, not plain E.164.
		return ""
	}
	if !phone.IsE164(user) {
		return ""
	}
	return user
}

func bareOf(jid string) string {
	if i := strings.IndexByte(jid, '/'); i >= 0 {
		return jid[:i]
	}
	return jid
}
