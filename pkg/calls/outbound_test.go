package calls

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// opsBefore asserts that op a happens before op b in the op log.
func opsBefore(t *testing.T, h *harness, a, b string) {
	t.Helper()
	ia, ib := h.ops.index(a), h.ops.index(b)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("want %q before %q; ops %v", a, b, h.ops.list())
	}
}

// startDialing runs an owner-triggered call up to Dialing (owner present in LiveKit).
func startDialing(t *testing.T, h *harness) *fakeOutCall {
	t.Helper()
	h.ownerStart("OWNERDEV")
	oc := h.dialed()
	h.ownerJoinedLK("OWNERDEV")
	return oc
}

// 1. Owner-initiated happy path with 183 early media.
func TestOutOwnerTriggerEarlyMedia(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.ownerStart("OWNERDEV")
		if got := h.host.lookupList(); len(got) != 1 || got[0] != testRoom {
			t.Fatalf("lookups %v", got)
		}
		states := h.ghost.stateCalls()
		if len(states) != 1 {
			t.Fatalf("state sends %d", len(states))
		}
		_, memberID, _ := matrixrtc.ParseMemberStateKey(states[0].key)
		device := memberID[:len(memberID)-len("_m.call")]
		want := matrixrtc.JoinContent(matrixrtc.JoinParams{
			UserID: testGhost, DeviceID: device, RoomID: testRoom, ServiceURL: testServiceURL,
			CreatedTS: testStart, Expires: matrixrtc.DefaultMembershipExpiry,
		})
		if !reflect.DeepEqual(states[0].content, want) {
			t.Errorf("membership content %v\nwant %v", states[0].content, want)
		}
		opsBefore(t, h, "publish", "join")
		opsBefore(t, h, "join", "dial")
		if h.dialer.count() != 1 {
			t.Fatalf("dials %d", h.dialer.count())
		}
		if req := h.dialer.req(0); req.Target != "+15552345678" || req.Format != sipua.FormatDigits {
			t.Errorf("dial request %+v", req)
		}
		if len(h.ghost.msgCalls()) != 0 {
			t.Error("ring sent on the owner-initiated path")
		}
		oc := h.dialed()
		h.ownerJoinedLK("OWNERDEV")

		oc.auth()
		oc.provisional(100, false)
		oc.provisional(183, true)
		synctest.Wait()
		br := h.media.bridge(0)
		if br == nil || !br.cfg.Early || br.sip != oc.leg {
			t.Fatalf("early bridge %+v", br)
		}
		if len(h.tones.patterns()) != 0 {
			t.Errorf("tone with early media: %v", h.tones.patterns())
		}
		oc.provisional(180, false) // later 18x: no tone, no second bridge
		synctest.Wait()
		if len(h.tones.patterns()) != 0 || h.media.bridge(1) != nil {
			t.Error("second 18x started a tone or a bridge")
		}
		if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "1"); !errors.Is(err, ErrNotConnected) {
			t.Errorf("DTMF while early: %v", err)
		}

		h.advance(2 * time.Second)
		oc.answer()
		synctest.Wait()
		ackAt := h.clk.Now()
		if br.connectCount() != 1 || h.media.bridge(1) != nil {
			t.Fatalf("connects %d, second bridge %v", br.connectCount(), h.media.bridge(1) != nil)
		}
		out := h.env.SpanByName("call.outbound")
		if out == nil || !out.EndTime().Equal(ackAt) {
			t.Fatalf("call.outbound end %v, want ACK %v", out, ackAt)
		}

		h.advance(80 * time.Second)
		oc.bye()
		synctest.Wait()
		if oc.hangupCount() != 0 {
			t.Error("Hangup issued after remote BYE")
		}
		states = h.ghost.stateCalls()
		if last := states[len(states)-1]; len(last.content) != 0 {
			t.Error("membership not cleared")
		}
		if s := h.joiner.lastSession(); s.leaveCount() != 1 {
			t.Errorf("session leaves %d", s.leaveCount())
		}
		assertNotices(t, h, "Outgoing call · 1m 20s")
		if by, reason := h.teardownAttrs(); by != "remote" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		if len(h.declines()) != 0 {
			t.Error("decline on an answered call")
		}
		h.assertEnded("answered", "2xx")
		if got := h.env.Sum(t, "mrtcsip.call.started", "call.direction", "outbound"); got != 1 {
			t.Errorf("call.started{outbound} %v", got)
		}
	})
}

