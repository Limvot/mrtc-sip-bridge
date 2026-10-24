// Package leg holds the phone-leg call API shared by every call-leg implementation
// (pkg/sipua for SIP, pkg/jingleleg for XMPP Jingle). pkg/calls is written against these
// types and does not care which leg implements them. See docs/DECISIONS.md ADR-014.
package leg

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// InviteHandler receives a new inbound call. See the leg configs' OnInvite.
type InviteHandler func(call InboundCall)

// InviteInfo holds facts about an inbound call. Nothing in it is secret; SDP and raw stanza
// bodies are not kept.
type InviteInfo struct {
	CallID       string    // SIP Call-ID or Jingle sid
	FromUser     string    // caller identity (SIP: From URI user part; Jingle: peer JID user part, usually "+E.164")
	FromDisplay  string    // From display name, "" if none
	AssertedUser string    // P-Asserted-Identity URI user part, "" if absent
	ToUser       string    // dialled number or our own user part
	RemoteAddr   string    // signalling source "ip:port" (SIP) or peer full JID (Jingle)
	ReceivedAt   time.Time // when the handler started
	TryingSentAt time.Time // SIP: when 100 Trying was sent (zero if sending failed)
	Offer        Offer
}

// Offer is what the remote side offers, reduced to the facts the core needs.
type Offer struct {
	HasSDP         bool
	PCMU           bool // PCMU (G.711 mu-law) offered on the audio m-line
	Opus           bool // Opus offered on the audio m-line (Jingle/WebRTC legs)
	TelephoneEvent bool // telephone-event/8000 offered
	// TelephoneEventPT is the payload type of telephone-event/8000 (valid if TelephoneEvent).
	TelephoneEventPT uint8
	MediaAddr        string   // "ip:port" from c= and m=audio, "" if unknown
	Codecs           []string // offered audio formats as "NAME/PT" (e.g. "PCMU/0", "opus/111"), for logs
}

// InboundCall is one inbound call dialog. Every method is non-blocking and safe for concurrent
// use; commands are executed in order on the call's own goroutine. Commands after the call ended
// are ignored.
type InboundCall interface {
	Info() InviteInfo
	// Ringing signals ringing towards the caller (SIP: 180; Jingle: session-info ringing).
	// Ignored once a final response was sent.
	Ringing()
	// Answer accepts the call and, once media is up, emits EventAnswered (or EventAnswerFailed).
	// Ignored after a final response.
	Answer()
	// Reject sends a final rejection (SIP: status code 300..699; Jingle: JMI reject or
	// session-terminate decline) and ends the call (EventEnded, EndRejected). Ignored once answered
	// or ended.
	Reject(code int, reason string)
	// Hangup ends an answered call (EventEnded, EndLocalHangup). Before answer it behaves like
	// Reject(480, "Temporarily Unavailable").
	Hangup()
	// Events delivers CallEvents. Exactly one EventEnded is delivered last, then the channel is
	// closed. At most one of EventAnswered/EventAnswerFailed precedes it. The buffer is large
	// enough that the call goroutine never blocks (capacity 4).
	Events() <-chan CallEvent
}

// CallEventKind identifies a CallEvent.
type CallEventKind int

// CallEvent kinds.
const (
	EventAnswered CallEventKind = iota + 1
	EventAnswerFailed
	EventEnded
	EventProvisional    // outbound only: 1xx received (StatusCode; EarlyMedia+Media on the first 183 with SDP)
	EventAuthChallenged // outbound only: the INVITE was challenged (401/407) and re-sent with credentials
)

// EndCause says why a call ended.
type EndCause int

// End causes.
const (
	EndCancelled      EndCause = iota + 1 // caller cancelled before a final response (SIP CANCEL; Jingle retract/terminate cancel)
	EndRemoteBye                          // remote side ended after answer (SIP BYE; Jingle session-terminate success)
	EndLocalHangup                        // we hung up (SIP BYE sent; Jingle session-terminate sent)
	EndRejected                           // we sent a final rejection (Reject, or Hangup before answer)
	EndFailed                             // transaction timeout, transport error, safety timeout
	EndLocalCancel                        // outbound: Hangup before answer
	EndRemoteRejected                     // outbound: remote rejected (SIP final non-2xx; Jingle JMI reject/terminate decline)
)

// String returns "cancelled", "remote_bye", "local_hangup", "rejected", "failed", "local_cancel" or
// "remote_rejected" ("unknown" for any other value).
func (c EndCause) String() string {
	switch c {
	case EndCancelled:
		return "cancelled"
	case EndRemoteBye:
		return "remote_bye"
	case EndLocalHangup:
		return "local_hangup"
	case EndRejected:
		return "rejected"
	case EndFailed:
		return "failed"
	case EndLocalCancel:
		return "local_cancel"
	case EndRemoteRejected:
		return "remote_rejected"
	default:
		return "unknown"
	}
}

// AnswerInfo describes the negotiated media after answer.
type AnswerInfo struct {
	Codec          string // "PCMU" (SIP legs) or "Opus" (Jingle legs)
	PtimeMS        int    // 20
	TelephoneEvent bool
	// TelephoneEventPT is the negotiated telephone-event payload type (valid if TelephoneEvent).
	TelephoneEventPT uint8
	LocalRTP         string // "ip:port" we bind (SIP) or "" (Jingle: ICE picks the pair)
	RemoteRTP        string // "ip:port" from the offer (SIP) or the selected ICE pair (Jingle)
	OKSentAt         time.Time
	AckAt            time.Time // inbound: ACK received (SIP) / session-accept sent (Jingle)
	OKReceivedAt     time.Time // outbound: 200 OK received (SIP) / session-accept received (Jingle)
	AckSentAt        time.Time // outbound: ACK sent (SIP)
}

