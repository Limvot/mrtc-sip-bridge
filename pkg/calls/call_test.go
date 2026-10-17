package calls

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

func run(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}

func assertNotices(t *testing.T, h *harness, want ...string) {
	t.Helper()
	got := h.host.noticeList()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("notices = %q, want %q", got, want)
	}
}

// 1. Happy path.
func TestAnsweredCallRemoteBye(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()

		if cmds := sc.commands(); len(cmds) != 1 || cmds[0] != "ringing" {
			t.Fatalf("SIP commands = %v, want [ringing]", cmds)
		}
		states := h.ghost.stateCalls()
		if len(states) != 1 {
			t.Fatalf("got %d state sends, want 1", len(states))
		}
		st := states[0]
		if st.typ != matrixrtc.CallMember {
			t.Errorf("state type = %v", st.typ)
		}
		userID, memberID, ok := matrixrtc.ParseMemberStateKey(st.key)
		device := strings.TrimSuffix(memberID, "_m.call")
		if !ok || userID != testGhost || !strings.HasPrefix(device, "SIP") || len(device) != 11 {
			t.Fatalf("state key %q (device %q)", st.key, device)
		}
		want := matrixrtc.JoinContent(matrixrtc.JoinParams{
			UserID: testGhost, DeviceID: device, RoomID: testRoom, ServiceURL: testServiceURL,
			CreatedTS: testStart, Expires: matrixrtc.DefaultMembershipExpiry,
		})
		if !reflect.DeepEqual(st.content, want) {
			t.Errorf("membership content = %v\nwant %v", st.content, want)
		}
		msgs := h.ghost.msgCalls()
		if len(msgs) != 1 || msgs[0].typ != matrixrtc.RTCNotification {
			t.Fatalf("ring messages = %+v", msgs)
		}
		if got := msgs[0].content[matrixrtc.FieldLifetime]; got != int64(30000) {
			t.Errorf("ring lifetime = %v (%T)", got, got)
		}
		rel := msgs[0].content[matrixrtc.FieldRelatesTo].(map[string]any)
		if rel[matrixrtc.FieldEventID] != "$member1" {
			t.Errorf("ring references %v", rel[matrixrtc.FieldEventID])
		}
		if h.joiner.attempts() != 1 {
			t.Fatalf("join attempts = %d", h.joiner.attempts())
		}
		jp := h.joiner.calls[0]
		if jp.Attempt != 1 {
			t.Errorf("join attempt = %d, want 1", jp.Attempt)
		}
		if jp.DeviceID != device || jp.Owner != testOwner || jp.ServiceURL != testServiceURL || jp.RoomID != testRoom {
			t.Errorf("join params %+v", jp)
		}

		sess := h.answer(sc, "OWNERDEV")
		if reads := h.host.readList(); len(reads) != 1 || reads[0] != h.ringID() {
			t.Errorf("read receipts = %v", reads)
		}
		br := h.media.bridge(0)
		if br.sip != sc.leg || br.rtc != sess.audio {
			t.Error("bridge not started with the SIP leg and the session audio")
		}
		if br.cfg.RTPTimeout != 60*time.Second || br.cfg.Clock != h.clk {
			t.Errorf("media config %+v", br.cfg)
		}

		h.advance(80 * time.Second)
		sc.bye()
		synctest.Wait()

		if sc.count("hangup") != 0 {
			t.Error("Hangup issued after remote BYE")
		}
		if br.stopCount() != 1 || sess.leaveCount() != 1 {
			t.Errorf("bridge stops = %d, session leaves = %d", br.stopCount(), sess.leaveCount())
		}
		states = h.ghost.stateCalls()
		if last := states[len(states)-1]; len(last.content) != 0 || last.key != st.key {
			t.Errorf("membership not cleared: %+v", last)
		}
		if h.ops.count("redact") != 0 {
			t.Error("ring redacted on an answered call")
		}
		assertNotices(t, h, "Call · 1m 20s")
		if by, reason := h.teardownAttrs(); by != "remote" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 2. Owner joins before LiveKit is ready.
func TestOwnerJoinsBeforeLiveKitReady(t *testing.T) {
	run(t, func(t *testing.T) {
		gate := make(chan struct{})
		h := newHarness(t, withJoinSteps(joinStep{gate: gate}))
		sc := h.invite()
		h.matrix(h.ownerMember("D1", true))
		if sc.has("answer") {
			t.Fatal("answered before the LiveKit session was ready")
		}
		close(gate)
		synctest.Wait()
		if !sc.has("answer") || h.media.bridge(0) == nil {
			t.Fatalf("not answered after join: %v", sc.commands())
		}
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// 3. Join fails once then succeeds.
func TestJoinRetrySucceeds(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withJoinSteps(joinStep{err: errors.New("boom")}))
		sc := h.invite()
		if h.joiner.attempts() != 2 {
			t.Fatalf("attempts = %d, want 2", h.joiner.attempts())
		}
		if h.joiner.calls[0].Attempt != 1 || h.joiner.calls[1].Attempt != 2 {
			t.Errorf("attempt numbers = %d, %d", h.joiner.calls[0].Attempt, h.joiner.calls[1].Attempt)
		}
		h.answer(sc, "D1")
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// 4. Join fails twice.
func TestJoinFailsTwice(t *testing.T) {
	run(t, func(t *testing.T) {
		e := errors.New("boom")
		h := newHarness(t, withJoinSteps(joinStep{err: e}, joinStep{err: e}))
		sc := h.invite()
		if h.joiner.attempts() != 2 || !sc.has("reject:480") {
			t.Fatalf("attempts %d, commands %v", h.joiner.attempts(), sc.commands())
		}
		if r, c := h.ops.index("redact"), h.ops.index("clear"); r < 0 || c < 0 || r > c {
			t.Errorf("ops %v: want redact then clear", h.ops.list())
		}
		assertNotices(t, h, noticeNoAudio)
		h.assertEnded("failed", "4xx")
	})
}

// 5. Decline from the owner.
func TestDecline(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.matrix(declineEvent(testOwner, h.ringID(), testRoom))
		if !sc.has("reject:603") {
			t.Fatalf("commands %v", sc.commands())
		}
		if h.ops.count("redact") != 0 || h.ops.count("clear") != 1 {
			t.Errorf("ops %v", h.ops.list())
		}
		if s := h.joiner.session(0); s == nil || s.leaveCount() != 1 {
			t.Error("LiveKit session not left")
		}
		if reads := h.host.readList(); len(reads) != 1 || reads[0] != h.ringID() {
			t.Errorf("read receipts = %v", reads)
		}
		assertNotices(t, h, noticeDeclined)
		h.assertEnded("declined", "6xx")
		if got := h.setupAttr("call.end_reason"); got != "decline" {
			t.Errorf("end_reason = %q", got)
		}
	})
}

// 6. Decline from another user or for another event is ignored.
func TestDeclineIgnored(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.matrix(declineEvent("@mallory:example.org", h.ringID(), testRoom))
		h.matrix(declineEvent(testOwner, "$other", testRoom))
		if sc.has("reject") {
			t.Fatalf("declined by an invalid decline: %v", sc.commands())
		}
		h.advance(30 * time.Second)
		if !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		h.assertEnded("timeout", "4xx")
	})
}