// 1b. A 180 without SDP delivered after a 183 with early media (providers can arrive out of order) keeps
// relaying early media: no ringback tone, the early bridge is neither stopped nor replaced.
func TestOutLate180AfterEarlyMedia(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.auth()
		oc.provisional(183, true)
		synctest.Wait()
		br := h.media.bridge(0)
		if br == nil || !br.cfg.Early {
			t.Fatalf("early bridge %+v", br)
		}
		oc.provisional(180, false)
		oc.provisional(180, false)
		synctest.Wait()
		if len(h.tones.patterns()) != 0 {
			t.Errorf("ringback tone after a late 180: %v", h.tones.patterns())
		}
		if br.stopCount() != 0 || h.media.bridge(1) != nil {
			t.Errorf("early bridge stopped (%d) or replaced after a late 180", br.stopCount())
		}
	})
}

// 2. 180 without SDP: generated ringback, stopped before the bridge starts.
func TestOutRingbackTone(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.provisional(180, false)
		synctest.Wait()
		if got := h.tones.patterns(); !reflect.DeepEqual(got, []string{"ringback"}) {
			t.Fatalf("tones %v", got)
		}
		if h.media.bridge(0) != nil {
			t.Fatal("bridge started before answer without early media")
		}
		oc.answer()
		synctest.Wait()
		opsBefore(t, h, "tone-stop:ringback", "media")
		br := h.media.bridge(0)
		if br == nil || br.cfg.Early || br.sip != oc.leg {
			t.Fatalf("bridge %+v", br)
		}
		// Owner leaves while connected: BYE.
		h.matrix(h.ownerMember("OWNERDEV", false))
		if oc.hangupCount() != 1 {
			t.Errorf("hangups %d", oc.hangupCount())
		}
		if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
			t.Errorf("teardown %s/%s", by, reason)
		}
		h.assertEnded("answered", "2xx")
	})
}

// 3/4. Remote rejections, dial timeout and provider failure.
func TestOutRemoteFailures(t *testing.T) {
	cases := []struct {
		name    string
		drive   func(h *harness, oc *fakeOutCall)
		outcome string
		reason  string
		class   string
		notice  string
		tone    string
		hangup  bool
	}{
		{"486", func(h *harness, oc *fakeOutCall) { oc.reject(486, "Busy Here") },
			"busy", "remote_reject", "4xx", "Call failed: 486 Busy Here", "busy", false},
		{"600", func(h *harness, oc *fakeOutCall) { oc.reject(600, "Busy Everywhere") },
			"busy", "remote_reject", "6xx", "Call failed: 600 Busy Everywhere", "busy", false},
		{"603", func(h *harness, oc *fakeOutCall) { oc.reject(603, "Decline") },
			"declined", "remote_reject", "6xx", "Call failed: 603 Decline", "reorder", false},
		{"404", func(h *harness, oc *fakeOutCall) { oc.reject(404, "Not Found") },
			"failed", "remote_reject", "4xx", "Call failed: 404 Not Found", "reorder", false},
		{"408", func(h *harness, oc *fakeOutCall) { oc.reject(408, "Request Timeout") },
			"timeout", "remote_reject", "4xx", "Call failed: no answer (408)", "reorder", false},
		{"480", func(h *harness, oc *fakeOutCall) { oc.reject(480, "whatever the carrier says") },
			"timeout", "remote_reject", "4xx", "Call failed: 480 Temporarily Unavailable", "reorder", false},
		{"499", func(h *harness, oc *fakeOutCall) { oc.reject(499, "odd") },
			"failed", "remote_reject", "4xx", "Call failed: SIP 499", "reorder", false},
		{"dial timeout", func(h *harness, oc *fakeOutCall) { h.advance(60 * time.Second) },
			"timeout", "ring_timeout", "4xx", "Call failed: no answer", "reorder", true},
		{"provider", func(h *harness, oc *fakeOutCall) {
			oc.end(sipua.EndFailed, 0, "", errors.New("transaction timeout"))
		}, "failed", "error", "none", "Call failed: the provider did not respond", "reorder", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				h := newHarness(t, withOutbound(""))
				oc := startDialing(t, h)
				oc.provisional(180, false)
				synctest.Wait()
				h.advance(59 * time.Second)
				if oc.hangupCount() != 0 {
					t.Fatal("cancelled before the dial timeout")
				}
				tc.drive(h, oc)
				synctest.Wait()
				if got := oc.hangupCount() > 0; got != tc.hangup {
					t.Errorf("hangup issued %v, want %v", got, tc.hangup)
				}
				// The failure tone runs for 2 s before the ghost declines and leaves.
				if len(h.declines()) != 0 || h.ops.count("clear") != 0 {
					t.Fatalf("declined/cleared before the failure tone ended: %v", h.ops.list())
				}
				h.advance(failureToneLength)
				if got := h.tones.patterns(); !reflect.DeepEqual(got, []string{"ringback", tc.tone}) {
					t.Errorf("tones %v", got)
				}
				opsBefore(t, h, "tone-stop:ringback", "tone:"+tc.tone)
				opsBefore(t, h, "tone-stop:"+tc.tone, "decline")
				opsBefore(t, h, "decline", "clear")
				opsBefore(t, h, "clear", "notice")
				d := h.declines()
				if len(d) != 1 || !reflect.DeepEqual(d[0].content, matrixrtc.DeclineContent(testNotif)) {
					t.Errorf("declines %+v", d)
				}
				if s := h.joiner.lastSession(); s.leaveCount() != 1 {
					t.Errorf("session leaves %d", s.leaveCount())
				}
				assertNotices(t, h, tc.notice)
				h.assertEnded(tc.outcome, tc.class)
				if got := h.outAttr("call.end_reason"); got != tc.reason {
					t.Errorf("end_reason %q", got)
				}
			})
		})
	}
}

