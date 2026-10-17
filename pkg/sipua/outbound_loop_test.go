package sipua

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// outLeg is a distinguishable SIP leg for identity checks.
type outLeg struct{ id int }

func (*outLeg) ReadRTP(*media.RTPPacket) error      { return nil }
func (*outLeg) WriteRTP([]byte, uint32, bool) error { return nil }

// step is one scripted chunk of an Invite/WaitAnswer call: report resp, then either keep waiting (cont)
// or return err.
type step struct {
	resp []int
	cont bool
	err  error
}

type fakeClient struct {
	script chan step
	done   chan struct{}
	clk    *clock.Fake

	// onCancel is what happens when the invite context is cancelled: responses reported (the final one is
	// stored for FinalStatus) and the error returned. Default: 487, context.Canceled.
	cancelResp []int
	cancelErr  error
	// ignoreCancel makes Invite/WaitAnswer wait only on the script (to force a 200 after Hangup).
	ignoreCancel bool
	ackErr       error

	leg *outLeg

	mu       sync.Mutex
	calls    []string
	final    int
	reason   string
	doneOnce sync.Once
}

func newFakeClient(clk *clock.Fake) *fakeClient {
	return &fakeClient{
		script:     make(chan step, 64),
		done:       make(chan struct{}),
		clk:        clk,
		cancelResp: []int{487},
		leg:        &outLeg{id: 1},
	}
}

func (f *fakeClient) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeClient) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeClient) count(s string) int {
	n := 0
	for _, c := range f.log() {
		if c == s {
			n++
		}
	}
	return n
}

func (f *fakeClient) report(resp []int, onResponse func(int)) {
	for _, r := range resp {
		if r >= 200 {
			f.mu.Lock()
			f.final, f.reason = r, fmt.Sprintf("reason %d", r)
			f.mu.Unlock()
		}
		onResponse(r)
	}
}

func (f *fakeClient) wait(ctx context.Context, onResponse func(int)) error {
	cdone := ctx.Done()
	var ignored <-chan struct{}
	if f.ignoreCancel {
		cdone, ignored = nil, ctx.Done()
	}
	for {
		select {
		case <-ignored:
			f.rec("ctxdone")
			ignored = nil
		case s := <-f.script:
			f.report(s.resp, onResponse)
			if s.cont {
				continue
			}
			return s.err
		case <-cdone:
			f.rec("cancelled")
			f.report(f.cancelResp, onResponse)
			if f.cancelErr != nil {
				return f.cancelErr
			}
			return fmt.Errorf("wait: %w", context.Canceled)
		}
	}
}

func (f *fakeClient) Info() DialInfo {
	return DialInfo{CallID: "cid", Target: "+15551230000", RequestUser: "15551230000"}
}
func (f *fakeClient) Invite(ctx context.Context, onResponse func(int)) error {
	f.rec("invite")
	return f.wait(ctx, onResponse)
}
func (f *fakeClient) WaitAnswer(ctx context.Context, onResponse func(int)) error {
	f.rec("wait")
	return f.wait(ctx, onResponse)
}
func (f *fakeClient) Leg() media.SIPLeg { return f.leg }
func (f *fakeClient) Answer() AnswerInfo {
	return AnswerInfo{Codec: "PCMU", PtimeMS: 20, TelephoneEvent: true, TelephoneEventPT: 101}
}
func (f *fakeClient) FinalStatus() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.final, f.reason
}
func (f *fakeClient) Ack(context.Context) error { f.rec("ack"); return f.ackErr }
func (f *fakeClient) Bye(context.Context) error { f.rec("bye"); return nil }
func (f *fakeClient) Done() <-chan struct{}     { return f.done }
func (f *fakeClient) Close()                    { f.rec("close") }
func (f *fakeClient) remoteBye()                { f.doneOnce.Do(func() { close(f.done) }) }

type outHarness struct {
	f   *fakeClient
	c   *outboundCall
	clk *clock.Fake
	ran chan struct{}
}

func startOutbound(t *testing.T, req DialRequest, setup func(*fakeClient)) *outHarness {
	t.Helper()
	clk := clock.NewFake(time.Unix(1000, 0))
	f := newFakeClient(clk)
	if setup != nil {
		setup(f)
	}
	if req.Target == "" {
		req.Target = "+15551230000"
	}
	h := &outHarness{f: f, clk: clk, ran: make(chan struct{})}
	h.c = newOutboundCall(f, zerolog.Nop(), clk, req)
	go func() {
		defer close(h.ran)
		h.c.run()
	}()
	t.Cleanup(func() {
		f.remoteBye()
		select {
		case <-h.ran:
		case <-time.After(5 * time.Second):
			t.Error("run did not return")
		}
	})
	return h
}

