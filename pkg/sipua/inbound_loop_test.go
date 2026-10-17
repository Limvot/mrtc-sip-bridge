package sipua

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

type fakeLeg struct{}

func (fakeLeg) ReadRTP(*media.RTPPacket) error      { return nil }
func (fakeLeg) WriteRTP([]byte, uint32, bool) error { return nil }

type fakeDialog struct {
	mu        sync.Mutex
	calls     []string
	done      chan struct{}
	doneOnce  sync.Once
	cancelled bool
	answerErr error
	answerGo  chan struct{} // if non-nil, Answer blocks until closed
	ackAt     time.Time
}

func newFakeDialog() *fakeDialog { return &fakeDialog{done: make(chan struct{})} }

func (f *fakeDialog) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeDialog) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeDialog) end(cancelled bool) {
	f.mu.Lock()
	f.cancelled = cancelled
	f.mu.Unlock()
	f.doneOnce.Do(func() { close(f.done) })
}

func (f *fakeDialog) Info() InviteInfo { return InviteInfo{CallID: "cid", FromUser: "+15551234567"} }
func (f *fakeDialog) Trying() error    { f.rec("trying"); return nil }
func (f *fakeDialog) Ringing() error   { f.rec("ringing"); return nil }
func (f *fakeDialog) Answer() (media.SIPLeg, AnswerInfo, error) {
	f.rec("answer")
	if f.answerGo != nil {
		<-f.answerGo
	}
	if f.answerErr != nil {
		return nil, AnswerInfo{}, f.answerErr
	}
	return fakeLeg{}, AnswerInfo{Codec: "PCMU", PtimeMS: 20, AckAt: f.ackAt}, nil
}
func (f *fakeDialog) Respond(code int, reason string) error {
	f.rec("respond:" + itoa(code))
	return nil
}
func (f *fakeDialog) Bye(context.Context) error { f.rec("bye"); return nil }
func (f *fakeDialog) Done() <-chan struct{}     { return f.done }
func (f *fakeDialog) Confirmed() bool           { return false }
func (f *fakeDialog) Cancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

