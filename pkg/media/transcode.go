package media

import (
	"errors"
	"fmt"
)

// OpusPCMU transcodes between Opus packets and PCMU 20 ms frames at 8 kHz, for call legs that
// carry Opus RTP (the Jingle/WebRTC leg, pkg/jingleleg) towards the PCMU media bridge. Opus
// internally coded at 8 kHz is a valid Opus stream for any receiver (RFC 6716), and telephone
// audio is 8 kHz anyway, so no resampling is needed.
type OpusPCMU struct {
	enc *opusEncoder
	dec *opusDecoder
	pcm [maxDecodePCM]int16
	// pending holds decoded samples not yet returned as a frame (multi-frame Opus packets).
	pending []int16
	out     [maxDecodePCM]byte
}

// ErrShortFrame is returned by PCMUToOpus for payloads shorter than FrameSamples bytes.
var ErrShortFrame = errors.New("media: PCMU frame shorter than 20 ms")

// NewOpusPCMU builds a transcoder.
func NewOpusPCMU() (*OpusPCMU, error) {
	enc, err := newOpusEncoder()
	if err != nil {
		return nil, err
	}
	dec, err := newOpusDecoder()
	if err != nil {
		return nil, err
	}
	return &OpusPCMU{enc: enc, dec: dec}, nil
}

// OpusToPCMU decodes one Opus packet into zero or more 160-byte PCMU frames (a 20 ms Opus
// packet yields exactly one). The returned frames must be consumed before the next call.
func (t *OpusPCMU) OpusToPCMU(pkt []byte) ([][]byte, error) {
	if len(pkt) == 0 || len(pkt) > maxOpusPacket {
		return nil, fmt.Errorf("media: bad opus packet length %d", len(pkt))
	}
	n, err := t.dec.decode(pkt, t.pcm[:])
	if err != nil {
		return nil, err
	}
	return t.frames(t.pcm[:n]), nil
}

// Conceal returns one 160-byte PCMU frame of packet-loss concealment audio (PLC), for gaps in
// the remote stream.
func (t *OpusPCMU) Conceal() ([]byte, error) {
	var pcm [FrameSamples]int16
	if err := t.dec.plc(pcm[:]); err != nil {
		return nil, err
	}
	frame := make([]byte, FrameSamples)
	ULawEncode(frame, pcm[:])
	return frame, nil
}

func (t *OpusPCMU) frames(pcm []int16) [][]byte {
	t.pending = append(t.pending, pcm...)
	var out [][]byte
	for len(t.pending) >= FrameSamples {
		frame := make([]byte, FrameSamples)
		ULawEncode(frame, t.pending[:FrameSamples])
		out = append(out, frame)
		t.pending = t.pending[FrameSamples:]
	}
	if len(t.pending) == 0 {
		t.pending = nil
	}
	return out
}

// PCMUToOpus encodes one 160-byte PCMU frame into one Opus packet.
func (t *OpusPCMU) PCMUToOpus(pcmu []byte) ([]byte, error) {
	if len(pcmu) != FrameSamples {
		return nil, ErrShortFrame
	}
	var pcm [FrameSamples]int16
	ULawDecode(pcm[:], pcmu)
	n, err := t.enc.encode(pcm[:], t.out[:])
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, n)
	copy(pkt, t.out[:n])
	return pkt, nil
}
