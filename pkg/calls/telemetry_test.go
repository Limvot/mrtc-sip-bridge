package calls

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

func hasLinkTo(s sdktrace.ReadOnlySpan, target trace.SpanContext) bool {
	for _, l := range s.Links() {
		if l.SpanContext.SpanID() == target.SpanID() && l.SpanContext.TraceID() == target.TraceID() {
			return true
		}
	}
	return false
}

func eventStatuses(s sdktrace.ReadOnlySpan, name string) []int64 {
	var out []int64
	for _, e := range s.Events() {
		if e.Name != name {
			continue
		}
		for _, kv := range e.Attributes {
			if kv.Key == obs.AttrSIPStatusCode {
				out = append(out, kv.Value.AsInt64())
			}
		}
	}
	return out
}

func hasEvent(s sdktrace.ReadOnlySpan, name string) bool {
	for _, e := range s.Events() {
		if e.Name == name {
			return true
		}
	}
	return false
}

// checkTraceShape asserts the common invariants: call.inbound is the only root that is neither a
// linked root nor an ingest span; every span carries call.id except ingest; no span lasts 10 minutes;
// call.session is a zero-length linked root to the setup span.
func checkTraceShape(t *testing.T, h *harness) sdktrace.ReadOnlySpan {
	t.Helper()
	setup := h.env.SpanByName("call.inbound")
	if setup == nil {
		t.Fatal("no call.inbound span")
	}
	if setup.Parent().IsValid() || setup.SpanKind() != trace.SpanKindServer {
		t.Errorf("call.inbound: parent valid %v, kind %v", setup.Parent().IsValid(), setup.SpanKind())
	}
	if !setup.StartTime().Equal(testStart) {
		t.Errorf("call.inbound start %v, want INVITE receipt %v", setup.StartTime(), testStart)
	}
	callID := obstest.Attr(setup, "call.id").AsString()
	if callID == "" {
		t.Error("call.inbound has no call.id")
	}
	for _, r := range h.env.Roots() {
		switch r.Name() {
		case "call.inbound", "matrixrtc.ingest":
		default:
			if !hasLinkTo(r, setup.SpanContext()) {
				t.Errorf("root %s is not linked to the setup span", r.Name())
			}
		}
	}
	for _, s := range h.env.Spans() {
		if d := s.EndTime().Sub(s.StartTime()); d >= 10*time.Minute || d < 0 {
			t.Errorf("span %s lasted %v", s.Name(), d)
		}
		if s.Name() != "matrixrtc.ingest" && obstest.Attr(s, "call.id").AsString() != callID {
			t.Errorf("span %s call.id = %q, want %q", s.Name(), obstest.Attr(s, "call.id").AsString(), callID)
		}
	}
	sess := h.env.SpanByName("call.session")
	if sess == nil {
		t.Fatal("no call.session span")
	}
	if sess.Parent().IsValid() || !sess.StartTime().Equal(sess.EndTime()) || !hasLinkTo(sess, setup.SpanContext()) {
		t.Errorf("call.session: root %v, zero-length %v, linked %v", !sess.Parent().IsValid(),
			sess.StartTime().Equal(sess.EndTime()), hasLinkTo(sess, setup.SpanContext()))
	}
	if got := h.env.Sum(t, "mrtcsip.call.started", "call.direction", "inbound"); got != 1 {
		t.Errorf("call.started = %v", got)
	}
	if got := h.env.Sum(t, "mrtcsip.call.setup.duration"); got != 1 {
		t.Errorf("setup.duration observations = %v", got)
	}
	obstest.AssertMetricAttrsAllowed(t, h.env.Reader, obstest.DefaultMetricAllowlist)
	obstest.AssertNoSecrets(t, h.env.Rec, h.logs.snapshot(), "SECRETTOKEN", "eyJhbGciOi.eyJzdWIi.c2ln", "v=0", "a=rtpmap")
	return setup
}

