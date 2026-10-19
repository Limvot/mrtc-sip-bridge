package widget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

// newFeatEnv is an env whose host offers every optional feature; ff and e.calls share one order log.
func newFeatEnv(t *testing.T) (*env, *fakeFeatures) {
	t.Helper()
	ff := &fakeFeatures{order: &orderLog{}, openRoom: hostkit.ChatRoom{RoomID: "!chat:hs.example", Created: true}}
	e := newEnv(t, withFeatures(ff), func(_ *hostkit.WidgetConfig, d *Deps) { d.CommandPrefix = "!sip" })
	e.calls.order = ff.order
	return e, ff
}

func TestFeaturesDerivedFromTypeAssertion(t *testing.T) {
	feat := func(e *env, tok string) (map[string]any, map[string]any) {
		m := decode(t, e.do(http.MethodGet, api+"state", tok, nil))
		return m["features"].(map[string]any), m
	}
	e := newEnv(t)
	f, m := feat(e, e.session(tokAlice, roomPortal))
	if f["roster_only"] != false || f["chat"] != false || f["save_contact"] != false || f["contacts"] != false || m["prefs"] != nil {
		t.Errorf("none: %v", m)
	}
	e = newEnv(t, withFeatures(prefsOnly{}))
	f, m = feat(e, e.session(tokAlice, roomPortal))
	if f["roster_only"] != true || f["chat"] != false || f["save_contact"] != false || f["contacts"] != false {
		t.Errorf("prefsOnly: %v", f)
	}
	if p := m["prefs"].(map[string]any); p["roster_only"] != true {
		t.Errorf("prefs %v", p)
	}
	e, _ = newFeatEnv(t)
	f, _ = feat(e, e.session(tokAlice, roomPortal))
	if f["roster_only"] != true || f["chat"] != true || f["save_contact"] != true || f["contacts"] != true {
		t.Errorf("full: %v", f)
	}
	// Not ready (no login): all false.
	e, _ = newFeatEnv(t)
	tb := e.session(tokBob, roomBobMgmt)
	f, m = feat(e, tb)
	if f["roster_only"] != false || f["chat"] != false || f["save_contact"] != false || f["contacts"] != false || m["prefs"] != nil {
		t.Errorf("not ready: %v", m)
	}
	// Standalone-style Deps: Features stays nil and the unsupported endpoints say so.
	e = newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	for _, p := range []string{"chat", "contact", "prefs/roster_only"} {
		body := map[string]any{"chat": map[string]any{"number": "+15557778888"}, "contact": map[string]any{"name": "A"},
			"prefs/roster_only": map[string]any{"on": true}}[p]
		w := e.do(http.MethodPost, api+p, tok, body)
		if w.Code != 404 || decode(t, w)["error"] != hostkit.CodeNotSupported {
			t.Errorf("%s without features: %d %s", p, w.Code, w.Body.String())
		}
	}
}

func TestStateContact(t *testing.T) {
	e, ff := newFeatEnv(t)
	ff.cName, ff.cSaved = "Alice A", true
	room := func(tok string) map[string]any {
		return decode(t, e.do(http.MethodGet, api+"state", tok, nil))["room"].(map[string]any)
	}
	c, _ := room(e.session(tokAlice, roomPortal))["contact"].(map[string]any)
	if c["name"] != "Alice A" || c["saved"] != true {
		t.Errorf("portal contact %v", c)
	}
	ff.cName, ff.cSaved = "", true
	c, _ = room(e.session(tokAlice, roomPortal))["contact"].(map[string]any)
	if c["name"] != "" || c["saved"] != true {
		t.Errorf("unnamed contact %v", c)
	}
	if _, ok := room(e.session(tokAlice, roomMgmt))["contact"]; ok {
		t.Error("management room got a contact")
	}
	// The session response never carries it.
	w := e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal)))
	if _, ok := decode(t, w)["room"].(map[string]any)["contact"]; ok {
		t.Error("session response has a contact")
	}
}

