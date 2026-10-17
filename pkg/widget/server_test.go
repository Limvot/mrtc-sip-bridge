package widget

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

const sessPath = "/sip-dialpad/api/v1/session"

func TestNewValidation(t *testing.T) {
	e := newEnv(t)
	base := e.s.d
	cases := map[string]func(*hostkit.WidgetConfig, *Deps){
		"disabled":       func(c *hostkit.WidgetConfig, _ *Deps) { c.Enabled = false },
		"root prefix":    func(c *hostkit.WidgetConfig, _ *Deps) { c.PublicURL = "https://dev.example/" },
		"matrix prefix":  func(c *hostkit.WidgetConfig, _ *Deps) { c.PublicURL = "https://dev.example/_matrix/x/" },
		"no slash":       func(c *hostkit.WidgetConfig, _ *Deps) { c.PublicURL = "https://dev.example/dialpad" },
		"short secret":   func(c *hostkit.WidgetConfig, _ *Deps) { c.Secret = "short" },
		"no resolve":     func(_ *hostkit.WidgetConfig, d *Deps) { d.ResolveRoom = nil },
		"no server name": func(_ *hostkit.WidgetConfig, d *Deps) { d.ServerName = "" },
		"bad hs url":     func(_ *hostkit.WidgetConfig, d *Deps) { d.HomeserverURL = "ftp://x" },
		"no as id":       func(_ *hostkit.WidgetConfig, d *Deps) { d.AppserviceID = "" },
	}
	for name, mod := range cases {
		cfg, d := testCfg(), base
		mod(&cfg, &d)
		if s, err := New(cfg, d); err == nil {
			s.Stop()
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestRoutingAndHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodGet, "/sip-dialpad/", "", nil)
	if w.Code != 200 {
		t.Fatalf("index: %d", w.Code)
	}
	h := w.Header()
	if got := h.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("index content type %q", got)
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors *", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if h.Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options must not be set")
	}
	for _, p := range []string{"/sip-dialpad/", "/sip-dialpad/static/app.js", "/sip-dialpad/api/v1/state"} {
		w := e.do(http.MethodGet, p, "", nil)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing nosniff/referrer headers", p)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header sent", p)
		}
	}
	if w := e.do(http.MethodGet, "/sip-dialpad/static/app.js", "", nil); w.Code != 200 ||
		w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("app.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := e.do(http.MethodGet, "/sip-dialpad/static/app.css", "", nil); w.Code != 200 ||
		w.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("app.css: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := e.do(http.MethodHead, "/sip-dialpad/", "", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d len %d", w.Code, w.Body.Len())
	}
	w = e.do(http.MethodGet, "/sip-dialpad/api/v1/state", "", nil)
	if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("state unauth: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/sip-dialpad/x", "/sip-dialpad/static/", "/sip-dialpad/static/../server.go",
		"/sip-dialpad/static/nope.js", "/sip-dialpad/api/v1/nope", "/sip-dialpad/api/v2/state", "/other/", "/_matrix/app/v1/ping"} {
		if w := e.do(http.MethodGet, p, "", nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := e.do(http.MethodPost, "/sip-dialpad/", "", "{}"); w.Code != 405 {
		t.Errorf("POST index: %d", w.Code)
	}
	if w := e.do(http.MethodGet, sessPath, "", nil); w.Code != 405 {
		t.Errorf("GET session: %d", w.Code)
	}
}

func TestMountOnMux(t *testing.T) {
	e := newEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/app/v1/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) })
	if err := MountOn(mux, e.s); err != nil {
		t.Fatal(err)
	}
	if err := MountOn(mux, e.s); err == nil {
		t.Error("double mount should fail, not panic")
	}
	for p, want := range map[string]int{"/sip-dialpad/": 200, "/_matrix/app/v1/ping": 299, "/elsewhere": 404} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != want {
			t.Errorf("%s: %d want %d", p, w.Code, want)
		}
	}
}