func TestTelemetryAnswer(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.advance(3 * time.Second)
		h.answer(sc, "D1")
		ackAt := h.clk.Now()
		h.advance(42 * time.Second)
		sc.bye()
		synctest.Wait()
		h.assertEnded("answered", "2xx")

		setup := checkTraceShape(t, h)
		if !setup.EndTime().Equal(ackAt) {
			t.Errorf("call.inbound ends %v, want ACK %v", setup.EndTime(), ackAt)
		}
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "matrixrtc.ring.send", "call.ring", "call.answer"}
		if got := names(h.env.Children(setup)); !reflect.DeepEqual(got, want) {
			t.Errorf("call.inbound children %v, want %v", got, want)
		}
		if got := eventStatuses(setup, "sip.response.sent"); !reflect.DeepEqual(got, []int64{100, 180}) {
			t.Errorf("setup sip.response.sent %v", got)
		}
		if got := obstest.Attr(setup, "call.outcome").AsString(); got != "answered" {
			t.Errorf("outcome %q", got)
		}
		if got := obstest.Attr(setup, "sip.response.status_code").AsInt64(); got != 200 {
			t.Errorf("status %d", got)
		}
		answer := h.env.SpanByName("call.answer")
		if got := names(h.env.Children(answer)); !reflect.DeepEqual(got, []string{"media.start"}) {
			t.Errorf("call.answer children %v", got)
		}
		if got := eventStatuses(answer, "sip.response.sent"); !reflect.DeepEqual(got, []int64{200}) || !hasEvent(answer, "sip.ack.received") {
			t.Errorf("call.answer events %+v", answer.Events())
		}
		ring := h.env.SpanByName("call.ring")
		ingest := h.env.SpansByName("matrixrtc.ingest")
		if len(ingest) != 1 {
			t.Fatalf("ingest spans %d", len(ingest))
		}
		if !hasLinkTo(ring, ingest[0].SpanContext()) {
			t.Error("call.ring has no link to the ingest span that ended it")
		}
		if got := obstest.Attr(ingest[0], "matrixrtc.routed_to").AsString(); got != "call" {
			t.Errorf("routed_to %q", got)
		}
		if got := obstest.Attr(ingest[0], "call.id").AsString(); got != obstest.Attr(setup, "call.id").AsString() {
			t.Errorf("ingest call.id %q", got)
		}
		td := h.env.SpanByName("call.teardown")
		if td == nil || td.Parent().IsValid() || !hasEvent(td, "sip.bye.received") {
			t.Fatalf("call.teardown %+v", td)
		}
		if got := names(h.env.Children(td)); !reflect.DeepEqual(got, []string{"media.stop", "rtc.leave", "matrixrtc.membership.clear", "call.notice.send"}) {
			t.Errorf("call.teardown children %v", got)
		}
		sess := h.env.SpanByName("call.session")
		if !hasLinkTo(sess, td.SpanContext()) {
			t.Error("call.session not linked to the teardown")
		}
		if got := obstest.Attr(sess, "call.duration_s").AsFloat64(); got != 42 {
			t.Errorf("call.duration_s = %v", got)
		}
		if got := obstest.Attr(sess, "call.hangup.by").AsString(); got != "remote" {
			t.Errorf("session hangup.by %q", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ring.duration", "call.outcome", "answered"); got != 1 {
			t.Errorf("ring.duration = %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.duration", "media.codec", "PCMU"); got != 1 {
			t.Errorf("call.duration = %v", got)
		}
	})
}

func TestTelemetryCancel(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.advance(5 * time.Second)
		sc.cancel()
		synctest.Wait()
		h.assertEnded("cancelled", "4xx")
		setup := checkTraceShape(t, h)
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "matrixrtc.ring.send", "call.ring",
			"matrixrtc.ring.redact", "matrixrtc.membership.clear", "rtc.leave", "call.notice.send"}
		if got := names(h.env.Children(setup)); !reflect.DeepEqual(got, want) {
			t.Errorf("children %v, want %v", got, want)
		}
		if !hasEvent(setup, "sip.cancel.received") {
			t.Error("no sip.cancel.received event")
		}
		if got := eventStatuses(setup, "sip.response.sent"); !reflect.DeepEqual(got, []int64{100, 180, 487}) {
			t.Errorf("sip.response.sent %v", got)
		}
		if h.env.SpanByName("call.teardown") != nil {
			t.Error("teardown span for an unanswered call")
		}
		if got := h.env.Sum(t, "mrtcsip.call.ring.duration", "call.outcome", "cancelled"); got != 1 {
			t.Errorf("ring.duration = %v", got)
		}
	})
}

