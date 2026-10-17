package sipua

import (
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// InviteHandler receives a new inbound call. See Config.OnInvite.
type InviteHandler func(call InboundCall)

// InviteInfo holds facts about an INVITE. Nothing in it is secret; SDP and raw headers are not kept.
type InviteInfo struct {
	CallID       string    // SIP Call-ID
	FromUser     string    // From URI user part (usually the caller number)
	FromDisplay  string    // From display name, "" if none
	AssertedUser string    // P-Asserted-Identity URI user part, "" if the header is absent
	ToUser       string    // To URI user part (the dialled number or the SIP username)
	RemoteAddr   string    // signalling source "ip:port"
	ReceivedAt   time.Time // when the handler started
	TryingSentAt time.Time // when 100 Trying was sent (zero if sending failed)
	Offer        Offer
}

// Offer is what the INVITE's SDP offers, reduced to the facts the core needs.
type Offer struct {
	HasSDP         bool
	PCMU           bool // payload type 0 offered on the audio m-line
	TelephoneEvent bool // telephone-event/8000 offered
	// TelephoneEventPT is the payload type of telephone-event/8000 (valid if TelephoneEvent).
	TelephoneEventPT uint8
	MediaAddr        string   // "ip:port" from c= and m=audio, "" if unknown
	Codecs           []string // offered audio formats as "NAME/PT" (e.g. "PCMU/0", "G729/18"), for logs
}

// InboundCall is one inbound INVITE dialog. Every method is non-blocking and safe for concurrent use;
// commands are executed in order on the call's own goroutine. Commands after the call ended are ignored.
type InboundCall interface {
	Info() InviteInfo
	// Ringing sends 180 Ringing (ignored once a final response was sent).
	Ringing()
	// Answer sends 200 OK with a PCMU (+telephone-event) SDP answer and symmetric RTP, waits for ACK,
	// then emits EventAnswered (or EventAnswerFailed). Ignored after a final response.
	Answer()
	// Reject sends a final non-2xx response (code 300..699) and ends the call (EventEnded, EndRejected).
	// Ignored once answered or ended.
	Reject(code int, reason string)
	// Hangup sends BYE if answered (EventEnded, EndLocalHangup). Before answer it behaves like
	// Reject(480, "Temporarily Unavailable").
	Hangup()
	// Events delivers CallEvents. Exactly one EventEnded is delivered last, then the channel is closed.
	// At most one of EventAnswered/EventAnswerFailed precedes it. The buffer is large enough that the
	// call goroutine never blocks (capacity 4).
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
	EndCancelled      EndCause = iota + 1 // caller sent CANCEL before a final response (sipgo answered 487)
	EndRemoteBye                          // caller sent BYE after answer
	EndLocalHangup                        // we sent BYE (Hangup)
	EndRejected                           // we sent a final non-2xx (Reject, or Hangup before answer)
	EndFailed                             // transaction timeout, no ACK, transport error, safety timeout
	EndLocalCancel                        // outbound: Hangup before answer (CANCEL sent; or 200 raced it: ACK+BYE sent)
	EndRemoteRejected                     // outbound: remote final non-2xx (StatusCode, Reason)
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

// AnswerInfo describes the negotiated media after ACK.
type AnswerInfo struct {
	Codec          string // "PCMU"
	PtimeMS        int    // 20
	TelephoneEvent bool
	// TelephoneEventPT is the negotiated telephone-event payload type (valid if TelephoneEvent).
	TelephoneEventPT uint8
	LocalRTP         string // "ip:port" we bind (SDP advertises the external IP)
	RemoteRTP        string // "ip:port" from the offer (symmetric RTP may re-learn it)
	OKSentAt         time.Time
	AckAt            time.Time // inbound: ACK received
	OKReceivedAt     time.Time // outbound: 200 OK received
	AckSentAt        time.Time // outbound: ACK sent
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