func TestPostChecks(t *testing.T) {
	e := newEnv(t)
	body := sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal))
	if w := e.do(http.MethodPost, sessPath, "", body, "Origin", "https://evil.example"); w.Code != 403 {
		t.Errorf("cross origin: %d", w.Code)
	}
	if w := e.do(http.MethodPost, sessPath, "", body, "Origin", "null"); w.Code != 403 {
		t.Errorf("null origin: %d", w.Code)
	}
	if w := e.do(http.MethodPost, sessPath, "", body, "Content-Type", "text/plain"); w.Code != 415 {
		t.Errorf("text/plain: %d", w.Code)
	}
	if w := e.do(http.MethodPost, sessPath, "", body, "Content-Type", ""); w.Code != 415 {
		t.Errorf("no content type: %d", w.Code)
	}
	big := `{"room_id":"` + strings.Repeat("a", 5000) + `"}`
	if w := e.do(http.MethodPost, sessPath, "", big); w.Code != 413 {
		t.Errorf("big body: %d", w.Code)
	}
	if w := e.do(http.MethodPost, sessPath, "", `{"room_id":"!x:y","extra":1}`, "remote", "192.0.2.50:1"); w.Code != 400 {
		t.Errorf("unknown field: %d", w.Code)
	}
	if w := e.do(http.MethodPost, sessPath, "", `{} {}`, "remote", "192.0.2.51:1"); w.Code != 400 {
		t.Errorf("trailing data: %d", w.Code)
	}
	// Same origin is fine.
	if w := e.do(http.MethodPost, sessPath, "", body, "Origin", testOrigin, "remote", "192.0.2.52:1"); w.Code != 200 {
		t.Errorf("same origin: %d %s", w.Code, w.Body.String())
	}
}

func TestSessionAuthMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*hostkit.WidgetConfig, *Deps)
		tok    string
		server string
		room   id.RoomID
		key    func(e *env) string
		status int
		code   string
	}{
		{name: "ok portal", tok: tokAlice, room: roomPortal, status: 200},
		{name: "ok management", tok: tokAlice, room: roomMgmt, status: 200},
		{name: "management without login", tok: tokBob, room: roomBobMgmt, status: 200},
		{name: "wrong key", tok: tokAlice, room: roomPortal, key: func(*env) string { return strings.Repeat("A", keyLen) }, status: 401, code: codeUnauthorized},
		{name: "missing key", tok: tokAlice, room: roomPortal, key: func(*env) string { return "" }, status: 401, code: codeUnauthorized},
		{name: "other room's key", tok: tokAlice, room: roomPortal, key: func(e *env) string { return e.key(roomMgmt) }, status: 401, code: codeUnauthorized},
		{name: "bad openid", tok: "nope", room: roomPortal, status: 401, code: codeUnauthorized},
		{name: "empty openid", tok: "", room: roomPortal, status: 401, code: codeUnauthorized},
		{name: "invalid server name", tok: tokAlice, server: "bad server!", room: roomPortal, status: 401, code: codeUnauthorized},
		{name: "sub on another server", tok: tokOther, room: roomPortal, status: 401, code: codeUnauthorized},
		{name: "remote not allowed", tok: "remote-ok", server: "other.example", room: roomPortal, status: 403, code: codeForbidden},
		{name: "remote allowed but no user", tok: "remote-ok", server: "other.example", room: roomPortal, status: 403, code: codeForbidden,
			mod: func(c *hostkit.WidgetConfig, _ *Deps) { c.AllowRemoteUsers = true }},
		{name: "remote allowed bad token", tok: "remote-bad", server: "other.example", room: roomPortal, status: 401, code: codeUnauthorized,
			mod: func(c *hostkit.WidgetConfig, _ *Deps) { c.AllowRemoteUsers = true }},
		{name: "no bridge user", tok: tokDave, room: roomPortal, status: 403, code: codeForbidden},
		{name: "no command permission", tok: tokCarol, room: roomPortal, status: 403, code: codeForbidden},
		{name: "room not owned", tok: tokBob, room: roomPortal, status: 403, code: codeForbidden},
		{name: "other room", tok: tokAlice, room: roomOther, status: 403, code: codeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*hostkit.WidgetConfig, *Deps)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			e := newEnv(t, mods...)
			server := tc.server
			if server == "" {
				server = testServer
			}
			key := e.key(tc.room)
			if tc.key != nil {
				key = tc.key(e)
			}
			w := e.do(http.MethodPost, sessPath, "", sessBody(tc.tok, server, tc.room, key))
			if w.Code != tc.status {
				t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			m := decode(t, w)
			if tc.status == 200 {
				if s, _ := m["session"].(string); len(s) != sessionTokenLen {
					t.Errorf("session %q", s)
				}
				return
			}
			if m["error"] != tc.code {
				t.Errorf("error %v want %s", m["error"], tc.code)
			}
		})
	}
}

func TestSessionResponseAndRoomMessage(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal)))
	m := decode(t, w)
	room := m["room"].(map[string]any)
	if room["kind"] != KindPortal || room["number"] != portalNumber || m["expires_in"].(float64) != 3600 {
		t.Errorf("resp %v", m)
	}
	w = e.do(http.MethodPost, sessPath, "", sessBody(tokAlice, testServer, roomOther, e.key(roomOther)))
	if decode(t, w)["message"] != MsgNotBridgeRoom {
		t.Errorf("refusal text: %s", w.Body.String())
	}
}