// 5. Owner leaves while Early: CANCEL, "Cancelled call", no tone, no decline.
func TestOutOwnerCancelsWhileEarly(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.provisional(183, true)
		synctest.Wait()
		h.matrix(h.ownerMember("OWNERDEV", false))
		if oc.hangupCount() != 1 {
			t.Fatalf("hangups %d", oc.hangupCount())
		}
		if got := h.tones.patterns(); len(got) != 0 {
			t.Errorf("tones %v", got)
		}
		if h.media.bridge(0).stopCount() != 1 {
			t.Error("early bridge not stopped")
		}
		if len(h.declines()) != 0 {
			t.Error("decline on owner cancel")
		}
		assertNotices(t, h, noticeOutCancelled)
		h.assertEnded("cancelled", "4xx")
		if got := h.outAttr("call.end_reason"); got != "cancel" {
			t.Errorf("end_reason %q", got)
		}
	})
}

// 6. CANCEL/200 race: sipua reports EndLocalCancel with status 200.
func TestOutCancelRace(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.dialer.cancelStatus = 200
		oc := startDialing(t, h)
		oc.provisional(180, false)
		synctest.Wait()
		if err := h.m.Hangup(context.Background(), testLogin, testRoom); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if oc.hangupCount() != 1 {
			t.Fatalf("hangups %d", oc.hangupCount())
		}
		assertNotices(t, h, noticeOutCancelled)
		h.assertEnded("cancelled", "2xx")
	})
}

// Owner on two devices: leaving one keeps dialing (edge case 3).
func TestOutTwoDevicesSnapshot(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.matrix(h.ownerMember("TABLET", true))
		h.advance(10 * time.Second)
		oc := startDialing(t, h)
		h.matrix(h.ownerMember("TABLET", false))
		if oc.hangupCount() != 0 {
			t.Fatal("cancelled although another owner device is in the call")
		}
		h.matrix(h.ownerMember("OWNERDEV", false))
		if oc.hangupCount() != 1 {
			t.Fatalf("hangups %d", oc.hangupCount())
		}
		h.assertEnded("cancelled", "4xx")
	})
}