func TestChat(t *testing.T) {
	e, ff := newFeatEnv(t)
	tok := e.session(tokAlice, roomPortal)
	post := func(body any) (*http.Response, map[string]any) {
		e.clk.Advance(20 * time.Second) // stay under the per-user limiter
		w := e.do(http.MethodPost, api+"chat", tok, body)
		return w.Result(), decode(t, w)
	}
	// Protocol checks.
	if w := e.do(http.MethodPost, api+"chat", "", map[string]string{"number": "+15557778888"}); w.Code != 401 {
		t.Errorf("unauthenticated %d", w.Code)
	}
	if w := e.do(http.MethodGet, api+"chat", tok, nil); w.Code != 405 {
		t.Errorf("GET %d", w.Code)
	}
	if w := e.do(http.MethodPost, api+"chat", tok, "{}", "Content-Type", "text/plain"); w.Code != 415 {
		t.Errorf("content type %d", w.Code)
	}
	if w := e.do(http.MethodPost, api+"chat", tok, "{}", "Origin", "https://evil.example"); w.Code != 403 {
		t.Errorf("origin %d", w.Code)
	}
	if w := e.do(http.MethodPost, api+"chat", tok, `{"number":"+15557778888","x":1}`); w.Code != 400 {
		t.Errorf("unknown field %d", w.Code)
	}
	// Validation.
	for _, n := range []string{"", "  ", strings.Repeat("1", 65)} {
		if r, m := post(map[string]string{"number": n}); r.StatusCode != 400 || m["error"] != "invalid_number" {
			t.Errorf("number %q: %d %v", n, r.StatusCode, m)
		}
	}
	if r, m := post(map[string]string{"number": "611"}); r.StatusCode != 400 || !strings.Contains(m["message"].(string), "`!sip pm <code>`") {
		t.Errorf("short code: %d %v", r.StatusCode, m)
	}
	if r, m := post(map[string]string{"number": "+15557778888", "name": "bad\nname"}); r.StatusCode != 400 || m["error"] != "invalid_name" {
		t.Errorf("name: %d %v", r.StatusCode, m)
	}
	if len(ff.opened)+len(ff.saved) != 0 {
		t.Fatal("host called for invalid input")
	}
	// Policy refusals do not block texting.
	e.calls.checkErr = calls.ErrNotAllowed
	r, m := post(map[string]string{"number": "+1 (555) 777-8888"})
	if r.StatusCode != 200 || m["number"] != "+15557778888" || m["room_id"] != "!chat:hs.example" || m["created"] != true ||
		m["pending"] != false || !strings.Contains(m["message"].(string), "created") {
		t.Errorf("created: %d %v", r.StatusCode, m)
	}
	e.calls.checkErr = nil
	ff.openRoom = hostkit.ChatRoom{RoomID: "!chat:hs.example"}
	if _, m = post(map[string]string{"number": "+15557778888"}); !strings.Contains(m["message"].(string), "is ready") {
		t.Errorf("existing: %v", m)
	}
	// Order: save, then open; the saved message is prefixed.
	ff.order = &orderLog{}
	e.calls.order = ff.order
	if r, m = post(map[string]string{"number": "+15557778888", "name": "Zed"}); r.StatusCode != 200 ||
		!strings.HasPrefix(m["message"].(string), "Saved as Zed. ") || m["contact"].(map[string]any)["changed"] != true {
		t.Errorf("save+open: %d %v", r.StatusCode, m)
	}
	if got := strings.Join(ff.order.get(), ","); got != "save,open" {
		t.Errorf("order %s", got)
	}
	if ff.saved[0] != "+15557778888|Zed" {
		t.Errorf("saved %v", ff.saved)
	}
}

