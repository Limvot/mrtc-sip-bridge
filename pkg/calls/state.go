package calls

import (
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

const (
	matrixOpTimeout       = 5 * time.Second  // each membership/ring/redact/notice/receipt request
	resolveRoomTimeout    = 15 * time.Second // ResolveCallRoom (may create the portal)
	joinTimeout           = 10 * time.Second // one rtc.Join attempt (cancelled by the actor's join timer)
	maxJoinAttempts       = 2
	answerTimeout         = 40 * time.Second // owner joined -> EventAnswered (covers rtc join + 32 s ACK wait)
	ownerRTCGrace         = 10 * time.Second // no owner participant in LiveKit after answer -> rtc_lost
	ringKeepalive         = 60 * time.Second // re-send 180 while ringing
	statsInterval         = 5 * time.Second
	membershipRefreshLead = 10 * time.Minute
	teardownStepTimeout   = 5 * time.Second
	staleMembershipSlack  = 5 * time.Second // owner membership older than ringSentAt-this is backlog
	byeRaceWindow         = time.Second     // media EOF -> wait this long for the SIP EventEnded (remote BYE)
	callEventBuffer       = 32

	// refreshRetryDelay is how long to wait before retrying a failed membership refresh.
	refreshRetryDelay = time.Minute
	// maxPastEvents bounds the mid-call events buffered for call.session.
	maxPastEvents = 32
	// degradedLossRatio and degradedSilence define call.media.degraded (TELEMETRY-SPEC §5.6).
	degradedLossRatio = 0.05
	degradedSilence   = 5 * time.Second
)

// Outbound call constants (PHASE3-SPEC §6.2).
const (
	triggerFreshness   = 30 * time.Second // owner notification/membership origin ts vs now
	triggerPairWindow  = 5 * time.Second  // notification before membership: wait this long
	freshJoinWindow    = 60 * time.Second // membership created within this of the notification
	triggerCooldown    = 5 * time.Second  // per room, after a call ends or a trigger is refused
	newRoomRingDelay   = 3 * time.Second  // D18
	failureToneLength  = 2 * time.Second  // D11
	dtmfReplyTimeout   = 2 * time.Second
	defaultDialTimeout = 60 * time.Second

	// ownerLiveRetention bounds how long an owner membership seen by the trigger detector is kept
	// (pruned on insert). The membership trigger mode keeps entries for the membership lifetime so a
	// second device joining a long call is still recognised as "not the first".
	ownerLiveRetention = 5 * time.Minute
)

// call.direction values.
const (
	dirInbound  = "inbound"
	dirOutbound = "outbound"
)

// call.trigger values.
const (
	triggerInvite          = "invite"
	triggerCommand         = "command"
	triggerOwnerMembership = "owner_membership"
)

// Owner-trigger detector modes (Config.OutboundTrigger).
const (
	modeRing       = "ring"
	modeMembership = "membership"
	modeOff        = "off"
)

// state is the call actor's state (PHASE2-SPEC §8.3, PHASE3-SPEC §6.2).
type state int

const (
	stSetup     state = iota // guards, room, membership, ring (inline, one step)
	stRinging                // ring sent (inbound, or the outbound callback ring); waiting for the owner
	stAnswering              // inbound only: owner joined; waiting for rtc ready and/or SIP ACK
	stJoining                // outbound: owner present, waiting for the LiveKit session before dialling
	stDialing                // outbound: INVITE sent, no 18x yet
	stEarly                  // outbound: 18x received (ringback tone or early-media bridge)
	stActive                 // answered; media bridged
	stEnding                 // teardown running (all inputs ignored)
	stEnded
)

// String returns the state name for logs.
func (s state) String() string {
	switch s {
	case stSetup:
		return "setup"
	case stRinging:
		return "ringing"
	case stAnswering:
		return "answering"
	case stJoining:
		return "joining"
	case stDialing:
		return "dialing"
	case stEarly:
		return "early"
	case stActive:
		return "active"
	case stEnding:
		return "ending"
	case stEnded:
		return "ended"
	default:
		return "unknown"
	}
}

// eventKind identifies an event on the call's own channel.
type eventKind int

const (
	evOwnerMembership eventKind = iota + 1 // Matrix: owner call.member (parsed in the actor)
	evDecline                              // Matrix: rtc.decline
	evShutdown                             // EndLogin / Stop (delivered by closing call.quit)
	evHangupCmd                            // Manager.Hangup
	evDTMF                                 // Manager.SendDTMF (digits, reply)
)

// event is one input on call.events.
type event struct {
	kind    eventKind
	rtc     matrixrtc.Event   // evOwnerMembership, evDecline
	from    trace.SpanContext // span context of the matrixrtc.ingest span (for links)
	session rtc.Session       // join result (on call.joinResults)
	err     error             // join result
	attempt int               // join result
	digits  string            // evDTMF: validated digits (never logged)
	reply   chan error        // evDTMF: capacity 1, always answered by the actor
}

// call.outcome values (TELEMETRY-SPEC §5.2, closed enum).
const (
	outcomeAnswered  = "answered"
	outcomeMissed    = "missed"
	outcomeDeclined  = "declined"
	outcomeCancelled = "cancelled"
	outcomeTimeout   = "timeout"
	outcomeBusy      = "busy"
	outcomeFailed    = "failed"
	outcomeRejected  = "rejected"
)

// call.end_reason values (TELEMETRY-SPEC §5.2).
const (
	reasonBye         = "bye"
	reasonCancel      = "cancel"
	reasonDecline     = "decline"
	reasonRingTimeout = "ring_timeout"
	reasonRTCLost     = "rtc_lost"
	reasonRTPTimeout  = "rtp_timeout"
	reasonRejected    = "rejected"
	reasonRemoteRej   = "remote_reject"
	reasonBridgeStop  = "bridge_stop"
	reasonError       = "error"
)

// call.hangup.by values (answered calls).
const (
	byRemote = "remote"
	byOwner  = "owner"
	byBridge = "bridge"
)

// failSpec describes how an unanswered call ends (PHASE2-SPEC §8.3 table, §8.6).
type failSpec struct {
	outcome string
	reason  string
	notice  string // "" = no notice
	// Outbound only (PHASE3-SPEC §6.6): failure tone (0 = none) and whether the owner's notification is
	// declined.
	tone    media.TonePattern
	decline bool
}
