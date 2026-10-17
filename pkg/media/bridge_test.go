package media

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

type sentRTP struct {
	payload []byte
	samples uint32
	marker  bool
}

type fakeSIP struct {
	ch      chan RTPPacket
	eof     chan struct{}
	eofOnce sync.Once
	reads   atomic.Int64
	buf     [1500]byte

	mu   sync.Mutex
	sent []sentRTP
	n    atomic.Int64
}

func newFakeSIP() *fakeSIP { return &fakeSIP{ch: make(chan RTPPacket), eof: make(chan struct{})} }

func (f *fakeSIP) ReadRTP(p *RTPPacket) error {
	f.reads.Add(1)
	select {
	case q := <-f.ch:
		n := copy(f.buf[:], q.Payload)
		*p = q
		p.Payload = f.buf[:n]
		return nil
	case <-f.eof:
		return io.EOF
	}
}

func (f *fakeSIP) closeEOF() { f.eofOnce.Do(func() { close(f.eof) }) }

func (f *fakeSIP) WriteRTP(payload []byte, samples uint32, marker bool) error {
	f.mu.Lock()
	f.sent = append(f.sent, sentRTP{append([]byte(nil), payload...), samples, marker})
	f.mu.Unlock()
	f.n.Add(1)
	return nil
}

// inject delivers p and waits until the reader has processed it.
func (f *fakeSIP) inject(p RTPPacket) {
	n := f.reads.Load()
	f.ch <- p
	for f.reads.Load() == n {
		time.Sleep(50 * time.Microsecond)
	}
}

func (f *fakeSIP) sentCopy() []sentRTP {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentRTP(nil), f.sent...)
}

type fakeRTC struct {
	mu     sync.Mutex
	frames [][]byte
	n      atomic.Int64
	recv   atomic.Pointer[PacketReceiver]
	nilSet atomic.Bool
}

func (f *fakeRTC) WriteOpus(frame []byte, d time.Duration) error {
	if d != 20*time.Millisecond {
		panic("bad duration")
	}
	f.mu.Lock()
	f.frames = append(f.frames, append([]byte(nil), frame...))
	f.mu.Unlock()
	f.n.Add(1)
	return nil
}

func (f *fakeRTC) SetReceiver(r PacketReceiver) {
	if r == nil {
		f.nilSet.Store(true)
		f.recv.Store(nil)
		return
	}
	f.recv.Store(&r)
}

func (f *fakeRTC) receive(p *RTPPacket) { (*f.recv.Load()).ReceiveRTP(p) }

type harness struct {
	t     *testing.T
	clk   *clock.Fake
	sip   *fakeSIP
	rtc   *fakeRTC
	b     Bridge
	ticks int64
}

func newHarness(t *testing.T, cfg Config) *harness { return newHarnessLeg(t, cfg, nil) }

// newHarnessLeg is newHarness with the SIP leg handed to Start wrapped (wrap may be nil).
func newHarnessLeg(t *testing.T, cfg Config, wrap func(*fakeSIP) SIPLeg) *harness {
	h := &harness{t: t, clk: clock.NewFake(time.Unix(1_000_000, 0)), sip: newFakeSIP(), rtc: &fakeRTC{}}
	cfg.Clock = h.clk
	cfg.Log = zerolog.Nop()
	var leg SIPLeg = h.sip
	if wrap != nil {
		leg = wrap(h.sip)
	}
	b, err := Start(cfg, leg, h.rtc)
	if err != nil {
		t.Fatal(err)
	}
	h.b = b
	t.Cleanup(func() { b.Stop(); h.sip.closeEOF() })
	if !h.clk.WaitForWaiters(2) {
		t.Fatal("tickers not created")
	}
	return h
}

