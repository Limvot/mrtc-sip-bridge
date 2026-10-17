package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	opus "gopkg.in/hraban/opus.v2"
	mevent "maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

type closedLeg struct{}

func (closedLeg) ReadRTP(*media.RTPPacket) error      { return io.EOF }
func (closedLeg) WriteRTP([]byte, uint32, bool) error { return nil }

// toneSIPLeg yields PCMU packets pushed by the test and collects what the bridge writes.
type toneSIPLeg struct {
	in   chan []byte
	once sync.Once
	done chan struct{}

	mu      sync.Mutex
	written [][]byte
	seq     uint16
}

func newToneSIPLeg() *toneSIPLeg {
	return &toneSIPLeg{in: make(chan []byte, 256), done: make(chan struct{})}
}

func (l *toneSIPLeg) ReadRTP(p *media.RTPPacket) error {
	select {
	case b := <-l.in:
		l.mu.Lock()
		l.seq++
		p.Seq = l.seq
		p.Timestamp = uint32(l.seq) * media.FrameSamples
		l.mu.Unlock()
		p.SSRC, p.PayloadType, p.Marker, p.Payload = 1234, media.PayloadPCMU, false, b
		return nil
	case <-l.done:
		return io.EOF
	}
}

func (l *toneSIPLeg) WriteRTP(payload []byte, _ uint32, _ bool) error {
	l.mu.Lock()
	l.written = append(l.written, append([]byte(nil), payload...))
	l.mu.Unlock()
	return nil
}

func (l *toneSIPLeg) close() { l.once.Do(func() { close(l.done) }) }

func (l *toneSIPLeg) writtenPackets() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.written...)
}

// toneRTCLeg collects published Opus frames and exposes the receiver.
type toneRTCLeg struct {
	mu     sync.Mutex
	frames [][]byte
	recv   media.PacketReceiver
}

func (l *toneRTCLeg) WriteOpus(frame []byte, _ time.Duration) error {
	l.mu.Lock()
	l.frames = append(l.frames, append([]byte(nil), frame...))
	l.mu.Unlock()
	return nil
}

func (l *toneRTCLeg) SetReceiver(r media.PacketReceiver) {
	l.mu.Lock()
	l.recv = r
	l.mu.Unlock()
}

func (l *toneRTCLeg) receiver() media.PacketReceiver {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recv
}

func (l *toneRTCLeg) published() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.frames...)
}

type fakeRegistrar struct {
	handlers map[string]func(context.Context, *mevent.Event)
}

func (r *fakeRegistrar) On(t mevent.Type, h func(context.Context, *mevent.Event)) {
	r.handlers[t.Type] = h
}

// deliver parses a JSON event like the appservice does (class from the state key) and calls the
// registered handler.
func (r *fakeRegistrar) deliver(t *testing.T, raw string) {
	t.Helper()
	var evt mevent.Event
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if evt.StateKey != nil {
		evt.Type.Class = mevent.StateEventType
	} else {
		evt.Type.Class = mevent.MessageEventType
	}
	h := r.handlers[evt.Type.Type]
	if h == nil {
		t.Fatalf("no handler for %s", evt.Type.Type)
	}
	h(context.Background(), &evt)
	synctest.Wait()
}

// Element X style owner membership (livekit_alias present) and the {} leave.
func ownerJoinFixture(now time.Time) string {
	ms := now.UnixMilli()
	return fmt.Sprintf(`{"type":"org.matrix.msc3401.call.member","room_id":"%s","event_id":"$ownerjoin",
"sender":"%s","state_key":"_%s_EXDEVICE_m.call","origin_server_ts":%d,
"content":{"application":"m.call","call_id":"","device_id":"EXDEVICE","expires":14400000,"created_ts":%d,
"focus_active":{"type":"livekit","focus_selection":"oldest_membership"},
"foci_preferred":[{"type":"livekit","livekit_alias":"%s","livekit_service_url":"https://lk.example.org/livekit/jwt"}],
"m.call.intent":"audio","scope":"m.room"}}`, testRoom, testOwner, testOwner, ms, ms, testRoom)
}

