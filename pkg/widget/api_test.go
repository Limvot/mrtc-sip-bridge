package widget

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

const api = "/sip-dialpad/api/v1/"

func TestStateViews(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	m := decode(t, e.do(http.MethodGet, api+"state", tok, nil))
	if m["call"] != nil || m["login"].(map[string]any)["ready"] != true || m["room"].(map[string]any)["number"] != portalNumber {
		t.Fatalf("idle state %v", m)
	}
	ans := t0.Add(5 * time.Second)
	e.calls.snap = &calls.Snapshot{ID: "c9", Direction: "outbound", State: "active", Outbound: true, RoomID: roomPortal,
		Remote: portalNumber, StartedAt: t0, AnsweredAt: ans}
	m = decode(t, e.do(http.MethodGet, api+"state", tok, nil))
	c := m["call"].(map[string]any)
	if c["phase"] != "connected" || c["can_dtmf"] != true || c["in_this_room"] != true || c["number"] != portalNumber ||
		c["answered_at"] != ans.Format(time.RFC3339) || c["direction"] != "outbound" {
		t.Errorf("call view %v", c)
	}
	e.calls.snap = &calls.Snapshot{ID: "c9", Direction: "outbound", State: "ringing", Outbound: true, RoomID: roomOther, StartedAt: t0}
	c = decode(t, e.do(http.MethodGet, api+"state", tok, nil))["call"].(map[string]any)
	if c["phase"] != "ringing_you" || c["can_dtmf"] != false || c["in_this_room"] != false || c["answered_at"] != nil {
		t.Errorf("ringing view %v", c)
	}
	// Bob's management room: session ok, login not ready, with the refusal text.
	tb := e.session(tokBob, roomBobMgmt)
	m = decode(t, e.do(http.MethodGet, api+"state", tb, nil))
	l := m["login"].(map[string]any)
	if l["ready"] != false || l["message"] != "No SIP account is set up for this login." || m["call"] != nil {
		t.Errorf("bob state %v", m)
	}
	// Manager not started: not ready.
	e.noMgr = true
	l = decode(t, e.do(http.MethodGet, api+"state", tok, nil))["login"].(map[string]any)
	if l["ready"] != false || l["message"] != msgNotReady {
		t.Errorf("no manager %v", l)
	}
}

func TestPhaseMapping(t *testing.T) {
	cases := []struct {
		state string
		out   bool
		want  string
	}{
		{"setup", true, "ringing_you"}, {"ringing", true, "ringing_you"}, {"joining", true, "connecting"},
		{"dialing", true, "calling"}, {"early", true, "ringing_them"}, {"active", true, "connected"},
		{"ringing", false, "incoming"}, {"answering", false, "incoming"}, {"joining", false, "connecting"},
		{"active", false, "connected"}, {"ending", false, "ending"},
	}
	for _, c := range cases {
		if got := phaseOf(calls.Snapshot{State: c.state, Outbound: c.out}); got != c.want {
			t.Errorf("%s/%v: %s want %s", c.state, c.out, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomMgmt)
	m := decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "(555) 123-4567"}))
	if m["ok"] != true || m["e164"] != "+15551234567" {
		t.Errorf("normalize %v", m)
	}
	w := e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "12"})
	m = decode(t, w)
	if w.Code != 200 || m["ok"] != false || m["error"] != "invalid_number" || m["message"] == "" {
		t.Errorf("invalid %d %v", w.Code, m)
	}
	e.calls.checkErr = calls.ErrNotAllowed
	m = decode(t, e.do(http.MethodPost, api+"normalize", tok, map[string]string{"number": "+15551234567"}))
	if m["ok"] != false || m["error"] != "not_allowed" {
		t.Errorf("not allowed %v", m)
	}
	if len(e.calls.placed) != 0 {
		t.Error("normalize placed a call")
	}
}