// 7. LiveKit join fails twice: nothing dialled.
func TestOutJoinFails(t *testing.T) {
	run(t, func(t *testing.T) {
		e := errors.New("boom")
		h := newHarness(t, withOutbound(""), withJoinSteps(joinStep{err: e}, joinStep{err: e}))
		h.ownerStart("OWNERDEV")
		if h.dialer.count() != 0 || h.joiner.attempts() != 2 {
			t.Fatalf("dials %d attempts %d", h.dialer.count(), h.joiner.attempts())
		}
		if len(h.declines()) != 1 || h.ops.count("clear") != 1 {
			t.Errorf("ops %v", h.ops.list())
		}
		if len(h.tones.patterns()) != 0 {
			t.Error("tone without a session")
		}
		assertNotices(t, h, "Call failed: could not connect audio")
		h.assertEnded("failed", "none")
	})
}

// 8. Not registered.
func TestOutNotRegistered(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.dialer.err = sipua.ErrNotRegistered
		h.ownerStart("OWNERDEV")
		if h.dialer.count() != 1 {
			t.Fatalf("dials %d", h.dialer.count())
		}
		if len(h.declines()) != 1 || len(h.tones.patterns()) != 0 {
			t.Errorf("declines %d tones %v", len(h.declines()), h.tones.patterns())
		}
		assertNotices(t, h, "Call failed: SIP account is not registered")
		h.assertEnded("rejected", "none")
		if h.env.SpanByName("sip.invite") != nil {
			t.Error("sip.invite without an INVITE")
		}
	})
}

// 9. Owner-trigger guards: no Dial, rejected, notice and decline.
func TestOutGuards(t *testing.T) {
	cases := []struct {
		name   string
		opt    harnessOpt
		setup  func(h *harness)
		notice string
	}{
		{"self", withConfig(func(c *Config) { c.LoginNumber = func(core.LoginRef) string { return "+15552345678" } }),
			nil, noticeOutSelf},
		{"prefix", withConfig(func(c *Config) { c.DialAllowedPrefixes = []string{"+44"} }), nil, noticeOutNotAllowed},
		{"encrypted", nil, func(h *harness) { h.host.encrypted = true }, noticeOutEncrypted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				opts := []harnessOpt{withOutbound("")}
				if tc.opt != nil {
					opts = append(opts, tc.opt)
				}
				h := newHarness(t, opts...)
				if tc.setup != nil {
					tc.setup(h)
				}
				h.ownerStart("OWNERDEV")
				if h.dialer.count() != 0 || len(h.ghost.stateCalls()) != 0 || h.joiner.attempts() != 0 {
					t.Errorf("dial/membership/join on a refused call: ops %v", h.ops.list())
				}
				if len(h.declines()) != 1 {
					t.Errorf("declines %d", len(h.declines()))
				}
				assertNotices(t, h, tc.notice)
				h.assertEnded("rejected", "none")
			})
		})
	}
	t.Run("not a call room", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			other := id.RoomID("!group:example.org")
			h.matrix(memberEvent(testOwner, "D1", true, h.clk.Now(), other))
			h.matrix(notifEvent(testOwner, other, testNotif, h.clk.Now()))
			if h.dialer.count() != 0 || len(h.declines()) != 0 {
				t.Errorf("ops %v", h.ops.list())
			}
			assertNotices(t, h)
			h.assertEnded("rejected", "none")
		})
	})
}