func TestTelemetryDecline(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		h.invite()
		h.matrix(declineEvent(testOwner, h.ringID(), testRoom))
		h.assertEnded("declined", "6xx")
		setup := checkTraceShape(t, h)
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "matrixrtc.ring.send", "call.ring",
			"matrixrtc.membership.clear", "rtc.leave", "call.notice.send"}
		if got := names(h.env.Children(setup)); !reflect.DeepEqual(got, want) {
			t.Errorf("children %v, want %v", got, want)
		}
		if !hasLinkTo(h.env.SpanByName("call.ring"), h.env.SpanByName("matrixrtc.ingest").SpanContext()) {
			t.Error("call.ring not linked to the decline's ingest span")
		}
		if got := eventStatuses(setup, "sip.response.sent"); !reflect.DeepEqual(got, []int64{100, 180, 603}) {
			t.Errorf("sip.response.sent %v", got)
		}
	})
}

func TestTelemetryTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		h.invite()
		h.advance(30 * time.Second)
		h.assertEnded("timeout", "4xx")
		setup := checkTraceShape(t, h)
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "matrixrtc.ring.send", "call.ring",
			"matrixrtc.ring.redact", "matrixrtc.membership.clear", "rtc.leave", "call.notice.send"}
		if got := names(h.env.Children(setup)); !reflect.DeepEqual(got, want) {
			t.Errorf("children %v, want %v", got, want)
		}
		if d := setup.EndTime().Sub(setup.StartTime()); d != 30*time.Second {
			t.Errorf("setup lasted %v", d)
		}
	})
}

func TestTelemetryBusy(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc1 := h.invite()
		h.env.Rec.Reset()
		h.invite()
		synctest.Wait()
		setup := h.env.SpanByName("call.inbound")
		if setup == nil || obstest.Attr(setup, "call.outcome").AsString() != "busy" {
			t.Fatalf("busy call.inbound %+v", setup)
		}
		if got := names(h.env.Children(setup)); !reflect.DeepEqual(got, []string{"call.resolve_room", "call.notice.send"}) {
			t.Errorf("children %v", got)
		}
		if got := eventStatuses(setup, "sip.response.sent"); !reflect.DeepEqual(got, []int64{100, 486}) {
			t.Errorf("sip.response.sent %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ended", "call.outcome", "busy"); got != 1 {
			t.Errorf("busy ended %v", got)
		}
		sc1.cancel()
		h.assertEnded("cancelled", "4xx")
		obstest.AssertMetricAttrsAllowed(t, h.env.Reader, obstest.DefaultMetricAllowlist)
	})
}

// A joiner failing with token-like error text: nothing secret reaches spans or logs.
func TestTelemetryNoSecrets(t *testing.T) {
	run(t, func(t *testing.T) {
		secretErr := errors.New("token_error: access_token=SECRETTOKEN jwt eyJhbGciOi.eyJzdWIi.c2ln")
		h := newHarness(t, withJoinSteps(joinStep{err: secretErr}, joinStep{err: secretErr}))
		h.invite()
		h.assertEnded("failed", "4xx")
		checkTraceShape(t, h)
		setup := h.env.SpanByName("call.inbound")
		if setup.Status().Code.String() != "Error" {
			t.Errorf("failed call status %v", setup.Status())
		}
		if !strings.Contains(h.logs.String(), "LiveKit join failed") {
			t.Error("join failure not logged")
		}
	})
}

// Mid-call linked roots: owner left (second device), degraded media, membership refresh; all replayed
// on call.session.
func TestTelemetryMidCall(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "D1")
		h.matrix(h.ownerMember("D2", true))
		h.matrix(h.ownerMember("D1", false))
		h.advance(matrixrtc.DefaultMembershipExpiry - membershipRefreshLead)
		sc.bye()
		synctest.Wait()
		h.assertEnded("answered", "2xx")
		checkTraceShape(t, h)
		for _, n := range []string{"call.owner.left", "call.media.degraded", "call.membership.refresh"} {
			if len(h.env.SpansByName(n)) != 1 {
				t.Errorf("%s spans = %d, want 1", n, len(h.env.SpansByName(n)))
			}
			if !hasEvent(h.env.SpanByName("call.session"), n) {
				t.Errorf("call.session lacks event %s", n)
			}
		}
	})
}

