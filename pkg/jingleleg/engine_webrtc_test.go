package jingleleg

import (
	"testing"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// TestWebRTCEngineLoopback wires two pion engines together through real Jingle signalling
// (Offer -> Accept -> SetRemoteAnswer) and pushes PCMU audio through: proves the SDP mapping,
// ICE/DTLS establishment and the Opus/PCMU transcode in one go. Needs loopback UDP.
func TestWebRTCEngineLoopback(t *testing.T) {
	a, err := newWebRTCEngine(WebRTCEngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newWebRTCEngine(WebRTCEngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	offerDesc, offerTr, err := a.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if offerDesc == nil || offerTr == nil {
		t.Fatal("no offer produced")
	}
	if !hasCodec(offerDesc, "opus") || !hasCodec(offerDesc, "PCMU") {
		t.Fatalf("offer codecs: %+v", offerDesc.PayloadTypes)
	}
	offer := &Jingle{Action: ActionSessionInitiate, SID: "s1",
		Contents: []Content{{Creator: "initiator", Name: "voice", Description: offerDesc, Transport: offerTr}}}

	ansDesc, ansTr, err := b.Accept(offer)
	if err != nil {
		t.Fatal(err)
	}
	answer := &Jingle{Action: ActionSessionAccept, SID: "s1",
		Contents: []Content{{Creator: "initiator", Name: "voice", Description: ansDesc, Transport: ansTr}}}
	if err := a.SetRemoteAnswer(answer); err != nil {
		t.Fatal(err)
	}

	// Wait for ICE/DTLS, then relay frames a -> b.
	got := make(chan *media.RTPPacket, 8)
	go func() {
		p := &media.RTPPacket{}
		for {
			if err := b.ReadRTP(p); err != nil {
				close(got)
				return
			}
			select {
			case got <- p:
			default:
			}
		}
	}()

	deadline := time.After(15 * time.Second)
	sent := 0
	for {
		if sent < 50 {
			frame := make([]byte, media.FrameSamples)
			for i := range frame {
				frame[i] = 0xFF // mu-law silence
			}
			if err := a.WriteRTP(frame, media.FrameSamples, sent == 0); err != nil {
				t.Fatalf("write: %v", err)
			}
			sent++
		}
		select {
		case p, ok := <-got:
			if !ok {
				t.Fatal("read loop ended early")
			}
			if p.PayloadType != media.PayloadPCMU || len(p.Payload) != media.FrameSamples {
				t.Fatalf("bad frame: pt=%d len=%d", p.PayloadType, len(p.Payload))
			}
			// DTMF through the negotiated telephone-event PT (only when the peer offered it).
			if pt, ok := b.TelephoneEventPT(); ok {
				payload := []byte{1, 10, 0, 0}
				if err := a.WriteEvent(payload, 0, true); err != nil {
					t.Fatalf("dtmf: %v", err)
				}
				_ = pt
			}
			return
		case <-deadline:
			t.Fatal("timeout waiting for relayed audio")
		case <-time.After(20 * time.Millisecond):
			// keep writing
		}
	}
}
