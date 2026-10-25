package jingleleg

// The production Engine: a pion/webrtc PeerConnection carrying Opus audio towards the peer,
// presented as PCMU 20 ms frames to the pkg/media bridge via pkg/media.OpusPCMU. DTMF rides as
// RFC 4733 telephone-event RTP on the same SSRC (a custom TrackLocal preserves the payload
// type per packet, which TrackLocalStaticRTP would rewrite to the bound codec's).

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// WebRTCEngineConfig configures the pion-backed engine.
type WebRTCEngineConfig struct {
	// ICEServers are STUN/TURN URLs ("stun:host:port" or "turn:host:port"); empty = host
	// candidates only.
	ICEServers []string
	// PortMin/PortMax bound the local UDP ports for media (0 = ephemeral). Set these to a
	// range the firewall opens towards the world.
	PortMin uint16
	PortMax uint16
	// ExternalIP, when set, advertises this address for every host candidate instead of the
	// machine's own (NAT1To1): the usual setup for a server behind 1:1 NAT or a firewall that
	// only opens a fixed UDP range.
	ExternalIP string
	Log        zerolog.Logger
}

// Default telephone-event payload type we advertise alongside Opus (RFC 4733 dynamic PT).
const defaultTelephoneEventPT = 101

// NewWebRTCEngine returns an EngineFactory building pion-backed engines.
func NewWebRTCEngine(cfg WebRTCEngineConfig) EngineFactory {
	return func() (Engine, error) { return newWebRTCEngine(cfg) }
}

type webRTCEngine struct {
	cfg   WebRTCEngineConfig
	pc    *webrtc.PeerConnection
	track *mixedPTTrack
	trans *media.OpusPCMU

	audioPT uint8 // negotiated audio payload type
	pcmuLeg bool  // negotiated codec is PCMU (raw passthrough) instead of Opus (transcode)
	tePT    uint8
	teOK    bool

	seq uint16
	ts  uint32

	mu        sync.Mutex
	remoteSet bool
	pending   []Candidate
	txLogged  int

	recv      chan *media.RTPPacket
	closed    chan struct{}
	closeOnce sync.Once
	localRTP  string
	remoteRTP string
}

var _ Engine = (*webRTCEngine)(nil)

func newWebRTCEngine(cfg WebRTCEngineConfig) (*webRTCEngine, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	// IPv4 only: the gateway's candidates are IPv4, and stray IPv6 candidates make remote
	// stacks pick unreachable pairs first.
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if cfg.PortMin > 0 && cfg.PortMax >= cfg.PortMin {
		if err := se.SetEphemeralUDPPortRange(cfg.PortMin, cfg.PortMax); err != nil {
			return nil, err
		}
	}
	if cfg.ExternalIP != "" {
		se.SetNAT1To1IPs([]string{cfg.ExternalIP}, webrtc.ICECandidateTypeHost)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	var servers []webrtc.ICEServer
	if len(cfg.ICEServers) > 0 {
		servers = []webrtc.ICEServer{{URLs: cfg.ICEServers}}
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return nil, err
	}
	trans, err := media.NewOpusPCMU()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	e := &webRTCEngine{
		cfg:     cfg,
		pc:      pc,
		track:   newMixedPTTrack("audio", "jmpcall"),
		trans:   trans,
		audioPT: 111,
		tePT:    defaultTelephoneEventPT,
		recv:    make(chan *media.RTPPacket, 64),
		closed:  make(chan struct{}),
	}
	if _, err := pc.AddTrack(e.track); err != nil {
		_ = pc.Close()
		return nil, err
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		cfg.Log.Info().Str("codec", tr.Codec().MimeType).Uint32("ssrc", uint32(tr.SSRC())).
			Msg("jingleleg: remote track received")
		e.readLoop(tr)
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		cfg.Log.Info().Str("ice_state", st.String()).Msg("jingleleg: ICE connection state")
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		cfg.Log.Info().Str("pc_state", st.String()).Msg("jingleleg: peer connection state")
	})
	return e, nil
}

