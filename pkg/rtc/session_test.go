package rtc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

type fakeGhost struct {
	err error
}

func (g *fakeGhost) UserID() id.UserID { return "@ghost:s" }
func (g *fakeGhost) SendState(context.Context, id.RoomID, event.Type, string, map[string]any) (id.EventID, error) {
	return "", nil
}
func (g *fakeGhost) SendMessage(context.Context, id.RoomID, event.Type, map[string]any) (id.EventID, error) {
	return "", nil
}
func (g *fakeGhost) Redact(context.Context, id.RoomID, id.EventID, string) error { return nil }
func (g *fakeGhost) RequestOpenIDToken(context.Context) (*core.OpenIDToken, error) {
	if g.err != nil {
		return nil, g.err
	}
	return testTok, nil
}

type fakeRoom struct {
	pubErr     error
	disconnect int
	block      chan struct{}
	mu         sync.Mutex
	present    map[string]bool
}

func (f *fakeRoom) HasParticipant(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present[id]
}

func (f *fakeRoom) Name() string                    { return "hashedroom" }
func (f *fakeRoom) LocalIdentity() string           { return "@ghost:s:DEV" }
func (f *fakeRoom) Publish(*lksdk.LocalTrack) error { return f.pubErr }
func (f *fakeRoom) ScanRemote(fn func(string) bool) {
	fn("@o:s:PHONE")
	fn("@other:s:X")
}
func (f *fakeRoom) Disconnect() {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.disconnect++
	f.mu.Unlock()
}

func testJoiner(dial dialFunc) *joiner {
	return &joiner{hc: nil, connectTimeout: time.Second, log: zerolog.Nop(), clk: clock.NewFake(time.Unix(0, 0)), dial: dial}
}

func params(g core.GhostIntent, srvURL string) JoinParams {
	return JoinParams{Ghost: g, RoomID: "!r:s", DeviceID: "DEV", ServiceURL: srvURL, Owner: "@o:s", CallID: "c1"}
}

func jwtServer(t *testing.T) string {
	srv := newTokenServer(t)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestJoinStages(t *testing.T) {
	url := jwtServer(t)
	fr := &fakeRoom{}
	var gotURL, gotJWT string
	j := testJoiner(func(u, jwt string, cb *lksdk.RoomCallback, to time.Duration) (roomConn, error) {
		gotURL, gotJWT = u, jwt
		return fr, nil
	})
	j.hc = defaultHTTPClient()
	s, err := j.Join(context.Background(), params(&fakeGhost{}, url))
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "wss://lk.test" || gotJWT != testJWT || s.Identity() != "@ghost:s:DEV" || s.LiveKitRoom() != "hashedroom" || s.ServerAddress() != "lk.test" {
		t.Fatalf("%q %q %q %q", gotURL, gotJWT, s.Identity(), s.ServerAddress())
	}
	ev := <-s.Events()
	if ev.Kind != EventOwnerJoined || ev.Identity != "@o:s:PHONE" {
		t.Fatalf("ev %+v", ev)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected %+v", ev)
	default:
	}
	if err := s.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("events not closed")
	}
	if fr.disconnect != 1 {
		t.Fatalf("disconnect=%d", fr.disconnect)
	}
}

func TestJoinErrors(t *testing.T) {
	url := jwtServer(t)
	stage := func(err error) string {
		var je *JoinError
		if !errors.As(err, &je) {
			t.Fatalf("not a JoinError: %v", err)
		}
		return je.Stage
	}
	dialOK := func(fr *fakeRoom) dialFunc {
		return func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) { return fr, nil }
	}
	j := testJoiner(dialOK(&fakeRoom{}))
	j.hc = defaultHTTPClient()

	_, err := j.Join(context.Background(), params(&fakeGhost{err: errors.New("no")}, url))
	if stage(err) != "openid_error" {
		t.Fatal(err)
	}
	_, err = j.Join(context.Background(), params(&fakeGhost{}, "http://127.0.0.1:1"))
	if stage(err) != "token_error" {
		t.Fatal(err)
	}
	j2 := testJoiner(func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) {
		return nil, errors.New("dial wss://lk.test/rtc?access_token=" + testJWT)
	})
	j2.hc = defaultHTTPClient()
	_, err = j2.Join(context.Background(), params(&fakeGhost{}, url))
	if stage(err) != "connect_error" || contains(err.Error(), testJWT) {
		t.Fatal(err)
	}
	j3 := testJoiner(func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) {
		return nil, lksdk.ErrConnectionTimeout
	})
	j3.hc = defaultHTTPClient()
	_, err = j3.Join(context.Background(), params(&fakeGhost{}, url))
	if stage(err) != "timeout" {
		t.Fatal(err)
	}
	fr := &fakeRoom{pubErr: errors.New("pub")}
	j4 := testJoiner(dialOK(fr))
	j4.hc = defaultHTTPClient()
	_, err = j4.Join(context.Background(), params(&fakeGhost{}, url))
	if stage(err) != "publish_error" || fr.disconnect != 1 {
		t.Fatal(err, fr.disconnect)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = j.Join(ctx, params(&fakeGhost{}, url))
	if stage(err) != "timeout" {
		t.Fatal(err)
	}
}

