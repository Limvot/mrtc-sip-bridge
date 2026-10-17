package calls

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// maxDTMFDigits is the most digits one SendDTMF accepts.
const maxDTMFDigits = 32

// PlaceCall validates the request (number, allowed prefix, self-dial, outbound enabled, line free),
// reserves the login's call slot and starts a callback-ring call actor. It does no network I/O and
// returns the call id and the normalized number ("" for the RoomID form: the actor looks the number up,
// and the destination guards run there). ctx carries the cmd.call span, which call.outbound links to.
func (m *Manager) PlaceCall(ctx context.Context, login core.LoginRef, req PlaceCallRequest) (callID, e164 string, err error) {
	if m.cfg.Dial == nil {
		return "", "", ErrOutboundDisabled
	}
	if (req.Number == "") == (req.RoomID == "") {
		return "", "", ErrInvalidNumber
	}
	if req.Number != "" {
		e164, err = phone.Normalize(req.Number, m.cfg.DefaultCountryCode)
		if err != nil || !phone.IsE164(e164) {
			return "", "", ErrInvalidNumber
		}
		if !validNANP(e164) {
			return "", "", ErrInvalidNumber
		}
		if !m.prefixAllowed(e164) {
			return "", "", ErrNotAllowed
		}
		if m.isSelf(login, e164) {
			return "", "", ErrSelfDial
		}
	}
	now := m.cfg.Clock.Now()
	m.mu.Lock()
	switch {
	case m.stopped:
		m.mu.Unlock()
		return "", "", ErrOutboundDisabled
	case m.active[login] != nil:
		m.mu.Unlock()
		return "", "", ErrLineBusy
	}
	c := newOutCall(m, login, triggerCommand, trace.SpanContextFromContext(ctx), now)
	if req.RoomID != "" {
		c.cmdRoom = true
		c.roomID = req.RoomID
		m.routable[login] = c
	} else {
		c.cmdNumber = e164
	}
	m.active[login] = c
	m.calls[c] = struct{}{}
	m.mu.Unlock()
	c.logFor(ctx).Info().Str("to", e164).Stringer("room_id", req.RoomID).Msg("Placing an outgoing call by command")
	go c.run()
	return c.id, e164, nil
}

// Hangup ends the login's active call (any state). roomID "" = whatever room it is in; otherwise it must
// match. Non-blocking.
func (m *Manager) Hangup(ctx context.Context, login core.LoginRef, roomID id.RoomID) error {
	c := m.commandCall(login, roomID)
	if c == nil {
		return ErrNoActiveCall
	}
	select {
	case c.events <- event{kind: evHangupCmd}:
	default:
		c.logFor(ctx).Warn().Msg("Call event queue full, dropping the hangup command")
	}
	return nil
}

// SendDTMF queues digits on the login's active, connected call (roomID as for Hangup). It waits up to 2 s
// for the actor's answer. Spaces are ignored; 1..32 digits of 0-9 * # A-D (case-insensitive).
// Returns the digit count queued. The digits are never logged or traced.
func (m *Manager) SendDTMF(ctx context.Context, login core.LoginRef, roomID id.RoomID, digits string) (int, error) {
	d, ok := normalizeDigits(digits)
	if !ok {
		return 0, ErrInvalidDigits
	}
	c := m.commandCall(login, roomID)
	if c == nil {
		return 0, ErrNoActiveCall
	}
	reply := make(chan error, 1)
	select {
	case c.events <- event{kind: evDTMF, digits: d, reply: reply}:
	default:
		return 0, ErrNotConnected
	}
	t := m.cfg.Clock.NewTimer(dtmfReplyTimeout)
	defer t.Stop()
	select {
	case err := <-reply:
		if err != nil {
			return 0, err
		}
		return len(d), nil
	case <-t.C():
		return 0, ErrNotConnected
	case <-c.done:
		return 0, ErrNoActiveCall
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// commandCall returns the call a command targets: the login's active call, which must be in roomID when
// roomID is set (then only calls whose room is resolved match).
func (m *Manager) commandCall(login core.LoginRef, roomID id.RoomID) *call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if roomID == "" {
		return m.active[login]
	}
	// roomID is written before the call becomes routable (under m.mu), so reading it here is ordered.
	if c := m.routable[login]; c != nil && c.roomID == roomID {
		return c
	}
	return nil
}

// normalizeDigits strips spaces, upper-cases a-d and validates 1..32 of 0-9 * # A-D.
func normalizeDigits(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t':
			continue
		case r >= '0' && r <= '9', r == '*', r == '#', r >= 'A' && r <= 'D':
		case r >= 'a' && r <= 'd':
			r -= 'a' - 'A'
		default:
			return "", false
		}
		if b.Len() >= maxDTMFDigits {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}