// readLoop transcodes the remote Opus stream into PCMU frames for the media bridge.
func (e *webRTCEngine) readLoop(tr *webrtc.TrackRemote) {
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			e.Close()
			return
		}
		if pkt.PayloadType == e.tePT {
			continue // inbound DTMF events are dropped (outbound DTMF is RFC 4733; see docs)
		}
		var frames [][]byte
		if e.pcmuLeg {
			frames = splitPCMU(pkt.Payload)
		} else {
			var err error
			frames, err = e.trans.OpusToPCMU(pkt.Payload)
			if err != nil {
				continue
			}
		}
		for _, f := range frames {
			p := &media.RTPPacket{
				SSRC:        pkt.SSRC,
				Seq:         pkt.SequenceNumber,
				Timestamp:   pkt.Timestamp,
				PayloadType: media.PayloadPCMU,
				Marker:      false,
				Payload:     f,
			}
			select {
			case e.recv <- p:
			case <-e.closed:
				return
			default:
				// overflow: drop (the bridge's jitter buffer tolerates gaps)
			}
		}
	}
}

// Accept implements Engine (responder role).
func (e *webRTCEngine) Accept(remote *Jingle) (*Description, *JingleTransport, error) {
	d := remote.AudioDescription()
	if d == nil {
		return nil, nil, fmt.Errorf("jingleleg: no audio description")
	}
	e.noteCodecs(d)
	if tr := remote.AudioTransport(); tr != nil && len(tr.Candidates) > 0 {
		c := tr.Candidates[0]
		e.remoteRTP = fmt.Sprintf("%s:%d", c.IP, c.Port)
	}
	offer, err := sdpFromJingle(stripTelephoneEvent(remote), "offer")
	if err != nil {
		return nil, nil, err
	}
	if err := e.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return nil, nil, err
	}
	e.flushPending()
	answer, err := e.pc.CreateAnswer(nil)
	if err != nil {
		return nil, nil, err
	}
	if err := e.pc.SetLocalDescription(answer); err != nil {
		return nil, nil, err
	}
	<-webrtc.GatheringCompletePromise(e.pc)
	return e.localJingle()
}

// Offer implements Engine (initiator role).
func (e *webRTCEngine) Offer() (*Description, *JingleTransport, error) {
	offer, err := e.pc.CreateOffer(nil)
	if err != nil {
		return nil, nil, err
	}
	if err := e.pc.SetLocalDescription(offer); err != nil {
		return nil, nil, err
	}
	<-webrtc.GatheringCompletePromise(e.pc)
	return e.localJingle()
}

// SetRemoteAnswer implements Engine (initiator role).
func (e *webRTCEngine) SetRemoteAnswer(remote *Jingle) error {
	d := remote.AudioDescription()
	if d == nil {
		return fmt.Errorf("jingleleg: no audio description in answer")
	}
	e.noteCodecs(d)
	sdp, err := sdpFromJingle(stripTelephoneEvent(remote), "answer")
	if err != nil {
		return err
	}
	if err := e.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return err
	}
	e.flushPending()
	return nil
}

// flushPending applies queued trickled candidates once the remote description exists.
func (e *webRTCEngine) flushPending() {
	e.mu.Lock()
	e.remoteSet = true
	pending := e.pending
	e.pending = nil
	e.mu.Unlock()
	if len(pending) > 0 {
		_ = e.addCandidates(pending)
	}
}

// localJingle turns our gathered local SDP into a Jingle description + transport, and keeps the
// ICE candidates as the "selected pair" approximation for logs.
func (e *webRTCEngine) localJingle() (*Description, *JingleTransport, error) {
	sdp := e.pc.LocalDescription()
	if sdp == nil {
		return nil, nil, fmt.Errorf("jingleleg: no local description")
	}
	desc, tr, err := jingleFromSDP(sdp.SDP)
	if err != nil {
		return nil, nil, err
	}
	// Advertise only what we actually bridge: PCMU (PSTN gateways reject Opus-only offers with
	// <incompatible-parameters/>) and Opus. Everything else pion registers is dropped.
	var pts []PayloadType
	for _, pt := range desc.PayloadTypes {
		if equalFold(pt.Name, "opus") || equalFold(pt.Name, "PCMU") {
			pts = append(pts, pt)
		}
	}
	if len(pts) == 0 {
		return nil, nil, fmt.Errorf("jingleleg: local description has neither opus nor PCMU")
	}
	if e.teOK {
		pts = append(pts, PayloadType{
			ID: e.tePT, Name: "telephone-event", ClockRate: 8000,
		})
	}
	desc.PayloadTypes = pts
	desc.RTCPMux = true
	var cands []Candidate
	for _, c := range tr.Candidates {
		if c.Component == 1 {
			cands = append(cands, c)
		}
	}
	tr.Candidates = cands
	if len(tr.Candidates) > 0 {
		c := tr.Candidates[0]
		e.localRTP = fmt.Sprintf("%s:%d", c.IP, c.Port)
	}
	return desc, tr, nil
}

