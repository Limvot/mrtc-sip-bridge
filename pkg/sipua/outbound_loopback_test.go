package sipua

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago"
	dmedia "github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

const (
	loopUser   = "12345678"
	loopPass   = "secret-pass"
	loopTarget = "+15551230000"
)

// outboundPair is our UA (bound to 127.0.0.1:bindPort, registrar that never answers) plus a diago UAS on
// 127.0.0.1 whose INVITE handler each test sets.
type outboundPair struct {
	ua       *diagoUA
	bindPort int
	uasAddr  string
	handler  atomic.Pointer[func(*diago.DialogServerSession)]
	logBuf   *syncBuf
}

func (p *outboundPair) setHandler(f func(*diago.DialogServerSession)) { p.handler.Store(&f) }

func startOutboundPair(t *testing.T) *outboundPair {
	t.Helper()
	reg, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	regPort := reg.LocalAddr().(*net.UDPAddr).Port

	p := &outboundPair{logBuf: &syncBuf{}}
	sctx, scancel := context.WithCancel(context.Background())
	t.Cleanup(scancel)
	var startErr error
	for attempt := 0; attempt < 5; attempt++ {
		uaPort, uasPort := twoFreeUDPPorts(t)
		u, err := New(Config{
			Username:  loopUser,
			Password:  loopPass,
			Registrar: net.JoinHostPort("127.0.0.1", strconv.Itoa(regPort)),
			BindIP:    "127.0.0.1",
			BindPort:  uaPort,
			Log:       zerolog.New(p.logBuf).Level(zerolog.DebugLevel),
		})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			if _, err := u.Dial(DialRequest{Target: loopTarget}); err == nil || errors.Is(err, ErrNotRegistered) {
				t.Fatalf("Dial before Start: %v", err)
			}
		}
		if startErr = u.Start(context.Background()); startErr != nil {
			continue
		}
		sua, err := sipgo.NewUA(sipgo.WithUserAgent("uas"))
		if err != nil {
			t.Fatal(err)
		}
		d := diago.NewDiago(sua, diago.WithTransport(diago.Transport{
			Transport:       "udp4",
			BindHost:        "127.0.0.1",
			BindPort:        uasPort,
			MediaExternalIP: net.ParseIP("127.0.0.1"),
		}), diago.WithMediaConfig(diago.MediaConfig{Codecs: []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000}}))
		startErr = d.ServeBackground(sctx, func(ds *diago.DialogServerSession) {
			if f := p.handler.Load(); f != nil {
				(*f)(ds)
			}
		})
		if startErr != nil {
			_ = sua.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = u.Stop(ctx)
			cancel()
			continue
		}
		t.Cleanup(func() { _ = sua.Close() })
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = u.Stop(ctx)
		})
		p.ua = u.(*diagoUA)
		p.bindPort = uaPort
		p.uasAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(uasPort))
		break
	}
	if p.ua == nil {
		t.Fatalf("could not start UA/UAS after 5 attempts: %v", startErr)
	}
	return p
}