func TestChatFailures(t *testing.T) {
	e, ff := newFeatEnv(t)
	tok := e.session(tokAlice, roomPortal)
	body := map[string]string{"number": "+15557778888", "name": "Zed"}
	// Save fails: no room.
	ff.saveErr = hostkit.NewActionError(hostkit.CodeUnavailable, "Not connected to Jabber. Use $cmdprefix x.")
	w := e.post("chat", tok, body)
	m := decode(t, w)
	if w.Code != 503 || m["error"] != "unavailable" || !strings.Contains(m["message"].(string), "!sip x") || len(ff.opened) != 0 {
		t.Errorf("save fail: %d %v opened=%v", w.Code, m, ff.opened)
	}
	ff.saveErr = nil
	// Pending.
	ff.openRoom = hostkit.ChatRoom{Pending: true}
	w = e.post("chat", tok, map[string]string{"number": "+15557778888"})
	m = decode(t, w)
	if w.Code != 200 || m["pending"] != true || m["room_id"] != "" || !strings.Contains(m["message"].(string), "Creating the chat") {
		t.Errorf("pending: %d %v", w.Code, m)
	}
	// Generic host error: 500, logged scrubbed, never the detail to the client.
	ff.openErr = errBoom
	w = e.post("chat", tok, map[string]string{"number": "+15557778888"})
	m = decode(t, w)
	if w.Code != 500 || m["error"] != "internal" || m["message"] != "Something went wrong." || !strings.Contains(e.logs(), "Widget host feature failed") {
		t.Errorf("generic: %d %v", w.Code, m)
	}
	// Deadline.
	ff.openErr = context.DeadlineExceeded
	w = e.post("chat", tok, map[string]string{"number": "+15557778888"})
	if m = decode(t, w); w.Code != 503 || m["error"] != "unavailable" || m["message"] != "The bridge took too long. Try again." {
		t.Errorf("deadline: %d %v", w.Code, m)
	}
	ff.openErr = nil
	// Names without a saver: not supported (chat-only host).
	e2 := newEnv(t, withFeatures(chatOnly{ff}))
	tok2 := e2.session(tokAlice, roomPortal)
	if w = e2.post("chat", tok2, body); w.Code != 404 {
		t.Errorf("name without saver: %d", w.Code)
	}
	if w = e2.post("chat", tok2, map[string]string{"number": "+15557778888"}); w.Code != 200 {
		t.Errorf("number only: %d", w.Code)
	}
}

type chatOnly struct{ f *fakeFeatures }

func (c chatOnly) OpenChat(ctx context.Context, ul *bridgev2.UserLogin, e164 string) (hostkit.ChatRoom, error) {
	return c.f.OpenChat(ctx, ul, e164)
}

func TestContact(t *testing.T) {
	e, ff := newFeatEnv(t)
	tp := e.session(tokAlice, roomPortal)
	post := func(tok string, body any) (int, map[string]any) {
		e.clk.Advance(10 * time.Second)
		w := e.do(http.MethodPost, api+"contact", tok, body)
		return w.Code, decode(t, w)
	}
	// Room form uses the session's room number.
	code, m := post(tp, map[string]string{"name": "Zed"})
	if code != 200 || m["number"] != portalNumber || m["message"] != "Saved "+portalNumber+" as Zed." || m["changed"] != true {
		t.Errorf("room form: %d %v", code, m)
	}
	if ff.saved[0] != portalNumber+"|Zed" {
		t.Errorf("saved %v", ff.saved)
	}
	// Room form in the management room.
	tm := e.session(tokAlice, roomMgmt)
	if code, m = post(tm, map[string]string{"name": "Zed"}); code != 400 || m["error"] != "bad_request" {
		t.Errorf("mgmt room form: %d %v", code, m)
	}
	// Number form, rename, unchanged.
	ff.prev = "Old"
	code, m = post(tm, map[string]string{"number": "(555) 777-8888", "name": "New"})
	if code != 200 || m["number"] != "+15557778888" || m["previous_name"] != "Old" ||
		m["message"] != "Renamed Old to New (+15557778888)." {
		t.Errorf("rename: %d %v", code, m)
	}
	ff.unchanged = true
	if code, m = post(tm, map[string]string{"number": "+15557778888", "name": "New"}); code != 200 || m["changed"] != false ||
		m["message"] != "+15557778888 is already saved as New." {
		t.Errorf("unchanged: %d %v", code, m)
	}
	// Name required / invalid; number problems.
	for _, b := range []map[string]string{{}, {"number": "+15557778888"}, {"name": "a‮b"}} {
		if code, m = post(tm, b); code != 400 || m["error"] != "invalid_name" {
			t.Errorf("%v: %d %v", b, code, m)
		}
	}
	if code, m = post(tm, map[string]string{"number": "611", "name": "x"}); code != 400 || m["error"] != "invalid_number" {
		t.Errorf("short code: %d %v", code, m)
	}
	// A foreign room in the body is not accepted at all.
	if w := e.do(http.MethodPost, api+"contact", tm, `{"name":"x","room_id":"!x:y"}`); w.Code != 400 {
		t.Errorf("room_id in body: %d", w.Code)
	}
}