func TestCallForms(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	w := e.do(http.MethodPost, api+"call", tok, "{}")
	m := decode(t, w)
	if w.Code != 200 || m["ok"] != true || m["number"] != portalNumber {
		t.Fatalf("room call %d %v", w.Code, m)
	}
	if p := e.calls.placed[0]; p.RoomID != roomPortal || p.Number != "" {
		t.Errorf("room form req %+v", p)
	}
	w = e.do(http.MethodPost, api+"call", tok, map[string]string{"number": "+1 737 727 3232"})
	if w.Code != 200 || decode(t, w)["number"] != "+17377273232" {
		t.Fatalf("number call %d %s", w.Code, w.Body.String())
	}
	if p := e.calls.placed[1]; p.Number != "+1 737 727 3232" || p.RoomID != "" {
		t.Errorf("number form req %+v", p)
	}
}

func TestActionErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		set    func(f *fakeCalls)
		path   string
		body   any
		status int
		code   string
	}{
		{"busy", func(f *fakeCalls) { f.placeErr = calls.ErrLineBusy }, "call", "{}", 409, "line_busy"},
		{"self", func(f *fakeCalls) { f.placeErr = calls.ErrSelfDial }, "call", map[string]string{"number": "+15550001111"}, 400, "self_dial"},
		{"disabled", func(f *fakeCalls) { f.placeErr = calls.ErrOutboundDisabled }, "call", "{}", 503, "outbound_disabled"},
		{"room precheck", func(f *fakeCalls) { f.checkErr = calls.ErrNotAllowed }, "call", "{}", 400, "not_allowed"},
		{"no call", func(f *fakeCalls) { f.hangErr = calls.ErrNoActiveCall }, "hangup", "{}", 409, "no_active_call"},
		{"hangup internal", func(f *fakeCalls) { f.hangErr = errBoom }, "hangup", "{}", 500, "internal"},
		{"not connected", func(f *fakeCalls) { f.dtmfErr = calls.ErrNotConnected }, "dtmf", map[string]string{"digits": "1"}, 409, "not_connected"},
		{"dtmf busy", func(f *fakeCalls) { f.dtmfErr = media.ErrDTMFBusy }, "dtmf", map[string]string{"digits": "1"}, 409, "dtmf_busy"},
		{"dtmf unsupported", func(f *fakeCalls) { f.dtmfErr = media.ErrDTMFUnsupported }, "dtmf", map[string]string{"digits": "1"}, 409, "dtmf_unsupported"},
		{"bad digits", nil, "dtmf", map[string]string{"digits": "12x"}, 400, "invalid_digits"},
		{"too many digits", nil, "dtmf", map[string]string{"digits": "123456789012345678901234567890123"}, 400, "invalid_digits"},
		{"empty digits", nil, "dtmf", "{}", 400, "invalid_digits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.set != nil {
				c.set(e.calls)
			}
			tok := e.session(tokAlice, roomPortal)
			w := e.do(http.MethodPost, api+c.path, tok, c.body)
			m := decode(t, w)
			if w.Code != c.status || m["error"] != c.code || m["message"] == "" {
				t.Errorf("%d %v, want %d %s", w.Code, m, c.status, c.code)
			}
		})
	}
}