// checkOutTraceShape asserts the outbound invariants: call.outbound is a CLIENT linked root to the trigger
// span, started at the trigger; the other roots are ingest/cmd spans or linked to call.outbound; every
// call span carries call.id; no span lasts 10 minutes; call.session is a zero-length outbound linked root;
// metric attributes are allowlisted and no secret reaches spans or logs.
func checkOutTraceShape(t *testing.T, h *harness, trigger trace.SpanContext, secrets ...string) sdktrace.ReadOnlySpan {
	t.Helper()
	out := h.env.SpanByName("call.outbound")
	if out == nil {
		t.Fatal("no call.outbound span")
	}
	if out.Parent().IsValid() || out.SpanKind() != trace.SpanKindClient {
		t.Errorf("call.outbound: parent valid %v, kind %v", out.Parent().IsValid(), out.SpanKind())
	}
	if !hasLinkTo(out, trigger) {
		t.Error("call.outbound not linked to the trigger span")
	}
	if got := obstest.Attr(out, "call.direction").AsString(); got != "outbound" {
		t.Errorf("direction %q", got)
	}
	callID := obstest.Attr(out, "call.id").AsString()
	for _, r := range h.env.Roots() {
		switch r.Name() {
		case "call.outbound", "matrixrtc.ingest", "cmd.call", "cmd.dtmf":
		default:
			if !hasLinkTo(r, out.SpanContext()) {
				t.Errorf("root %s is not linked to call.outbound", r.Name())
			}
		}
	}
	for _, s := range h.env.Spans() {
		if d := s.EndTime().Sub(s.StartTime()); d >= 10*time.Minute || d < 0 {
			t.Errorf("span %s lasted %v", s.Name(), d)
		}
		switch s.Name() {
		case "matrixrtc.ingest", "cmd.call", "cmd.dtmf":
		default:
			if got := obstest.Attr(s, "call.id").AsString(); got != callID {
				t.Errorf("span %s call.id = %q, want %q", s.Name(), got, callID)
			}
		}
	}
	sess := h.env.SpanByName("call.session")
	if sess == nil {
		t.Fatal("no call.session span")
	}
	if sess.Parent().IsValid() || !sess.StartTime().Equal(sess.EndTime()) || !hasLinkTo(sess, out.SpanContext()) ||
		obstest.Attr(sess, "call.direction").AsString() != "outbound" {
		t.Errorf("call.session %+v", sess)
	}
	if got := h.env.Sum(t, "mrtcsip.call.started", "call.direction", "outbound"); got != 1 {
		t.Errorf("call.started{outbound} = %v", got)
	}
	if got := h.env.Sum(t, "mrtcsip.call.setup.duration", "call.direction", "outbound"); got != 1 {
		t.Errorf("setup.duration observations = %v", got)
	}
	obstest.AssertMetricAttrsAllowed(t, h.env.Reader, obstest.DefaultMetricAllowlist)
	obstest.AssertNoSecrets(t, h.env.Rec, h.logs.snapshot(), append([]string{"SECRETTOKEN", "v=0", "a=rtpmap"}, secrets...)...)
	return out
}

// notifIngest returns the matrixrtc.ingest span of the owner's notification.
func notifIngest(t *testing.T, h *harness) trace.SpanContext {
	t.Helper()
	for _, s := range h.env.SpansByName("matrixrtc.ingest") {
		if obstest.Attr(s, "matrix.event_id").AsString() == string(testNotif) {
			return s.SpanContext()
		}
	}
	t.Fatal("no ingest span for the owner's notification")
	return trace.SpanContext{}
}

