package jingleleg

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/leg"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// --- fakes ---

type sentStanza struct {
	kind    string // "iq" or "message"
	to      string
	typ     string
	id      string
	payload string
}

type fakeTransport struct {
	mu   sync.Mutex
	sent []sentStanza
}

func (f *fakeTransport) SendIQ(_ context.Context, to, iqType, id string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentStanza{kind: "iq", to: to, typ: iqType, id: id, payload: string(payload)})
	return nil
}

func (f *fakeTransport) SendMessage(_ context.Context, to, msgType, id string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentStanza{kind: "message", to: to, typ: msgType, id: id, payload: string(payload)})
	return nil
}

func (f *fakeTransport) last(substr string) (sentStanza, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if substr == "" || strings.Contains(f.sent[i].payload, substr) {
			return f.sent[i], true
		}
	}
	return sentStanza{}, false
}

func (f *fakeTransport) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if strings.Contains(s.payload, substr) {
			n++
		}
	}
	return n
}

type fakeEngine struct {
	mu       sync.Mutex
	closed   bool
	readCh   chan struct{}
	accepted bool
	offered  bool
	answer   bool
}

func newFakeEngine() *fakeEngine { return &fakeEngine{readCh: make(chan struct{}, 8)} }

func (e *fakeEngine) Accept(remote *Jingle) (*Description, *JingleTransport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.accepted = true
	return answerDescription(), answerTransport(), nil
}

func (e *fakeEngine) Offer() (*Description, *JingleTransport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.offered = true
	return offerDescription(), answerTransport(), nil
}

func (e *fakeEngine) SetRemoteAnswer(remote *Jingle) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.answer = true
	return nil
}

func (e *fakeEngine) SelectedPair() (string, string) { return "10.0.0.1:10000", "203.0.113.1:45678" }

func (e *fakeEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.readCh)
	}
	return nil
}

func (e *fakeEngine) ReadRTP(p *media.RTPPacket) error {
	<-e.readCh
	return io.EOF
}

func (e *fakeEngine) WriteRTP(payload []byte, samples uint32, marker bool) error { return nil }
func (e *fakeEngine) TelephoneEventPT() (uint8, bool)                            { return 101, true }
func (e *fakeEngine) WriteEvent(payload []byte, samples uint32, marker bool) error {
	return nil
}

func offerDescription() *Description {
	return &Description{
		Media: "audio",
		PayloadTypes: []PayloadType{
			{ID: 111, Name: "opus", ClockRate: 48000, Channels: 2},
			{ID: 101, Name: "telephone-event", ClockRate: 8000},
		},
	}
}

func answerDescription() *Description {
	return &Description{
		Media:        "audio",
		PayloadTypes: []PayloadType{{ID: 111, Name: "opus", ClockRate: 48000, Channels: 2}},
	}
}

func answerTransport() *JingleTransport {
	return &JingleTransport{
		Ufrag: "u1", Pwd: "p1",
		Fingerprints: []Fingerprint{{Hash: "sha-256", Setup: "active", Value: "AA:BB"}},
	}
}

// remoteOffer is a session-initiate payload a peer would send.
func remoteOffer(sid string) []byte {
	raw, _ := MarshalJingle(&Jingle{
		Action:    ActionSessionInitiate,
		SID:       sid,
		Initiator: "+15550001111@cheogram.com",
		Contents: []Content{{
			Creator: "initiator", Name: "voice", Senders: "both",
			Description: offerDescription(),
			Transport:   answerTransport(),
		}},
	})
	return raw
}

func remoteAnswer(sid string) []byte {
	raw, _ := MarshalJingle(&Jingle{
		Action:    ActionSessionAccept,
		SID:       sid,
		Responder: "+15550001111@cheogram.com",
		Contents: []Content{{
			Creator: "initiator", Name: "voice", Senders: "both",
			Description: answerDescription(),
			Transport:   answerTransport(),
		}},
	})
	return raw
}

// --- harness ---

type harness struct {
	ua       *UA
	tp       *fakeTransport
	engines  []*fakeEngine
	invites  []leg.InboundCall
	inviteCh chan leg.InboundCall
}

