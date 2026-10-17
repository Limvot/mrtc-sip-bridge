package calls

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// M1: media EOF and a remote BYE race; the BYE wins in both orders.
func TestBridgeEOFThenRemoteBye(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.media.bridge(0).finish(io.EOF)
		synctest.Wait()
		if h.callCount() != 1 {
			t.Fatal("call ended before the SIP event or the wait window")
		}
		h.advance(500 * time.Millisecond)
		sc.bye()
		synctest.Wait()
		if sc.has("hangup") {
			t.Error("Hangup issued after remote BYE")
		}
		if by, reason := h.teardownAttrs(); by != "remote" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		if td := h.env.SpanByName("call.teardown"); td == nil || !hasEvent(td, "sip.bye.received") {
			t.Error("no sip.bye.received span event")
		}
		h.assertEnded("answered", "2xx")
	})
}

func TestRemoteByeThenBridgeEOF(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		sc.bye()
		h.media.bridge(0).finish(io.EOF)
		synctest.Wait()
		if by, reason := h.teardownAttrs(); by != "remote" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

func TestBridgeEOFWithoutByeIsBridgeError(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.media.bridge(0).finish(io.EOF)
		synctest.Wait()
		h.advance(byeRaceWindow)
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "bridge" || reason != "error" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

func TestBridgeNonEOFErrorDoesNotWait(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.media.bridge(0).finish(errors.New("boom"))
		synctest.Wait() // no clock advance: must not wait
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		h.assertEnded("answered", "2xx")
	})
}

// M3a: join and leave are matched by state key even if content device_id differs.
func TestDevicesKeyedByStateKey(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		join := h.ownerMember("D1", true)
		join.Content.VeryRaw = []byte(strings.Replace(string(join.Content.VeryRaw), `"device_id":"D1"`, `"device_id":"OTHER"`, 1))
		h.matrix(join)
		s := h.joiner.lastSession()
		s.push(rtc.Event{Kind: rtc.EventOwnerJoined, Identity: string(testOwner) + ":D1"})
		synctest.Wait()
		if !sc.has("answer") {
			t.Fatalf("not answered: %v", sc.commands())
		}
		h.matrix(h.ownerMember("D1", false))
		if sc.count("hangup") != 1 {
			t.Fatalf("owner leave not matched to join: %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// M3b: while ringing, owner memberships older than ringSentAt-5s are late backlog.
func TestRingingIgnoresStaleOwnerMembership(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.advance(10 * time.Second)
		ringSent := h.clk.Now().Add(-10 * time.Second)
		h.matrix(memberEvent(testOwner, "D1", true, ringSent.Add(-6*time.Second), testRoom))
		if sc.has("answer") || len(h.host.readList()) != 0 {
			t.Fatal("stale membership answered the call")
		}
		h.matrix(memberEvent(testOwner, "D1", true, ringSent.Add(-4*time.Second), testRoom))
		if !sc.has("answer") {
			t.Fatalf("recent membership ignored: %v", sc.commands())
		}
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// M4: grace expiry logs at Warn.
func TestGraceExpiryWarns(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		s := h.answer(sc, "D1")
		s.push(rtc.Event{Kind: rtc.EventOwnerLeft, Identity: string(testOwner) + ":D1"})
		synctest.Wait()
		h.advance(ownerRTCGrace)
		var found bool
		for _, l := range strings.Split(h.logs.String(), "\n") {
			if strings.Contains(l, "Owner absent from LiveKit") && strings.Contains(l, `"level":"warn"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("no warn log for grace expiry:\n%s", h.logs.String())
		}
		h.assertEnded("answered", "2xx")
	})
}

// M5: the SIP answer is issued before the read receipt requests.
func TestAnswerBeforeReadReceipt(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		answeredAtRead := false
		h.host.readHook = func() { answeredAtRead = sc.has("answer") }
		h.answer(sc, "D1")
		if len(h.host.readList()) != 1 {
			t.Fatalf("reads = %v", h.host.readList())
		}
		if !answeredAtRead {
			t.Error("read receipt sent before the SIP answer was issued")
		}
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// L1: a join result is delivered even when the event queue is full.
func TestJoinResultSurvivesFullEventQueue(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.newSIPCall()
		c := newCall(h.m, testLogin, testLocal, sc)
		c.room = &core.CallRoom{RoomID: testRoom, Ghost: h.ghost}
		for i := 0; i < cap(c.events); i++ {
			c.events <- event{}
		}
		c.startJoin()
		synctest.Wait()
		select {
		case ev := <-c.joinResults:
			if ev.err != nil || ev.session == nil || ev.attempt != 1 {
				t.Fatalf("result %+v", ev)
			}
			if h.joiner.session(0).leaveCount() != 0 {
				t.Error("session was left although the result was delivered")
			}
			c.joinCancel()
		default:
			t.Fatal("join result dropped")
		}
	})
}

// L2: a redial during teardown is accepted (not 486).
func TestRedialDuringTeardown(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		gate := make(chan struct{})
		h.host.noticeGate = gate
		sc := h.invite()
		h.answer(sc, "D1")
		sc.bye() // teardown now blocks sending the notice
		synctest.Wait()
		if h.callCount() != 1 {
			t.Fatal("teardown not in progress")
		}
		sc2 := h.invite(func(i *sipua.InviteInfo) { i.CallID = "second" })
		if sc2.has("reject:486") || !sc2.has("ringing") {
			t.Fatalf("redial during teardown: %v", sc2.commands())
		}
		close(gate)
		synctest.Wait()
		sc2.cancel()
		synctest.Wait()
		if h.callCount() != 0 {
			t.Errorf("calls left: %d", h.callCount())
		}
	})
}

// L3: shutdown during inline setup takes the failure branch with bridge_stop.
func TestShutdownDuringSetup(t *testing.T) {
	run(t, func(t *testing.T) {
		gate := make(chan struct{})
		h := newHarness(t, func(h *harness) {
			h.cfg.ServiceURL = func(context.Context) (string, error) {
				<-gate
				return testServiceURL, nil
			}
		})
		sc := h.invite()
		done := make(chan struct{})
		go func() { h.m.EndLogin(context.Background(), testLogin); close(done) }()
		synctest.Wait()
		close(gate)
		synctest.Wait()
		<-done
		if sc.has("ringing") || !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		if h.ops.count("publish") != 0 || h.ops.count("ring") != 0 {
			t.Errorf("ops %v", h.ops.list())
		}
		if got := h.setupAttr("call.end_reason"); got != "bridge_stop" {
			t.Errorf("end_reason = %q", got)
		}
		h.assertEnded("failed", "4xx")
	})
}