// CallEvent is delivered on InboundCall.Events and OutboundCall.Events.
type CallEvent struct {
	Kind   CallEventKind
	At     time.Time
	Media  media.SIPLeg // EventAnswered and early EventProvisional; valid until EventEnded
	Answer AnswerInfo   // EventAnswered only
	Cause  EndCause     // EventEnded only
	// StatusCode: inbound EventEnded = final status WE sent (200 if answered, 0 if none, 487 for CANCEL);
	// outbound EventProvisional = the 1xx; outbound EventEnded = final status RECEIVED (200 for answered or
	// raced calls, 487 for a completed CANCEL, 0 if none).
	StatusCode int
	Err        error // EventAnswerFailed / EndFailed: underlying error (scrubbed before logging)
	// EarlyMedia is set on EventProvisional when Media is the early-media leg (first 183 with SDP only).
	EarlyMedia bool
	// Reason is the remote reason phrase of EventEnded/EndRemoteRejected (logs and spans only, scrubbed).
	Reason string
}

// ErrNotRegistered is returned by UA.Dial when the leg is not currently usable
// (SIP: not registered with the registrar; Jingle: no usable XMPP session).
var ErrNotRegistered = errors.New("leg: not registered")

// NumberFormat selects the Request-URI user part for a dialled E.164 number (SIP legs).
type NumberFormat int

// Number formats.
const (
	FormatDigits NumberFormat = iota // "15551234567" (E.164 without "+"; JMP/Bandwidth default)
	FormatE164                       // "+15551234567"
)

// DialRequest describes one outbound call.
type DialRequest struct {
	Target   string        // E.164, validated by the caller (phone.IsE164 is re-checked)
	Format   NumberFormat  // Request-URI user format (SIP legs)
	CallerID string        // From user part; "" = the account's own user (required by JMP/Bandwidth SIP)
	Timeout  time.Duration // safety bound until a final response; 0 = 5 min (the core cancels earlier)
}

// DialInfo holds facts about an outbound call. Nothing in it is secret.
type DialInfo struct {
	CallID      string    // SIP Call-ID or Jingle sid
	Target      string    // E.164
	RequestUser string    // Request-URI user part (SIP) or peer JID (Jingle) actually sent
	StartedAt   time.Time // Dial called
}

// OutboundCall is one outbound call dialog. Every method is non-blocking and safe for concurrent use.
type OutboundCall interface {
	// Info returns the call's facts.
	Info() DialInfo
	// Hangup ends the call: before a final response it cancels the call attempt; after answer it
	// hangs up. Idempotent; ignored after EventEnded.
	Hangup()
	// Events delivers, in order: zero or more EventAuthChallenged/EventProvisional, at most one
	// EventAnswered, then exactly one EventEnded, then the channel is closed. Capacity 16; provisional
	// events are dropped (never Answered/Ended) when fewer than 2 slots are free.
	Events() <-chan CallEvent
}

// State is the readiness state of a UA.
type State int

const (
	// StateIdle is the state before Start.
	StateIdle State = iota
	// StateRegistering is the state while the leg is coming up (SIP: first REGISTER in flight).
	StateRegistering
	// StateRegistered means the leg is usable (SIP: registrar accepted our REGISTER).
	StateRegistered
	// StateAuthFailed means the far side rejected the credentials.
	StateAuthFailed
	// StateTransientError means a network or server error; retrying with backoff.
	StateTransientError
	// StateStopped is the final state after Stop.
	StateStopped
)

// String returns the lowercase state name.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRegistering:
		return "registering"
	case StateRegistered:
		return "registered"
	case StateAuthFailed:
		return "auth_failed"
	case StateTransientError:
		return "transient_error"
	case StateStopped:
		return "stopped"
	}
	return "state(" + strconv.Itoa(int(s)) + ")"
}

// StateEvent describes the UA state and the details of the latest attempt.
type StateEvent struct {
	State         State
	StatusCode    int           // SIP status of the failing final response; 0 for timeouts/network/DNS errors
	Reason        string        // reason phrase or short error text; never contains credentials
	Err           error         // underlying error for logs; may be nil
	RegistrarAddr string        // SIP: pinned "ip:port" used for the attempt ("" if DNS failed)
	Contact       string        // SIP: our Contact "host:port" after success (NAT-rewritten by rport/received)
	NextRetry     time.Duration // for AuthFailed/TransientError: delay before the next attempt
}

// UA is one call leg endpoint (one SIP account or one XMPP session's call capability).
type UA interface {
	// Start brings the leg up synchronously (bind errors are returned) and keeps it up until Stop.
	// Calling Start twice, or after Stop, returns an error.
	Start(ctx context.Context) error
	// Stop is idempotent. It releases every resource the leg holds and emits StateStopped.
	Stop(ctx context.Context) error
	// State returns the latest StateEvent (StateIdle before Start).
	State() StateEvent
	// Dial starts an outbound call and returns immediately (the call runs on its own goroutine).
	// Errors: ErrNotRegistered; invalid Target; UA not started or stopped.
	Dial(req DialRequest) (OutboundCall, error)
}