func TestCallWithName(t *testing.T) {
	e, ff := newFeatEnv(t)
	tok := e.session(tokAlice, roomPortal)
	// Save before place.
	w := e.post("call", tok, map[string]string{"number": "+1 737 727 3232", "name": "Zed"})
	m := decode(t, w)
	if w.Code != 200 || m["contact"].(map[string]any)["name"] != "Zed" || m["warning"] != nil {
		t.Fatalf("%d %v", w.Code, m)
	}
	if got := strings.Join(ff.order.get(), ","); got != "save,place" {
		t.Errorf("order %s", got)
	}
	if ff.saved[0] != "+17377273232|Zed" {
		t.Errorf("saved %v", ff.saved)
	}
	// Room form saves the room's number.
	e.post("call", tok, map[string]string{"name": "Yan"})
	if ff.saved[1] != portalNumber+"|Yan" {
		t.Errorf("room form saved %v", ff.saved)
	}
	// Save error: the call is still placed, with a warning.
	ff.saveErr = hostkit.NewActionError(hostkit.CodeUnavailable, "Not connected to Jabber.")
	n := len(e.calls.placed)
	w = e.post("call", tok, map[string]string{"name": "Xi"})
	m = decode(t, w)
	if w.Code != 200 || len(e.calls.placed) != n+1 || m["warning"] != "Could not save the contact: Not connected to Jabber." || m["contact"] != nil {
		t.Errorf("save error: %d %v", w.Code, m)
	}
	if strings.Contains(e.logs(), "Xi") && strings.Contains(e.logs(), `"Xi"`) {
		t.Error("name logged")
	}
	ff.saveErr = nil
	// Invalid name: 400, nothing happens.
	n, ns := len(e.calls.placed), len(ff.saved)
	w = e.post("call", tok, map[string]string{"name": "a\nb"})
	if w.Code != 400 || decode(t, w)["error"] != "invalid_name" || len(e.calls.placed) != n || len(ff.saved) != ns {
		t.Errorf("invalid name: %d %s", w.Code, w.Body.String())
	}
	// A refused number saves nothing.
	e.calls.checkErr = calls.ErrNotAllowed
	w = e.post("call", tok, map[string]string{"number": "+17377273232", "name": "Zed"})
	if w.Code != 400 || decode(t, w)["error"] != "not_allowed" || len(ff.saved) != ns {
		t.Errorf("refused: %d %s", w.Code, w.Body.String())
	}
	e.calls.checkErr = nil
	// Without a name: the old behaviour, no save and no extra fields.
	w = e.post("call", tok, "{}")
	m = decode(t, w)
	if w.Code != 200 || m["contact"] != nil || m["warning"] != nil || len(ff.saved) != ns {
		t.Errorf("regression: %d %v", w.Code, m)
	}
	// A host without a saver refuses names.
	e2 := newEnv(t, withFeatures(prefsOnly{}))
	if w = e2.post("call", e2.session(tokAlice, roomPortal), map[string]string{"name": "Zed"}); w.Code != 404 {
		t.Errorf("no saver: %d", w.Code)
	}
}

func TestPrefs(t *testing.T) {
	e, ff := newFeatEnv(t)
	ff.warning = "Use `$cmdprefix roster-only-calls status`."
	tok := e.session(tokAlice, roomMgmt)
	if w := e.do(http.MethodPost, api+"prefs/roster_only", tok, "{}"); w.Code != 400 || decode(t, w)["error"] != "bad_request" {
		t.Errorf("missing on: %d", w.Code)
	}
	w := e.do(http.MethodPost, api+"prefs/roster_only", tok, `{"on":true}`)
	m := decode(t, w)
	if w.Code != 200 || m["roster_only"] != true || !strings.Contains(m["message"].(string), "only for your contacts") ||
		m["warning"] != "Use `!sip roster-only-calls status`." {
		t.Errorf("on: %d %v", w.Code, m)
	}
	st := decode(t, e.do(http.MethodGet, api+"state", tok, nil))["prefs"].(map[string]any)
	if st["roster_only"] != true {
		t.Errorf("state after on: %v", st)
	}
	w = e.do(http.MethodPost, api+"prefs/roster_only", tok, `{"on":false}`)
	if m = decode(t, w); w.Code != 200 || m["roster_only"] != false || m["message"] != "Incoming calls now ring for everyone." {
		t.Errorf("off: %d %v", w.Code, m)
	}
	ff.setErr = errBoom
	if w = e.do(http.MethodPost, api+"prefs/roster_only", tok, `{"on":true}`); w.Code != 500 {
		t.Errorf("error: %d", w.Code)
	}
}

