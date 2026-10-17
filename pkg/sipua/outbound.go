package sipua

import (
	"errors"
	"time"
)

// ErrNotRegistered is returned by Dial when the account is not currently registered (no pinned
// registrar destination or State != StateRegistered).
var ErrNotRegistered = errors.New("sipua: not registered")

// NumberFormat selects the Request-URI user part for a dialled E.164 number.
type NumberFormat int

// Number formats.
const (
	FormatDigits NumberFormat = iota // "15551234567" (E.164 without "+"; JMP/Bandwidth default)
	FormatE164                       // "+15551234567"
)

// DialRequest describes one outbound call.
type DialRequest struct {
	Target   string        // E.164, validated by the caller (phone.IsE164 is re-checked)
	Format   NumberFormat  // Request-URI user format
	CallerID string        // From user part; "" = Config.Username (required by JMP/Bandwidth)
	Timeout  time.Duration // safety bound until a final response; 0 = 5 min (the core cancels earlier)
}

// DialInfo holds facts about an outbound call. Nothing in it is secret.
type DialInfo struct {
	CallID      string    // SIP Call-ID
	Target      string    // E.164
	RequestUser string    // Request-URI user part actually sent
	StartedAt   time.Time // Dial called
}

// OutboundCall is one outbound INVITE dialog. Every method is non-blocking and safe for concurrent use.
type OutboundCall interface {
	// Info returns the call's facts.
	Info() DialInfo
	// Hangup ends the call: before a final response it cancels the INVITE (CANCEL is sent once a
	// provisional response has arrived, per RFC 3261 section 9.1; a 200 that races the CANCEL is ACKed and
	// immediately BYEd); after answer it sends BYE. Idempotent; ignored after EventEnded.
	Hangup()
	// Events delivers, in order: zero or more EventAuthChallenged/EventProvisional, at most one
	// EventAnswered, then exactly one EventEnded, then the channel is closed. Capacity 16; provisional
	// events are dropped (never Answered/Ended) when fewer than 2 slots are free.
	Events() <-chan CallEvent
}
