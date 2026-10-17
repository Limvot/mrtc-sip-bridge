package calls

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
)

// detectCase feeds events into a harness and expects a number of Dial calls / outbound actors.
func TestDetectorRingMode(t *testing.T) {
	type step struct {
		advance time.Duration
		evt     func(h *harness) *mevent.Event
	}
	member := func(device string) func(h *harness) *mevent.Event {
		return func(h *harness) *mevent.Event { return h.ownerMember(device, true) }
	}
	notif := func(age time.Duration) func(h *harness) *mevent.Event {
		return func(h *harness) *mevent.Event {
			return notifEvent(testOwner, testRoom, testNotif, h.clk.Now().Add(-age))
		}
	}
	cases := []struct {
		name    string
		steps   []step
		trigger bool
	}{
		{"membership then notification", []step{{0, member("D1")}, {time.Second, notif(0)}}, true},
		{"notification then membership", []step{{0, notif(0)}, {4 * time.Second, member("D1")}}, true},
		{"notification then membership too late", []step{{0, notif(0)}, {6 * time.Second, member("D1")}}, false},
		{"stale notification", []step{{0, member("D1")}, {0, notif(31 * time.Second)}}, false},
		{"notification from the future", []step{{0, member("D1")}, {0, notif(-20 * time.Second)}}, true},
		{"replayed membership", []step{{0, func(h *harness) *mevent.Event {
			old := h.clk.Now().Add(-2 * time.Minute)
			return memberAt(testOwner, "D1", testRoom, old, old)
		}}, {0, notif(0)}}, false},
		{"re-published old membership", []step{{0, func(h *harness) *mevent.Event {
			return memberAt(testOwner, "D1", testRoom, h.clk.Now().Add(-2*time.Minute), h.clk.Now())
		}}, {0, notif(0)}}, false},
		{"membership created too long before the notification", []step{{0, member("D1")}, {61 * time.Second, notif(0)}}, false},
		{"notification from another user", []step{{0, member("D1")}, {0, func(h *harness) *mevent.Event {
			return notifEvent("@mallory:example.org", testRoom, testNotif, h.clk.Now())
		}}}, false},
		{"ghost membership and notification", []step{
			{0, func(h *harness) *mevent.Event { return memberEvent(testGhost, "SIPX", true, h.clk.Now(), testRoom) }},
			{0, func(h *harness) *mevent.Event { return notifEvent(testGhost, testRoom, testNotif, h.clk.Now()) }},
		}, false},
		{"second device joins without notification", []step{{0, member("D1")}, {10 * time.Second, member("D2")}}, false},
		{"membership only", []step{{0, member("D1")}}, false},
		{"left before the notification", []step{{0, member("D1")}, {0, func(h *harness) *mevent.Event {
			return h.ownerMember("D1", false)
		}}, {0, notif(0)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				h := newHarness(t, withOutbound(""))
				for _, s := range tc.steps {
					if s.advance > 0 {
						h.advance(s.advance)
					}
					h.matrix(s.evt(h))
				}
				got := h.dialer.count() == 1
				if got != tc.trigger {
					t.Fatalf("triggered %v, want %v (ops %v)", got, tc.trigger, h.ops.list())
				}
				if !tc.trigger {
					if h.callCount() != 0 || h.env.SpanByName("call.outbound") != nil {
						t.Error("actor created without a trigger")
					}
					return
				}
				h.dialed().reject(404, "Not Found")
				synctest.Wait()
				h.advance(failureToneLength)
				if d := h.declines(); len(d) != 1 {
					t.Errorf("declines %d", len(d))
				}
				h.assertEnded("failed", "4xx")
				if got := h.outAttr("call.trigger"); got != "owner_membership" {
					t.Errorf("trigger %q", got)
				}
			})
		})
	}
}