// tick advances one frame and waits until both ticker goroutines processed it (or the bridge ended).
func (h *harness) tick() {
	h.ticks++
	h.clk.Advance(FrameDuration)
	deadline := time.Now().Add(5 * time.Second)
	for h.rtc.n.Load() < h.ticks || h.sip.n.Load() < h.ticks {
		select {
		case <-h.b.Done():
			return
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatal("tick not processed")
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func pcmuSine(start int) []byte {
	pcm := sine(440, start, FrameSamples, 10000)
	out := make([]byte, FrameSamples)
	ULawEncode(out, pcm)
	return out
}

func TestBridgeSIPToRTC(t *testing.T) {
	h := newHarness(t, Config{})
	for i := 0; i < 60; i++ {
		h.sip.inject(RTPPacket{SSRC: 7, Seq: uint16(i), Timestamp: uint32(i * 160), Payload: pcmuSine(i * FrameSamples)})
		h.tick()
	}
	st := h.b.Stats()
	if st.SIPRx != 60 || st.ConcealedSIPToRTC != 2 || st.SIPLost != 0 || st.SIPLate != 0 {
		t.Fatalf("%+v", st)
	}
	h.rtc.mu.Lock()
	frames := h.rtc.frames
	h.rtc.mu.Unlock()
	dec, _ := newOpusDecoder()
	pcm := make([]int16, maxDecodePCM)
	var out []int16
	for i, f := range frames {
		m, err := dec.decode(f, pcm)
		if err != nil || m != FrameSamples {
			t.Fatal(m, err)
		}
		if i >= 10 {
			out = append(out, pcm[:m]...)
		}
	}
	checkTone(t, out, 10000)
	if st.LastSIPRx.IsZero() {
		t.Fatal("LastSIPRx")
	}
}

func TestBridgeRTCToSIP(t *testing.T) {
	h := newHarness(t, Config{})
	enc, _ := newOpusEncoder()
	buf := make([]byte, maxOpusPacket)
	for i := 0; i < 60; i++ {
		n, err := enc.encode(sine(440, i*FrameSamples, FrameSamples, 10000), buf)
		if err != nil {
			t.Fatal(err)
		}
		h.rtc.receive(&RTPPacket{SSRC: 9, Seq: uint16(i), Timestamp: uint32(i * 160), Payload: buf[:n]})
		h.tick()
	}
	if got := h.b.Stats().RTCRx; got != 60 {
		t.Fatal(got)
	}
	var out []int16
	for i, s := range h.sip.sentCopy() {
		if len(s.payload) != FrameSamples || s.samples != FrameSamples || s.marker != (i == 0) {
			t.Fatalf("packet %d: %d %d %v", i, len(s.payload), s.samples, s.marker)
		}
		if i >= 10 {
			pcm := make([]int16, FrameSamples)
			ULawDecode(pcm, s.payload)
			out = append(out, pcm...)
		}
	}
	checkTone(t, out, 10000)
}

func TestBridgeSilenceFill(t *testing.T) {
	h := newHarness(t, Config{})
	for i := 0; i < 5; i++ {
		h.tick()
	}
	sent := h.sip.sentCopy()
	if len(sent) != 5 {
		t.Fatal(len(sent))
	}
	for i, s := range sent {
		if s.marker != (i == 0) || s.samples != 160 {
			t.Fatalf("pkt %d marker=%v", i, s.marker)
		}
		for _, c := range s.payload {
			if c != ULawSilence {
				t.Fatalf("pkt %d not silence", i)
			}
		}
	}
	if h.b.Stats().SIPTx != 5 {
		t.Fatal(h.b.Stats().SIPTx)
	}
}

func TestBridgeRTPTimeout(t *testing.T) {
	h := newHarness(t, Config{RTPTimeout: time.Second})
	for i := 0; i < 100; i++ {
		select {
		case <-h.b.Done():
		default:
			h.tick()
		}
	}
	select {
	case <-h.b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("no timeout")
	}
	if h.b.Err() != ErrRTPTimeout {
		t.Fatal(h.b.Err())
	}
	h.b.Stop()
	if h.b.Err() != ErrRTPTimeout {
		t.Fatal("Stop overwrote the error")
	}
}

func TestBridgeSIPEOF(t *testing.T) {
	h := newHarness(t, Config{})
	h.tick()
	h.sip.closeEOF()
	select {
	case <-h.b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("no done")
	}
	if h.b.Err() != io.EOF {
		t.Fatal(h.b.Err())
	}
}

func TestBridgeStop(t *testing.T) {
	h := newHarness(t, Config{})
	if h.b.Err() != nil {
		t.Fatal("Err before Done")
	}
	h.tick()
	h.b.Stop()
	h.b.Stop()
	if !h.rtc.nilSet.Load() {
		t.Fatal("receiver not detached")
	}
	select {
	case <-h.b.Done():
	default:
		t.Fatal("Done not closed")
	}
	if h.b.Err() != nil {
		t.Fatal(h.b.Err())
	}
}

func TestBridgeTelephoneEvent(t *testing.T) {
	h := newHarness(t, Config{})
	for i := 0; i < 5; i++ {
		h.sip.inject(RTPPacket{SSRC: 7, Seq: uint16(i), PayloadType: 101, Payload: []byte{1, 0, 0, 160}})
	}
	if got := h.b.Stats().SIPRx; got != 5 {
		t.Fatal(got)
	}
	if n := h.b.(*bridge).jSIP.buffered(); n != 0 {
		t.Fatalf("telephone-event reached jitter buffer: %d", n)
	}
	h.tick()
	if got := h.b.Stats().ConcealedSIPToRTC; got != 1 {
		t.Fatal(got)
	}
}

func TestBridgeStatsLossLate(t *testing.T) {
	h := newHarness(t, Config{})
	for i := 0; i < 12; i++ {
		if i == 5 { // lost
			h.tick()
			continue
		}
		h.sip.inject(RTPPacket{SSRC: 7, Seq: uint16(i), Timestamp: uint32(i * 160), Payload: pcmuSine(0)})
		h.tick()
	}
	h.sip.inject(RTPPacket{SSRC: 7, Seq: 3, Payload: pcmuSine(0)}) // duplicate, already played
	st := h.b.Stats()
	if st.SIPLost != 1 || st.SIPLate != 1 || st.SIPRx != 12 {
		t.Fatalf("%+v", st)
	}
	if st.ConcealedSIPToRTC != 3 { // two initial frames plus the lost one
		t.Fatalf("concealed %d", st.ConcealedSIPToRTC)
	}
}

// decodeFrames decodes Opus frames written to the fake RTC leg.
func decodeFrames(t *testing.T, frames [][]byte) [][]int16 {
	t.Helper()
	dec, _ := newOpusDecoder()
	out := make([][]int16, len(frames))
	for i, f := range frames {
		pcm := make([]int16, maxDecodePCM)
		m, err := dec.decode(f, pcm)
		if err != nil || m != FrameSamples {
			t.Fatal(m, err)
		}
		out[i] = pcm[:m]
	}
	return out
}

func (f *fakeRTC) framesCopy() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func TestEarlyModeSilenceAndNoReceiver(t *testing.T) {
	h := newHarness(t, Config{Early: true})
	if h.rtc.recv.Load() != nil {
		t.Fatal("receiver attached in early mode")
	}
	for i := 0; i < 5; i++ {
		h.tick()
	}
	for i, s := range h.sip.sentCopy() {
		if s.marker != (i == 0) || len(s.payload) != FrameSamples || s.samples != FrameSamples {
			t.Fatalf("pkt %d", i)
		}
		for _, c := range s.payload {
			if c != ULawSilence {
				t.Fatalf("pkt %d not silence", i)
			}
		}
	}
	if h.b.Stats().ConcealedRTCToSIP != 0 {
		t.Fatal("early silence counted as concealment")
	}
	if err := h.b.SendDTMF("1"); err != ErrDTMFNotConnected {
		t.Fatal(err)
	}
}

func TestEarlyModeRingbackThenRelay(t *testing.T) {
	h := newHarness(t, Config{Early: true, EarlyFillAfter: time.Second})
	for i := 0; i < 50; i++ { // 1 s of ringback fill (ringback is on for the first 2 s)
		h.tick()
	}
	for i := 0; i < 25; i++ {
		h.tick()
	}
	frames := decodeFrames(t, h.rtc.framesCopy())
	// Frames 0..48 are before the fill threshold (silence), then ringback.
	for i, f := range frames {
		loud := rms(f) > 1000
		if i < 49 && loud {
			t.Fatalf("frame %d loud before fill", i)
		}
		if i >= 55 && !loud {
			t.Fatalf("frame %d silent during fill", i)
		}
	}
	if got := h.b.Stats().ConcealedSIPToRTC; got != 49 { // only the pre-fill second counts
		t.Fatalf("ringback counted as concealment: %d", got)
	}
	// SIP audio arrives: relayed instead of ringback.
	base := len(frames)
	for i := 0; i < 40; i++ {
		h.sip.inject(RTPPacket{SSRC: 7, Seq: uint16(i), Timestamp: uint32(i * 160), Payload: pcmuSine(i * FrameSamples)})
		h.tick()
	}
	frames = decodeFrames(t, h.rtc.framesCopy())
	var out []int16
	for _, f := range frames[base+10:] {
		out = append(out, f...)
	}
	checkTone(t, out, 10000) // 440 Hz relayed, not the 440+480 ringback
}

func TestEarlyModeNoRTPTimeoutThenConnect(t *testing.T) {
	h := newHarness(t, Config{Early: true, RTPTimeout: time.Second})
	for i := 0; i < 200; i++ { // 4 s, far over the timeout
		h.tick()
	}
	select {
	case <-h.b.Done():
		t.Fatal("RTP timeout fired while early")
	default:
	}
	h.b.Connect()
	h.b.Connect()
	if h.rtc.recv.Load() == nil {
		t.Fatal("receiver not attached by Connect")
	}
	for i := 0; i < 40; i++ { // 0.8 s after Connect: still fine
		h.tick()
	}
	select {
	case <-h.b.Done():
		t.Fatal("timeout counted from before Connect")
	default:
	}
	for i := 0; i < 20; i++ {
		select {
		case <-h.b.Done():
		default:
			h.tick()
		}
	}
	select {
	case <-h.b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("no timeout after Connect")
	}
	if h.b.Err() != ErrRTPTimeout {
		t.Fatal(h.b.Err())
	}
}

func TestConnectNoopWhenNotEarly(t *testing.T) {
	h := newHarness(t, Config{})
	h.b.Connect()
	h.tick()
}