func TestDTMFAndNotReady(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	w := e.do(http.MethodPost, api+"dtmf", tok, map[string]string{"digits": "12#"})
	if w.Code != 200 || decode(t, w)["count"].(float64) != 3 || e.calls.dtmfRoom[0] != roomPortal {
		t.Fatalf("dtmf %d %s", w.Code, w.Body.String())
	}
	e.noMgr = true
	w = e.do(http.MethodPost, api+"hangup", tok, "{}")
	if w.Code != 503 || decode(t, w)["error"] != codeNotReady {
		t.Errorf("not ready %d %s", w.Code, w.Body.String())
	}
	tb := e.session(tokBob, roomBobMgmt)
	w = e.do(http.MethodPost, api+"call", tb, map[string]string{"number": "+15551112222"})
	if w.Code != 403 || decode(t, w)["error"] != codeLoginRequired {
		t.Errorf("bob call %d %s", w.Code, w.Body.String())
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	// call: 5/min per user.
	for i := 0; i < 5; i++ {
		e.do(http.MethodPost, api+"call", tok, "{}")
	}
	w := e.do(http.MethodPost, api+"call", tok, "{}")
	if w.Code != 429 || decode(t, w)["error"] != codeRateLimited {
		t.Fatalf("call limit: %d", w.Code)
	}
	ra, _ := strconv.Atoi(w.Header().Get("Retry-After"))
	if ra < 1 || ra > 60 {
		t.Errorf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	e.clk.Advance(13 * time.Second)
	if w := e.do(http.MethodPost, api+"call", tok, "{}"); w.Code != 200 {
		t.Errorf("after refill: %d", w.Code)
	}
	// state: 4/s per session.
	for i := 0; i < 4; i++ {
		if w := e.do(http.MethodGet, api+"state", tok, nil); w.Code != 200 {
			t.Fatalf("state %d: %d", i, w.Code)
		}
	}
	if w := e.do(http.MethodGet, api+"state", tok, nil); w.Code != 429 {
		t.Errorf("state limit: %d", w.Code)
	}
	// dtmf: burst 32.
	n := 0
	for i := 0; i < 40; i++ {
		if e.do(http.MethodPost, api+"dtmf", tok, map[string]string{"digits": "1"}).Code == 200 {
			n++
		}
	}
	if n != 32 {
		t.Errorf("dtmf burst %d", n)
	}
}

func TestSessionRateLimitsPerIP(t *testing.T) {
	e := newEnv(t)
	ip := "203.0.113.20:5"
	for i := 0; i < 10; i++ {
		w := e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal)), "remote", ip)
		if w.Code != 200 {
			t.Fatalf("session %d: %d", i, w.Code)
		}
	}
	w := e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal)), "remote", ip)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("ip limit: %d", w.Code)
	}
	// Failure lockout: 5 failures, then 15 minutes locked even with good credentials.
	bad := "203.0.113.21:5"
	for i := 0; i < 5; i++ {
		e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, "wrongwrongwrongwrongwr"), "remote", bad)
	}
	good := sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal))
	if w := e.do(http.MethodPost, sessPath, "", good, "remote", bad); w.Code != 429 {
		t.Fatalf("lockout: %d", w.Code)
	}
	e.clk.Advance(14 * time.Minute)
	if w := e.do(http.MethodPost, sessPath, "", good, "remote", bad); w.Code != 429 {
		t.Fatalf("still locked: %d", w.Code)
	}
	e.clk.Advance(time.Minute + time.Second)
	if w := e.do(http.MethodPost, sessPath, "", good, "remote", bad); w.Code != 200 {
		t.Fatalf("after lockout: %d %s", w.Code, w.Body.String())
	}
}

func TestLimiterEviction(t *testing.T) {
	e := newEnv(t)
	l := newLimiter(e.clk, 1, time.Minute, 1)
	l.allow("a")
	l.allow("b")
	e.clk.Advance(bucketIdle)
	l.evict()
	if l.size() != 0 {
		t.Errorf("idle buckets kept: %d", l.size())
	}
	f := newLimiter(e.clk, 1, time.Minute, 1)
	f.lockout = failLockout
	f.fail("x")
	if locked, _ := f.locked("x"); !locked {
		t.Fatal("not locked")
	}
	e.clk.Advance(failLockout)
	if locked, _ := f.locked("x"); locked {
		t.Fatal("still locked")
	}
	e.clk.Advance(bucketIdle)
	f.evict()
	if f.size() != 0 {
		t.Error("expired lockout bucket kept")
	}
}

func TestRefusalExpandsCmdPrefix(t *testing.T) {
	var e *env
	e = newEnv(t, func(_ *hostkit.WidgetConfig, d *Deps) {
		d.CommandPrefix = "!sip"
		d.ResolveRoom = func(ctx context.Context, u *bridgev2.User, r id.RoomID) (RoomContext, error) {
			rc, err := e.resolve(ctx, u, r)
			if rc.Refusal != "" {
				rc.Refusal = "Use `$cmdprefix login` first."
			}
			return rc, err
		}
	})
	tb := e.session(tokBob, roomBobMgmt)
	m := decode(t, e.do(http.MethodGet, api+"state", tb, nil))
	if got := m["login"].(map[string]any)["message"]; got != "Use `!sip login` first." {
		t.Errorf("state refusal = %v", got)
	}
}