func TestOpenIDTimeoutIsTransient(t *testing.T) {
	old := verifyTimeout
	verifyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { verifyTimeout = old })
	e := newEnv(t)
	e.s.hc = newUserinfoClient(e.hs.srv.Client().Transport)
	w := e.do(http.MethodPost, sessPath, "", sessBody(tokSlow, testServer, roomPortal, e.key(roomPortal)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout: %d %s", w.Code, w.Body.String())
	}
	if e.s.neg.has(tokenHash(testServer, tokSlow)) {
		t.Error("transient failure must not be cached")
	}
}

func TestNegativeCache(t *testing.T) {
	e := newEnv(t)
	body := sessBody("bad-token", testServer, roomPortal, e.key(roomPortal))
	e.do(http.MethodPost, sessPath, "", body)
	n := e.hs.hitCount()
	e.do(http.MethodPost, sessPath, "", body)
	if e.hs.hitCount() != n {
		t.Error("rejected token hit the homeserver again")
	}
	e.clk.Advance(negCacheTTL + time.Second)
	e.clk.Advance(failLockout) // let the IP lockout pass
	e.do(http.MethodPost, sessPath, "", body)
	if e.hs.hitCount() != n+1 {
		t.Error("negative cache did not expire")
	}
}

func TestSessionExpiryAndCaps(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	if w := e.do(http.MethodGet, "/sip-dialpad/api/v1/state", tok, nil); w.Code != 200 {
		t.Fatalf("state: %d", w.Code)
	}
	e.clk.Advance(maxSessionTTL)
	if w := e.do(http.MethodGet, "/sip-dialpad/api/v1/state", tok, nil); w.Code != 401 {
		t.Fatalf("expired: %d", w.Code)
	}
	// Short OpenID lifetime caps the session.
	b := sessBody(tokAlice, testServer, roomPortal, e.key(roomPortal))
	b["openid"].(map[string]any)["expires_in"] = 60
	w := e.do(http.MethodPost, sessPath, "", b, "remote", "192.0.2.9:1")
	if decode(t, w)["expires_in"].(float64) != 60 {
		t.Errorf("expires_in %s", w.Body.String())
	}

	// Per-user cap: the oldest session is evicted.
	st := newSessionStore(e.clk)
	var toks []string
	for i := 0; i < maxSessionsUser+1; i++ {
		tk, _, err := st.create(alice, roomPortal, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tk)
		e.clk.Advance(time.Millisecond)
	}
	if st.get(toks[0]) != nil || st.get(toks[1]) == nil || st.count() != maxSessionsUser {
		t.Errorf("per-user eviction: count %d", st.count())
	}
	// Global cap.
	for i := 0; i < maxSessionsTotal+5; i++ {
		if _, _, err := st.create(id.UserID(fmt.Sprintf("@u%d:hs.example", i)), roomPortal, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if st.count() > maxSessionsTotal {
		t.Errorf("global cap: %d", st.count())
	}
	if st.get("short") != nil || st.get(strings.Repeat("!", sessionTokenLen)) != nil {
		t.Error("malformed tokens must not resolve")
	}
}

func TestSessionBindsRoom(t *testing.T) {
	e := newEnv(t)
	tokMgmt := e.session(tokAlice, roomMgmt)
	tokPortal := e.session(tokAlice, roomPortal)
	// The portal session targets its room; the management session targets "" (any room).
	e.do(http.MethodPost, "/sip-dialpad/api/v1/hangup", tokPortal, "{}")
	e.do(http.MethodPost, "/sip-dialpad/api/v1/hangup", tokMgmt, "{}")
	if len(e.calls.hangups) != 2 || e.calls.hangups[0] != roomPortal || e.calls.hangups[1] != "" {
		t.Fatalf("hangups %v", e.calls.hangups)
	}
	// A request cannot switch rooms: room_id is an unknown field on actions.
	if w := e.do(http.MethodPost, "/sip-dialpad/api/v1/hangup", tokMgmt, `{"room_id":"!portal:hs.example"}`); w.Code != 400 {
		t.Errorf("room switch: %d", w.Code)
	}
	// "call {}" in the management room has no room number.
	if w := e.do(http.MethodPost, "/sip-dialpad/api/v1/call", tokMgmt, "{}"); w.Code != 400 {
		t.Errorf("call {} in management: %d", w.Code)
	}
	// A modified session token is rejected.
	forged := "x" + tokPortal[1:]
	if tokPortal[0] == 'x' {
		forged = "y" + tokPortal[1:]
	}
	if w := e.do(http.MethodGet, "/sip-dialpad/api/v1/state", forged, nil); w.Code != 401 {
		t.Errorf("forged token: %d", w.Code)
	}
}

func TestLogoutTakesEffect(t *testing.T) {
	e := newEnv(t)
	tok := e.session(tokAlice, roomPortal)
	e.loggedOut = true
	w := e.do(http.MethodPost, "/sip-dialpad/api/v1/call", tok, "{}")
	if w.Code != 403 || decode(t, w)["error"] != codeLoginRequired {
		t.Fatalf("after logout: %d %s", w.Code, w.Body.String())
	}
	e.users[alice].Permissions.Commands = false
	if w := e.do(http.MethodGet, "/sip-dialpad/api/v1/state", tok, nil); w.Code != 403 {
		t.Errorf("permission revoked: %d", w.Code)
	}
}

func TestClientIP(t *testing.T) {
	pfx := func(ss ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, x := range ss {
			out = append(out, netip.MustParsePrefix(x))
		}
		return out
	}
	cases := []struct {
		remote, xff string
		trusted     []netip.Prefix
		want        string
	}{
		{"203.0.113.5:1", "", nil, "203.0.113.5"},
		{"203.0.113.5:1", "198.51.100.1", nil, "203.0.113.5"}, // public peer: XFF ignored
		{"10.0.0.2:1", "1.2.3.4, 198.51.100.7", nil, "198.51.100.7"},
		{"127.0.0.1:1", "198.51.100.8", nil, "198.51.100.8"},
		{"10.0.0.2:1", "garbage", nil, "10.0.0.2"},
		{"[::ffff:10.0.0.2]:1", "198.51.100.9", nil, "198.51.100.9"},
		// Trusted list: only peers inside it are believed (private peers outside it are not).
		{"203.0.113.5:1", "198.51.100.1", pfx("203.0.113.0/24"), "198.51.100.1"},
		{"10.0.0.2:1", "198.51.100.1", pfx("203.0.113.0/24"), "10.0.0.2"},
		{"127.0.0.1:1", "198.51.100.1", pfx("203.0.113.0/24"), "127.0.0.1"},
		{"203.0.113.9:1", "198.51.100.1", pfx("203.0.113.9/32"), "198.51.100.1"},
		// IPv6 clients are keyed by /64.
		{"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:1", "", nil, "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:1::1]:1", "", nil, "2001:db8:1:2::/64"},
		{"[fd00::1]:1", "2001:db8:5:6:7:8:9:a", nil, "2001:db8:5:6::/64"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r, c.trusted); got != c.want {
			t.Errorf("%s / %s / %v: %s want %s", c.remote, c.xff, c.trusted, got, c.want)
		}
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Dev.Example.org/w/":      "https://dev.example.org",
		"https://dev.example.org:443/w/":  "https://dev.example.org",
		"https://dev.example.org:8443/w/": "https://dev.example.org:8443",
		"http://localhost:80/w/":          "http://localhost",
		"http://localhost:8080/w/":        "http://localhost:8080",
		"https://[2001:db8::1]:443/w/":    "https://[2001:db8::1]",
	} {
		u, _ := url.Parse(in)
		if got := normalizeOrigin(u); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
}

func TestStoppedServerAnswers503(t *testing.T) {
	e := newEnv(t)
	if w := e.do(http.MethodGet, "/sip-dialpad/", "", nil); w.Code != 200 {
		t.Fatalf("before stop: %d", w.Code)
	}
	e.s.Stop()
	for _, p := range []string{"/sip-dialpad/", "/sip-dialpad/static/app.js", "/sip-dialpad/api/v1/state"} {
		w := e.do(http.MethodGet, p, "", nil)
		if w.Code != 503 || decode(t, w)["error"] != codeNotReady {
			t.Errorf("%s after stop: %d %s", p, w.Code, w.Body.String())
		}
	}
}

func TestRoomKey(t *testing.T) {
	k := roomKey([]byte(testSecret), roomPortal)
	if len(k) != keyLen || k == roomKey([]byte(testSecret), roomMgmt) || k == roomKey([]byte(testSecret+"x"), roomPortal) {
		t.Fatalf("key %q", k)
	}
	if !keyMatches([]byte(testSecret), roomPortal, k) || keyMatches([]byte(testSecret), roomMgmt, k) {
		t.Fatal("keyMatches")
	}
}