// 7. Ring timeout.
func TestRingTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.advance(29 * time.Second)
		if sc.has("reject") {
			t.Fatal("rejected before the ring timeout")
		}
		h.advance(time.Second)
		if !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		r, c := h.ops.index("redact"), h.ops.index("clear")
		if r < 0 || c < 0 || r > c {
			t.Errorf("ops %v: want redact then clear", h.ops.list())
		}
		if h.ghost.redacts[0] != h.ringID() {
			t.Errorf("redacted %v", h.ghost.redacts)
		}
		if reads := h.host.readList(); len(reads) != 1 || reads[0] != h.ringID() {
			t.Errorf("read receipts = %v", reads)
		}
		assertNotices(t, h, noticeMissed)
		h.assertEnded("timeout", "4xx")
	})
}

// 8. CANCEL while ringing.
func TestCancelWhileRinging(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		sc.cancel()
		synctest.Wait()
		if sc.has("reject") || sc.has("hangup") {
			t.Errorf("SIP command after CANCEL: %v", sc.commands())
		}
		if h.ops.count("redact") != 1 || h.ops.count("clear") != 1 {
			t.Errorf("ops %v", h.ops.list())
		}
		if s := h.joiner.session(0); s == nil || s.leaveCount() != 1 {
			t.Error("LiveKit session not left")
		}
		if reads := h.host.readList(); len(reads) != 1 || reads[0] != h.ringID() {
			t.Errorf("read receipts = %v", reads)
		}
		assertNotices(t, h, noticeMissed)
		h.assertEnded("cancelled", "4xx")
	})
}