func TestLeaveBoundedByCtx(t *testing.T) {
	fr := &fakeRoom{block: make(chan struct{})}
	s := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	s.room = fr
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Leave(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("events not closed after Leave")
	}
	close(fr.block)
}

func TestEventsNeverBlockAndDrop(t *testing.T) {
	s := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	cb := s.callbacks()
	if cb.OnDisconnectedWithReason == nil {
		t.Fatal("no callback")
	}
	for i := 0; i < eventBuffer+10; i++ {
		s.emit(Event{Kind: EventOwnerJoined})
	}
	if len(s.events) != eventBuffer {
		t.Fatalf("len=%d", len(s.events))
	}
	cb.OnDisconnectedWithReason(lksdk.RoomClosed) // full: dropped, returns
	s.leaving.Store(true)
	s.closeEvents()
	s.emit(Event{}) // after close: no panic
	cb.OnDisconnectedWithReason(lksdk.Failed)
}

func TestDisconnectedEvent(t *testing.T) {
	s := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	s.callbacks().OnDisconnectedWithReason(lksdk.RoomClosed)
	ev := <-s.events
	if ev.Kind != EventDisconnected || ev.Reason != string(lksdk.RoomClosed) {
		t.Fatalf("%+v", ev)
	}
}

type chanReader struct{ c chan []byte }

func (r *chanReader) Read(b []byte) (int, error) {
	p, ok := <-r.c
	if !ok {
		return 0, errors.New("closed")
	}
	return copy(b, p), nil
}

type recv struct {
	mu   sync.Mutex
	seqs []uint16
	pl   [][]byte
}

func (r *recv) ReceiveRTP(p *media.RTPPacket) {
	r.mu.Lock()
	r.seqs = append(r.seqs, p.Seq)
	r.pl = append(r.pl, append([]byte(nil), p.Payload...))
	r.mu.Unlock()
}
func (r *recv) get() []uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint16(nil), r.seqs...)
}

func rtpBytes(t *testing.T, seq uint16, payload string) []byte {
	b, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: 960, SSRC: 7, Marker: true}, Payload: []byte(payload)}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestOwnerTrackForwardingAndSwitching(t *testing.T) {
	s := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	s.leg = &rtcLeg{s: s}
	r := &recv{}
	a, b := &chanReader{make(chan []byte, 8)}, &chanReader{make(chan []byte, 8)}

	a.c <- rtpBytes(t, 1, "dropped-no-receiver")
	s.addTrack("@o:s:D1", a)
	time.Sleep(30 * time.Millisecond)
	s.Audio().SetReceiver(r)
	a.c <- rtpBytes(t, 2, "hello")
	eventually(t, func() bool { return len(r.get()) == 1 })
	if r.get()[0] != 2 || string(r.pl[0]) != "hello" {
		t.Fatalf("%v %q", r.get(), r.pl)
	}

	// Newer track becomes current; the old one is ignored.
	s.addTrack("@o:s:D2", b)
	b.c <- rtpBytes(t, 10, "b")
	a.c <- rtpBytes(t, 3, "a-ignored")
	eventually(t, func() bool { return len(r.get()) == 2 })
	time.Sleep(30 * time.Millisecond)
	if got := r.get(); len(got) != 2 || got[1] != 10 {
		t.Fatalf("%v", got)
	}

	// Owner device D2 leaves: switch back to D1's track.
	s.removeIdentity("@o:s:D2")
	if cur := s.current.Load(); cur == nil || cur.identity != "@o:s:D1" {
		t.Fatalf("current=%+v", cur)
	}
	a.c <- rtpBytes(t, 4, "a")
	eventually(t, func() bool { return len(r.get()) == 3 })

	// Detach.
	s.Audio().SetReceiver(nil)
	a.c <- rtpBytes(t, 5, "x")
	time.Sleep(30 * time.Millisecond)
	if len(r.get()) != 3 {
		t.Fatal("received after detach")
	}
	close(a.c)
	close(b.c)
	eventually(t, func() bool { return s.current.Load() == nil })
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOwnerLeftSuppressedWhenParticipantReconnected(t *testing.T) {
	s := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	fr := &fakeRoom{present: map[string]bool{"@o:s:PHONE": true}}
	s.roomPtr.Store(&roomHolder{fr})
	// Stale Disconnected processed after the new Connected: identity is present again.
	s.onParticipantDisconnected("@o:s:PHONE")
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected %+v", ev)
	default:
	}
	// Really gone: reported.
	fr.mu.Lock()
	delete(fr.present, "@o:s:PHONE")
	fr.mu.Unlock()
	s.onParticipantDisconnected("@o:s:PHONE")
	if ev := <-s.events; ev.Kind != EventOwnerLeft || ev.Identity != "@o:s:PHONE" {
		t.Fatalf("%+v", ev)
	}
	// Before the room is known (callback during dial): reported.
	s2 := newSession(params(&fakeGhost{}, ""), zerolog.Nop(), clock.Real())
	s2.onParticipantDisconnected("@o:s:PHONE")
	if ev := <-s2.events; ev.Kind != EventOwnerLeft {
		t.Fatalf("%+v", ev)
	}
}