func TestTelemetryOutboundOwnerSuccess(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		trigAt := h.clk.Now()
		oc := startDialing(t, h)
		oc.auth()
		oc.provisional(100, false)
		h.advance(time.Second)
		oc.provisional(180, false)
		h.advance(time.Second)
		oc.provisional(183, true)
		h.advance(3 * time.Second)
		oc.answer()
		synctest.Wait()
		ackAt := h.clk.Now()
		h.advance(20 * time.Second)
		oc.bye()
		synctest.Wait()
		h.assertEnded("answered", "2xx")

		out := checkOutTraceShape(t, h, notifIngest(t, h))
		if !out.StartTime().Equal(trigAt) || !out.EndTime().Equal(ackAt) {
			t.Errorf("call.outbound %v..%v, want %v..%v (ACK)", out.StartTime(), out.EndTime(), trigAt, ackAt)
		}
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "call.ringback", "sip.invite"}
		if got := names(h.env.Children(out)); !reflect.DeepEqual(got, want) {
			t.Errorf("call.outbound children %v, want %v", got, want)
		}
		for key, want := range map[string]string{
			"call.trigger": "owner_membership", "call.remote.e164": "+15552345678", "call.local.e164": testLocal,
			"matrix.room_id": string(testRoom), "matrix.ghost_id": string(testGhost), "matrix.owner_id": string(testOwner),
			"sip.call_id": "out1@sip.example.com", "call.outcome": "answered", "sip.response.status_code": "200",
		} {
			if got := obstest.Attr(out, key).Emit(); got != want {
				t.Errorf("call.outbound %s = %q, want %q", key, got, want)
			}
		}
		if !hasEvent(out, "sip.ack.sent") {
			t.Error("no sip.ack.sent event")
		}
		inv := h.env.SpanByName("sip.invite")
		if inv.SpanKind() != trace.SpanKindClient || obstest.Attr(inv, "sip.method").AsString() != "INVITE" ||
			!obstest.Attr(inv, "sip.auth.challenged").AsBool() || !hasEvent(inv, "sip.auth.challenge") {
			t.Errorf("sip.invite %+v", inv.Attributes())
		}
		if got := eventStatuses(inv, "sip.response.received"); !reflect.DeepEqual(got, []int64{100, 180, 183, 200}) {
			t.Errorf("sip.invite responses %v", got)
		}
		rb := h.env.SpanByName("call.ringback")
		if !obstest.Attr(rb, "sip.early_media").AsBool() || obstest.Attr(rb, "call.outcome").AsString() != "answered" {
			t.Errorf("call.ringback %+v", rb.Attributes())
		}
		if got := names(h.env.Children(rb)); !reflect.DeepEqual(got, []string{"media.start"}) {
			t.Errorf("call.ringback children %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ring.duration", "call.direction", "outbound", "call.outcome", "answered"); got != 1 {
			t.Errorf("ring.duration %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.duration", "call.direction", "outbound"); got != 1 {
			t.Errorf("call.duration %v", got)
		}
		td := h.env.SpanByName("call.teardown")
		if td == nil || !hasEvent(td, "sip.bye.received") {
			t.Fatal("no call.teardown with sip.bye.received")
		}
		if got := obstest.Attr(h.env.SpanByName("call.session"), "call.duration_s").AsFloat64(); got != 20 {
			t.Errorf("call.duration_s %v", got)
		}
	})
}

func TestTelemetryOutboundBusy(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.provisional(180, false)
		h.advance(4 * time.Second)
		oc.reject(486, "Busy access_token=SECRETTOKEN")
		synctest.Wait()
		h.advance(failureToneLength)
		h.assertEnded("busy", "4xx")

		out := checkOutTraceShape(t, h, notifIngest(t, h))
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "sip.invite", "call.ringback",
			"matrixrtc.decline.send", "matrixrtc.membership.clear", "rtc.leave", "call.notice.send"}
		if got := names(h.env.Children(out)); !reflect.DeepEqual(got, want) {
			t.Errorf("call.outbound children %v, want %v", got, want)
		}
		for key, want := range map[string]string{"call.outcome": "busy", "call.end_reason": "remote_reject",
			"sip.response.status_code": "486"} {
			if got := obstest.Attr(out, key).Emit(); got != want {
				t.Errorf("call.outbound %s = %q, want %q", key, got, want)
			}
		}
		if out.Status().Code.String() == "Error" {
			t.Error("busy is not an error")
		}
		inv := h.env.SpanByName("sip.invite")
		if got := eventStatuses(inv, "sip.response.received"); !reflect.DeepEqual(got, []int64{180, 486}) {
			t.Errorf("sip.invite responses %v", got)
		}
		if got := obstest.Attr(inv, "sip.response.reason").AsString(); !strings.HasPrefix(got, "Busy") {
			t.Errorf("sip.response.reason %q", got)
		}
		dec := h.env.SpanByName("matrixrtc.decline.send")
		if dec.SpanKind() != trace.SpanKindClient || obstest.Attr(dec, "matrix.event_id").AsString() != string(testNotif) {
			t.Errorf("decline span %+v", dec.Attributes())
		}
		if got := obstest.Attr(h.env.SpanByName("call.ringback"), "call.outcome").AsString(); got != "busy" {
			t.Errorf("call.ringback outcome %q", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ring.duration", "call.direction", "outbound", "call.outcome", "busy"); got != 1 {
			t.Errorf("ring.duration %v", got)
		}
		if h.env.SpanByName("call.teardown") != nil {
			t.Error("teardown span for an unanswered call")
		}
	})
}