// The trigger's ingest span is the call.outbound link and is marked routed to the call.
func TestDetectorIngestSpan(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.ownerStart("D1")
		h.dialed()
		var trig sdktrace.ReadOnlySpan
		for _, s := range h.env.SpansByName("matrixrtc.ingest") {
			if obstest.Attr(s, "matrix.event_id").AsString() == string(testNotif) {
				trig = s
			}
		}
		if trig == nil {
			t.Fatal("no ingest span for the notification")
		}
		out := h.env.SpanByName("call.outbound")
		if out != nil {
			t.Fatal("call.outbound ended while dialing")
		}
		h.m.Hangup(context.Background(), testLogin, "")
		synctest.Wait()
		out = h.env.SpanByName("call.outbound")
		if out == nil || out.Parent().IsValid() || !hasLinkTo(out, trig.SpanContext()) {
			t.Fatalf("call.outbound not a linked root to the trigger: %+v", out)
		}
		if got := obstest.Attr(trig, "matrixrtc.routed_to").AsString(); got != "call" {
			t.Errorf("routed_to %q", got)
		}
		if got := obstest.Attr(trig, "call.id").AsString(); got == "" || got != obstest.Attr(out, "call.id").AsString() {
			t.Errorf("ingest call.id %q", got)
		}
	})
}

// A duplicate notification (Element X retry) goes to the active call's room: ignored, no second call.
func TestDetectorDuplicateNotification(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		h.matrix(notifEvent(testOwner, testRoom, "$notif2", h.clk.Now()))
		h.matrix(h.ownerMember("D9", true))
		h.matrix(notifEvent(testOwner, testRoom, "$notif3", h.clk.Now()))
		if h.dialer.count() != 1 || h.callCount() != 1 {
			t.Fatalf("dials %d calls %d", h.dialer.count(), h.callCount())
		}
		oc.answer()
		oc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// An inbound call ringing in the room: the owner's membership answers it, no outbound trigger.
func TestDetectorInboundRingingSameRoom(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		sc := h.invite()
		h.ownerStart("D1")
		if h.dialer.count() != 0 {
			t.Fatal("dialled while an inbound call rings in the room")
		}
		if !sc.has("answer") {
			t.Fatalf("inbound not answered: %v", sc.commands())
		}
		sc.bye()
		h.assertEnded("answered", "2xx")
	})
}

// A trigger while another room has an active call: busy actor (decline + notice), no Dial.
func TestDetectorBusyOtherRoom(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		sc := h.invite() // ringing in testRoom
		h.matrix(memberEvent(testOwner, "D1", true, h.clk.Now(), testRoom2))
		h.matrix(notifEvent(testOwner, testRoom2, testNotif, h.clk.Now()))
		if h.dialer.count() != 0 || len(h.ghost.stateCalls()) != 1 {
			t.Fatalf("dials %d ops %v", h.dialer.count(), h.ops.list())
		}
		d := h.declines()
		if len(d) != 1 {
			t.Fatalf("declines %+v", d)
		}
		assertNotices(t, h, noticeOutBusyLine)
		if got := h.env.Sum(t, "mrtcsip.call.ended", "call.direction", "outbound", "call.outcome", "rejected"); got != 1 {
			t.Errorf("busy outbound ended %v", got)
		}
		if sc.has("reject") || h.callCount() != 1 {
			t.Errorf("inbound call affected: %v", sc.commands())
		}
		// The refused room is in cooldown: a retry right away is ignored (no second busy actor).
		h.matrix(notifEvent(testOwner, testRoom2, "$notif2", h.clk.Now()))
		if len(h.declines()) != 1 {
			t.Error("retry during cooldown produced another refusal")
		}
		sc.cancel()
		synctest.Wait()
		if h.callCount() != 0 {
			t.Errorf("calls %d", h.callCount())
		}
	})
}

// Cooldown after a call ends.
func TestDetectorCooldown(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		startDialing(t, h)
		h.matrix(h.ownerMember("OWNERDEV", false)) // owner cancels
		h.assertEnded("cancelled", "4xx")
		h.advance(time.Second)
		h.ownerStart("OWNERDEV")
		if h.dialer.count() != 1 {
			t.Fatalf("triggered during the cooldown: dials %d", h.dialer.count())
		}
		h.advance(triggerCooldown)
		h.matrix(h.ownerMember("OWNERDEV", false))
		h.ownerStart("OWNERDEV")
		if h.dialer.count() != 2 {
			t.Fatalf("no trigger after the cooldown: dials %d", h.dialer.count())
		}
		h.m.Hangup(context.Background(), testLogin, "")
		synctest.Wait()
	})
}

