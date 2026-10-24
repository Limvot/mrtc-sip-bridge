package sipua

// This file re-exports the shared phone-leg API (pkg/leg, ADR-014) under the historical sipua
// names. The SIP implementation below this package works with these aliases unchanged, and
// pkg/calls stays written against sipua.*. New call legs (pkg/jingleleg) implement leg.* directly.

import "github.com/williamkray/mrtc-sip-bridge/pkg/leg"

// Call setup API.
type (
	InviteHandler = leg.InviteHandler
	InviteInfo    = leg.InviteInfo
	Offer         = leg.Offer
	InboundCall   = leg.InboundCall
	CallEventKind = leg.CallEventKind
	EndCause      = leg.EndCause
	AnswerInfo    = leg.AnswerInfo
	CallEvent     = leg.CallEvent
	NumberFormat  = leg.NumberFormat
	DialRequest   = leg.DialRequest
	DialInfo      = leg.DialInfo
	OutboundCall  = leg.OutboundCall
	State         = leg.State
	StateEvent    = leg.StateEvent
	UA            = leg.UA
)

// CallEvent kinds.
const (
	EventAnswered       = leg.EventAnswered
	EventAnswerFailed   = leg.EventAnswerFailed
	EventEnded          = leg.EventEnded
	EventProvisional    = leg.EventProvisional
	EventAuthChallenged = leg.EventAuthChallenged
)

// End causes.
const (
	EndCancelled      = leg.EndCancelled
	EndRemoteBye      = leg.EndRemoteBye
	EndLocalHangup    = leg.EndLocalHangup
	EndRejected       = leg.EndRejected
	EndFailed         = leg.EndFailed
	EndLocalCancel    = leg.EndLocalCancel
	EndRemoteRejected = leg.EndRemoteRejected
)

// Number formats.
const (
	FormatDigits = leg.FormatDigits
	FormatE164   = leg.FormatE164
)

// UA states.
const (
	StateIdle           = leg.StateIdle
	StateRegistering    = leg.StateRegistering
	StateRegistered     = leg.StateRegistered
	StateAuthFailed     = leg.StateAuthFailed
	StateTransientError = leg.StateTransientError
	StateStopped        = leg.StateStopped
)

// ErrNotRegistered is returned by Dial when the account is not currently registered.
var ErrNotRegistered = leg.ErrNotRegistered