// noteCodecs records the peer's audio and telephone-event payload types. PSTN gateways
// (Cheogram's /tel) speak G.711 only and reject Opus offers with <incompatible-parameters/>,
// so PCMU wins when present: the media bridge works in PCMU anyway, making that leg a passthrough.
func (e *webRTCEngine) noteCodecs(d *Description) {
	var opus, pcmu bool
	var opusPT, pcmuPT uint8
	for _, pt := range d.PayloadTypes {
		switch {
		case equalFold(pt.Name, "opus"):
			opus, opusPT = true, pt.ID
		case equalFold(pt.Name, "PCMU"):
			pcmu, pcmuPT = true, pt.ID
		case equalFold(pt.Name, "telephone-event"):
			e.tePT = pt.ID
			e.teOK = true
		}
	}
	switch {
	case pcmu:
		e.pcmuLeg, e.audioPT = true, pcmuPT
	case opus:
		e.pcmuLeg, e.audioPT = false, opusPT
	}
}

// ReadRTP implements media.SIPLeg: one PCMU frame per call, io.EOF after close.
func (e *webRTCEngine) ReadRTP(p *media.RTPPacket) error {
	select {
	case pkt, ok := <-e.recv:
		if !ok {
			return io.EOF
		}
		*p = *pkt
		return nil
	case <-e.closed:
		return io.EOF
	}
}

// WriteRTP implements media.SIPLeg: one PCMU frame in, one RTP packet out (Opus is transcoded,
// PCMU passes through).
func (e *webRTCEngine) WriteRTP(payload []byte, samples uint32, marker bool) error {
	pkt := payload
	if !e.pcmuLeg {
		var err error
		pkt, err = e.trans.PCMUToOpus(payload)
		if err != nil {
			return err
		}
	}
	ts := e.ts
	seq := e.seq
	inc := samples
	if !e.pcmuLeg {
		inc = samples * 6 // Opus RTP clock is 48 kHz; PCMU's is 8 kHz (RFC 3551)
	}
	e.ts += inc
	e.seq++
	if e.txLogged < 5 {
		e.txLogged++
		e.cfg.Log.Debug().Uint8("pt", e.audioPT).Uint16("seq", seq).Uint32("ts", ts).
			Int("len", len(pkt)).Bool("pcmu", e.pcmuLeg).Msg("jingleleg: outgoing RTP")
	}
	return e.track.WritePacket(e.audioPT, pkt, ts, seq, marker)
}

// TelephoneEventPT implements media.SIPEventLeg.
func (e *webRTCEngine) TelephoneEventPT() (uint8, bool) { return e.tePT, e.teOK }

// WriteEvent implements media.SIPEventLeg: one RFC 4733 telephone-event packet.
func (e *webRTCEngine) WriteEvent(payload []byte, samples uint32, marker bool) error {
	if !e.teOK {
		return media.ErrDTMFUnsupported
	}
	ts := e.ts
	seq := e.seq
	inc := samples
	if !e.pcmuLeg {
		inc = samples * 6
	}
	e.ts += inc
	e.seq++
	return e.track.WritePacket(e.tePT, payload, ts, seq, marker)
}