func newHarness(t *testing.T, proposeFirst bool) *harness {
	t.Helper()
	tp := &fakeTransport{}
	h := &harness{tp: tp, inviteCh: make(chan leg.InboundCall, 8)}
	pf := proposeFirst
	ua, err := New(Config{
		LocalBare:    "me@example.com/bridge",
		Transport:    tp,
		PeerDomain:   "cheogram.com",
		ProposeFirst: &pf,
		NewEngine: func() (Engine, error) {
			e := newFakeEngine()
			h.engines = append(h.engines, e)
			return e, nil
		},
		OnInvite: func(c leg.InboundCall) { h.inviteCh <- c },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ua.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.ua = ua
	return h
}

func (h *harness) waitInvite(t *testing.T) leg.InboundCall {
	t.Helper()
	select {
	case c := <-h.inviteCh:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no invite")
		return nil
	}
}

func waitEvent(t *testing.T, ch <-chan leg.CallEvent, kind leg.CallEventKind) leg.CallEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("events closed before kind %d", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for event kind %d", kind)
		}
	}
}

// --- inbound ---

func TestInboundJMIProposeAnswerHangup(t *testing.T) {
	h := newHarness(t, true)
	propose, _ := JMI{Action: JMIPropose, ID: "sid1", Descriptions: []Description{{Media: "audio"}}}.Marshal()
	if !h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", propose) {
		t.Fatal("propose not handled")
	}
	call := h.waitInvite(t)
	if call.Info().FromUser != "+15550001111" {
		t.Fatalf("FromUser = %q", call.Info().FromUser)
	}

	call.Answer()
	if st, ok := h.tp.last("proceed"); !ok || st.kind != "message" {
		t.Fatalf("proceed not sent: %+v", h.tp.sent)
	}

	// Peer now starts the Jingle session; we answered already, so accept comes immediately.
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq1", remoteOffer("sid1"))
	if st, ok := h.tp.last("session-accept"); !ok || st.kind != "iq" {
		t.Fatalf("session-accept not sent: %+v", h.tp.sent)
	}
	ev := waitEvent(t, call.Events(), leg.EventAnswered)
	if ev.Answer.Codec != "Opus" || !ev.Answer.TelephoneEvent || ev.Media == nil {
		t.Fatalf("answer mismatch: %+v", ev.Answer)
	}

	call.Hangup()
	if h.tp.count("session-terminate") != 1 {
		t.Fatalf("session-terminate not sent")
	}
	if h.tp.count("finish") != 1 {
		t.Fatalf("finish not sent")
	}
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndLocalHangup {
		t.Fatalf("cause = %v", end.Cause)
	}
}

func TestInboundDirectInitiateAnswer(t *testing.T) {
	h := newHarness(t, true)
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq1", remoteOffer("sid1"))
	call := h.waitInvite(t)
	if call.Info().Offer.Opus == false {
		t.Fatal("opus offer not detected")
	}
	call.Answer()
	if _, ok := h.tp.last("session-accept"); !ok {
		t.Fatalf("session-accept not sent: %+v", h.tp.sent)
	}
	waitEvent(t, call.Events(), leg.EventAnswered)
}

func TestInboundRejectBeforeSession(t *testing.T) {
	h := newHarness(t, true)
	propose, _ := JMI{Action: JMIPropose, ID: "sid1", Descriptions: []Description{{Media: "audio"}}}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", propose)
	call := h.waitInvite(t)
	call.Reject(603, "Declined")
	if st, ok := h.tp.last("reject"); !ok || st.kind != "message" {
		t.Fatalf("reject not sent: %+v", h.tp.sent)
	}
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndRejected || end.StatusCode != 603 {
		t.Fatalf("end mismatch: %+v", end)
	}
}

func TestInboundCallerCancels(t *testing.T) {
	h := newHarness(t, true)
	propose, _ := JMI{Action: JMIPropose, ID: "sid1", Descriptions: []Description{{Media: "audio"}}}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", propose)
	call := h.waitInvite(t)
	retract, _ := JMI{Action: JMIRetract, ID: "sid1", Reason: &Reason{Condition: ReasonCancel}}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m2", retract)
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndCancelled {
		t.Fatalf("cause = %v", end.Cause)
	}
}

func TestInboundAnsweredElsewhere(t *testing.T) {
	h := newHarness(t, true)
	propose, _ := JMI{Action: JMIPropose, ID: "sid1", Descriptions: []Description{{Media: "audio"}}}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", propose)
	call := h.waitInvite(t)
	// A carbon of our own other resource proceeding with the same sid.
	proceed, _ := JMI{Action: JMIProceed, ID: "sid1"}.Marshal()
	h.ua.HandleMessage(context.Background(), "me@example.com/phone", "chat", "m2", proceed)
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndCancelled || end.Reason != "answered on another device" {
		t.Fatalf("end mismatch: %+v", end)
	}
}