func (h *outHarness) next(t *testing.T) CallEvent {
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

// ended expects EventEnded next, then a closed channel.
func (h *outHarness) ended(t *testing.T, cause EndCause, status int) CallEvent {
	t.Helper()
	ev := h.next(t)
	if ev.Kind != EventEnded || ev.Cause != cause || ev.StatusCode != status {
		t.Fatalf("got %+v, want EventEnded %v %d", ev, cause, status)
	}
	select {
	case _, ok := <-h.c.Events():
		if ok {
			t.Fatal("event after EventEnded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events not closed")
	}
	<-h.ran
	if h.f.count("close") != 1 {
		t.Fatalf("close count %d: %v", h.f.count("close"), h.f.log())
	}
	return ev
}

func expectKind(t *testing.T, ev CallEvent, k CallEventKind, status int) {
	t.Helper()
	if ev.Kind != k || ev.StatusCode != status {
		t.Fatalf("got %+v, want kind %d status %d", ev, k, status)
	}
}

func TestOutboundProvisionalThenAnswered(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{resp: []int{100, 180}, cont: true}
	expectKind(t, h.next(t), EventProvisional, 100)
	expectKind(t, h.next(t), EventProvisional, 180)
	h.f.script <- step{resp: []int{200}}
	ev := h.next(t)
	expectKind(t, ev, EventAnswered, 0)
	if !slices.Contains(h.f.log(), "ack") {
		t.Fatalf("EventAnswered before ACK: %v", h.f.log())
	}
	if ev.Media != media.SIPLeg(h.f.leg) || ev.Answer.Codec != "PCMU" || !ev.Answer.TelephoneEvent ||
		ev.Answer.TelephoneEventPT != 101 || ev.Answer.OKReceivedAt.IsZero() || ev.Answer.AckSentAt.IsZero() {
		t.Fatalf("answer %+v", ev)
	}
	if info := h.c.Info(); info.CallID != "cid" || info.RequestUser != "15551230000" || info.StartedAt.IsZero() {
		t.Fatalf("info %+v", info)
	}
	h.f.remoteBye()
	h.ended(t, EndRemoteBye, 200)
	if n := h.f.count("bye"); n != 0 {
		t.Fatalf("bye sent on remote BYE: %v", h.f.log())
	}
}

func TestOutboundAuthChallengedOnce(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{resp: []int{401, 100, 407, 180}, cont: true}
	expectKind(t, h.next(t), EventAuthChallenged, 401)
	expectKind(t, h.next(t), EventProvisional, 100)
	expectKind(t, h.next(t), EventProvisional, 180)
	h.f.script <- step{resp: []int{486}, err: &finalError{Status: 486, Reason: "Busy Here"}}
	ev := h.ended(t, EndRemoteRejected, 486)
	if ev.Reason != "Busy Here" || ev.Err != nil {
		t.Fatalf("%+v", ev)
	}
}

func TestOutboundEarlyMediaThenAnswer(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{resp: []int{100}, err: errEarlyMedia}
	expectKind(t, h.next(t), EventProvisional, 100)
	ev := h.next(t)
	expectKind(t, ev, EventProvisional, 183)
	if !ev.EarlyMedia || ev.Media != media.SIPLeg(h.f.leg) {
		t.Fatalf("early %+v", ev)
	}
	h.f.script <- step{resp: []int{183, 200}}
	expectKind(t, h.next(t), EventProvisional, 183) // a later 183 is a plain provisional
	ev = h.next(t)
	expectKind(t, ev, EventAnswered, 0)
	if ev.Media != media.SIPLeg(h.f.leg) {
		t.Fatal("answered leg differs from early leg")
	}
	if got := h.f.log(); !slices.Equal(got, []string{"invite", "wait", "ack"}) {
		t.Fatalf("calls %v", got)
	}
	h.c.Hangup()
	h.c.Hangup()
	h.ended(t, EndLocalHangup, 200)
	if n := h.f.count("bye"); n != 1 {
		t.Fatalf("bye count %d", n)
	}
}

func TestOutboundRemoteRejections(t *testing.T) {
	for _, code := range []int{404, 486, 603} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := startOutbound(t, DialRequest{}, nil)
			h.f.script <- step{resp: []int{100, code}, err: &finalError{Status: code, Reason: "r"}}
			expectKind(t, h.next(t), EventProvisional, 100)
			ev := h.ended(t, EndRemoteRejected, code)
			if ev.Reason != "r" {
				t.Fatalf("%+v", ev)
			}
			if h.f.count("ack") != 0 || h.f.count("bye") != 0 {
				t.Fatalf("calls %v", h.f.log())
			}
		})
	}
}