// 9. CANCEL while answering.
func TestCancelWhileAnswering(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.newSIPCall()
		sc.autoAnswer = false
		h.m.HandleInvite(testLogin, testLocal, sc)
		synctest.Wait()
		h.matrix(h.ownerMember("D1", true))
		if !sc.has("answer") {
			t.Fatal("answer not issued")
		}
		sc.answerFailCancelled()
		synctest.Wait()
		if sc.has("hangup") || sc.has("reject") {
			t.Errorf("BYE/reject sent after CANCEL: %v", sc.commands())
		}
		assertNotices(t, h, noticeMissed)
		h.assertEnded("cancelled", "4xx")
	})
}

// 10. Busy.
func TestBusy(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc1 := h.invite()
		sc2 := h.invite(func(i *sipua.InviteInfo) { i.CallID = "second"; i.FromUser = "+15559998888" })
		if !sc2.has("reject:486") {
			t.Fatalf("second call commands %v", sc2.commands())
		}
		if cmds := sc1.commands(); len(cmds) != 1 || cmds[0] != "ringing" {
			t.Errorf("first call affected: %v", cmds)
		}
		assertNotices(t, h, noticeBusy)
		if got := h.env.Sum(t, "mrtcsip.call.ended", "call.outcome", "busy", "sip.response.status_class", "4xx"); got != 1 {
			t.Errorf("busy ended = %v", got)
		}
		if h.callCount() != 1 {
			t.Errorf("calls = %d, want 1", h.callCount())
		}
		// A Matrix event goes to call 1 (call 2 is never routable).
		h.matrix(declineEvent(testOwner, h.ringID(), testRoom))
		if !sc1.has("reject:603") {
			t.Fatalf("first call not declined: %v", sc1.commands())
		}
		synctest.Wait()
		sc3 := h.invite(func(i *sipua.InviteInfo) { i.CallID = "third" })
		if cmds := sc3.commands(); len(cmds) != 1 || cmds[0] != "ringing" {
			t.Errorf("third call commands %v", cmds)
		}
		sc3.cancel()
		synctest.Wait()
	})
}

// 11. Encrypted room.
func TestEncryptedRoom(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		h.host.encrypted = true
		sc := h.invite()
		if !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		if len(h.ghost.stateCalls()) != 0 || len(h.ghost.msgCalls()) != 0 || h.joiner.attempts() != 0 {
			t.Error("membership, ring or join on an encrypted room")
		}
		assertNotices(t, h, noticeEncrypted)
		h.assertEnded("rejected", "4xx")
		if !strings.Contains(h.logs.String(), "Refusing to ring in an encrypted room") {
			t.Error("no encrypted-room error log")
		}
	})
}

// 12. Offer without PCMU.
func TestNoPCMU(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite(func(i *sipua.InviteInfo) { i.Offer.PCMU = false; i.Offer.Codecs = []string{"G729/18"} })
		if !sc.has("reject:488") {
			t.Fatalf("commands %v", sc.commands())
		}
		assertNotices(t, h, noticeCodec)
		h.assertEnded("rejected", "4xx")
	})
}