// 10. Command path (callback ring).
func TestOutCommandPath(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			cid, e164, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "(555) 234-5678"})
			if err != nil || cid == "" || e164 != "+15552345678" {
				t.Fatalf("PlaceCall = %q %q %v", cid, e164, err)
			}
			synctest.Wait()
			if got := h.host.resolveList(); len(got) != 1 || got[0] != "+15552345678" {
				t.Fatalf("resolves %v", got)
			}
			msgs := h.ghost.msgCalls()
			if len(msgs) != 1 || msgs[0].typ != matrixrtc.RTCNotification {
				t.Fatalf("ring %+v", msgs)
			}
			if h.joiner.attempts() != 1 || h.dialer.count() != 0 {
				t.Fatalf("attempts %d dials %d", h.joiner.attempts(), h.dialer.count())
			}
			h.matrix(h.ownerMember("D1", true))
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			if reads := h.host.readList(); len(reads) != 1 || reads[0] != h.ringID() {
				t.Errorf("reads %v", reads)
			}
			h.matrix(h.ownerMember("D2", true)) // a second device does not dial again
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			h.ownerJoinedLK("D1")
			oc := h.dialed()
			oc.provisional(180, false)
			oc.answer()
			synctest.Wait()
			h.advance(5 * time.Second)
			oc.bye()
			synctest.Wait()
			if h.ops.count("redact") != 0 {
				t.Error("ring redacted on an answered call")
			}
			assertNotices(t, h, "Outgoing call · 5s")
			h.assertEnded("answered", "2xx")
			if got := h.outAttr("call.trigger"); got != "command" {
				t.Errorf("trigger %q", got)
			}
		})
	})
	t.Run("answer before join", func(t *testing.T) {
		run(t, func(t *testing.T) {
			gate := make(chan struct{})
			h := newHarness(t, withOutbound(""), withJoinSteps(joinStep{gate: gate}))
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.matrix(h.ownerMember("D1", true))
			if h.dialer.count() != 0 {
				t.Fatal("dialled before the LiveKit session was ready")
			}
			close(gate)
			synctest.Wait()
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			h.dialed().reject(486, "Busy")
			synctest.Wait()
			h.advance(failureToneLength)
			assertNotices(t, h, "Call failed: 486 Busy Here")
			if len(h.declines()) != 0 {
				t.Error("decline on the command path (no owner notification)")
			}
			h.assertEnded("busy", "4xx")
		})
	})
	t.Run("decline", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.matrix(declineEvent(testOwner, h.ringID(), testRoom))
			if h.dialer.count() != 0 || h.ops.count("redact") != 0 {
				t.Errorf("dials %d ops %v", h.dialer.count(), h.ops.list())
			}
			assertNotices(t, h, noticeOutNotPlaced)
			h.assertEnded("declined", "none")
		})
	})
	t.Run("ring timeout", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.advance(30 * time.Second)
			if h.dialer.count() != 0 || h.ops.count("redact") != 1 {
				t.Errorf("dials %d ops %v", h.dialer.count(), h.ops.list())
			}
			assertNotices(t, h, noticeOutNoAnswer)
			h.assertEnded("timeout", "none")
		})
	})
	t.Run("created room waits", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			h.host.created = true
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.advance(newRoomRingDelay - time.Millisecond)
			if len(h.ghost.stateCalls()) != 0 {
				t.Fatal("membership published before the new-room delay")
			}
			h.advance(time.Millisecond)
			if len(h.ghost.stateCalls()) != 1 || len(h.ghost.msgCalls()) != 1 {
				t.Fatalf("ops %v", h.ops.list())
			}
			if err := h.m.Hangup(context.Background(), testLogin, ""); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			assertNotices(t, h, noticeOutCancelled)
			h.assertEnded("cancelled", "none")
		})
	})
	t.Run("room form", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			cid, e164, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{RoomID: testRoom2})
			if err != nil || cid == "" || e164 != "" {
				t.Fatalf("PlaceCall = %q %q %v", cid, e164, err)
			}
			synctest.Wait()
			if got := h.host.lookupList(); len(got) != 1 || got[0] != testRoom2 || len(h.host.resolveList()) != 0 {
				t.Fatalf("lookups %v resolves %v", got, h.host.resolveList())
			}
			h.matrix(memberEvent(testOwner, "D1", true, h.clk.Now(), testRoom2))
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			if err := h.m.Hangup(context.Background(), testLogin, testRoom); !errors.Is(err, ErrNoActiveCall) {
				t.Errorf("hangup in another room: %v", err)
			}
			if err := h.m.Hangup(context.Background(), testLogin, testRoom2); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			h.assertEnded("cancelled", "4xx")
		})
	})
	t.Run("room form guard", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""), withConfig(func(c *Config) { c.DialAllowedPrefixes = []string{"+44"} }))
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{RoomID: testRoom}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if len(h.ghost.msgCalls()) != 0 || h.dialer.count() != 0 {
				t.Errorf("ops %v", h.ops.list())
			}
			assertNotices(t, h, noticeOutNotAllowed)
			h.assertEnded("rejected", "none")
		})
	})
}