func ownerLeaveFixture(now time.Time) string {
	return fmt.Sprintf(`{"type":"org.matrix.msc3401.call.member","room_id":"%s","event_id":"$ownerleave",
"sender":"%s","state_key":"_%s_EXDEVICE_m.call","origin_server_ts":%d,"content":{}}`,
		testRoom, testOwner, testOwner, now.UnixMilli())
}

func declineFixture(ring string) string {
	return fmt.Sprintf(`{"type":"org.matrix.msc4310.rtc.decline","room_id":"%s","event_id":"$decl","sender":"%s",
"origin_server_ts":1,"content":{"m.relates_to":{"rel_type":"m.reference","event_id":"%s"}}}`, testRoom, testOwner, ring)
}

func sine(freq float64, n, offset int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(8000 * math.Sin(2*math.Pi*freq*float64(offset+i)/media.SampleRate))
	}
	return out
}

// dominantFreq estimates the frequency of pcm (8 kHz) from zero crossings.
func dominantFreq(pcm []int16) float64 {
	crossings := 0
	for i := 1; i < len(pcm); i++ {
		if (pcm[i-1] < 0) != (pcm[i] < 0) {
			crossings++
		}
	}
	return float64(crossings) / 2 / (float64(len(pcm)) / media.SampleRate)
}

type integration struct {
	h   *harness
	reg *fakeRegistrar
	rtc *toneRTCLeg
}

func newIntegration(t *testing.T, opts ...harnessOpt) *integration {
	h := newHarness(t, opts...)
	rtcLeg := &toneRTCLeg{}
	h.joiner.audio = rtcLeg
	// Rebuild the manager with the real media bridge.
	cfg := h.cfg
	cfg.StartMedia = nil
	cfg.Log = zerolog.New(h.logs)
	h.m = NewManager(cfg)
	h.m.startTone = h.tones.start
	h.ingest = core.NewIngest(h.m)
	reg := &fakeRegistrar{handlers: map[string]func(context.Context, *mevent.Event){}}
	core.RegisterMatrixRTCHandlers(reg, func(_ context.Context, evt *mevent.Event) (core.LoginRef, bool) {
		return testLogin, evt.RoomID == testRoom
	}, h.ingest)
	t.Cleanup(func() { h.m.Stop(context.Background()) })
	return &integration{h: h, reg: reg, rtc: rtcLeg}
}