// 13. Withheld caller.
func TestWithheldCaller(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite(func(i *sipua.InviteInfo) { i.FromUser = "anonymous"; i.AssertedUser = "" })
		if !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		if len(h.host.resolveList()) != 0 {
			t.Error("room resolved for a withheld caller")
		}
		assertNotices(t, h)
		h.assertEnded("rejected", "4xx")
		if got := h.setupAttr("call.remote.withheld"); got != "true" {
			t.Errorf("withheld attr = %q", got)
		}
	})
}

// 14. P-Asserted-Identity preferred over From.
func TestAssertedIdentityPreferred(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite(func(i *sipua.InviteInfo) { i.FromUser = "5551234567"; i.AssertedUser = "+15557654321" })
		if got := h.host.resolveList(); len(got) != 1 || got[0] != "+15557654321" {
			t.Errorf("resolved %v", got)
		}
		sc.cancel()
		h.assertEnded("cancelled", "")
	})
}

// 15. No service URL.
func TestNoServiceURL(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withServiceErr(errors.New("no rtc_foci")))
		sc := h.invite()
		if !sc.has("reject:480") || sc.has("ringing") {
			t.Fatalf("commands %v", sc.commands())
		}
		if len(h.ghost.stateCalls()) != 0 {
			t.Error("membership sent without a service URL")
		}
		assertNotices(t, h, noticeNoAudio)
		h.assertEnded("failed", "4xx")
	})
}

// 16. Owner membership leave while active.
func TestOwnerLeaveActive(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.matrix(h.ownerMember("D1", false))
		if sc.count("hangup") != 1 {
			t.Errorf("hangups = %d, want 1", sc.count("hangup"))
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 17. Owner on two devices.
func TestOwnerTwoDevices(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.matrix(h.ownerMember("D2", true))
		h.matrix(h.ownerMember("D1", false))
		if sc.has("hangup") || h.callCount() != 1 {
			t.Fatal("call ended after the first device left")
		}
		if h.env.SpanByName("call.owner.left") == nil {
			t.Error("no call.owner.left span")
		}
		h.matrix(h.ownerMember("D2", false))
		if sc.count("hangup") != 1 {
			t.Errorf("hangups = %d", sc.count("hangup"))
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 18. Owner leaves LiveKit: grace period, and rejoin within it.
func TestOwnerRTCGrace(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		s := h.answer(sc, "D1")
		ident := string(testOwner) + ":D1"
		s.push(rtc.Event{Kind: rtc.EventOwnerLeft, Identity: ident})
		synctest.Wait()
		h.advance(5 * time.Second)
		s.push(rtc.Event{Kind: rtc.EventOwnerJoined, Identity: ident})
		synctest.Wait()
		h.advance(10 * time.Second)
		if sc.has("hangup") {
			t.Fatal("call ended although the owner rejoined within the grace period")
		}
		s.push(rtc.Event{Kind: rtc.EventOwnerLeft, Identity: ident})
		synctest.Wait()
		h.advance(9 * time.Second)
		if sc.has("hangup") {
			t.Fatal("call ended before the grace period")
		}
		h.advance(time.Second)
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "rtc_lost" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 19. Our LiveKit session disconnects while active.
func TestSessionDisconnectedActive(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		s := h.answer(sc, "D1")
		s.push(rtc.Event{Kind: rtc.EventDisconnected, Reason: "SIGNAL_CLOSE"})
		synctest.Wait()
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "bridge" || reason != "rtc_lost" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 20. RTP timeout.
func TestRTPTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.media.bridge(0).finish(media.ErrRTPTimeout)
		synctest.Wait()
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "bridge" || reason != "rtp_timeout" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 21. BYE and owner leave queued together: exactly one teardown.
func TestByeAndOwnerLeaveRace(t *testing.T) {
	for i := 0; i < 10; i++ {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			h.answer(sc, "D1")
			sc.bye()
			h.ingest.HandleMatrixRTCEvent(context.Background(), testLogin, h.ownerMember("D1", false))
			synctest.Wait()
			by, _ := h.teardownAttrs()
			switch hang := sc.count("hangup"); {
			case by == "remote" && hang != 0:
				t.Errorf("BYE first but %d hangups", hang)
			case by == "owner" && hang != 1:
				t.Errorf("owner first but %d hangups", hang)
			case by != "remote" && by != "owner":
				t.Errorf("hangup.by = %q", by)
			}
			h.assertEnded("answered", "2xx")
		})
	}
}

// 22. EndLogin while ringing and while active.
func TestEndLogin(t *testing.T) {
	t.Run("ringing", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			h.m.EndLogin(context.Background(), testLogin)
			if h.callCount() != 0 {
				t.Fatal("EndLogin returned before the call finished")
			}
			if !sc.has("reject:480") || h.ops.count("redact") != 1 || h.ops.count("clear") != 1 {
				t.Errorf("commands %v ops %v", sc.commands(), h.ops.list())
			}
			h.assertEnded("failed", "4xx")
			if got := h.setupAttr("call.end_reason"); got != "bridge_stop" {
				t.Errorf("end_reason %q", got)
			}
		})
	})
	t.Run("active", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			h.answer(sc, "D1")
			h.m.EndLogin(context.Background(), testLogin)
			if h.callCount() != 0 {
				t.Fatal("EndLogin returned before the call finished")
			}
			if sc.count("hangup") != 1 {
				t.Errorf("commands %v", sc.commands())
			}
			if by, reason := h.teardownAttrs(); by != "bridge" || reason != "bridge_stop" {
				t.Errorf("teardown %s/%s", by, reason)
			}
			h.assertEnded("answered", "2xx")
		})
	})
}