// 11. PlaceCall refusals create no actor and no metrics.
func TestPlaceCallRefusals(t *testing.T) {
	cases := []struct {
		name string
		opts []harnessOpt
		busy bool
		req  PlaceCallRequest
		want error
	}{
		{"invalid", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: "hello"}, ErrInvalidNumber},
		{"emergency", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: "911"}, ErrInvalidNumber},
		{"short code", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: "611"}, ErrInvalidNumber},
		{"empty", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{}, ErrInvalidNumber},
		{"both", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: "+15552345678", RoomID: testRoom}, ErrInvalidNumber},
		{"prefix", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: "+447700900123"}, ErrNotAllowed},
		{"self", []harnessOpt{withOutbound("")}, false, PlaceCallRequest{Number: testLocal}, ErrSelfDial},
		{"busy", []harnessOpt{withOutbound("")}, true, PlaceCallRequest{Number: "+15552345678"}, ErrLineBusy},
		{"disabled", nil, false, PlaceCallRequest{Number: "+15552345678"}, ErrOutboundDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				h := newHarness(t, tc.opts...)
				var sc *fakeSIPCall
				if tc.busy {
					sc = h.invite()
				}
				before := h.callCount()
				_, _, err := h.m.PlaceCall(context.Background(), testLogin, tc.req)
				if !errors.Is(err, tc.want) {
					t.Fatalf("err %v, want %v", err, tc.want)
				}
				synctest.Wait()
				if h.callCount() != before {
					t.Error("refused PlaceCall created an actor")
				}
				if got := h.env.Sum(t, "mrtcsip.call.started", "call.direction", "outbound"); got != 0 {
					t.Errorf("call.started{outbound} %v", got)
				}
				if sc != nil {
					sc.cancel()
					synctest.Wait()
				}
			})
		})
	}
	t.Run("stopped", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			h.m.Stop(context.Background())
			if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); !errors.Is(err, ErrOutboundDisabled) {
				t.Errorf("err %v", err)
			}
		})
	})
}

// 12. Inbound INVITE while an outbound call is active: 486 and "Missed call (line busy)".
func TestInboundWhileOutboundActive(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		sc := h.invite(func(i *sipua.InviteInfo) { i.CallID = "in2"; i.FromUser = "+15559998888" })
		if !sc.has("reject:486") {
			t.Fatalf("inbound commands %v", sc.commands())
		}
		assertNotices(t, h, noticeBusy)
		if oc.hangupCount() != 0 {
			t.Error("outbound call affected")
		}
		oc.answer()
		synctest.Wait()
		oc.bye()
		synctest.Wait()
		if got := h.env.Sum(t, "mrtcsip.call.ended", "call.direction", "outbound", "call.outcome", "answered"); got != 1 {
			t.Errorf("outbound ended %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ended", "call.direction", "inbound", "call.outcome", "busy"); got != 1 {
			t.Errorf("inbound busy %v", got)
		}
	})
}

// 13. Hangup command: CANCEL in Dialing, BYE when Active; inbound ringing is declined.
func TestHangupCommand(t *testing.T) {
	t.Run("dialing", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			if err := h.m.Hangup(context.Background(), testLogin, ""); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if oc.hangupCount() != 1 {
				t.Fatalf("hangups %d", oc.hangupCount())
			}
			assertNotices(t, h, noticeOutCancelled)
			h.assertEnded("cancelled", "4xx")
			if err := h.m.Hangup(context.Background(), testLogin, ""); !errors.Is(err, ErrNoActiveCall) {
				t.Errorf("second hangup: %v", err)
			}
		})
	})
	t.Run("active", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			oc.answer()
			synctest.Wait()
			if err := h.m.Hangup(context.Background(), testLogin, testRoom); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if oc.hangupCount() != 1 {
				t.Fatalf("hangups %d", oc.hangupCount())
			}
			if by, reason := h.teardownAttrs(); by != "owner" || reason != "bye" {
				t.Errorf("teardown %s/%s", by, reason)
			}
			h.assertEnded("answered", "2xx")
		})
	})
	t.Run("inbound ringing", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			if err := h.m.Hangup(context.Background(), testLogin, testRoom); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if !sc.has("reject:603") {
				t.Fatalf("commands %v", sc.commands())
			}
			assertNotices(t, h, noticeDeclined)
			h.assertEnded("declined", "6xx")
		})
	})
	t.Run("inbound active", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			h.answer(sc, "D1")
			if err := h.m.Hangup(context.Background(), testLogin, ""); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if sc.count("hangup") != 1 {
				t.Fatalf("commands %v", sc.commands())
			}
			h.assertEnded("answered", "2xx")
		})
	})
	t.Run("none", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			if err := h.m.Hangup(context.Background(), testLogin, ""); !errors.Is(err, ErrNoActiveCall) {
				t.Errorf("err %v", err)
			}
		})
	})
}