func TestNewLimiters(t *testing.T) {
	cases := []struct {
		path string
		n    int
		body any
	}{
		{"chat", 5, map[string]string{"number": "+15557778888"}},
		{"contact", 10, map[string]string{"number": "+15557778888", "name": "Z"}},
		{"prefs/roster_only", 10, map[string]any{"on": true}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			e, _ := newFeatEnv(t)
			tok := e.session(tokAlice, roomMgmt)
			for i := 0; i < c.n; i++ {
				if w := e.do(http.MethodPost, api+c.path, tok, c.body); w.Code != 200 {
					t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			w := e.do(http.MethodPost, api+c.path, tok, c.body)
			if w.Code != 429 || w.Header().Get("Retry-After") == "" {
				t.Errorf("limit: %d", w.Code)
			}
		})
	}
	// The janitor evicts them.
	e := newEnv(t)
	l := newLimits(e.clk)
	for _, x := range []*limiter{l.chat, l.contact, l.prefs} {
		x.allow("a")
	}
	e.clk.Advance(bucketIdle)
	l.evict()
	for i, x := range []*limiter{l.chat, l.contact, l.prefs} {
		if x.size() != 0 {
			t.Errorf("limiter %d not evicted", i)
		}
	}
}

func TestNormalizeChatOK(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomMgmt)
	m := decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "5551234567"}))
	if _, ok := m["chat_ok"]; ok {
		t.Errorf("chat_ok without features: %v", m)
	}
	e, _ = newFeatEnv(t)
	tok = e.session(tokAlice, roomMgmt)
	if m = decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "5551234567"})); m["ok"] != true || m["chat_ok"] != true {
		t.Errorf("ok: %v", m)
	}
	e.calls.checkErr = calls.ErrNotAllowed
	if m = decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "5551234567"})); m["ok"] != false ||
		m["error"] != "not_allowed" || m["chat_ok"] != true {
		t.Errorf("not allowed: %v", m)
	}
	e.calls.checkErr = nil
	if m = decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "611"})); m["ok"] != false || m["chat_ok"] != false {
		t.Errorf("short: %v", m)
	}
}

func TestFeatureBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bctx, bcancel := budget(ctx, time.Minute)
	defer bcancel()
	dl, _ := bctx.Deadline()
	if left := time.Until(dl); left > 1600*time.Millisecond || left < time.Second {
		t.Errorf("deadline left %v, want ~1.5s", left)
	}
	b2, c2 := budget(context.Background(), time.Second)
	defer c2()
	if dl, ok := b2.Deadline(); !ok || time.Until(dl) > time.Second {
		t.Error("no deadline without a parent")
	}
}