// assertOutboundLogsClean checks the privacy and structure rules for our log output.
func assertOutboundLogsClean(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{loopPass, "v=0", "m=audio", "Digest", "WWW-Authenticate", "nonce"} {
		if strings.Contains(out, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
	assertSingleComponent(t, out)
}

func nextOut(t *testing.T, c OutboundCall) CallEvent {
	t.Helper()
	select {
	case ev, ok := <-c.Events():
		if !ok {
			t.Fatal("events closed early")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return CallEvent{}
}

// nextNon100 skips 100 Trying provisionals.
func nextNon100(t *testing.T, c OutboundCall) CallEvent {
	t.Helper()
	for {
		ev := nextOut(t, c)
		if ev.Kind == EventProvisional && ev.StatusCode == 100 {
			continue
		}
		return ev
	}
}

func ulawFrame() []byte {
	b := make([]byte, media.FrameSamples)
	for i := range b {
		b[i] = 0xD5
	}
	return b
}

type uasInvite struct {
	source   string
	fromUser string
	ruriUser string
}

// legPump reads our leg continuously (counting PCMU frames) and, while writing is on, writes one PCMU
// frame every 20 ms, like the media bridge in early mode (D10). It spans the 183 -> 200 transition so the
// race detector sees the production access pattern.
type legPump struct {
	leg       media.SIPLeg
	read      atomic.Int64
	stopWrite chan struct{}
	wrote     chan struct{}
	readDone  chan struct{}
}

func startLegPump(leg media.SIPLeg) *legPump {
	p := &legPump{leg: leg, stopWrite: make(chan struct{}), wrote: make(chan struct{}), readDone: make(chan struct{})}
	go func() {
		defer close(p.readDone)
		for {
			var pkt media.RTPPacket
			if err := leg.ReadRTP(&pkt); err != nil {
				return
			}
			if pkt.PayloadType == media.PayloadPCMU && len(pkt.Payload) == media.FrameSamples {
				p.read.Add(1)
			}
		}
	}()
	go func() {
		defer close(p.wrote)
		payload := ulawFrame()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-p.stopWrite:
				return
			case <-tick.C:
				_ = leg.WriteRTP(payload, media.FrameSamples, i == 0)
			}
		}
	}()
	return p
}

// stopWriting stops the writer goroutine and waits for it (the caller may then write on the leg).
func (p *legPump) stopWriting() {
	close(p.stopWrite)
	<-p.wrote
}

func waitCount(t *testing.T, what string, c *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d packets, want %d", what, c.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// uasPump reads the UAS media through diago's DTMF reader (counting PCMU frames and decoding digits)
// and writes one PCMU frame every 20 ms until stop.
type uasPump struct {
	read  atomic.Int64
	dtmf  chan rune
	stop  chan struct{}
	wrote chan struct{}
}

func startUASPump(t *testing.T, med *diago.DialogMedia) *uasPump {
	t.Helper()
	p := &uasPump{dtmf: make(chan rune, 4), stop: make(chan struct{}), wrote: make(chan struct{})}
	dr, err := med.AudioReaderDTMF()
	if err != nil {
		t.Fatal(err)
	}
	dr.OnDTMF(func(r rune) error {
		select {
		case p.dtmf <- r:
		default:
		}
		return nil
	})
	go func() {
		buf := make([]byte, 1500)
		for {
			k, err := dr.Read(buf)
			if err != nil {
				return
			}
			if k == media.FrameSamples {
				p.read.Add(1)
			}
		}
	}()
	go func() {
		defer close(p.wrote)
		payload := ulawFrame()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				_, _ = med.RTPPacketWriter.WriteSamples(payload, media.FrameSamples, false, 0)
			}
		}
	}()
	return p
}

func (p *uasPump) stopWriting() {
	close(p.stop)
	<-p.wrote
}

// sendDigit5 sends one RFC 4733 digit ("5") the way pkg/media does (D13): 5 event packets, then 3 end
// packets, one per 20 ms, all with the digit's start timestamp.
func sendDigit5(t *testing.T, el media.SIPEventLeg) {
	t.Helper()
	ev4733 := func(end bool, dur uint16) []byte {
		b := []byte{5, 10, 0, 0}
		if end {
			b[1] |= 0x80
		}
		binary.BigEndian.PutUint16(b[2:], dur)
		return b
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 1; i <= 5; i++ {
		<-tick.C
		if err := el.WriteEvent(ev4733(false, uint16(160*i)), 0, i == 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		<-tick.C
		samples := uint32(0)
		if i == 2 {
			samples = 800
		}
		if err := el.WriteEvent(ev4733(true, 800), samples, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutboundLoopbackEarlyMedia(t *testing.T) {
	for _, tc := range []struct {
		name string
		move bool // the 200 answers from a new media session (different RTP port than the 183)
	}{{"same media", false}, {"200 moves media", true}} {
		t.Run(tc.name, func(t *testing.T) { testOutboundLoopbackEarlyMedia(t, tc.move) })
	}
}

func testOutboundLoopbackEarlyMedia(t *testing.T, move bool) {
	p := startOutboundPair(t)
	auth := diago.NewDigestServer()
	t.Cleanup(auth.Close)

	invites := make(chan uasInvite, 1)
	stopEarly := make(chan struct{})
	uasMedia := make(chan *diago.DialogMedia, 1)
	hangup := make(chan struct{})
	uasErr := make(chan error, 4)
	var earlyWritten atomic.Int64
	codecs := []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000}
	p.setHandler(func(d *diago.DialogServerSession) {
		if err := auth.AuthorizeDialog(d, diago.DigestAuth{Username: loopUser, Password: loopPass, Realm: "test"}); err != nil {
			return // 401 sent; the UAC retries with credentials
		}
		invites <- uasInvite{source: d.InviteRequest.Source(), fromUser: d.FromUser(), ruriUser: d.InviteRequest.Recipient.User}
		if err := d.Ringing(); err != nil {
			uasErr <- err
			return
		}
		med, err := d.ProgressMedia(diago.ProgressMediaOptions{Codecs: codecs})
		if err != nil {
			uasErr <- err
			return
		}
		payload := ulawFrame()
		tick := time.NewTicker(20 * time.Millisecond)
	early:
		for i := 0; ; i++ {
			select {
			case <-stopEarly:
				break early
			case <-d.Context().Done():
				tick.Stop()
				return
			case <-tick.C:
				if _, err := med.RTPPacketWriter.WriteSamples(payload, media.FrameSamples, i == 0, 0); err == nil {
					earlyWritten.Add(1)
				}
			}
		}
		tick.Stop()
		if move {
			if med, err = d.Answer(diago.AnswerOptions{Codecs: codecs}); err != nil {
				uasErr <- err
				return
			}
		} else if err := d.AnswerEarlyMedia(med, diago.AnswerOptions{}); err != nil {
			uasErr <- err
			return
		}
		uasMedia <- med
		select {
		case <-hangup:
		case <-d.Context().Done():
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Hangup(ctx); err != nil {
			uasErr <- err
		}
	})

	call, err := p.ua.dialTo(DialRequest{Target: loopTarget}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	if info := call.Info(); info.CallID == "" || info.Target != loopTarget || info.RequestUser != "15551230000" || info.StartedAt.IsZero() {
		t.Fatalf("info %+v", info)
	}

	if ev := nextNon100(t, call); ev.Kind != EventAuthChallenged || ev.StatusCode != 401 {
		t.Fatalf("want EventAuthChallenged, got %+v", ev)
	}
	// The 180 is informational and may legitimately be missing (sipgo can drop a provisional that races a
	// following response), so it is optional (and may arrive after the 183); only the early-media 183 is required.
	ev := nextNon100(t, call)
	if ev.Kind == EventProvisional && ev.StatusCode == 180 && !ev.EarlyMedia {
		ev = nextNon100(t, call)
	}
	if ev.Kind != EventProvisional || ev.StatusCode != 183 || !ev.EarlyMedia || ev.Media == nil {
		t.Fatalf("want early 183, got %+v", ev)
	}
	earlyLeg := ev.Media
	pump := startLegPump(earlyLeg)

	select {
	case inv := <-invites:
		if _, port, _ := net.SplitHostPort(inv.source); port != strconv.Itoa(p.bindPort) {
			t.Errorf("INVITE source %s, want port %d (the REGISTER socket)", inv.source, p.bindPort)
		}
		if inv.fromUser != loopUser {
			t.Errorf("From user %q", inv.fromUser)
		}
		if inv.ruriUser != "15551230000" {
			t.Errorf("Request-URI user %q", inv.ruriUser)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UAS saw no authorized INVITE")
	}

	waitCount(t, "early media at our leg", &pump.read, 3)
	close(stopEarly)

	ev = nextNon100(t, call)
	// sipgo handles each UDP datagram on its own goroutine, so a 180 can be delivered after the 183.
	if ev.Kind == EventProvisional && ev.StatusCode == 180 && !ev.EarlyMedia {
		ev = nextNon100(t, call)
	}
	if ev.Kind != EventAnswered {
		t.Fatalf("want EventAnswered, got %+v", ev)
	}
	if ev.Media != earlyLeg {
		t.Fatal("answered leg is not the early-media leg")
	}
	a := ev.Answer
	if a.Codec != "PCMU" || !a.TelephoneEvent || a.TelephoneEventPT != 101 || a.OKReceivedAt.IsZero() ||
		a.AckSentAt.IsZero() || a.AckSentAt.Before(a.OKReceivedAt) || a.LocalRTP == "" || a.RemoteRTP == "" {
		t.Fatalf("answer %+v", a)
	}

	var med *diago.DialogMedia
	select {
	case med = <-uasMedia:
	case err := <-uasErr:
		t.Fatalf("uas: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("UAS did not answer")
	}

	// RTP both ways after the answer (our writer kept running across the 200).
	uas := startUASPump(t, med)
	before := pump.read.Load()
	waitCount(t, "answered media at our leg", &pump.read, before+5)
	waitCount(t, "our media at the UAS", &uas.read, 5)
	pump.stopWriting()
	uas.stopWriting()

	// One RFC 4733 digit through the SIPEventLeg, decoded by the UAS.
	el, ok := earlyLeg.(media.SIPEventLeg)
	if !ok {
		t.Fatal("leg is not a media.SIPEventLeg")
	}
	if pt, ok := el.TelephoneEventPT(); !ok || pt != 101 {
		t.Fatalf("telephone-event PT %d %v", pt, ok)
	}
	sendDigit5(t, el)
	select {
	case r := <-uas.dtmf:
		if r != '5' {
			t.Fatalf("UAS decoded %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UAS decoded no DTMF")
	}

	close(hangup)
	ev = nextNon100(t, call)
	if ev.Kind != EventEnded || ev.Cause != EndRemoteBye || ev.StatusCode != 200 {
		t.Fatalf("want EndRemoteBye, got %+v", ev)
	}
	if _, ok := <-call.Events(); ok {
		t.Fatal("events not closed")
	}
	select {
	case <-pump.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("leg reader did not stop after the call ended")
	}
	select {
	case err := <-uasErr:
		t.Fatalf("uas: %v", err)
	default:
	}
	if earlyWritten.Load() == 0 {
		t.Fatal("UAS wrote no early media")
	}
	assertOutboundLogsClean(t, p.logBuf.String())
}

func TestOutboundLoopbackAnswerNoEarlyMedia(t *testing.T) {
	p := startOutboundPair(t)
	uasMedia := make(chan *diago.DialogMedia, 1)
	byeSeen := make(chan bool, 1)
	p.setHandler(func(d *diago.DialogServerSession) {
		_ = d.Trying()
		_ = d.Ringing()
		med, err := d.Answer(diago.AnswerOptions{Codecs: []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000}})
		if err != nil {
			return
		}
		uasMedia <- med
		<-d.Context().Done()
		byeSeen <- d.LoadState() == sip.DialogStateEnded
	})
	call, err := p.ua.dialTo(DialRequest{Target: loopTarget}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	// The 180 is optional (informational; may be dropped under load); EventAnswered is required.
	ev := nextNon100(t, call)
	if ev.Kind == EventProvisional && ev.StatusCode == 180 {
		ev = nextNon100(t, call)
	}
	if ev.Kind != EventAnswered || ev.Media == nil || ev.Answer.AckSentAt.IsZero() || !ev.Answer.TelephoneEvent {
		t.Fatalf("want EventAnswered, got %+v", ev)
	}
	var med *diago.DialogMedia
	select {
	case med = <-uasMedia:
	case <-time.After(5 * time.Second):
		t.Fatal("UAS did not answer")
	}
	pump := startLegPump(ev.Media)
	uas := startUASPump(t, med)
	waitCount(t, "media at our leg", &pump.read, 5)
	waitCount(t, "our media at the UAS", &uas.read, 5)
	pump.stopWriting()
	uas.stopWriting()

	call.Hangup()
	ev = nextNon100(t, call)
	if ev.Kind != EventEnded || ev.Cause != EndLocalHangup || ev.StatusCode != 200 {
		t.Fatalf("want EndLocalHangup, got %+v", ev)
	}
	select {
	case ok := <-byeSeen:
		if !ok {
			t.Fatal("UAS dialog not ended by BYE")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UAS saw no BYE")
	}
	assertOutboundLogsClean(t, p.logBuf.String())
}

func TestOutboundLoopbackRejected(t *testing.T) {
	p := startOutboundPair(t)
	// The fake registrar never answers, so the UA is not registered.
	if _, err := p.ua.Dial(DialRequest{Target: loopTarget}); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Dial while unregistered: %v", err)
	}
	if _, err := p.ua.Dial(DialRequest{Target: "15551230000"}); err == nil || errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Dial non-E.164: %v", err)
	}
	p.setHandler(func(d *diago.DialogServerSession) {
		_ = d.Respond(sip.StatusBusyHere, "Busy Here", nil)
	})
	call, err := p.ua.dialTo(DialRequest{Target: loopTarget, Format: FormatE164}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	if call.Info().RequestUser != loopTarget {
		t.Fatalf("e164 request user %q", call.Info().RequestUser)
	}
	ev := nextNon100(t, call)
	if ev.Kind != EventEnded || ev.Cause != EndRemoteRejected || ev.StatusCode != 486 || ev.Reason != "Busy Here" {
		t.Fatalf("want EndRemoteRejected 486, got %+v", ev)
	}
	if _, ok := <-call.Events(); ok {
		t.Fatal("events not closed")
	}
	assertOutboundLogsClean(t, p.logBuf.String())
}

func TestOutboundLoopbackCancel(t *testing.T) {
	p := startOutboundPair(t)
	cancelled := make(chan bool, 1)
	p.setHandler(func(d *diago.DialogServerSession) {
		_ = d.Ringing()
		<-d.Context().Done()
		cancelled <- errors.Is(context.Cause(d.Context()), sip.ErrTransactionCanceled)
	})
	call, err := p.ua.dialTo(DialRequest{Target: loopTarget}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextNon100(t, call); ev.Kind != EventProvisional || ev.StatusCode != 180 {
		t.Fatalf("want 180, got %+v", ev)
	}
	call.Hangup()
	ev := nextNon100(t, call)
	if ev.Kind != EventEnded || ev.Cause != EndLocalCancel || ev.StatusCode != 487 {
		t.Fatalf("want EndLocalCancel 487, got %+v", ev)
	}
	select {
	case c := <-cancelled:
		if !c {
			t.Fatal("UAS dialog ended without CANCEL")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UAS dialog not ended")
	}
	call.Hangup() // after the end: ignored, never blocks
	assertOutboundLogsClean(t, p.logBuf.String())
}

// A long ring (more 18x than sipgo's response limit) restarts WaitAnswer; a later 183 with SDP and the 200
// must still answer the call with RTP both ways. Early media after the restart is not relayed.
func TestOutboundLoopbackLongRingThenEarlyMedia(t *testing.T) {
	p := startOutboundPair(t)
	uasMedia := make(chan *diago.DialogMedia, 1)
	uasErr := make(chan error, 4)
	codecs := []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000}
	p.setHandler(func(d *diago.DialogServerSession) {
		for i := 0; i < 12; i++ {
			if err := d.Ringing(); err != nil {
				uasErr <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		med, err := d.ProgressMedia(diago.ProgressMediaOptions{Codecs: codecs})
		if err != nil {
			uasErr <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		if err := d.AnswerEarlyMedia(med, diago.AnswerOptions{}); err != nil {
			uasErr <- err
			return
		}
		uasMedia <- med
		<-d.Context().Done()
	})
	call, err := p.ua.dialTo(DialRequest{Target: loopTarget}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	var ans CallEvent
	for {
		ev := nextNon100(t, call)
		if ev.Kind == EventEnded {
			t.Fatalf("call ended: %+v", ev)
		}
		if ev.Kind == EventAnswered {
			ans = ev
			break
		}
	}
	var med *diago.DialogMedia
	select {
	case med = <-uasMedia:
	case err := <-uasErr:
		t.Fatalf("uas: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("UAS did not answer")
	}
	if ans.Media == nil || !ans.Answer.TelephoneEvent || ans.Answer.TelephoneEventPT != 101 {
		t.Fatalf("answer %+v", ans)
	}
	pump := startLegPump(ans.Media)
	uas := startUASPump(t, med)
	waitCount(t, "media at our leg", &pump.read, 5)
	waitCount(t, "our media at the UAS", &uas.read, 5)
	pump.stopWriting()
	uas.stopWriting()

	call.Hangup()
	ev := nextNon100(t, call)
	if ev.Kind != EventEnded || ev.Cause != EndLocalHangup {
		t.Fatalf("want EndLocalHangup, got %+v", ev)
	}
	assertOutboundLogsClean(t, p.logBuf.String())
}

// A 200 without SDP after early media keeps the 183's media description (telephone-event stays usable).
func TestOutboundLoopbackAnswerWithoutSDPAfterEarlyMedia(t *testing.T) {
	p := startOutboundPair(t)
	p.setHandler(func(d *diago.DialogServerSession) {
		if _, err := d.ProgressMedia(diago.ProgressMediaOptions{
			Codecs: []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000},
		}); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
		_ = d.DialogServerSession.Respond(200, "OK", nil)
		<-d.Context().Done()
	})
	call, err := p.ua.dialTo(DialRequest{Target: loopTarget}, p.uasAddr)
	if err != nil {
		t.Fatal(err)
	}
	for {
		ev := nextNon100(t, call)
		if ev.Kind == EventEnded {
			t.Fatalf("call ended: %+v", ev)
		}
		if ev.Kind == EventAnswered {
			a := ev.Answer
			if !a.TelephoneEvent || a.TelephoneEventPT != 101 || a.RemoteRTP == "" {
				t.Fatalf("answer %+v", a)
			}
			if el, ok := ev.Media.(media.SIPEventLeg); !ok {
				t.Fatal("leg is not a media.SIPEventLeg")
			} else if pt, ok := el.TelephoneEventPT(); !ok || pt != 101 {
				t.Fatalf("telephone-event PT %d %v", pt, ok)
			}
			break
		}
	}
	call.Hangup()
	if ev := nextNon100(t, call); ev.Kind != EventEnded {
		t.Fatalf("got %+v", ev)
	}
}
