package jingleleg

import (
	"context"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// Transport sends stanzas for the leg. The host owns the XMPP session and implements this
// (the embedded jmpchat-matrix-bridge shim maps it onto its mellium session). Implementations
// must not block for network I/O longer than a normal stanza send; errors are logged, not retried.
type Transport interface {
	// SendIQ sends <iq type=to id=payload> to to. payload is the raw XML of the single child
	// element (a <jingle/> action or an empty <query/>-less result marker). iqType is "set",
	// "result" or "error".
	SendIQ(ctx context.Context, to, iqType, id string, payload []byte) error
	// SendMessage sends <message type=to id=payload> (JMI actions).
	SendMessage(ctx context.Context, to, msgType, id string, payload []byte) error
}

// Engine is the media engine of one call: a WebRTC endpoint on the Jingle side that presents
// PCMU 20 ms frames towards the pkg/media bridge (which transcodes PCMU to Opus for LiveKit).
// The production implementation is pion-backed (engine_webrtc.go); tests use a fake.
type Engine interface {
	media.SIPLeg
	media.SIPEventLeg

	// Accept processes the remote Jingle offer (responder role) and returns our answer to put
	// into session-accept: an RTP description and an ICE-UDP/DTLS transport.
	Accept(remote *Jingle) (desc *Description, transport *JingleTransport, err error)
	// Offer returns our Jingle offer to put into session-initiate (initiator role).
	Offer() (desc *Description, transport *JingleTransport, err error)
	// SetRemoteAnswer processes the remote session-accept (initiator role).
	SetRemoteAnswer(remote *Jingle) error
	// AddRemoteTransport applies trickled ICE candidates from <transport-info/>.
	AddRemoteTransport(tr *JingleTransport) error
	// SelectedPair returns the chosen ICE pair as "ip:port"/"ip:port" (logs only; "" if unknown).
	SelectedPair() (local, remote string)
	// Close tears the engine down; ReadRTP returns io.EOF afterwards. Idempotent.
	Close() error
}

// EngineFactory builds an engine for one call. nil means calls fail with ErrNoEngine.
type EngineFactory func() (Engine, error)
