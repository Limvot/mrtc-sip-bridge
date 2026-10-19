package widget

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

var noSC trace.SpanContext

func startTestSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return obs.Start(ctx, name)
}

func spanContextOf(ctx context.Context) trace.SpanContext { return trace.SpanContextFromContext(ctx) }

// widgetAllowlist is obstest's default allowlist, which includes the widget metrics (TELEMETRY-SPEC §4.3).
// The widget's own enum constants are checked against it by TestAllowlistMatchesEnums.
func widgetAllowlist() obstest.MetricAllowlist { return obstest.DefaultMetricAllowlist }

func TestAllowlistMatchesEnums(t *testing.T) {
	al := obstest.DefaultMetricAllowlist
	want := map[string][]string{
		metricRequests + "/widget.action": {actSession, actState, actNormalize, actCall, actDTMF, actHangup, actPrefs, actChat, actContact, actContacts,
			actDevEnroll, actDevSession, actDevConfirm, actDevForget},
		metricRequests + "/outcome": {mOK, mUnauthorized, mForbidden, mRateLimited, mInvalid, mBusy, mNotReady, mError,
			mExpired, mUsed, mReuse},
		metricRequests + "/widget.client": {clientWidget, clientPWA},
		metricDeviceLinks + "/state":      {string(DeviceStatePending), string(DeviceStateActive)},
		metricDeviceEvents + "/event": {evIssued, evEnrolled, evRotated, evGraceUsed, evReuseRevoked, evRevoked,
			evExpired, evOrphanPurged, evNoticeRedacted},
		metricInstalls + "/outcome": {outcomeInstalled, outcomeUpdated, outcomeUnchanged, outcomeRemoved, outcomeInstallErr},
	}
	for k, vals := range want {
		name, attr, _ := strings.Cut(k, "/")
		got := al[name][attr]
		if strings.Join(got, ",") != strings.Join(vals, ",") {
			t.Errorf("%s: allowlist %v, enums %v", k, got, vals)
		}
	}
	if _, ok := al[metricSessions]; !ok {
		t.Errorf("%s missing from the default allowlist", metricSessions)
	}
}

func TestSpansAndClasses(t *testing.T) {
	ot := obstest.Setup(t)
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	sess := ot.SpanByName(spanSession)
	if sess == nil || sess.Parent().IsValid() || sess.SpanKind() != trace.SpanKindServer {
		t.Fatalf("session span %+v", sess)
	}
	if obstest.Attr(sess, "outcome").AsString() != outcomeOK || obstest.Attr(sess, "matrix.sender").AsString() != string(alice) ||
		obstest.Attr(sess, "widget.room.kind").AsString() != KindPortal || obstest.Attr(sess, "matrix.room_id").AsString() != string(roomPortal) {
		t.Errorf("session attrs %v", sess.Attributes())
	}
	ver := ot.SpanByName(spanVerify)
	if ver == nil || ver.Parent().SpanID() != sess.SpanContext().SpanID() || ver.SpanKind() != trace.SpanKindClient ||
		obstest.Attr(ver, "server.address").AsString() != testServer {
		t.Fatalf("verify span %+v", ver)
	}
	// The userinfo HTTP span uses the fixed template, under the verify span.
	for _, s := range ot.InTrace(sess) {
		if s.Name() != spanSession && s.Name() != spanVerify && s.Name() != "GET "+userinfoPath {
			t.Errorf("unexpected span %q in session trace", s.Name())
		}
	}

	n := len(ot.Spans())
	e.do(http.MethodGet, api+"state", tok, nil)
	e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "5551234567"})
	if len(ot.Spans()) != n {
		t.Errorf("state/normalize produced spans")
	}

	e.do(http.MethodPost, api+"call", tok, map[string]string{"number": "+17377273232"})
	c := ot.SpanByName(spanCall)
	if c == nil || c.Parent().IsValid() || c.SpanKind() != trace.SpanKindServer ||
		obstest.Attr(c, "widget.call.form").AsString() != "number" || obstest.Attr(c, "call.remote.e164").AsString() != "+17377273232" ||
		obstest.Attr(c, "outcome").AsString() != outcomeOK {
		t.Errorf("call span %+v", c)
	}
	e.do(http.MethodPost, api+"hangup", tok, "{}")
	if h := ot.SpanByName(spanHangup); h == nil || h.Parent().IsValid() || obstest.Attr(h, "outcome").AsString() != outcomeOK {
		t.Errorf("hangup span %+v", h)
	}
	// Failed session outcome.
	e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, "AAAAAAAAAAAAAAAAAAAAAA"), "remote", "198.51.100.3:1")
	var badKey bool
	for _, s := range ot.SpansByName(spanSession) {
		if obstest.Attr(s, "outcome").AsString() == outcomeBadKey {
			badKey = true
		}
	}
	if !badKey {
		t.Error("no bad_key session span")
	}
	// Only entry-point roots.
	for _, r := range ot.Roots() {
		switch r.Name() {
		case spanSession, spanCall, spanDTMF, spanHangup, spanSweep:
		default:
			t.Errorf("orphan root %q", r.Name())
		}
	}
	if ot.Sum(t, metricRequests, "widget.action", actState, "outcome", mOK) != 1 {
		t.Error("state request metric")
	}
	obstest.AssertMetricAttrsAllowed(t, ot.Reader, widgetAllowlist())
}