func itoa(i int) string {
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

type inboundHarness struct {
	d      *fakeDialog
	c      *inboundCall
	clk    *clock.Fake
	served chan struct{}
	invite chan InboundCall
}

func startInbound(t *testing.T, d *fakeDialog) *inboundHarness {
	t.Helper()
	h := &inboundHarness{d: d, clk: clock.NewFake(time.Unix(1000, 0)), served: make(chan struct{}), invite: make(chan InboundCall, 1)}
	h.c = newInboundCall(d, zerolog.Nop(), h.clk)
	go func() {
		defer close(h.served)
		h.c.serve(func(c InboundCall) { h.invite <- c })
	}()
	select {
	case <-h.invite:
	case <-time.After(5 * time.Second):
		t.Fatal("OnInvite not called")
	}
	t.Cleanup(func() { d.end(false) })
	return h
}

func (h *inboundHarness) next(t *testing.T) CallEvent {
	t.Helper()
	select {
	case ev, ok := <-h.c.Events():
		if !ok {
			t.Fatal("events closed early")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return CallEvent{}
}

func (h *inboundHarness) expectClosed(t *testing.T) {
	t.Helper()
	select {
	case ev, ok := <-h.c.Events():
		if ok {
			t.Fatalf("unexpected extra event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events not closed")
	}
	select {
	case <-h.served:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

func (h *inboundHarness) expectEnded(t *testing.T, cause EndCause, status int) {
	t.Helper()
	ev := h.next(t)
	if ev.Kind != EventEnded || ev.Cause != cause || ev.StatusCode != status {
		t.Fatalf("got %+v, want EventEnded %v/%d", ev, cause, status)
	}
	h.expectClosed(t)
}

func eqCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("calls %v, want %v", got, want)
		}
	}
}

func TestInboundTryingFirstAndInfo(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	info := h.c.Info()
	if info.ReceivedAt.IsZero() || info.TryingSentAt.IsZero() || info.CallID != "cid" {
		t.Fatalf("info %+v", info)
	}
	if got := d.log(); len(got) < 1 || got[0] != "trying" {
		t.Fatalf("calls %v", got)
	}
	h.c.Reject(486, "Busy")
	h.expectEnded(t, EndRejected, 486)
}

func TestInboundAnswerThenRemoteBye(t *testing.T) {
	d := newFakeDialog()
	d.ackAt = time.Unix(1234, 0)
	h := startInbound(t, d)
	h.c.Ringing()
	h.c.Answer()
	h.c.Ringing() // after final: ignored
	ev := h.next(t)
	if ev.Kind != EventAnswered || ev.Media == nil || !ev.At.Equal(d.ackAt) || ev.Answer.Codec != "PCMU" {
		t.Fatalf("got %+v", ev)
	}
	d.end(false)
	h.expectEnded(t, EndRemoteBye, 200)
	// answer, ringing(before) only; the late ringing was ignored or processed before... check ordering
	eqCalls(t, d.log()[:3], []string{"trying", "ringing", "answer"})
	for _, c := range d.log() {
		if c == "bye" {
			t.Fatal("unexpected bye")
		}
	}
}

func TestInboundRingingAfterFinalIgnored(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Reject(603, "Decline")
	h.c.Ringing()
	h.expectEnded(t, EndRejected, 603)
	eqCalls(t, d.log(), []string{"trying", "respond:603"})
}

func TestInboundCommandsAfterEndNeverBlock(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Reject(603, "Decline")
	h.expectEnded(t, EndRejected, 603)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			h.c.Ringing()
			h.c.Answer()
			h.c.Reject(486, "x")
			h.c.Hangup()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("commands blocked after end")
	}
}

func TestInboundCancel(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Ringing()
	d.end(true)
	h.expectEnded(t, EndCancelled, 487)
}

func TestInboundDoneNotCancelledIsFailed(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	d.end(false)
	h.expectEnded(t, EndFailed, 0)
}

func TestInboundAnswerErrorCancelled(t *testing.T) {
	d := newFakeDialog()
	d.answerErr = errors.New("transaction canceled")
	d.answerGo = make(chan struct{})
	h := startInbound(t, d)
	h.c.Answer()
	d.end(true)
	close(d.answerGo)
	ev := h.next(t)
	if ev.Kind != EventAnswerFailed || ev.Err == nil {
		t.Fatalf("got %+v", ev)
	}
	h.expectEnded(t, EndCancelled, 487)
}

func TestInboundAnswerErrorNoAck(t *testing.T) {
	d := newFakeDialog()
	d.answerErr = errors.New("no ack")
	h := startInbound(t, d)
	h.c.Answer()
	if ev := h.next(t); ev.Kind != EventAnswerFailed {
		t.Fatalf("got %+v", ev)
	}
	h.expectEnded(t, EndFailed, 0)
}

func TestInboundHangupBeforeAnswer(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Hangup()
	h.expectEnded(t, EndRejected, 480)
	eqCalls(t, d.log(), []string{"trying", "respond:480"})
}

func TestInboundHangupAfterAnswerByeOnce(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Answer()
	h.c.Hangup()
	h.c.Hangup()
	if ev := h.next(t); ev.Kind != EventAnswered {
		t.Fatalf("got %+v", ev)
	}
	h.expectEnded(t, EndLocalHangup, 200)
	n := 0
	for _, c := range d.log() {
		if c == "bye" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("bye sent %d times: %v", n, d.log())
	}
}

func TestInboundRejectAfterAnswerIgnored(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Answer()
	h.c.Reject(486, "x")
	if ev := h.next(t); ev.Kind != EventAnswered {
		t.Fatalf("got %+v", ev)
	}
	d.end(false)
	h.expectEnded(t, EndRemoteBye, 200)
	for _, c := range d.log() {
		if c == "respond:486" {
			t.Fatal("reject after answer was sent")
		}
	}
}

func TestInboundSafetyTimeout(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	if !h.clk.WaitForWaiters(1) {
		t.Fatal("safety timer not armed")
	}
	h.clk.Advance(inboundSafetyTimeout)
	h.expectEnded(t, EndFailed, 480)
	eqCalls(t, d.log(), []string{"trying", "respond:480"})
}

func TestInboundSafetyTimerDisabledAfterAnswer(t *testing.T) {
	d := newFakeDialog()
	h := startInbound(t, d)
	h.c.Answer()
	if ev := h.next(t); ev.Kind != EventAnswered {
		t.Fatalf("got %+v", ev)
	}
	h.clk.Advance(2 * inboundSafetyTimeout)
	select {
	case <-h.served:
		t.Fatal("call ended by safety timer after answer")
	case <-time.After(50 * time.Millisecond):
	}
	d.end(false)
	h.expectEnded(t, EndRemoteBye, 200)
}