// 23. Ring keepalive.
func TestRingKeepalive(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withRingTimeout(150*time.Second))
		sc := h.invite()
		h.advance(60 * time.Second)
		if n := sc.count("ringing"); n != 2 {
			t.Errorf("ringing at 60 s = %d", n)
		}
		h.advance(60 * time.Second)
		if n := sc.count("ringing"); n != 3 {
			t.Errorf("ringing at 120 s = %d", n)
		}
		h.advance(30 * time.Second)
		if n := sc.count("ringing"); n != 3 || !sc.has("reject:480") {
			t.Errorf("commands %v", sc.commands())
		}
		msgs := h.ghost.msgCalls()
		if got := msgs[0].content[matrixrtc.FieldLifetime]; got != int64(150000) {
			t.Errorf("lifetime = %v", got)
		}
		h.assertEnded("timeout", "4xx")
	})
}

// 24. Membership refresh for long calls.
func TestMembershipRefresh(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.advance(matrixrtc.DefaultMembershipExpiry - membershipRefreshLead - time.Second)
		if n := len(h.ghost.stateCalls()); n != 1 {
			t.Fatalf("refreshed early: %d state sends", n)
		}
		h.advance(time.Second)
		states := h.ghost.stateCalls()
		if len(states) != 2 {
			t.Fatalf("got %d state sends, want 2", len(states))
		}
		first, second := states[0].content, states[1].content
		if first[matrixrtc.FieldCreatedTS] != second[matrixrtc.FieldCreatedTS] {
			t.Errorf("created_ts changed: %v -> %v", first[matrixrtc.FieldCreatedTS], second[matrixrtc.FieldCreatedTS])
		}
		if got := second[matrixrtc.FieldExpires]; got != (8 * time.Hour).Milliseconds() {
			t.Errorf("expires = %v", got)
		}
		if h.env.SpanByName("call.membership.refresh") == nil {
			t.Error("no call.membership.refresh span")
		}
		sc.bye()
		synctest.Wait()
		h.assertEnded("answered", "2xx")
		for _, s := range h.env.Spans() {
			if d := s.EndTime().Sub(s.StartTime()); d >= 10*time.Minute || d < 0 {
				t.Errorf("span %s lasted %v", s.Name(), d)
			}
		}
	})
}

