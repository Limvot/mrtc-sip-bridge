package media

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type nopSIP struct{ n int }

func (*nopSIP) ReadRTP(*RTPPacket) error                    { select {} }
func (s *nopSIP) WriteRTP(_ []byte, _ uint32, _ bool) error { s.n++; return nil }

type nopRTC struct{ n int }

func (r *nopRTC) WriteOpus([]byte, time.Duration) error { r.n++; return nil }
func (*nopRTC) SetReceiver(PacketReceiver)              {}

type hotPath struct {
	b       *bridge
	sipPkt  RTPPacket
	rtcPkt  RTPPacket
	seq     uint16
	opusBuf []byte
}

func newHotPath(tb testing.TB) *hotPath {
	b, err := newBridge(Config{Log: zerolog.Nop()}, &nopSIP{}, &nopRTC{})
	if err != nil {
		tb.Fatal(err)
	}
	enc, _ := newOpusEncoder()
	buf := make([]byte, maxOpusPacket)
	n, err := enc.encode(sine(440, 0, FrameSamples, 10000), buf)
	if err != nil {
		tb.Fatal(err)
	}
	h := &hotPath{b: b, opusBuf: buf[:n]}
	h.sipPkt = RTPPacket{SSRC: 1, Payload: pcmuSine(0)}
	h.rtcPkt = RTPPacket{SSRC: 2, Payload: h.opusBuf}
	for i := 0; i < 20; i++ { // warm up past the jitter delay
		h.iter()
	}
	return h
}

func (h *hotPath) iter() {
	h.seq++
	h.sipPkt.Seq, h.sipPkt.Timestamp = h.seq, uint32(h.seq)*160
	h.rtcPkt.Seq, h.rtcPkt.Timestamp = h.seq, uint32(h.seq)*160
	h.b.handleSIPPacket(&h.sipPkt)
	h.b.sipToRTCStep()
	h.b.ReceiveRTP(&h.rtcPkt)
	h.b.rtcToSIPStep()
}

func TestHotPathZeroAllocs(t *testing.T) {
	h := newHotPath(t)
	if n := testing.AllocsPerRun(1000, h.iter); n != 0 {
		t.Fatalf("allocs per iteration = %v", n)
	}
	if st := h.b.Stats(); st.ConcealedSIPToRTC != 2 && st.ConcealedSIPToRTC > 3 {
		t.Fatalf("unexpected concealment %+v", st)
	}
}

func BenchmarkSIPToRTCFrame(b *testing.B) {
	h := newHotPath(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.seq++
		h.sipPkt.Seq, h.sipPkt.Timestamp = h.seq, uint32(h.seq)*160
		h.b.handleSIPPacket(&h.sipPkt)
		h.b.sipToRTCStep()
	}
}

func BenchmarkRTCToSIPFrame(b *testing.B) {
	h := newHotPath(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.seq++
		h.rtcPkt.Seq, h.rtcPkt.Timestamp = h.seq, uint32(h.seq)*160
		h.b.ReceiveRTP(&h.rtcPkt)
		h.b.rtcToSIPStep()
	}
}

func TestHotPathZeroAllocsDTMF(t *testing.T) {
	h := newHotPath(t)
	ev := &nopEventSIP{}
	h.b.sip, h.b.events = ev, ev
	_ = h.b.SendDTMF("0123456789")
	if n := testing.AllocsPerRun(50, h.iter); n != 0 {
		t.Fatalf("allocs with DTMF in progress = %v", n)
	}
	if ev.events == 0 {
		t.Fatal("no events sent")
	}
}

func TestHotPathZeroAllocsEarly(t *testing.T) {
	b, err := newBridge(Config{Log: zerolog.Nop(), Early: true}, &nopSIP{}, &nopRTC{})
	if err != nil {
		t.Fatal(err)
	}
	pkt := RTPPacket{SSRC: 1, Payload: pcmuSine(0)}
	var seq uint16
	iter := func() {
		seq++
		pkt.Seq, pkt.Timestamp = seq, uint32(seq)*160
		b.handleSIPPacket(&pkt)
		b.sipToRTCStep()
		b.rtcToSIPStep()
	}
	for i := 0; i < 20; i++ {
		iter()
	}
	if n := testing.AllocsPerRun(1000, iter); n != 0 {
		t.Fatalf("early allocs = %v", n)
	}
	// Ringback fill path (no SIP RTP).
	b.cfg.EarlyFillAfter = -1
	if n := testing.AllocsPerRun(1000, func() { b.sipToRTCStep(); b.rtcToSIPStep() }); n != 0 {
		t.Fatalf("ringback allocs = %v", n)
	}
}

type nopEventSIP struct {
	nopSIP
	events int
}

func (*nopEventSIP) TelephoneEventPT() (uint8, bool) { return 101, true }
func (e *nopEventSIP) WriteEvent([]byte, uint32, bool) error {
	e.events++
	return nil
}

func BenchmarkRTCToSIPFrameDTMF(b *testing.B) {
	h := newHotPath(b)
	ev := &nopEventSIP{}
	h.b.sip, h.b.events = ev, ev
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if h.b.dtmf.pending.Load() == 0 {
			b.StopTimer()
			_ = h.b.SendDTMF("0123456789")
			b.StartTimer()
		}
		h.rtcPkt.Seq++
		h.b.ReceiveRTP(&h.rtcPkt)
		h.b.rtcToSIPStep()
	}
}

func BenchmarkSIPToRTCFrameEarly(b *testing.B) {
	bb, _ := newBridge(Config{Log: zerolog.Nop(), Early: true, EarlyFillAfter: -1}, &nopSIP{}, &nopRTC{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bb.sipToRTCStep()
		bb.rtcToSIPStep()
	}
}