func TestTelemetryOutboundOwnerCancel(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		oc := startDialing(t, h)
		oc.provisional(183, true)
		h.advance(3 * time.Second)
		h.matrix(h.ownerMember("OWNERDEV", false))
		h.assertEnded("cancelled", "4xx")
		out := checkOutTraceShape(t, h, notifIngest(t, h))
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "sip.invite", "call.ringback",
			"matrixrtc.membership.clear", "rtc.leave", "call.notice.send"}
		if got := names(h.env.Children(out)); !reflect.DeepEqual(got, want) {
			t.Errorf("call.outbound children %v, want %v", got, want)
		}
		if got := obstest.Attr(h.env.SpanByName("sip.invite"), "sip.response.status_code").AsInt64(); got != 487 {
			t.Errorf("sip.invite status %d", got)
		}
		if got := obstest.Attr(out, "call.end_reason").AsString(); got != "cancel" {
			t.Errorf("end_reason %q", got)
		}
	})
}

// Command path success with DTMF: call.outbound links to cmd.call; no digit reaches spans or logs.
func TestTelemetryOutboundCommandDTMF(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		cmdCtx, cmdSpan := obs.Start(context.Background(), "cmd.call", trace.WithSpanKind(trace.SpanKindConsumer))
		if _, _, err := h.m.PlaceCall(cmdCtx, testLogin, PlaceCallRequest{Number: "+15552345678"}); err != nil {
			t.Fatal(err)
		}
		cmdSpan.End()
		synctest.Wait()
		h.advance(2 * time.Second)
		h.matrix(h.ownerMember("D1", true))
		h.ownerJoinedLK("D1")
		oc := h.dialed()
		oc.provisional(180, false)
		h.advance(time.Second)
		oc.answer()
		synctest.Wait()
		dctx, dspan := obs.Start(context.Background(), "cmd.dtmf")
		if n, err := h.m.SendDTMF(dctx, testLogin, testRoom, "*9876#"); err != nil || n != 6 {
			t.Fatalf("SendDTMF = %d, %v", n, err)
		}
		dspan.End()
		h.media.bridge(0).setStats(media.Stats{DTMFSent: 6, LastSIPRx: h.clk.Now().Add(time.Hour)})
		h.advance(statsInterval)
		oc.bye()
		synctest.Wait()
		h.assertEnded("answered", "2xx")

		out := checkOutTraceShape(t, h, cmdSpan.SpanContext(), "*9876#", "*9876", "9876#")
		want := []string{"call.resolve_room", "matrixrtc.membership.publish", "matrixrtc.ring.send", "call.ring",
			"call.ringback", "sip.invite", "media.start"}
		if got := names(h.env.Children(out)); !reflect.DeepEqual(got, want) {
			t.Errorf("call.outbound children %v, want %v", got, want)
		}
		if got := obstest.Attr(out, "call.trigger").AsString(); got != "command" {
			t.Errorf("trigger %q", got)
		}
		ring := h.env.SpanByName("call.ring")
		var memberIngest sdktrace.ReadOnlySpan
		for _, s := range h.env.SpansByName("matrixrtc.ingest") {
			if obstest.Attr(s, "matrixrtc.kind").AsString() == "call_member" {
				memberIngest = s
			}
		}
		if memberIngest == nil || !hasLinkTo(ring, memberIngest.SpanContext()) ||
			obstest.Attr(ring, "call.outcome").AsString() != "answered" {
			t.Error("call.ring not ended as answered by the owner's membership")
		}
		if got := h.env.Sum(t, "mrtcsip.dtmf.sent", "dtmf.method", "rfc4733"); got != 6 {
			t.Errorf("dtmf.sent %v", got)
		}
		if got := h.env.Sum(t, "mrtcsip.call.ring.duration", "call.direction", "outbound"); got != 1 {
			t.Errorf("ring.duration %v (the callback ring must not be recorded)", got)
		}
		if !strings.Contains(h.logs.String(), `"digit_count":6`) {
			t.Error("DTMF not logged by count")
		}
	})
}