func TestIntegrationAnsweredCall(t *testing.T) {
	run(t, func(t *testing.T) {
		it := newIntegration(t)
		h := it.h
		sipLeg := newToneSIPLeg()
		sc := h.newSIPCall()
		sc.leg = sipLeg
		h.m.HandleInvite(testLogin, testLocal, sc)
		synctest.Wait()
		if !sc.has("ringing") || len(h.ghost.msgCalls()) != 1 {
			t.Fatalf("not ringing: %v", sc.commands())
		}
		it.reg.deliver(t, ownerJoinFixture(h.clk.Now()))
		if !sc.has("answer") {
			t.Fatalf("not answered: %v", sc.commands())
		}
		if it.rtc.receiver() == nil {
			t.Fatal("media bridge did not install the RTC receiver")
		}
		h.joiner.lastSession().push(rtcOwnerJoined("EXDEVICE"))
		synctest.Wait()

		enc, err := opus.NewEncoder(media.SampleRate, 1, opus.AppVoIP)
		if err != nil {
			t.Fatal(err)
		}
		opusBuf := make([]byte, 1275)
		ulaw := make([]byte, media.FrameSamples)
		const frames = 100 // 2 s
		for i := 0; i < frames; i++ {
			media.ULawEncode(ulaw, sine(440, media.FrameSamples, i*media.FrameSamples))
			sipLeg.in <- append([]byte(nil), ulaw...)
			n, err := enc.Encode(sine(1000, media.FrameSamples, i*media.FrameSamples), opusBuf)
			if err != nil {
				t.Fatal(err)
			}
			it.rtc.receiver().ReceiveRTP(&media.RTPPacket{SSRC: 99, Seq: uint16(i + 1), Timestamp: uint32(i * 960),
				PayloadType: 111, Payload: opusBuf[:n]})
			synctest.Wait()
			h.advance(media.FrameDuration)
		}

		// SIP leg: the owner's 1000 Hz tone, PCMU, one packet per tick.
		written := sipLeg.writtenPackets()
		if len(written) < frames-5 {
			t.Fatalf("SIP leg got %d packets, want about %d", len(written), frames)
		}
		var pcm []int16
		for _, p := range written[len(written)/2:] {
			s := make([]int16, len(p))
			media.ULawDecode(s, p)
			pcm = append(pcm, s...)
		}
		if f := dominantFreq(pcm); math.Abs(f-1000) > 50 {
			t.Errorf("SIP leg tone %.0f Hz, want 1000", f)
		}
		// RTC leg: the caller's 440 Hz tone, Opus.
		dec, err := opus.NewDecoder(media.SampleRate, 1)
		if err != nil {
			t.Fatal(err)
		}
		published := it.rtc.published()
		if len(published) < frames-5 {
			t.Fatalf("RTC leg got %d frames", len(published))
		}
		pcm = pcm[:0]
		buf := make([]int16, 960)
		for i, f := range published {
			n, err := dec.Decode(f, buf)
			if err != nil {
				t.Fatalf("decode frame %d: %v", i, err)
			}
			if i >= len(published)/2 {
				pcm = append(pcm, buf[:n]...)
			}
		}
		if f := dominantFreq(pcm); math.Abs(f-440) > 30 {
			t.Errorf("RTC leg tone %.0f Hz, want 440", f)
		}

		it.reg.deliver(t, ownerLeaveFixture(h.clk.Now()))
		if sc.count("hangup") != 1 {
			t.Fatalf("BYE not issued: %v", sc.commands())
		}
		states := h.ghost.stateCalls()
		if last := states[len(states)-1]; len(last.content) != 0 {
			t.Error("ghost membership not cleared")
		}
		if !strings.HasPrefix(h.host.noticeList()[0], "Call · ") {
			t.Errorf("notice %q", h.host.noticeList())
		}
		h.assertEnded("answered", "2xx")
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
	})
}

func TestIntegrationDecline(t *testing.T) {
	run(t, func(t *testing.T) {
		it := newIntegration(t)
		h := it.h
		sc := h.invite()
		it.reg.deliver(t, declineFixture(string(h.ringID())))
		if !sc.has("reject:603") {
			t.Fatalf("commands %v", sc.commands())
		}
		assertNotices(t, h, noticeDeclined)
		h.assertEnded("declined", "6xx")
	})
}

func rtcOwnerJoined(device string) rtc.Event {
	return rtc.Event{Kind: rtc.EventOwnerJoined, Identity: string(testOwner) + ":" + device}
}

// Element X style owner notification (MSC4075, unstable name).
func ownerNotifFixture(now time.Time) string {
	ms := now.UnixMilli()
	return fmt.Sprintf(`{"type":"org.matrix.msc4075.rtc.notification","room_id":"%s","event_id":"%s","sender":"%s",
"origin_server_ts":%d,"content":{"notification_type":"ring","sender_ts":%d,"lifetime":30000,
"m.mentions":{"user_ids":[],"room":true},"m.relates_to":{"rel_type":"m.reference","event_id":"$ownerjoin"}}}`,
		testRoom, testNotif, testOwner, ms, ms)
}