func TestFeatureTelemetry(t *testing.T) {
	ot := obstest.Setup(t)
	e, ff := newFeatEnv(t)
	const marker = "ZqxName"
	tok := e.session(tokAlice, roomPortal)
	ff.prev = marker + "Old"
	e.do(http.MethodPost, api+"chat", tok, map[string]string{"number": "+15557778888", "name": marker})
	e.do(http.MethodPost, api+"contact", tok, map[string]string{"name": marker})
	e.do(http.MethodPost, api+"prefs/roster_only", tok, `{"on":true}`)
	ff.saveErr = hostkit.NewActionError(hostkit.CodeUnavailable, "offline")
	e.do(http.MethodPost, api+"call", tok, map[string]string{"name": marker})
	ff.saveErr = errBoom
	e.do(http.MethodPost, api+"contact", tok, map[string]string{"name": marker})

	ch := ot.SpanByName(spanChat)
	if ch == nil || ch.Parent().IsValid() || ch.SpanKind() != trace.SpanKindServer ||
		!obstest.Attr(ch, "widget.chat.created").AsBool() || obstest.Attr(ch, "widget.chat.pending").AsBool() ||
		!obstest.Attr(ch, "widget.contact.saved").AsBool() || obstest.Attr(ch, "call.remote.e164").AsString() != "+15557778888" {
		t.Errorf("chat span %+v", ch)
	}
	var sawChanged bool
	for _, s := range ot.SpansByName(spanContact) {
		if obstest.Attr(s, "widget.contact.form").AsString() != "room" {
			t.Errorf("contact form %v", s.Attributes())
		}
		if obstest.Attr(s, "outcome").AsString() == outcomeOK {
			sawChanged = obstest.Attr(s, "widget.contact.changed").AsBool()
		}
	}
	if !sawChanged {
		t.Error("contact span: changed not set")
	}
	if p := ot.SpanByName(spanPrefs); p == nil || p.Parent().IsValid() || !obstest.Attr(p, "widget.prefs.roster_only").AsBool() {
		t.Errorf("prefs span %+v", p)
	}
	var callSaved bool
	for _, s := range ot.SpansByName(spanCall) {
		callSaved = callSaved || obstest.Attr(s, "widget.contact.saved").Type().String() == "BOOL"
	}
	if !callSaved {
		t.Error("call span: widget.contact.saved missing")
	}
	if ot.Sum(t, metricRequests, "widget.action", actChat, "outcome", mOK) != 1 ||
		ot.Sum(t, metricRequests, "widget.action", actContact, "outcome", mOK) != 1 ||
		ot.Sum(t, metricRequests, "widget.action", actContact, "outcome", mError) != 1 ||
		ot.Sum(t, metricRequests, "widget.action", actPrefs, "outcome", mOK) != 1 {
		t.Error("request metrics")
	}
	for _, r := range ot.Roots() {
		switch r.Name() {
		case spanSession, spanCall, spanChat, spanContact, spanPrefs:
		default:
			t.Errorf("orphan root %q", r.Name())
		}
	}
	obstest.AssertMetricAttrsAllowed(t, ot.Reader, widgetAllowlist())
	obstest.AssertNoSecrets(t, ot.Rec, e.logBuf, marker)
}

func TestMetricOutcomeUnavailable(t *testing.T) {
	for code, want := range map[string]string{hostkit.CodeUnavailable: mNotReady, hostkit.CodeInvalidName: mInvalid, hostkit.CodeNotSupported: mInvalid} {
		if got := metricOutcome(code); got != want {
			t.Errorf("%s: %s want %s", code, got, want)
		}
	}
}

// post is a POST to an API path that first advances the clock, so many requests stay under the limiters.
func (e *env) post(path, tok string, body any) *httptest.ResponseRecorder {
	e.clk.Advance(20 * time.Second)
	return e.do(http.MethodPost, api+path, tok, body)
}

// ---- contacts ----

func contactsPath(query string) string {
	if query == "" {
		return api + "contacts"
	}
	return api + "contacts?" + query
}