// 14. DTMF.
func TestSendDTMF(t *testing.T) {
	t.Run("outbound", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "12"); !errors.Is(err, ErrNotConnected) {
				t.Errorf("before answer: %v", err)
			}
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "12x"); !errors.Is(err, ErrInvalidDigits) {
				t.Errorf("invalid: %v", err)
			}
			oc.answer()
			synctest.Wait()
			n, err := h.m.SendDTMF(context.Background(), testLogin, testRoom, " 9 8 7 6 # ")
			if err != nil || n != 5 {
				t.Fatalf("SendDTMF = %d, %v", n, err)
			}
			n, err = h.m.SendDTMF(context.Background(), testLogin, "", "*abcd")
			if err != nil || n != 5 {
				t.Fatalf("SendDTMF = %d, %v", n, err)
			}
			br := h.media.bridge(0)
			if got := br.dtmfList(); !reflect.DeepEqual(got, []string{"9876#", "*ABCD"}) {
				t.Errorf("bridge digits %v", got)
			}
			br.setDTMFErr(media.ErrDTMFUnsupported)
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "1"); !errors.Is(err, media.ErrDTMFUnsupported) {
				t.Errorf("unsupported: %v", err)
			}
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "!other:example.org", "1"); !errors.Is(err, ErrNoActiveCall) {
				t.Errorf("other room: %v", err)
			}
			br.setStats(media.Stats{DTMFSent: 10, LastSIPRx: h.clk.Now().Add(time.Hour)})
			oc.bye()
			synctest.Wait()
			h.assertEnded("answered", "2xx")
			if got := h.env.Sum(t, "mrtcsip.dtmf.sent", "dtmf.method", "rfc4733"); got != 10 {
				t.Errorf("dtmf.sent %v", got)
			}
		})
	})
	t.Run("inbound", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t)
			sc := h.invite()
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "1"); !errors.Is(err, ErrNotConnected) {
				t.Errorf("ringing: %v", err)
			}
			h.answer(sc, "D1")
			if n, err := h.m.SendDTMF(context.Background(), testLogin, "", "0"); err != nil || n != 1 {
				t.Fatalf("SendDTMF = %d, %v", n, err)
			}
			if got := h.media.bridge(0).dtmfList(); !reflect.DeepEqual(got, []string{"0"}) {
				t.Errorf("digits %v", got)
			}
			sc.bye()
			synctest.Wait()
		})
	})
	t.Run("validation", func(t *testing.T) {
		for in, want := range map[string]string{"1": "1", "a#*": "A#*", "1 2\t3": "123",
			"12345678901234567890123456789012": "12345678901234567890123456789012"} {
			if got, ok := normalizeDigits(in); !ok || got != want {
				t.Errorf("normalizeDigits(%q) = %q, %v", in, got, ok)
			}
		}
		for _, in := range []string{"", "   ", "e", "1-2", "123456789012345678901234567890123", "٣"} {
			if _, ok := normalizeDigits(in); ok {
				t.Errorf("normalizeDigits(%q) accepted", in)
			}
		}
	})
	t.Run("no call", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			if _, err := h.m.SendDTMF(context.Background(), testLogin, "", "1"); !errors.Is(err, ErrNoActiveCall) {
				t.Errorf("err %v", err)
			}
		})
	})
}