// pumpFrames feeds n frames: a 440 Hz caller tone on the SIP leg and, if owner is set, a 1000 Hz owner
// tone through the RTC receiver; one 20 ms tick per frame.
func pumpFrames(t *testing.T, h *harness, sipLeg *toneSIPLeg, rtcLeg *toneRTCLeg, n, offset int, owner bool) {
	t.Helper()
	enc, err := opus.NewEncoder(media.SampleRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	opusBuf := make([]byte, 1275)
	ulaw := make([]byte, media.FrameSamples)
	for i := offset; i < offset+n; i++ {
		media.ULawEncode(ulaw, sine(440, media.FrameSamples, i*media.FrameSamples))
		sipLeg.in <- append([]byte(nil), ulaw...)
		if r := rtcLeg.receiver(); owner && r != nil {
			k, err := enc.Encode(sine(1000, media.FrameSamples, i*media.FrameSamples), opusBuf)
			if err != nil {
				t.Fatal(err)
			}
			r.ReceiveRTP(&media.RTPPacket{SSRC: 99, Seq: uint16(i + 1), Timestamp: uint32(i * 960),
				PayloadType: 111, Payload: opusBuf[:k]})
		}
		synctest.Wait()
		h.advance(media.FrameDuration)
	}
}

// publishedFreq decodes the second half of frames[from:] and returns the dominant frequency.
func publishedFreq(t *testing.T, frames [][]byte) float64 {
	t.Helper()
	dec, err := opus.NewDecoder(media.SampleRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	var pcm []int16
	buf := make([]int16, 960)
	for i, f := range frames {
		n, err := dec.Decode(f, buf)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if i >= len(frames)/2 {
			pcm = append(pcm, buf[:n]...)
		}
	}
	return dominantFreq(pcm)
}

func sipFreq(packets [][]byte) float64 {
	var pcm []int16
	for _, p := range packets[len(packets)/2:] {
		s := make([]int16, len(p))
		media.ULawDecode(s, p)
		pcm = append(pcm, s...)
	}
	return dominantFreq(pcm)
}

// Owner-initiated outbound call end to end: real core.Ingest and media bridge (early mode, Connect).
func TestIntegrationOutboundCall(t *testing.T) {
	run(t, func(t *testing.T) {
		it := newIntegration(t, withOutbound(""))
		h := it.h
		sipLeg := newToneSIPLeg()
		h.dialer.newLeg = func() closableLeg { return sipLeg }

		it.reg.deliver(t, ownerJoinFixture(h.clk.Now()))
		it.reg.deliver(t, ownerNotifFixture(h.clk.Now()))
		oc := h.dialed()
		h.joiner.lastSession().push(rtcOwnerJoined("EXDEVICE"))
		synctest.Wait()

		// 183 with early media: the carrier's audio reaches LiveKit, the owner's audio does not reach SIP.
		oc.provisional(183, true)
		synctest.Wait()
		pumpFrames(t, h, sipLeg, it.rtc, 50, 0, false)
		early := it.rtc.published()
		if len(early) < 40 {
			t.Fatalf("RTC leg got %d frames during early media", len(early))
		}
		if f := publishedFreq(t, early); math.Abs(f-440) > 30 {
			t.Errorf("early media tone %.0f Hz, want 440", f)
		}
		if it.rtc.receiver() != nil {
			t.Error("owner audio receiver attached before answer")
		}
		if len(sipLeg.writtenPackets()) < 40 {
			t.Errorf("SIP leg got %d packets during early media (NAT keepalive silence)", len(sipLeg.writtenPackets()))
		}
		for i, p := range sipLeg.writtenPackets() {
			for _, b := range p {
				if b != media.ULawSilence {
					t.Fatalf("SIP packet %d during early media is not silence", i)
				}
			}
		}

		// 200 OK: Connect, two-way audio.
		oc.answer()
		synctest.Wait()
		if it.rtc.receiver() == nil {
			t.Fatal("owner audio receiver not attached after answer")
		}
		nEarly, nSIP := len(it.rtc.published()), len(sipLeg.writtenPackets())
		pumpFrames(t, h, sipLeg, it.rtc, 100, 50, true)
		written := sipLeg.writtenPackets()[nSIP:]
		if len(written) < 95 {
			t.Fatalf("SIP leg got %d packets after answer", len(written))
		}
		if f := sipFreq(written); math.Abs(f-1000) > 50 {
			t.Errorf("SIP leg tone %.0f Hz, want 1000", f)
		}
		if f := publishedFreq(t, it.rtc.published()[nEarly:]); math.Abs(f-440) > 30 {
			t.Errorf("RTC leg tone %.0f Hz, want 440", f)
		}

		it.reg.deliver(t, ownerLeaveFixture(h.clk.Now()))
		if oc.hangupCount() != 1 {
			t.Fatalf("BYE not issued: %d", oc.hangupCount())
		}
		if n := h.host.noticeList(); len(n) != 1 || !strings.HasPrefix(n[0], "Outgoing call · ") {
			t.Errorf("notice %q", n)
		}
		h.assertEnded("answered", "2xx")
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
	})
}