func TestContactsNotSupportedAndAuth(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 404 || decode(t, w)["error"] != hostkit.CodeNotSupported {
		t.Errorf("without features: %d %s", w.Code, w.Body.String())
	}
	e = newEnv(t, withFeatures(prefsOnly{}))
	tok = e.session(tokAlice, roomPortal)
	if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 404 {
		t.Errorf("prefsOnly: %d", w.Code)
	}
	e, ff := newFeatEnv(t)
	if w := e.do(http.MethodGet, contactsPath(""), "", nil); w.Code != 401 {
		t.Errorf("no session: %d", w.Code)
	}
	if w := e.do(http.MethodGet, contactsPath(""), "not-a-session", nil); w.Code != 401 {
		t.Errorf("bad session: %d", w.Code)
	}
	// Login not ready (bob has no login): the standard refusal.
	if w := e.do(http.MethodGet, contactsPath(""), e.session(tokBob, roomBobMgmt), nil); w.Code != 403 || decode(t, w)["error"] != codeLoginRequired {
		t.Errorf("not ready: %d %s", w.Code, w.Body.String())
	}
	if n := len(ff.listCalls); n != 0 {
		t.Errorf("host called %d times for refused requests", n)
	}
	if w := e.do(http.MethodPost, contactsPath(""), e.session(tokAlice, roomPortal), map[string]string{}); w.Code != 405 || w.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST: %d allow %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestContactsParams(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name, query string
		want        int
		call        listCall // checked when want == 200
	}{
		{"defaults", "", 200, listCall{limit: 25}},
		{"explicit", "q=ali&offset=50&limit=10", 200, listCall{q: "ali", offset: 50, limit: 10}},
		{"trim", "q=%20ali%20", 200, listCall{q: "ali", limit: 25}},
		{"control stripped", "q=a%00l%0Ai", 200, listCall{q: "ali", limit: 25}},
		{"q 64 runes", "q=" + strings.Repeat("%C3%A9", 64), 200, listCall{q: strings.Repeat("é", 64), limit: 25}},
		{"q 65 runes", "q=" + long, 400, listCall{}},
		{"q invalid utf8", "q=%FF", 400, listCall{}},
		{"limit max", "limit=50", 200, listCall{limit: 50}},
		{"limit 0", "limit=0", 400, listCall{}},
		{"limit 51", "limit=51", 400, listCall{}},
		{"limit abc", "limit=abc", 400, listCall{}},
		{"offset -1", "offset=-1", 400, listCall{}},
		{"offset max", "offset=10000", 200, listCall{offset: 10000, limit: 25}},
		{"offset huge", "offset=10001", 400, listCall{}},
		{"offset overflow", "offset=99999999999999999999", 400, listCall{}},
		{"unknown ignored", "zzz=1", 200, listCall{limit: 25}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, ff := newFeatEnv(t)
			ff.listPage = hostkit.ContactPage{Loaded: true}
			w := e.do(http.MethodGet, contactsPath(c.query), e.session(tokAlice, roomPortal), nil)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want != 200 {
				if decode(t, w)["error"] != codeBadRequest || len(ff.listCalls) != 0 {
					t.Errorf("error %s, host calls %d", w.Body.String(), len(ff.listCalls))
				}
				return
			}
			if len(ff.listCalls) != 1 {
				t.Fatalf("host calls %d", len(ff.listCalls))
			}
			got := ff.listCalls[0]
			if got.ul != e.login || got.q != c.call.q || got.offset != c.call.offset || got.limit != c.call.limit {
				t.Errorf("host call %+v, want %+v", got, c.call)
			}
		})
	}
}

func TestContactsResponse(t *testing.T) {
	e, ff := newFeatEnv(t)
	tok := e.session(tokAlice, roomPortal)
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 7, Items: []hostkit.ContactEntry{
		{Name: "Alice", E164: "+15550000001"},
		{Name: "Bob‮  B", E164: "+15550000002"},
		{Name: strings.Repeat("é", 70), E164: "+15550000003"},
		{Name: "‮\x00", E164: "+15550000004"}, // empty after cleaning: dropped
	}}
	w := e.do(http.MethodGet, contactsPath("limit=4&offset=2"), tok, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d cache %q", w.Code, w.Header().Get("Cache-Control"))
	}
	m := decode(t, w)
	items := m["items"].([]any)
	if m["ok"] != true || m["loaded"] != true || m["total"] != float64(7) || len(items) != 3 {
		t.Fatalf("resp %v", m)
	}
	if m["next_offset"] != float64(6) { // offset 2 + 4 host items
		t.Errorf("next_offset %v", m["next_offset"])
	}
	for i, want := range []map[string]string{
		{"name": "Alice", "number": "+15550000001"},
		{"name": "Bob B", "number": "+15550000002"},
		{"name": strings.Repeat("é", 64), "number": "+15550000003"},
	} {
		it := items[i].(map[string]any)
		if it["name"] != want["name"] || it["number"] != want["number"] {
			t.Errorf("item %d: %v", i, it)
		}
		if _, ok := it["jid"]; ok || len(it) != 2 {
			t.Errorf("item %d has unexpected fields: %v", i, it)
		}
	}
	// Last page: next_offset is null.
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 2, Items: ff.listPage.Items[:1]}
	m = decode(t, e.do(http.MethodGet, contactsPath("offset=1"), tok, nil))
	if v, present := m["next_offset"]; !present || v != nil {
		t.Errorf("next_offset %v present=%v", v, present)
	}
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 1, Items: ff.listPage.Items[:1]}
	if m = decode(t, e.do(http.MethodGet, contactsPath(""), tok, nil)); m["next_offset"] != nil {
		t.Errorf("single page next_offset %v", m["next_offset"])
	}
	// Offset beyond the end: no items, correct total, empty array (not null).
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 3}
	m = decode(t, e.do(http.MethodGet, contactsPath("offset=100"), tok, nil))
	if it, ok := m["items"].([]any); !ok || len(it) != 0 || m["total"] != float64(3) || m["next_offset"] != nil {
		t.Errorf("past end: %v", m)
	}
	// Not loaded: passthrough, items dropped.
	ff.listPage = hostkit.ContactPage{Loaded: false, Total: 9, Items: []hostkit.ContactEntry{{Name: "X", E164: "+15550000009"}}}
	m = decode(t, e.do(http.MethodGet, contactsPath(""), tok, nil))
	if m["loaded"] != false || m["total"] != float64(0) || len(m["items"].([]any)) != 0 || m["next_offset"] != nil {
		t.Errorf("not loaded: %v", m)
	}
	// A host returning more than limit is capped.
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 5, Items: []hostkit.ContactEntry{
		{Name: "A", E164: "+15550000001"}, {Name: "B", E164: "+15550000002"}, {Name: "C", E164: "+15550000003"}}}
	m = decode(t, e.do(http.MethodGet, contactsPath("limit=2"), tok, nil))
	if len(m["items"].([]any)) != 2 || m["next_offset"] != float64(2) { // the next page starts after the shown items
		t.Errorf("cap: %v next %v", m["items"], m["next_offset"])
	}
	// Dropped entries still advance next_offset; a page whose entries were all dropped keeps paging.
	ff.listPage = hostkit.ContactPage{Loaded: true, Total: 5, Items: []hostkit.ContactEntry{
		{Name: "\u202e", E164: "+15550000001"}, {Name: "", E164: "+15550000002"}}}
	m = decode(t, e.do(http.MethodGet, contactsPath("limit=2&offset=1"), tok, nil))
	if len(m["items"].([]any)) != 0 || m["next_offset"] != float64(3) {
		t.Errorf("all dropped: %v next %v", m["items"], m["next_offset"])
	}
}