// 25. Matrix event for another room of the same login is not routed.
func TestOtherRoomNotRouted(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.matrix(memberEvent(testOwner, "D1", true, h.clk.Now(), "!other:example.org"))
		if sc.has("answer") {
			t.Fatal("answered by an event from another room")
		}
		spans := h.env.SpansByName("matrixrtc.ingest")
		if len(spans) != 1 || obstest.Attr(spans[0], "matrixrtc.routed_to").AsString() != "none" {
			t.Errorf("ingest spans %d, routed_to %q", len(spans), obstest.Attr(spans[0], "matrixrtc.routed_to").AsString())
		}
		sc.cancel()
		h.assertEnded("cancelled", "4xx")
	})
}

// 26. Answer timeout without join success.
func TestAnswerTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		gate := make(chan struct{})
		h := newHarness(t, withJoinSteps(joinStep{gate: gate, ignoreCtx: true}))
		sc := h.invite()
		h.matrix(h.ownerMember("D1", true))
		h.advance(answerTimeout)
		if !sc.has("hangup") {
			t.Fatalf("commands %v", sc.commands())
		}
		h.advance(joinTimeout + time.Second) // the actor gives up waiting for the stuck join
		h.assertEnded("failed", "4xx")
		close(gate) // the late session is left by the join goroutine
		synctest.Wait()
		if s := h.joiner.session(0); s == nil || s.leaveCount() != 1 {
			t.Error("late LiveKit session not left")
		}
	})
}

// 27. Owner joins then leaves before the join completes.
func TestOwnerLeavesBeforeJoin(t *testing.T) {
	run(t, func(t *testing.T) {
		gate := make(chan struct{})
		h := newHarness(t, withJoinSteps(joinStep{gate: gate}))
		sc := h.invite()
		h.matrix(h.ownerMember("D1", true))
		h.matrix(h.ownerMember("D1", false))
		if !sc.has("reject:480") || sc.has("answer") {
			t.Fatalf("commands %v", sc.commands())
		}
		assertNotices(t, h, noticeMissed)
		h.assertEnded("missed", "4xx")
	})
}

// Join attempt cancelled by the join timer counts as a failed attempt.
func TestJoinTimeoutRetries(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withJoinSteps(joinStep{block: true}))
		sc := h.invite()
		h.advance(joinTimeout)
		if h.joiner.attempts() != 2 {
			t.Fatalf("attempts = %d", h.joiner.attempts())
		}
		h.answer(sc, "D1")
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// LiveKit session drops while ringing: a new attempt replaces it.
func TestSessionDisconnectedRinging(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.joiner.session(0).push(rtc.Event{Kind: rtc.EventDisconnected, Reason: "x"})
		synctest.Wait()
		if h.joiner.attempts() != 2 || h.joiner.session(0).leaveCount() != 1 {
			t.Fatalf("attempts %d", h.joiner.attempts())
		}
		h.joiner.session(1).push(rtc.Event{Kind: rtc.EventDisconnected, Reason: "x"})
		synctest.Wait()
		if !sc.has("reject:480") {
			t.Fatalf("commands %v", sc.commands())
		}
		h.assertEnded("failed", "4xx")
	})
}

