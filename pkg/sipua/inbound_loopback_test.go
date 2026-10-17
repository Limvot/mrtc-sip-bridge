package sipua

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago"
	dmedia "github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// twoFreeUDPPorts opens both sockets at once (so the ports differ), then closes both.
func twoFreeUDPPorts(t *testing.T) (int, int) {
	t.Helper()
	var ports [2]int
	var cs [2]net.PacketConn
	for i := range cs {
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot bind 127.0.0.1: %v", err)
		}
		cs[i] = c
		ports[i] = c.LocalAddr().(*net.UDPAddr).Port
	}
	for _, c := range cs {
		_ = c.Close()
	}
	return ports[0], ports[1]
}

func TestInboundLoopback(t *testing.T) {
	// Fake registrar: held for the whole test and never answered, so registration fails but the socket serves.
	reg, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1: %v", err)
	}
	defer reg.Close()
	regPort := reg.LocalAddr().(*net.UDPAddr).Port

	var logBuf syncBuf
	calls := make(chan InboundCall, 1)
	var ua UA
	var uasPort int
	sctx, scancel := context.WithCancel(context.Background())
	t.Cleanup(scancel)
	var startErr error
	for attempt := 0; attempt < 5; attempt++ {
		uasPort, _ = twoFreeUDPPorts(t)
		u, err := New(Config{
			Username:  "12345678",
			Password:  "secret",
			Registrar: net.JoinHostPort("127.0.0.1", itoa(regPort)),
			BindIP:    "127.0.0.1",
			BindPort:  uasPort,
			Log:       zerolog.New(&logBuf).Level(zerolog.DebugLevel),
			OnInvite: func(c InboundCall) {
				calls <- c
				c.Ringing()
				c.Answer()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if startErr = u.Start(context.Background()); startErr != nil {
			continue
		}
		ua = u
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = ua.Stop(ctx)
		})
		break
	}
	if ua == nil {
		t.Fatalf("could not start UA after 5 attempts: %v", startErr)
	}

	var target sip.Uri
	if err := sip.ParseUri("sip:x@127.0.0.1:"+itoa(uasPort), &target); err != nil {
		t.Fatal(err)
	}
	ictx, icancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer icancel()
	// The UAC client transport binds lazily at INVITE time, so the pre-picked port can be taken by then;
	// rebuild the UAC on a fresh port and retry on bind races.
	var (
		dlg *diago.DialogClientSession
		med *diago.DialogMedia
	)
	var inviteErr error
	for attempt := 0; attempt < 5; attempt++ {
		_, uacPort := twoFreeUDPPorts(t)
		cua, err := sipgo.NewUA(sipgo.WithUserAgent("uac"))
		if err != nil {
			t.Fatal(err)
		}
		d := diago.NewDiago(cua, diago.WithTransport(diago.Transport{
			Transport:       "udp4",
			BindHost:        "127.0.0.1",
			BindPort:        uacPort,
			MediaExternalIP: net.ParseIP("127.0.0.1"),
		}), diago.WithMediaConfig(diago.MediaConfig{Codecs: []dmedia.Codec{dmedia.CodecAudioUlaw}}))
		if inviteErr = d.ServeBackground(sctx, func(*diago.DialogServerSession) {}); inviteErr == nil {
			dlg, med, inviteErr = d.Invite(ictx, target, diago.InviteOptions{Headers: []sip.Header{}})
		}
		if inviteErr == nil {
			t.Cleanup(func() { _ = cua.Close() })
			break
		}
		_ = cua.Close()
		if !strings.Contains(inviteErr.Error(), "address already in use") {
			break
		}
	}
	if inviteErr != nil {
		t.Fatalf("invite: %v", inviteErr)
	}
	t.Cleanup(func() { _ = dlg.Close() })

	var call InboundCall
	select {
	case call = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("OnInvite not called")
	}
	info := call.Info()
	if info.CallID == "" || info.ToUser != "x" || info.FromUser == "" || !info.Offer.HasSDP || !info.Offer.PCMU ||
		info.RemoteAddr == "" || info.ReceivedAt.IsZero() || info.TryingSentAt.IsZero() {
		t.Fatalf("info %+v", info)
	}

	var leg media.SIPLeg
	select {
	case ev := <-call.Events():
		if ev.Kind != EventAnswered || ev.Media == nil || ev.Answer.Codec != "PCMU" || ev.Answer.AckAt.IsZero() || ev.Answer.OKSentAt.IsZero() {
			t.Fatalf("event %+v", ev)
		}
		leg = ev.Media
	case <-time.After(10 * time.Second):
		t.Fatal("no EventAnswered")
	}

	// RTP both ways for 200 ms.
	payload := make([]byte, media.FrameSamples)
	for i := range payload {
		payload[i] = 0xD5
	}
	gotSIP := make(chan int, 1)
	gotUAC := make(chan int, 1)
	count := func(read func() (bool, error), out chan<- int) {
		n := 0
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && n < 3 {
			ok, err := read()
			if err != nil {
				break
			}
			if ok {
				n++
			}
		}
		out <- n
	}
	go count(func() (bool, error) {
		var p media.RTPPacket
		err := leg.ReadRTP(&p)
		return err == nil && p.PayloadType == media.PayloadPCMU && len(p.Payload) == media.FrameSamples, err
	}, gotSIP)
	go count(func() (bool, error) {
		buf := make([]byte, 1500)
		n, err := med.RTPPacketReader.Read(buf)
		return n == media.FrameSamples, err
	}, gotUAC)

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 0; i < 10; i++ {
		<-tick.C
		if _, err := med.RTPPacketWriter.WriteSamples(payload, media.FrameSamples, i == 0, 0); err != nil {
			t.Fatalf("uac write: %v", err)
		}
		if err := leg.WriteRTP(payload, media.FrameSamples, i == 0); err != nil {
			t.Fatalf("leg write: %v", err)
		}
	}
	for name, ch := range map[string]chan int{"sip leg": gotSIP, "uac": gotUAC} {
		select {
		case n := <-ch:
			if n < 3 {
				t.Fatalf("%s received only %d packets", name, n)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s receive timeout", name)
		}
	}

	// UAC BYE.
	hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hcancel()
	if err := dlg.Hangup(hctx); err != nil {
		t.Fatalf("uac hangup: %v", err)
	}
	select {
	case ev := <-call.Events():
		if ev.Kind != EventEnded || ev.Cause != EndRemoteBye || ev.StatusCode != 200 {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no EventEnded")
	}
	assertSingleComponent(t, logBuf.String())
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// assertSingleComponent fails if any log line carries more than one "component" key.
func assertSingleComponent(t *testing.T, out string) {
	t.Helper()
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		lines++
		if n := strings.Count(l, `"component":`); n != 1 {
			t.Errorf("%d component keys in %s", n, l)
		}
	}
	if lines == 0 {
		t.Error("no log lines captured")
	}
}