func TestContactsHostErrors(t *testing.T) {
	e, ff := newFeatEnv(t)
	tok := e.session(tokAlice, roomPortal)
	ff.listErr = hostkit.NewActionError(hostkit.CodeUnavailable, "Offline.")
	if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 503 || decode(t, w)["error"] != hostkit.CodeUnavailable {
		t.Errorf("ActionError: %d %s", w.Code, w.Body.String())
	}
	ff.listErr = context.DeadlineExceeded
	if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 503 {
		t.Errorf("deadline: %d", w.Code)
	}
	ff.listErr = errors.New("boom")
	if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 500 || decode(t, w)["error"] != codeInternal {
		t.Errorf("internal: %d %s", w.Code, w.Body.String())
	}
}

func TestContactsRateLimitPerUser(t *testing.T) {
	e, ff := newFeatEnv(t)
	ff.listPage = hostkit.ContactPage{Loaded: true}
	t1 := e.session(tokAlice, roomPortal)
	t2 := e.session(tokAlice, roomMgmt) // a second session of the same user shares the bucket
	for i := 0; i < 10; i++ {
		tok := t1
		if i%2 == 1 {
			tok = t2
		}
		if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	for _, tok := range []string{t1, t2} {
		if w := e.do(http.MethodGet, contactsPath(""), tok, nil); w.Code != 429 || w.Header().Get("Retry-After") == "" {
			t.Errorf("11th request: %d", w.Code)
		}
	}
	e.clk.Advance(time.Second)
	if w := e.do(http.MethodGet, contactsPath(""), t1, nil); w.Code != 200 {
		t.Errorf("after refill: %d", w.Code)
	}
}

func TestContactsDeviceSession(t *testing.T) {
	ff := &fakeFeatures{order: &orderLog{}, listPage: hostkit.ContactPage{Loaded: true}}
	p := newPWAEnv(t, withFeatures(ff))
	_, tok := p.enroll()
	s := p.exchange(tok).Session
	if f := decode(t, p.do(http.MethodGet, api+"state", s, nil))["features"].(map[string]any); f["contacts"] != true {
		t.Errorf("features %v", f)
	}
	if w := p.do(http.MethodGet, contactsPath("q=x"), s, nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(ff.listCalls) != 1 || ff.listCalls[0].ul != p.login {
		t.Errorf("host calls %+v, want the device's own login", ff.listCalls)
	}
}