// Owner leaves after the answer was issued but before ACK: activate then tear down (owner/bye).
func TestOwnerLeavesDuringAnswer(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.newSIPCall()
		sc.autoAnswer = false
		h.m.HandleInvite(testLogin, testLocal, sc)
		synctest.Wait()
		h.matrix(h.ownerMember("D1", true))
		h.matrix(h.ownerMember("D1", false))
		if sc.has("reject") {
			t.Fatal("rejected although the answer was issued")
		}
		sc.answer()
		synctest.Wait()
		if sc.count("hangup") != 1 {
			t.Fatalf("commands %v", sc.commands())
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// Membership and ring failures go to the failure branch; the membership is cleared after a ring
// failure.
func TestSetupSendFailures(t *testing.T) {
	t.Run("membership", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			h.ghost.stateErr = errors.New("M_FORBIDDEN")
			sc := h.invite()
			if !sc.has("reject:480") || len(h.ghost.msgCalls()) != 0 || h.ops.count("clear") != 0 {
				t.Errorf("commands %v ops %v", sc.commands(), h.ops.list())
			}
			assertNotices(t, h, noticeNoAudio)
			h.assertEnded("failed", "4xx")
		})
	})
	t.Run("ring", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			h.ghost.msgErr = errors.New("M_FORBIDDEN")
			sc := h.invite()
			if !sc.has("reject:480") || h.ops.count("clear") != 1 || h.ops.count("redact") != 0 {
				t.Errorf("commands %v ops %v", sc.commands(), h.ops.list())
			}
			h.assertEnded("failed", "4xx")
		})
	})
}

// Room resolution failure and unknown owner: 480, no notice.
func TestResolveFailures(t *testing.T) {
	t.Run("resolve", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			h.host.resolveErr = errors.New("portal creation failed")
			sc := h.invite()
			if !sc.has("reject:480") {
				t.Errorf("commands %v", sc.commands())
			}
			assertNotices(t, h)
			h.assertEnded("failed", "4xx")
		})
	})
	t.Run("owner", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			h.host.owner = ""
			sc := h.invite()
			if !sc.has("reject:480") || len(h.host.resolveList()) != 0 {
				t.Errorf("commands %v", sc.commands())
			}
			h.assertEnded("failed", "4xx")
		})
	})
}

// Media stats are flushed as deltas every 5 s and at the end.
func TestStatsFlush(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		br := h.media.bridge(0)
		br.setStats(media.Stats{SIPRx: 250, SIPTx: 250, SIPLost: 2, SIPLate: 1, SIPJitter: 3 * time.Millisecond,
			ConcealedSIPToRTC: 3, ConcealedRTCToSIP: 4, LastSIPRx: h.clk.Now().Add(5 * time.Second)})
		h.advance(5 * time.Second)
		if got := h.env.Sum(t, "mrtcsip.media.rtp.packets", "media.direction", "rx"); got != 250 {
			t.Errorf("rx = %v", got)
		}
		br.setStats(media.Stats{SIPRx: 500, SIPTx: 510, SIPLost: 2, SIPLate: 1, SIPJitter: 3 * time.Millisecond,
			ConcealedSIPToRTC: 3, ConcealedRTCToSIP: 4, LastSIPRx: h.clk.Now()})
		sc.bye()
		synctest.Wait()
		checks := map[string]float64{}
		checks["rx"] = h.env.Sum(t, "mrtcsip.media.rtp.packets", "media.direction", "rx")
		checks["tx"] = h.env.Sum(t, "mrtcsip.media.rtp.packets", "media.direction", "tx")
		checks["lost"] = h.env.Sum(t, "mrtcsip.media.rtp.lost")
		checks["late"] = h.env.Sum(t, "mrtcsip.media.rtp.late")
		checks["c1"] = h.env.Sum(t, "mrtcsip.media.concealed", "media.leg", "sip_to_rtc")
		checks["c2"] = h.env.Sum(t, "mrtcsip.media.concealed", "media.leg", "rtc_to_sip")
		want := map[string]float64{"rx": 500, "tx": 510, "lost": 2, "late": 1, "c1": 3, "c2": 4}
		if !reflect.DeepEqual(checks, want) {
			t.Errorf("stats %v, want %v", checks, want)
		}
		if got := h.env.Sum(t, "mrtcsip.media.rtp.jitter"); got != 1 {
			t.Errorf("jitter observations = %v", got)
		}
		h.assertEnded("answered", "2xx")
	})
}