// 15. Shutdown: in Dialing (CANCEL) and while joining.
func TestOutShutdown(t *testing.T) {
	t.Run("dialing", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			oc.provisional(180, false)
			synctest.Wait()
			h.m.EndLogin(context.Background(), testLogin)
			if oc.hangupCount() != 1 || h.callCount() != 0 {
				t.Fatalf("hangups %d calls %d", oc.hangupCount(), h.callCount())
			}
			if got := h.tones.patterns(); !reflect.DeepEqual(got, []string{"ringback"}) || h.tones.running() != 0 {
				t.Errorf("tones %v running %d", got, h.tones.running())
			}
			assertNotices(t, h, "Call failed: the bridge stopped")
			h.assertEnded("failed", "4xx")
			if got := h.outAttr("call.end_reason"); got != "bridge_stop" {
				t.Errorf("end_reason %q", got)
			}
		})
	})
	t.Run("joining", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""), withJoinSteps(joinStep{block: true}))
			h.ownerStart("OWNERDEV")
			h.m.Stop(context.Background())
			if h.dialer.count() != 0 || h.callCount() != 0 {
				t.Fatalf("dials %d calls %d", h.dialer.count(), h.callCount())
			}
			assertNotices(t, h, noticeOutNotPlaced)
			h.assertEnded("failed", "none")
		})
	})
	t.Run("during failure tone", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			oc.reject(486, "Busy")
			synctest.Wait()
			h.m.Stop(context.Background()) // ends the 2 s tone early
			if h.callCount() != 0 {
				t.Fatal("Stop returned before the call finished")
			}
			h.assertEnded("busy", "4xx")
		})
	})
}

// 16. Every span ends within 10 minutes of fake time, even with a 300 s dial timeout and the callback ring.
func TestOutSpanDurationBound(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""), withRingTimeout(120*time.Second),
			withConfig(func(c *Config) { c.DialTimeout = 300 * time.Second }))
		if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		h.advance(119 * time.Second)
		h.matrix(h.ownerMember("D1", true))
		h.ownerJoinedLK("D1")
		oc := h.dialed()
		oc.provisional(180, false)
		synctest.Wait()
		h.advance(300 * time.Second)
		h.advance(failureToneLength)
		h.assertEnded("timeout", "4xx")
		if oc.hangupCount() != 1 {
			t.Errorf("hangups %d", oc.hangupCount())
		}
		for _, s := range h.env.Spans() {
			if d := s.EndTime().Sub(s.StartTime()); d >= 10*time.Minute || d < 0 {
				t.Errorf("span %s lasted %v", s.Name(), d)
			}
		}
	})
}

// Early bridge dies before answer: fall back to ringback.
func TestOutEarlyBridgeDies(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.provisional(183, true)
		synctest.Wait()
		h.media.bridge(0).finish(errors.New("boom"))
		synctest.Wait()
		if got := h.tones.patterns(); !reflect.DeepEqual(got, []string{"ringback"}) {
			t.Fatalf("tones %v", got)
		}
		oc.answer()
		synctest.Wait()
		if h.media.bridge(1) == nil || h.media.bridge(1).cfg.Early {
			t.Fatal("no regular bridge after answer")
		}
		oc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// LiveKit problems while dialing: owner absent (grace) and our session disconnected.
func TestOutLiveKitLossWhileDialing(t *testing.T) {
	t.Run("grace", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			h.joiner.lastSession().push(rtc.Event{Kind: rtc.EventOwnerLeft, Identity: string(testOwner) + ":OWNERDEV"})
			synctest.Wait()
			h.advance(ownerRTCGrace)
			if oc.hangupCount() != 1 {
				t.Fatalf("hangups %d", oc.hangupCount())
			}
			assertNotices(t, h, noticeOutCancelled)
			h.assertEnded("cancelled", "4xx")
			if got := h.outAttr("call.end_reason"); got != "rtc_lost" {
				t.Errorf("end_reason %q", got)
			}
		})
	})
	t.Run("disconnected", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			oc := startDialing(t, h)
			h.joiner.lastSession().push(rtc.Event{Kind: rtc.EventDisconnected, Reason: "x"})
			synctest.Wait()
			if oc.hangupCount() != 1 {
				t.Fatalf("hangups %d", oc.hangupCount())
			}
			assertNotices(t, h, "Call failed: could not connect audio")
			h.assertEnded("failed", "4xx")
		})
	})
}
