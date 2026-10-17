package media

import (
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

// Audio constants shared by every leg.
const (
	SampleRate    = 8000                  // PCM and PCMU sample rate
	FrameDuration = 20 * time.Millisecond // media clock tick, SIP ptime
	FrameSamples  = 160                   // samples per 20 ms frame at 8 kHz
	PayloadPCMU   = 0                     // RTP payload type of PCMU
	ULawSilence   = 0xFF                  // G.711 mu-law encoding of digital silence
)

// RTPPacket is one received RTP packet. Payload aliases the producer's buffer and is valid only during
// the call that delivered it (ReceiveRTP) or until the next ReadRTP.
type RTPPacket struct {
	SSRC        uint32
	Seq         uint16
	Timestamp   uint32
	PayloadType uint8
	Marker      bool
	Payload     []byte
}

// SIPLeg is the SIP side of a call (implemented by pkg/sipua).
type SIPLeg interface {
	// ReadRTP blocks for the next packet. io.EOF once the SIP media is closed.
	ReadRTP(p *RTPPacket) error
	// WriteRTP sends one PCMU packet now (no pacing); samples is the RTP timestamp increment.
	WriteRTP(payload []byte, samples uint32, marker bool) error
}

// PacketReceiver receives the owner's Opus packets. ReceiveRTP is called from a LiveKit track goroutine;
// it must not block and must copy what it keeps.
type PacketReceiver interface {
	ReceiveRTP(p *RTPPacket)
}

// RTCLeg is the LiveKit side of a call (implemented by pkg/rtc).
type RTCLeg interface {
	// WriteOpus publishes one Opus frame of duration d now. The caller may reuse frame after return.
	WriteOpus(frame []byte, d time.Duration) error
	// SetReceiver installs the receiver for the owner's audio (nil detaches).
	SetReceiver(r PacketReceiver)
}

// Config configures Start. Zero values get the defaults in brackets.
type Config struct {
	Clock       clock.Clock    // [clock.Real()]
	Log         zerolog.Logger // clean logger with call_id
	JitterDelay time.Duration  // initial playout delay per direction [60 ms]
	JitterMax   time.Duration  // max buffered audio per direction [400 ms]
	RTPTimeout  time.Duration  // no SIP RTP for this long ends the bridge with ErrRTPTimeout [60 s]
	// Early starts the bridge in early-media mode (D10): SIP audio is relayed to LiveKit with ringback
	// filling silence, nothing from the owner is sent to SIP (mu-law silence instead), the RTC receiver is
	// not attached and the RTP timeout is disabled. Connect leaves early mode.
	Early bool
	// EarlyFillAfter is how long without SIP RTP (or since Start) before ringback fills in [1 s].
	EarlyFillAfter time.Duration
}

// Stats is a snapshot of the bridge counters (monotonic since Start).
type Stats struct {
	SIPRx             uint64        // SIP RTP packets received (all payload types)
	SIPTx             uint64        // SIP RTP packets sent
	SIPLost           uint64        // SIP sequence gaps not filled before playout
	SIPLate           uint64        // SIP packets discarded (late, duplicate or overflow)
	SIPJitter         time.Duration // RFC 3550 interarrival jitter estimate, current value
	ConcealedSIPToRTC uint64        // frames sent to LiveKit as PLC/silence because SIP audio was missing
	ConcealedRTCToSIP uint64        // frames sent to SIP as PLC/silence because owner audio was missing
	RTCRx             uint64        // owner Opus packets received
	LastSIPRx         time.Time     // zero if none yet
	DTMFSent          uint64        // DTMF digits fully sent (RFC 4733)
}

// SIPEventLeg is implemented by SIP legs that negotiated telephone-event (RFC 4733).
type SIPEventLeg interface {
	// TelephoneEventPT returns the negotiated payload type; ok=false if not negotiated.
	TelephoneEventPT() (pt uint8, ok bool)
	// WriteEvent sends one telephone-event packet now with that PT. samples is the RTP timestamp increment
	// applied after this packet (0 for every packet of an event except the last).
	WriteEvent(payload []byte, samples uint32, marker bool) error
}

// Bridge moves audio between one SIPLeg and one RTCLeg.
type Bridge interface {
	Stats() Stats          // lock-free (atomics)
	Done() <-chan struct{} // closed when the bridge stops by itself (ErrRTPTimeout, SIP EOF) or after Stop
	Err() error            // after Done: ErrRTPTimeout, io.EOF (SIP closed) or nil (Stop)
	Stop()                 // idempotent; waits for the clocked goroutines; detaches the RTC receiver
	// Connect leaves early mode (no-op otherwise): attaches the RTC receiver (owner audio flows to SIP),
	// stops the ringback fill and starts the RTP timeout from now. Idempotent, non-blocking.
	Connect()
	// SendDTMF queues digits (0-9, *, #, A-D) for RFC 4733 transmission. Non-blocking. Errors:
	// ErrDTMFUnsupported (leg is not a SIPEventLeg or PT not negotiated), ErrDTMFNotConnected (early mode),
	// ErrDTMFBusy (more than 32 digits queued), ErrDTMFInvalid (other characters).
	SendDTMF(digits string) error
}

// ErrRTPTimeout is Bridge.Err when no RTP arrived from the SIP side for Config.RTPTimeout.
var ErrRTPTimeout = errors.New("media: no RTP received from the SIP side")

// DTMF errors returned by Bridge.SendDTMF.
var (
	ErrDTMFUnsupported  = errors.New("media: DTMF (telephone-event) not negotiated on this call")
	ErrDTMFNotConnected = errors.New("media: call not connected")
	ErrDTMFBusy         = errors.New("media: too many DTMF digits queued")
	ErrDTMFInvalid      = errors.New("media: invalid DTMF digit")
)

// TonePattern selects a generated call-progress tone (North American values, ITU-T E.180 supplement).
type TonePattern int

// Tone patterns.
const (
	ToneRingback TonePattern = iota + 1 // 440+480 Hz, 2 s on / 4 s off
	ToneBusy                            // 480+620 Hz, 0.5 s on / 0.5 s off
	ToneReorder                         // 480+620 Hz, 0.25 s on / 0.25 s off
)

// Tone plays a call-progress tone into an RTCLeg on its own 20 ms ticker.
type Tone interface {
	// Stop is idempotent and waits for the goroutine; the RTCLeg is free for another writer afterwards.
	Stop()
}

// StartTone starts playing p into rtc (Opus 8 kHz mono, same encoder settings as the bridge). Errors only
// on encoder creation. Only one writer may use an RTCLeg at a time: stop the Tone before Start'ing a
// bridge on the same leg.
func StartTone(cfg Config, rtc RTCLeg, p TonePattern) (Tone, error) { return startTone(cfg, rtc, p) }

// Start creates the codecs and starts the bridge. Errors only on codec creation.
func Start(cfg Config, sip SIPLeg, rtc RTCLeg) (Bridge, error) { return start(cfg, sip, rtc) }