func TestOutboundLocalCancel(t *testing.T) {
	t.Run("before provisional", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, nil)
		h.c.Hangup()
		h.ended(t, EndLocalCancel, 487)
		if h.f.count("cancelled") != 1 || h.f.count("ack") != 0 || h.f.count("bye") != 0 {
			t.Fatalf("calls %v", h.f.log())
		}
	})
	t.Run("after 180", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, nil)
		h.f.script <- step{resp: []int{180}, cont: true}
		expectKind(t, h.next(t), EventProvisional, 180)
		h.c.Hangup()
		h.ended(t, EndLocalCancel, 487)
		if h.f.count("cancelled") != 1 || h.f.count("ack") != 0 || h.f.count("bye") != 0 {
			t.Fatalf("calls %v", h.f.log())
		}
	})
	t.Run("during early media", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, nil)
		h.f.script <- step{err: errEarlyMedia}
		expectKind(t, h.next(t), EventProvisional, 183)
		h.c.Hangup()
		h.ended(t, EndLocalCancel, 487)
	})
	t.Run("rejected while cancelling", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.ignoreCancel = true })
		h.c.Hangup()
		h.f.script <- step{resp: []int{486}, err: &finalError{Status: 486, Reason: "Busy"}}
		h.ended(t, EndRemoteRejected, 486)
	})
}

func TestOutboundCancelRace200(t *testing.T) {
	t.Run("200 during CANCEL", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.cancelResp = []int{200} })
		h.f.script <- step{resp: []int{180}, cont: true}
		expectKind(t, h.next(t), EventProvisional, 180)
		h.c.Hangup()
		h.ended(t, EndLocalCancel, 200)
		if got := h.f.log(); !slices.Equal(got, []string{"invite", "cancelled", "ack", "bye", "close"}) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("non-provisional during CANCEL", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, func(f *fakeClient) {
			f.cancelResp = []int{200}
			f.cancelErr = errors.New("non provisional response received during CANCEL")
		})
		h.c.Hangup()
		h.ended(t, EndLocalCancel, 200)
		if h.f.count("ack") != 1 || h.f.count("bye") != 1 {
			t.Fatalf("calls %v", h.f.log())
		}
	})
	t.Run("200 returned after Hangup", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.ignoreCancel = true })
		h.f.script <- step{resp: []int{180}, cont: true}
		expectKind(t, h.next(t), EventProvisional, 180)
		h.c.Hangup()
		waitFor(t, func() bool { return h.f.count("ctxdone") == 1 })
		h.f.script <- step{resp: []int{200}}
		h.ended(t, EndLocalCancel, 200)
		if got := h.f.log(); !slices.Equal(got, []string{"invite", "ctxdone", "ack", "bye", "close"}) {
			t.Fatalf("calls %v", got)
		}
	})
}