func TestPrivacyNoSecretsInLogsOrSpans(t *testing.T) {
	ot := obstest.Setup(t)
	e := newEnv(t)
	key := e.key(roomPortal)
	tok := e.session(tokAlice, roomPortal)
	w := e.do(http.MethodPost, api+"dtmf", tok, map[string]string{"digits": "159#"})
	if w.Code != 200 {
		t.Fatalf("dtmf %d %s", w.Code, w.Body.String())
	}
	d := ot.SpanByName(spanDTMF)
	if d == nil || obstest.Attr(d, "dtmf.digit_count").AsInt64() != 4 {
		t.Fatalf("dtmf span %+v", d)
	}
	// Failure paths too: bad token, bad key, transient HS error.
	e.do(http.MethodPost, sessPath, "", sessBody("bad-secret-openid-token", testServer, roomPortal, key), "remote", "198.51.100.4:1")
	e.do(http.MethodPost, api+"dtmf", tok, map[string]string{"digits": "159#x"})
	if e.logs() == "" {
		t.Fatal("captured no logs")
	}
	obstest.AssertNoSecrets(t, ot.Rec, e.logBuf, "159#", tokAlice, "bad-secret-openid-token", tok, key, testSecret)
}

func TestMetricOutcomeMapping(t *testing.T) {
	for code, want := range map[string]string{
		"": mOK, codeUnauthorized: mUnauthorized, codeLoginRequired: mForbidden, codeRateLimited: mRateLimited,
		"line_busy": mBusy, "outbound_disabled": mNotReady, codeInternal: mError, "invalid_number": mInvalid,
	} {
		if got := metricOutcome(code); got != want {
			t.Errorf("%q: %s want %s", code, got, want)
		}
	}
}

// GET contacts records the metric only: no span, and neither names nor the query reach spans or logs.
func TestContactsTelemetry(t *testing.T) {
	ot := obstest.Setup(t)
	e, ff := newFeatEnv(t)
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 1, Items: []hostkit.ContactEntry{{Name: "Zebediah Quux", E164: "+15550001234"}}}
	tok := e.session(tokAlice, roomPortal)
	before := len(ot.Rec.Ended())
	if w := e.do(http.MethodGet, api+"contacts?q=Zebediah+secretquery", tok, nil); w.Code != 200 {
		t.Fatalf("contacts %d %s", w.Code, w.Body.String())
	}
	ff.listErr = errors.New("host failed")
	e.do(http.MethodGet, api+"contacts?q=secretquery", tok, nil)
	for _, sp := range ot.Rec.Ended()[before:] {
		t.Errorf("contacts started span %q", sp.Name())
	}
	if n := ot.Sum(t, metricRequests, "widget.action", actContacts, "outcome", mOK); n != 1 {
		t.Errorf("ok metric %v", n)
	}
	if n := ot.Sum(t, metricRequests, "widget.action", actContacts, "outcome", mError); n != 1 {
		t.Errorf("error metric %v", n)
	}
	obstest.AssertNoSecrets(t, ot.Rec, e.logBuf, "Zebediah", "Quux", "secretquery")
}