// AddRemoteTransport applies trickled ICE candidates from a <transport-info/>. Candidates that
// arrive before SetRemoteDescription/Accept are queued and applied afterwards (pion rejects
// them with InvalidStateError otherwise).
func (e *webRTCEngine) AddRemoteTransport(tr *JingleTransport) error {
	if tr == nil {
		return nil
	}
	e.mu.Lock()
	if !e.remoteSet {
		e.pending = append(e.pending, tr.Candidates...)
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	return e.addCandidates(tr.Candidates)
}

func (e *webRTCEngine) addCandidates(cands []Candidate) error {
	for _, c := range cands {
		typ := webrtc.ICECandidateTypeHost
		switch c.Type {
		case "srflx":
			typ = webrtc.ICECandidateTypeSrflx
		case "relay":
			typ = webrtc.ICECandidateTypeRelay
		case "prflx":
			typ = webrtc.ICECandidateTypePrflx
		}
		ic := webrtc.ICECandidateInit{
			Candidate: fmt.Sprintf("candidate:%s %d %s %d %s %d typ %s",
				orDefault(c.Foundation, "1"), c.Component, strings.ToUpper(orDefault(c.Protocol, "udp")),
				c.Priority, c.IP, c.Port, c.Type),
			SDPMid:        ptrStr("0"),
			SDPMLineIndex: ptrUint16(0),
		}
		_ = typ
		if err := e.pc.AddICECandidate(ic); err != nil {
			e.cfg.Log.Warn().Err(err).Str("candidate", ic.Candidate).Msg("jingleleg: AddICECandidate failed")
		}
	}
	return nil
}

// SelectedPair implements Engine (approximation from the gathered candidates; logs only).
func (e *webRTCEngine) SelectedPair() (string, string) { return e.localRTP, e.remoteRTP }

// Close implements Engine. Idempotent.
func (e *webRTCEngine) Close() error {
	e.closeOnce.Do(func() {
		close(e.closed)
		_ = e.pc.Close()
	})
	return nil
}

// --- custom TrackLocal preserving per-packet payload types ---

type mixedPTTrack struct {
	id, streamID string

	mu    sync.Mutex
	w     webrtc.TrackLocalWriter
	ssrc  webrtc.SSRC
	codec webrtc.RTPCodecParameters
	bound bool
}

func newMixedPTTrack(id, streamID string) *mixedPTTrack {
	return &mixedPTTrack{id: id, streamID: streamID}
}

var _ webrtc.TrackLocal = (*mixedPTTrack)(nil)

func (t *mixedPTTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codecs := ctx.CodecParameters()
	if len(codecs) == 0 {
		return webrtc.RTPCodecParameters{}, errors.New("jingleleg: no negotiated codec")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w = ctx.WriteStream()
	t.ssrc = ctx.SSRC()
	t.codec = codecs[0]
	t.bound = true
	return codecs[0], nil
}

func (t *mixedPTTrack) Unbind(webrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bound = false
	t.w = nil
	return nil
}

func (t *mixedPTTrack) ID() string                { return t.id }
func (t *mixedPTTrack) RID() string               { return "" }
func (t *mixedPTTrack) StreamID() string          { return t.streamID }
func (t *mixedPTTrack) Kind() webrtc.RTPCodecType { return webrtc.RTPCodecTypeAudio }

// WritePacket writes one RTP packet with the given payload type (audio or telephone-event).
func (t *mixedPTTrack) WritePacket(pt uint8, payload []byte, ts uint32, seq uint16, marker bool) error {
	t.mu.Lock()
	w, ssrc, bound := t.w, t.ssrc, t.bound
	t.mu.Unlock()
	if !bound {
		return errors.New("jingleleg: track not bound")
	}
	h := rtp.Header{
		Version:        2,
		PayloadType:    pt,
		SSRC:           uint32(ssrc),
		SequenceNumber: seq,
		Timestamp:      ts,
		Marker:         marker,
	}
	_, err := w.WriteRTP(&h, payload)
	return err
}

// --- small helpers ---

func ptrStr(s string) *string    { return &s }
func ptrUint16(v uint16) *uint16 { return &v }

// splitPCMU chunks a G.711 payload into 20 ms (160 byte) frames.
func splitPCMU(payload []byte) [][]byte {
	var out [][]byte
	for len(payload) >= media.FrameSamples {
		frame := make([]byte, media.FrameSamples)
		copy(frame, payload[:media.FrameSamples])
		out = append(out, frame)
		payload = payload[media.FrameSamples:]
	}
	if len(payload) > 0 && len(out) == 0 {
		out = append(out, payload)
	}
	return out
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// stripTelephoneEvent removes telephone-event payload types: pion only negotiates registered
// codecs, and DTMF PT is carried implicitly (we advertise it back in localJingle).
func stripTelephoneEvent(j *Jingle) *Jingle {
	out := *j
	out.Contents = make([]Content, len(j.Contents))
	copy(out.Contents, j.Contents)
	for i := range out.Contents {
		if out.Contents[i].Description == nil {
			continue
		}
		d := *out.Contents[i].Description
		var pts []PayloadType
		for _, pt := range d.PayloadTypes {
			if !equalFold(pt.Name, "telephone-event") {
				pts = append(pts, pt)
			}
		}
		d.PayloadTypes = pts
		out.Contents[i].Description = &d
	}
	return &out
}

// ensure time is referenced even if config fields change.
var _ = time.Now