func TestOutboundTooManyResponsesReentersWaitAnswer(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{err: errors.New("more than 10 responses received")}
	h.f.script <- step{err: errors.New("more than 10 responses received")}
	h.f.script <- step{resp: []int{200}}
	expectKind(t, h.next(t), EventAnswered, 0)
	if got := h.f.log(); !slices.Equal(got, []string{"invite", "wait", "wait", "ack"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestOutboundTooManyReentriesGivesUp(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	for i := 0; i <= maxWaitAnswerReentries; i++ {
		h.f.script <- step{err: errors.New("more than 10 responses received")}
	}
	h.ended(t, EndFailed, 487)
	// invite + 10 re-entries + the CANCEL attempt
	if n := h.f.count("wait"); n != maxWaitAnswerReentries+1 || h.f.count("cancelled") != 1 {
		t.Fatalf("calls %v", h.f.log())
	}
}

func TestOutboundSafetyTimeout(t *testing.T) {
	h := startOutbound(t, DialRequest{Timeout: time.Minute}, nil)
	h.f.script <- step{resp: []int{180}, cont: true}
	expectKind(t, h.next(t), EventProvisional, 180)
	if !h.clk.WaitForWaiters(1) {
		t.Fatal("no safety timer")
	}
	h.clk.Advance(time.Minute)
	ev := h.ended(t, EndFailed, 487)
	if ev.Err == nil {
		t.Fatalf("%+v", ev)
	}
	if h.f.count("cancelled") != 1 {
		t.Fatalf("calls %v", h.f.log())
	}
}

func TestOutboundDefaultSafetyTimeout(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	if !h.clk.WaitForWaiters(1) {
		t.Fatal("no safety timer")
	}
	h.clk.Advance(outboundDefaultTimeout - time.Second)
	select {
	case ev := <-h.c.Events():
		t.Fatalf("early event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	h.clk.Advance(time.Second)
	h.ended(t, EndFailed, 487)
}

func TestOutboundAckFailure(t *testing.T) {
	h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.ackErr = errors.New("write failed") })
	h.f.script <- step{resp: []int{200}}
	ev := h.ended(t, EndFailed, 200)
	if ev.Err == nil || h.f.count("bye") != 1 {
		t.Fatalf("%+v %v", ev, h.f.log())
	}
}

func TestOutboundTransportErrorAfter200(t *testing.T) {
	// e.g. a 200 without SDP: diago returns an error without ACKing; the loop must ACK and BYE it.
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{resp: []int{200}, err: errors.New("no SDP in response")}
	h.ended(t, EndFailed, 200)
	if h.f.count("ack") != 1 || h.f.count("bye") != 1 {
		t.Fatalf("calls %v", h.f.log())
	}
}

func TestOutboundHangupAfterEndNeverBlocks(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	h.f.script <- step{err: &finalError{Status: 404}}
	h.ended(t, EndRemoteRejected, 404)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.c.Hangup()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hangup blocked")
	}
}

func TestOutboundProvisionalFloodNeverBlocks(t *testing.T) {
	h := startOutbound(t, DialRequest{}, nil)
	flood := make([]int, 500)
	for i := range flood {
		flood[i] = 180
	}
	h.f.script <- step{resp: flood, cont: true}
	h.f.script <- step{resp: []int{401}, cont: true}
	h.f.script <- step{err: errEarlyMedia}
	h.f.script <- step{resp: flood, cont: true}
	h.f.script <- step{resp: []int{200}}
	// Nobody reads until the answer was processed.
	waitFor(t, func() bool { return h.f.count("ack") == 1 })
	h.f.remoteBye()
	<-h.ran
	var evs []CallEvent
	for ev := range h.c.Events() {
		evs = append(evs, ev)
	}
	if len(evs) > outboundEventBuffer {
		t.Fatalf("%d events", len(evs))
	}
	n := len(evs)
	if n < 2 || evs[n-2].Kind != EventAnswered || evs[n-1].Kind != EventEnded || evs[n-1].Cause != EndRemoteBye {
		t.Fatalf("tail %+v", evs[max(0, n-2):])
	}
}

func TestOutboundHangupOnlyOneCancel(t *testing.T) {
	h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.ignoreCancel = true })
	for i := 0; i < 10; i++ {
		h.c.Hangup()
	}
	waitFor(t, func() bool { return h.f.count("ctxdone") == 1 })
	h.f.script <- step{resp: []int{487}, err: &finalError{Status: 487}}
	h.ended(t, EndLocalCancel, 487)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOutboundCancelFailedEndsFailed(t *testing.T) {
	// CANCEL got a non-200 (or was sent before a provisional on a re-sent INVITE): no final status, error is
	// not context.Canceled, so the callee may still be ringing.
	h := startOutbound(t, DialRequest{}, func(f *fakeClient) {
		f.cancelResp = nil
		f.cancelErr = errors.New("cancel failed with non 200")
	})
	h.c.Hangup()
	ev := h.ended(t, EndFailed, 0)
	if ev.Err == nil {
		t.Fatalf("%+v", ev)
	}
}

func TestOutboundLocalFailureSendsCancel(t *testing.T) {
	t.Run("local error before a final response", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, func(f *fakeClient) { f.cancelResp = nil })
		h.f.script <- step{resp: []int{180}, err: errors.New("apply 183 SDP")}
		expectKind(t, h.next(t), EventProvisional, 180)
		ev := h.ended(t, EndFailed, 0)
		if ev.Err == nil {
			t.Fatalf("%+v", ev)
		}
		if got := h.f.log(); !slices.Equal(got, []string{"invite", "wait", "cancelled", "close"}) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("final response needs no CANCEL", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, nil)
		h.f.script <- step{resp: []int{486}, err: &finalError{Status: 486}}
		h.ended(t, EndRemoteRejected, 486)
		if h.f.count("wait") != 0 {
			t.Fatalf("calls %v", h.f.log())
		}
	})
	t.Run("2xx then local error needs no CANCEL", func(t *testing.T) {
		h := startOutbound(t, DialRequest{}, nil)
		h.f.script <- step{resp: []int{200}, err: errors.New("no SDP in response")}
		h.ended(t, EndFailed, 200)
		if h.f.count("wait") != 0 {
			t.Fatalf("calls %v", h.f.log())
		}
	})
}