func TestInboundPeerHangupAfterAnswer(t *testing.T) {
	h := newHarness(t, true)
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq1", remoteOffer("sid1"))
	call := h.waitInvite(t)
	call.Answer()
	waitEvent(t, call.Events(), leg.EventAnswered)
	term, _ := MarshalJingle(&Jingle{Action: ActionSessionTerminate, SID: "sid1", Reason: &Reason{Condition: ReasonSuccess}})
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq2", term)
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndRemoteBye {
		t.Fatalf("cause = %v", end.Cause)
	}
}

// --- outbound ---

func TestOutboundProposeFirstHappyPath(t *testing.T) {
	h := newHarness(t, true)
	call, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Info().Target != "+15550001111" {
		t.Fatalf("target = %q", call.Info().Target)
	}
	st, ok := h.tp.last("propose")
	if !ok || st.to != "+15550001111@cheogram.com" {
		t.Fatalf("propose not sent to peer: %+v", h.tp.sent)
	}

	proceed, _ := JMI{Action: JMIProceed, ID: call.Info().CallID}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", proceed)
	if h.tp.count("session-initiate") != 1 {
		t.Fatalf("session-initiate not sent: %+v", h.tp.sent)
	}
	waitEvent(t, call.Events(), leg.EventProvisional)

	// Peer answers the session.
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq1", remoteAnswer(call.Info().CallID))
	ev := waitEvent(t, call.Events(), leg.EventAnswered)
	if ev.Media == nil {
		t.Fatal("no media leg on answer")
	}

	call.Hangup()
	if h.tp.count("session-terminate") != 1 || h.tp.count("finish") != 1 {
		t.Fatalf("hangup stanzas missing: %+v", h.tp.sent)
	}
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndLocalHangup {
		t.Fatalf("cause = %v", end.Cause)
	}
}

func TestOutboundRejected(t *testing.T) {
	h := newHarness(t, true)
	call, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"})
	if err != nil {
		t.Fatal(err)
	}
	reject, _ := JMI{Action: JMIReject, ID: call.Info().CallID, Reason: &Reason{Condition: ReasonDecline}}.Marshal()
	h.ua.HandleMessage(context.Background(), "+15550001111@cheogram.com", "chat", "m1", reject)
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndRemoteRejected {
		t.Fatalf("cause = %v", end.Cause)
	}
}

func TestOutboundCancelBeforeAnswer(t *testing.T) {
	h := newHarness(t, true)
	call, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"})
	if err != nil {
		t.Fatal(err)
	}
	call.Hangup()
	if h.tp.count("retract") != 1 {
		t.Fatalf("retract not sent: %+v", h.tp.sent)
	}
	end := waitEvent(t, call.Events(), leg.EventEnded)
	if end.Cause != leg.EndLocalCancel {
		t.Fatalf("cause = %v", end.Cause)
	}
}

func TestOutboundDirectInitiate(t *testing.T) {
	h := newHarness(t, false)
	call, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"})
	if err != nil {
		t.Fatal(err)
	}
	if h.tp.count("session-initiate") != 1 {
		t.Fatalf("session-initiate not sent immediately: %+v", h.tp.sent)
	}
	if h.tp.count("propose") != 0 {
		t.Fatal("propose must not be sent in direct mode")
	}
	h.ua.HandleIQ(context.Background(), "+15550001111@cheogram.com", "set", "iq1", remoteAnswer(call.Info().CallID))
	waitEvent(t, call.Events(), leg.EventAnswered)
	_ = call
}

func TestOutboundDialValidation(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.ua.Dial(leg.DialRequest{Target: "not-a-number"}); err == nil {
		t.Fatal("expected error for invalid target")
	}
	if _, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"}); err != nil {
		t.Fatal(err)
	}
	if err := h.ua.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ua.Dial(leg.DialRequest{Target: "+15550001111"}); !errors.Is(err, leg.ErrNotRegistered) {
		t.Fatalf("expected ErrNotRegistered after stop, got %v", err)
	}
}

func TestHandleIQIgnoresForeignNamespaces(t *testing.T) {
	h := newHarness(t, true)
	if h.ua.HandleIQ(context.Background(), "x@y", "set", "i", []byte(`<query xmlns="jabber:iq:version"/>`)) {
		t.Fatal("foreign iq claimed")
	}
	if h.ua.HandleMessage(context.Background(), "x@y", "chat", "m", []byte(`<body>hi</body>`)) {
		t.Fatal("foreign message claimed")
	}
}