func TestDetectorMembershipMode(t *testing.T) {
	t.Run("first device", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound("membership"))
			h.matrix(h.ownerMember("D1", true))
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			h.dialed().reject(486, "Busy")
			synctest.Wait()
			h.advance(failureToneLength)
			if len(h.declines()) != 0 {
				t.Error("decline without an owner notification")
			}
			h.assertEnded("busy", "4xx")
		})
	})
	t.Run("second device", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound("membership"), withConfig(func(c *Config) { c.Dial = nil }))
			h.matrix(h.ownerMember("D1", true)) // outbound disabled: nothing, but the device is remembered
			h.m.cfg.Dial = h.dialer.dial
			h.advance(10 * time.Minute)
			h.matrix(h.ownerMember("D2", true))
			if h.dialer.count() != 0 {
				t.Fatalf("second device triggered a call: dials %d", h.dialer.count())
			}
		})
	})
	t.Run("same device republish", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound("membership"))
			h.matrix(h.ownerMember("D1", true))
			if h.dialer.count() != 1 {
				t.Fatalf("dials %d", h.dialer.count())
			}
			h.dialed().reject(486, "Busy")
			synctest.Wait()
			h.advance(failureToneLength)
			h.assertEnded("busy", "4xx")
			h.advance(triggerCooldown)
			h.matrix(h.ownerMember("D1", true)) // same state key re-published while still live
			if h.dialer.count() != 1 {
				t.Fatalf("republish re-dialled: dials %d", h.dialer.count())
			}
		})
	})
	t.Run("stale", func(t *testing.T) {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound("membership"))
			old := h.clk.Now().Add(-time.Minute)
			h.matrix(memberAt(testOwner, "D1", testRoom, old, old))
			h.matrix(notifEvent(testOwner, testRoom, testNotif, h.clk.Now()))
			if h.dialer.count() != 0 {
				t.Fatal("stale membership triggered a call")
			}
		})
	})
}

func TestDetectorDisabled(t *testing.T) {
	cases := map[string][]harnessOpt{
		"off":     {withOutbound("off")},
		"no dial": {withOutbound(""), withConfig(func(c *Config) { c.Dial = nil })},
		"unknown": {withOutbound("sometimes")},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				h := newHarness(t, opts...)
				h.ownerStart("D1")
				if h.dialer.count() != 0 || h.callCount() != 0 {
					t.Fatalf("dials %d calls %d", h.dialer.count(), h.callCount())
				}
			})
		})
	}
}

// HandleRTCEvent never blocks, also when the actor is stuck in a host call.
func TestDetectorNeverBlocks(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		gate := make(chan struct{})
		h.host.lookupGate = gate
		h.ownerStart("D1")
		if h.callCount() != 1 {
			t.Fatalf("calls %d", h.callCount())
		}
		for i := 0; i < 3*callEventBuffer; i++ {
			h.ingest.HandleMatrixRTCEvent(context.Background(), testLogin, h.ownerMember("D1", true))
			h.ingest.HandleMatrixRTCEvent(context.Background(), testLogin,
				notifEvent(testOwner, testRoom, id.EventID("$n"+string(rune('a'+i%26))), h.clk.Now()))
			h.ingest.HandleMatrixRTCEvent(context.Background(), testLogin,
				memberEvent(testOwner, "X", true, h.clk.Now(), testRoom2))
		}
		close(gate)
		synctest.Wait()
		h.m.Hangup(context.Background(), testLogin, "")
		synctest.Wait()
	})
}

func TestValidNANP(t *testing.T) {
	for _, tt := range []struct {
		n  string
		ok bool
	}{
		{"+17377273232", true},
		{"+442071234567", true},
		{"+19110000", false},
		{"+19115551234", false},
		{"+15559111234", false},
		{"+19005551234", false},
		{"+19765551234", false},
		{"+11235551234", false},
		{"+12350551234", false},
		{"+173772732321", false},
	} {
		if got := validNANP(tt.n); got != tt.ok {
			t.Errorf("validNANP(%s) = %v", tt.n, got)
		}
	}
}

// An owner trigger in the room of the active call is refused without a busy actor.
func TestDetectorSameRoomBusy(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		sc := h.invite() // inbound ringing in testRoom
		h.matrix(memberEvent(testOwner, "D9", true, h.clk.Now(), testRoom))
		h.matrix(notifEvent(testOwner, testRoom, "$other", h.clk.Now()))
		if h.dialer.count() != 0 || len(h.declines()) != 0 || h.callCount() != 1 {
			t.Fatalf("dials %d declines %d calls %d", h.dialer.count(), len(h.declines()), h.callCount())
		}
		sc.cancel()
		synctest.Wait()
	})
}
