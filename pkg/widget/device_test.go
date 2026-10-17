package widget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

const (
	dev  = "/sip-dialpad/api/v1/device/"
	app  = "/sip-dialpad/app/"
	uaCA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Mobile Safari/537.36"
	uaFD = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0"
)

type sentNotice struct {
	room id.RoomID
	md   string
}

// pwaEnv is a widget server with the PWA enabled over a real SQLite device store.
type pwaEnv struct {
	*env
	st         *DeviceStore
	dbLog      *bytes.Buffer
	mu         sync.Mutex
	notices    []sentNotice
	redacts    []id.EventID
	failRedact bool
	members    []id.UserID
	loginGone  bool
	ownerOK    bool
}

func newPWAEnv(t *testing.T, mod ...func(*hostkit.WidgetConfig, *Deps)) *pwaEnv {
	t.Helper()
	p := &pwaEnv{dbLog: &bytes.Buffer{}, members: []id.UserID{alice, "@sipbot:hs.example"}, ownerOK: true}
	var dbMu sync.Mutex
	all := append([]func(*hostkit.WidgetConfig, *Deps){func(c *hostkit.WidgetConfig, d *Deps) {
		c.PWA = hostkit.PWAConfig{Enabled: true, InactivityDays: 90, RotationGraceSeconds: 120, EnrollMinutes: 15, MaxLinksPerUser: 5}
		db := newDSDB(t, p.dbLog, &dbMu)
		p.st = NewDeviceStore(db, dsBridge, DeviceStoreConfig{}, d.Clock, d.Log)
		if err := p.st.Upgrade(context.Background()); err != nil {
			t.Fatal(err)
		}
		d.Devices = p.st
		d.CommandPrefix = "!sip"
		d.ResolveDevice = func(_ context.Context, user *bridgev2.User, login networkid.UserLoginID) (RoomContext, error) {
			p.mu.Lock()
			gone := p.loginGone
			p.mu.Unlock()
			if gone || user.MXID != alice || login != dsLogin {
				return RoomContext{}, ErrDeviceLoginGone
			}
			return RoomContext{Kind: KindDevice, Login: p.login}, nil
		}
		d.RoomMembers = func(context.Context, id.RoomID) ([]id.UserID, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return append([]id.UserID(nil), p.members...), nil
		}
		d.Notify = func(_ context.Context, room id.RoomID, md string) (id.EventID, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.notices = append(p.notices, sentNotice{room, md})
			return id.EventID(fmt.Sprintf("$n%d", len(p.notices))), nil
		}
		d.Redact = func(_ context.Context, _ id.RoomID, evt id.EventID) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.redacts = append(p.redacts, evt)
			if p.failRedact {
				return errBoom
			}
			return nil
		}
		d.DeviceOwner = func(context.Context, id.UserID, networkid.UserLoginID) (bool, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.ownerOK, nil
		}
	}}, mod...)
	p.env = newEnv(t, all...)
	return p
}

func (p *pwaEnv) set(f func()) {
	p.mu.Lock()
	f()
	p.mu.Unlock()
}

func (p *pwaEnv) sentNotices() []sentNotice {
	p.s.bg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sentNotice(nil), p.notices...)
}

// logs waits for background notices first (env.logs is not synchronized with writers).
func (p *pwaEnv) logs() string {
	p.s.bg.Wait()
	return p.env.logs()
}

func (p *pwaEnv) redacted() []id.EventID {
	p.s.bg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]id.EventID(nil), p.redacts...)
}

// issue creates a link for alice through the DialpadLinks implementation and returns it and its code.
func (p *pwaEnv) issue(name string) (hostkit.LinkIssue, string) {
	p.t.Helper()
	li, err := p.s.DialpadLinks().Issue(context.Background(), alice, dsLogin, roomMgmt, name)
	if err != nil {
		p.t.Fatalf("Issue: %v", err)
	}
	_, code, ok := strings.Cut(li.Link, "#c=")
	if !ok || !ValidCode(code) {
		p.t.Fatalf("link %q", li.Link)
	}
	return li, code
}

// enroll issues and redeems a link; returns the link ID and the first device token.
func (p *pwaEnv) enroll() (string, string) {
	p.t.Helper()
	li, code := p.issue("Pixel")
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true}, "User-Agent", uaCA)
	if w.Code != http.StatusOK {
		p.t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	var r enrollResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if !ValidDeviceToken(r.DeviceToken) || r.Device.ID != li.ID || r.Device.User != alice {
		p.t.Fatalf("enroll resp %s", w.Body.String())
	}
	return li.ID, r.DeviceToken
}

// exchange runs device/session and returns the response.
func (p *pwaEnv) exchange(tok string, hdr ...string) deviceSessionResp {
	p.t.Helper()
	w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok}, append([]string{"User-Agent", uaCA}, hdr...)...)
	if w.Code != http.StatusOK {
		p.t.Fatalf("device session: %d %s", w.Code, w.Body.String())
	}
	var r deviceSessionResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.Session == "" || !ValidDeviceToken(r.NextToken) || r.Room.Kind != KindDevice {
		p.t.Fatalf("device session resp %s", w.Body.String())
	}
	return r
}

func (p *pwaEnv) confirmTok(tok string) {
	p.t.Helper()
	w := p.do(http.MethodPost, dev+"confirm", "", map[string]string{"device_token": tok})
	if w.Code != http.StatusOK {
		p.t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
}

func wantAPIErr(t *testing.T, w interface {
	Result() *http.Response
}, status int, code string) map[string]any {
	t.Helper()
	res := w.Result()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	if res.StatusCode != status || m["error"] != code {
		t.Fatalf("want %d %s, got %d %v", status, code, res.StatusCode, m)
	}
	return m
}

// ---- routes and headers ----

func TestPWARoutesDisabled(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{app, app + "manifest.webmanifest", app + "sw.js", "/sip-dialpad/app"} {
		if w := e.do(http.MethodGet, p, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	w := e.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": "x"})
	if w.Code != http.StatusNotFound {
		t.Errorf("device/session: %d", w.Code)
	}
	if e.s.DialpadLinks() != nil {
		t.Error("DialpadLinks must be nil when the PWA is disabled")
	}
}

func TestPWANewNeedsStore(t *testing.T) {
	cfg := testCfg()
	cfg.PWA = hostkit.PWAConfig{Enabled: true, InactivityDays: 90, RotationGraceSeconds: 120, EnrollMinutes: 15, MaxLinksPerUser: 5}
	_, err := New(cfg, Deps{Calls: func() hostkit.CallAPI { return nil },
		ResolveRoom:   func(context.Context, *bridgev2.User, id.RoomID) (RoomContext, error) { return RoomContext{}, nil },
		EligibleRooms: func(context.Context) ([]RoomRef, error) { return nil, nil },
		HomeserverURL: "https://hs.example", ServerName: testServer, AppserviceID: "x",
		LookupUser: func(context.Context, id.UserID) (*bridgev2.User, error) { return nil, nil }, State: newFakeState()})
	if err == nil || !strings.Contains(err.Error(), "Devices") {
		t.Fatalf("New without Devices: %v", err)
	}
}

func TestAppHeaders(t *testing.T) {
	p := newPWAEnv(t)
	w := p.do(http.MethodGet, app, "", nil)
	h := w.Header()
	if w.Code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Fatalf("app/: %d %s", w.Code, h.Get("Content-Type"))
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "manifest-src 'self'", "worker-src 'self'", "script-src 'self'", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("app CSP %q lacks %q", csp, want)
		}
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
		h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("app headers %v", h)
	}
	// The widget page stays framable with its own CSP.
	w = p.do(http.MethodGet, "/sip-dialpad/", "", nil)
	if w.Header().Get("X-Frame-Options") != "" || strings.Contains(w.Header().Get("Content-Security-Policy"), "manifest-src") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors *") {
		t.Errorf("widget page headers changed: %v", w.Header())
	}
	w = p.do(http.MethodGet, "/sip-dialpad/app", "", nil)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != app {
		t.Errorf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = p.do(http.MethodGet, app+"manifest.webmanifest", "", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var man struct {
		ID, StartURL, Scope, Display string
		Icons                        []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		ID       *string `json:"id"`
		StartURL *string `json:"start_url"`
		Scope    *string `json:"scope"`
		Display  *string `json:"display"`
		Icons    any     `json:"icons"`
	}{&man.ID, &man.StartURL, &man.Scope, &man.Display, &man.Icons}); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if man.StartURL != "./" || man.Scope != "./" || man.ID != "./" || man.Display != "standalone" {
		t.Errorf("manifest %+v", man)
	}
	sizes := map[string]bool{}
	for _, ic := range man.Icons {
		if ic.Type == "image/png" {
			sizes[ic.Sizes+"/"+ic.Purpose] = true
		}
	}
	if !sizes["192x192/"] || !sizes["512x512/"] || !sizes["512x512/maskable"] {
		t.Errorf("manifest icons %+v", man.Icons)
	}
	w = p.do(http.MethodGet, app+"sw.js", "", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") ||
		w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("Service-Worker-Allowed") != app {
		t.Errorf("sw.js: %d %v", w.Code, w.Header())
	}
	for _, bad := range []string{app + "../index.html", app + ".hidden", app + "nope.js", app + "a/b.js"} {
		if w := p.do(http.MethodGet, bad, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := p.do(http.MethodPost, app, "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST app/: %d", w.Code)
	}
}

func TestAppIcons(t *testing.T) {
	for name, size := range map[string]int{"icon-192.png": 192, "icon-512.png": 512, "icon-maskable-512.png": 512} {
		data, err := fs.ReadFile(staticFS, "static/app/"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("%s: %v", name, b)
		}
		if name == "icon-maskable-512.png" {
			// The maskable icon fills its background (no transparent pixels): content is inside the safe zone.
			for _, pt := range []image.Point{{0, 0}, {511, 0}, {0, 511}, {511, 511}, {256, 2}} {
				if _, _, _, a := img.At(pt.X, pt.Y).RGBA(); a != 0xffff {
					t.Errorf("maskable icon is transparent at %v", pt)
				}
			}
		}
	}
}

// swShellVersion computes the service worker cache version: "dialpad-" + the first 12 hex chars of
// sha256 over every SHELL entry of sw.js in order (name, NUL, content, NUL; "./" is index.html).
func swShellVersion(t *testing.T, sw string) string {
	t.Helper()
	m := regexp.MustCompile(`var SHELL = \[([^\]]*)\]`).FindStringSubmatch(sw)
	if m == nil {
		t.Fatal("sw.js has no SHELL list")
	}
	h := sha256.New()
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		name := q[1]
		file := name
		if name == "./" {
			file = "index.html"
		}
		data, err := fs.ReadFile(staticFS, "static/app/"+file)
		if err != nil {
			t.Fatalf("shell file %s: %v", name, err)
		}
		h.Write([]byte(name + "\x00"))
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12] // the cache name adds "dialpad-<scope hash>-" in sw.js
}

func TestSWCacheVersionMatchesShell(t *testing.T) {
	data, err := fs.ReadFile(staticFS, "static/app/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	sw := string(data)
	want := swShellVersion(t, sw)
	got := regexp.MustCompile(`var VERSION = '([^']+)'`).FindStringSubmatch(sw)
	if got == nil || got[1] != want {
		t.Fatalf("sw.js VERSION %v, want %q (update the VERSION line whenever a shell file changes)", got, want)
	}
	// The worker never intercepts the API.
	if strings.Contains(sw, "api/") {
		t.Error("sw.js mentions api/")
	}
}

func TestStaticLint(t *testing.T) {
	banned := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "document.write"}
	extURL := regexp.MustCompile(`(?i)\bhttps?://[^\s'"]*`)
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".html")) {
			return err
		}
		data, _ := fs.ReadFile(staticFS, path)
		for _, b := range banned {
			if bytes.Contains(data, []byte(b)) {
				t.Errorf("%s contains %q", path, b)
			}
		}
		for _, u := range extURL.FindAllString(string(data), -1) {
			if u != "https://matrix.to/#/" { // the informational "Open in Matrix" deep link (§9.4)
				t.Errorf("%s contains external URL %q", path, u)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- enrollment ----

func TestEnrollFlow(t *testing.T) {
	p := newPWAEnv(t)
	li, code := p.issue("Pixel")
	if li.AppURL != testPublic+"app/" || !strings.HasPrefix(li.Link, testPublic+"app/#c="+DeviceCodePrefix) ||
		li.Name != "Pixel" || !li.ExpiresAt.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("issue %+v", li)
	}
	if err := p.s.DialpadLinks().AttachNotice(context.Background(), li.ID, roomMgmt, "$link"); err != nil {
		t.Fatal(err)
	}
	// Peek: no state change, can be repeated.
	for range 2 {
		w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": false})
		m := decode(t, w)
		if w.Code != 200 || m["user"] != string(alice) || m["name"] != "Pixel" || m["expires_in"].(float64) != 900 {
			t.Fatalf("peek: %d %v", w.Code, m)
		}
	}
	links, _ := p.s.DialpadLinks().List(context.Background(), alice)
	if len(links) != 1 || links[0].State != "pending" {
		t.Fatalf("after peek %+v", links)
	}
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true}, "User-Agent", uaCA)
	if w.Code != 200 {
		t.Fatalf("enroll %d %s", w.Code, w.Body.String())
	}
	links, _ = p.s.DialpadLinks().List(context.Background(), alice)
	if links[0].State != "active" || links[0].Client != ClientChromeAndroid {
		t.Fatalf("after enroll %+v", links)
	}
	if r := p.redacted(); len(r) != 1 || r[0] != "$link" {
		t.Errorf("link message not redacted: %v", r)
	}
	if due, _ := p.st.DueNotices(context.Background()); len(due) != 0 {
		t.Errorf("notice still due: %v", due)
	}
	n := p.sentNotices()
	if len(n) != 1 || n[0].room != roomMgmt || !strings.Contains(n[0].md, "set up on Chrome Android") ||
		!strings.Contains(n[0].md, "`!sip dialpad-revoke "+li.ID+"`") || strings.Contains(n[0].md, code) {
		t.Errorf("enroll notice %+v", n)
	}
	// Reuse of the code.
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true})
	wantAPIErr(t, w, http.StatusGone, codeLinkUsed)
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": false}, "remote", "203.0.113.9:1")
	wantAPIErr(t, w, http.StatusGone, codeLinkUsed)
	// Expired.
	_, code2 := p.issue("Tab")
	p.clk.Advance(16 * time.Minute)
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code2, "confirm": true}, "remote", "203.0.113.10:1")
	wantAPIErr(t, w, http.StatusGone, codeLinkExpired)
	// Unknown and malformed.
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": DeviceCodePrefix + strings.Repeat("A", 43), "confirm": true}, "remote", "203.0.113.11:1")
	wantAPIErr(t, w, http.StatusUnauthorized, codeLinkInvalid)
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": "mrtcsip_dpt_" + strings.Repeat("A", 43)}, "remote", "203.0.113.11:1")
	wantAPIErr(t, w, http.StatusUnauthorized, codeLinkInvalid)
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": 5}, "remote", "203.0.113.11:1")
	wantAPIErr(t, w, http.StatusBadRequest, codeBadRequest)
}

func TestNotifyOnEnrollOff(t *testing.T) {
	f := false
	p := newPWAEnv(t, func(c *hostkit.WidgetConfig, _ *Deps) { c.PWA.NotifyOnEnroll = &f })
	p.enroll()
	if n := p.sentNotices(); len(n) != 0 {
		t.Errorf("notices %+v", n)
	}
}

func TestEnrollRequiresPermission(t *testing.T) {
	p := newPWAEnv(t)
	_, code := p.issue("Pixel")
	p.users[alice].Permissions.Commands = false
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true})
	wantAPIErr(t, w, http.StatusForbidden, codeForbidden)
}

func TestMalformedSecretsNeverHitDB(t *testing.T) {
	p := newPWAEnv(t)
	before := strings.Count(p.dbLog.String(), "SELECT")
	for i, req := range []struct{ path, body string }{
		{"enroll", `{"code":"mrtcsip_dpc_short","confirm":true}`},
		{"enroll", `{"code":"garbage"}`},
		{"session", `{"device_token":"mrtcsip_dpc_` + strings.Repeat("A", 43) + `"}`},
		{"session", `{"device_token":"mrtcsip_dpt_` + strings.Repeat("!", 43) + `"}`},
		{"confirm", `{"device_token":""}`},
	} {
		w := p.do(http.MethodPost, dev+req.path, "", req.body, "remote", fmt.Sprintf("198.51.100.%d:1", i+1))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", req.path, req.body, w.Code)
		}
	}
	if after := strings.Count(p.dbLog.String(), "SELECT"); after != before {
		t.Errorf("malformed secrets reached the DB (%d -> %d queries)", before, after)
	}
}

func TestEnrollReplacesPreviousDevice(t *testing.T) {
	p := newPWAEnv(t)
	oldID, oldTok := p.enroll()
	_, code := p.issue("Pixel 2")
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true, "previous_token": oldTok})
	if w.Code != 200 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	p.s.bg.Wait()
	links, _ := p.s.DialpadLinks().List(context.Background(), alice)
	if len(links) != 1 || links[0].ID == oldID {
		t.Fatalf("old device not forgotten: %+v", links)
	}
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": oldTok})
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceInvalid)
}

// previous_token is only looked up: another user's token changes nothing (no confirm, no reuse, no
// revoke); a stale (dead) token of the same user's other device revokes that device without a reuse notice.
func TestEnrollPreviousTokenNoSideEffects(t *testing.T) {
	p := newPWAEnv(t)
	ctx := context.Background()
	bi, err := p.st.Issue(ctx, IssueParams{User: bob, Login: dsLoginB, Room: roomBobMgmt, Name: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	be, err := p.st.Enroll(ctx, bi.Code, ClientChromeAndroid)
	if err != nil {
		t.Fatal(err)
	}
	bobChild, err := p.st.Rotate(ctx, be.Token, ClientChromeAndroid) // unconfirmed child of bob's token
	if err != nil {
		t.Fatal(err)
	}
	_, code := p.issue("Pixel")
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true, "previous_token": bobChild.Next})
	if w.Code != 200 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	p.s.bg.Wait()
	if l, _ := p.st.List(ctx, bob); len(l) != 1 || l[0].State != DeviceStateActive {
		t.Fatalf("bob's device touched: %+v", l)
	}
	if r, err := p.st.Rotate(ctx, bobChild.Next, ClientChromeAndroid); err != nil || r.TokenState != TokenStateUnconfirmed {
		t.Fatalf("bob's child was confirmed by alice's enrollment: %+v %v", r, err)
	}

	// Same user, stale token of the other device: revoked quietly.
	oldID, t1 := p.enroll()
	r := p.exchange(t1)
	p.confirmTok(r.NextToken)
	p.clk.Advance(time.Hour) // t1 dead
	_, code = p.issue("Pixel 3")
	w = p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true, "previous_token": t1})
	if w.Code != 200 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	p.s.bg.Wait()
	links, _ := p.st.List(ctx, alice)
	for _, l := range links {
		if l.ID == oldID {
			t.Fatalf("old device not forgotten: %+v", links)
		}
	}
	for _, n := range p.sentNotices() {
		if strings.Contains(n.md, "signed out") {
			t.Errorf("reuse notice for a previous_token: %q", n.md)
		}
	}
}

// A device revoked while its exchange is in flight (between Rotate and session creation) gets no session.
func TestDeviceSessionRevokedInFlight(t *testing.T) {
	var p *pwaEnv
	var hook func()
	p = newPWAEnv(t, func(_ *hostkit.WidgetConfig, d *Deps) {
		inner := d.ResolveDevice
		d.ResolveDevice = func(ctx context.Context, user *bridgev2.User, login networkid.UserLoginID) (RoomContext, error) {
			p.mu.Lock()
			h := hook
			p.mu.Unlock()
			if h != nil {
				h()
			}
			return inner(ctx, user, login)
		}
	})
	devID, tok := p.enroll()
	p.set(func() { hook = func() { p.s.sessions.revokeDevice(devID) } })
	w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok}, "User-Agent", uaCA)
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceRevoked)
	// A later exchange (after the revocation instant) works again.
	p.set(func() { hook = nil })
	p.clk.Advance(time.Millisecond)
	p.exchange(tok)
}

// ---- sessions and rotation ----

func TestDeviceSessionRotationAndReuse(t *testing.T) {
	p := newPWAEnv(t)
	devID, t1 := p.enroll()
	r1 := p.exchange(t1)
	if r1.Device.ID != devID || r1.Device.User != alice || r1.Device.InactiveExpiryAt == nil || r1.ExpiresIn != 3600 {
		t.Fatalf("exchange %+v", r1)
	}
	w := p.do(http.MethodGet, api+"state", r1.Session, nil)
	m := decode(t, w)
	if w.Code != 200 || m["room"].(map[string]any)["kind"] != KindDevice || m["login"].(map[string]any)["ready"] != true {
		t.Fatalf("state: %d %v", w.Code, m)
	}
	// Lost exchange response: t1 is still current days later.
	p.clk.Advance(3 * 24 * time.Hour)
	r2 := p.exchange(t1)
	p.confirmTok(r2.NextToken)
	p.confirmTok(r2.NextToken) // idempotent
	// t1 is in grace now.
	p.clk.Advance(60 * time.Second)
	p.exchange(t1)
	// After grace: reuse.
	p.clk.Advance(2 * time.Minute)
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": t1})
	m = wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceRevoked)
	if m["reason"] != "reuse" {
		t.Errorf("reason %v", m)
	}
	// Whole family refused, live sessions killed, Warn log without the token, one notice.
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": r2.NextToken})
	if m := wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceRevoked); m["reason"] != "reuse" {
		t.Errorf("sibling reason %v", m)
	}
	if w := p.do(http.MethodGet, api+"state", r1.Session, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("session survived the reuse revoke: %d", w.Code)
	}
	logs := p.logs()
	if !strings.Contains(logs, "Dialpad device token reuse detected; device revoked") || strings.Contains(logs, t1) {
		t.Errorf("reuse log: %s", logs)
	}
	reuse := 0
	for _, n := range p.sentNotices() {
		if strings.Contains(n.md, "an old copy of its sign-in was used") {
			reuse++
		}
	}
	if reuse != 1 {
		t.Errorf("reuse notices: %d", reuse)
	}
	links, _ := p.s.DialpadLinks().List(context.Background(), alice)
	if len(links) != 1 || links[0].State != "expired" {
		t.Errorf("links after reuse %+v", links)
	}
}

func TestDeviceConfirmErrors(t *testing.T) {
	p := newPWAEnv(t)
	w := p.do(http.MethodPost, dev+"confirm", "", map[string]string{"device_token": DeviceTokenPrefix + strings.Repeat("B", 43)})
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceInvalid)
	// Confirm needs no session but checks Origin and content type.
	w = p.do(http.MethodPost, dev+"confirm", "", map[string]string{"device_token": "x"}, "Origin", "https://evil.example")
	wantAPIErr(t, w, http.StatusForbidden, codeForbidden)
	w = p.do(http.MethodPost, dev+"confirm", "", `{"device_token":"x"}`, "Content-Type", "text/plain")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", w.Code)
	}
	if w := p.do(http.MethodGet, dev+"session", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET session: %d", w.Code)
	}
}

func TestDeviceExpired(t *testing.T) {
	p := newPWAEnv(t)
	_, tok := p.enroll()
	p.clk.Advance(91 * 24 * time.Hour)
	w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok})
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceExpired)
}

func TestDeviceUserAndLoginChecks(t *testing.T) {
	p := newPWAEnv(t)
	_, tok := p.enroll()
	r := p.exchange(tok)
	p.users[alice].Permissions.Commands = false
	w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok})
	wantAPIErr(t, w, http.StatusForbidden, codeForbidden)
	if w := p.do(http.MethodGet, api+"state", r.Session, nil); w.Code != http.StatusForbidden {
		t.Errorf("state without permission: %d", w.Code)
	}
	p.users[alice].Permissions.Commands = true
	p.set(func() { p.loginGone = true })
	w = p.do(http.MethodGet, api+"state", r.Session, nil)
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceRevoked)
	p.set(func() { p.loginGone = false })
	if w := p.do(http.MethodGet, api+"state", r.Session, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("session survived login loss: %d", w.Code)
	}
	p.set(func() { p.loginGone = true })
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok})
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceRevoked)
}

func TestDeviceSessionScope(t *testing.T) {
	p := newPWAEnv(t)
	_, tok := p.enroll()
	s := p.exchange(tok).Session
	w := p.do(http.MethodPost, api+"call", s, map[string]string{})
	if m := wantAPIErr(t, w, http.StatusBadRequest, codeBadRequest); m["message"] != "Enter a number to call." {
		t.Errorf("call {}: %v", m)
	}
	if w := p.do(http.MethodPost, api+"call", s, map[string]string{"number": "+17377273232"}); w.Code != 200 {
		t.Fatalf("call number: %d %s", w.Code, w.Body.String())
	}
	if w := p.do(http.MethodPost, api+"hangup", s, map[string]string{}); w.Code != 200 {
		t.Fatalf("hangup: %d", w.Code)
	}
	if w := p.do(http.MethodPost, api+"dtmf", s, map[string]string{"digits": "12"}); w.Code != 200 {
		t.Fatalf("dtmf: %d", w.Code)
	}
	if p.calls.hangups[0] != "" || p.calls.dtmfRoom[0] != "" {
		t.Errorf("device actions must target room \"\": %v %v", p.calls.hangups, p.calls.dtmfRoom)
	}
	if n := len(p.s.inst.queue); n != 0 {
		t.Errorf("device call enqueued %d installer jobs", n)
	}
}

func TestRevokeKillsSessions(t *testing.T) {
	p := newPWAEnv(t)
	devID, tok := p.enroll()
	s := p.exchange(tok).Session
	links := p.s.DialpadLinks()
	if n, err := links.Revoke(context.Background(), bob, devID); n != 0 || err != nil {
		t.Fatalf("foreign revoke: %d %v", n, err)
	}
	if n, err := links.Revoke(context.Background(), alice, "zzzzzz"); n != 0 || err != nil {
		t.Fatalf("unknown revoke: %d %v", n, err)
	}
	if n, err := links.Revoke(context.Background(), alice, devID); n != 1 || err != nil {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if w := p.do(http.MethodGet, api+"state", s, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("state after revoke: %d", w.Code)
	}
	w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok})
	wantAPIErr(t, w, http.StatusUnauthorized, codeDeviceInvalid)
	// Revoking a pending link redacts its message.
	li, _ := p.issue("Tab")
	_ = links.AttachNotice(context.Background(), li.ID, roomMgmt, "$pending")
	if n, _ := links.Revoke(context.Background(), alice, "all"); n != 1 {
		t.Fatalf("revoke all: %d", n)
	}
	if r := p.redacted(); len(r) == 0 || r[len(r)-1] != "$pending" {
		t.Errorf("pending notice not redacted: %v", r)
	}
}

func TestForget(t *testing.T) {
	p := newPWAEnv(t)
	widgetSess := p.session(tokAlice, roomMgmt)
	if w := p.do(http.MethodPost, dev+"forget", widgetSess, map[string]any{}); w.Code != http.StatusUnauthorized {
		t.Errorf("forget with a widget session: %d", w.Code)
	}
	if w := p.do(http.MethodPost, dev+"forget", "", map[string]any{}); w.Code != http.StatusUnauthorized {
		t.Errorf("forget without a session: %d", w.Code)
	}
	_, tok := p.enroll()
	r := p.exchange(tok)
	r2 := p.exchange(tok) // a second tab
	if w := p.do(http.MethodPost, dev+"forget", r.Session, map[string]any{}); w.Code != 200 {
		t.Fatalf("forget: %d %s", w.Code, w.Body.String())
	}
	if links, _ := p.s.DialpadLinks().List(context.Background(), alice); len(links) != 0 {
		t.Errorf("links after forget %+v", links)
	}
	if w := p.do(http.MethodGet, api+"state", r2.Session, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("other session survived forget: %d", w.Code)
	}
	if w := p.do(http.MethodGet, api+"state", widgetSess, nil); w.Code != 200 {
		t.Errorf("widget session was killed by forget: %d", w.Code)
	}
}

func TestClientChangeNotice(t *testing.T) {
	p := newPWAEnv(t)
	_, tok := p.enroll()
	p.exchange(tok, "User-Agent", uaFD)
	p.exchange(tok, "User-Agent", uaFD) // at most once per day
	n := 0
	for _, x := range p.sentNotices() {
		if strings.Contains(x.md, "was used from Firefox Desktop, set up on Chrome Android") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("client change notices: %d", n)
	}
}

// ---- rate limits ----

func TestDeviceRateLimits(t *testing.T) {
	p := newPWAEnv(t)
	// enroll: 5/min per IP.
	for i := range 5 {
		w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": "x"}, "remote", "203.0.113.1:1")
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("enroll %d limited early", i)
		}
	}
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": "x"}, "remote", "203.0.113.1:1")
	wantAPIErr(t, w, http.StatusTooManyRequests, codeRateLimited)
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// The failures above locked the IP out of device/session too (shared failure limiter).
	devID, tok := p.enroll()
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok}, "remote", "203.0.113.1:1")
	wantAPIErr(t, w, http.StatusTooManyRequests, codeRateLimited)
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "1" {
		t.Errorf("lockout Retry-After %q", ra)
	}
	// device/session: 30/h per device (from varying IPs).
	for i := range 30 {
		p.clk.Advance(3 * time.Second)
		w := p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok}, "remote", fmt.Sprintf("192.0.2.%d:1", i%8+1))
		if w.Code != 200 {
			t.Fatalf("session %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w = p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok}, "remote", "192.0.2.99:1")
	wantAPIErr(t, w, http.StatusTooManyRequests, codeRateLimited)
	_ = devID
	// device/session per IP: 10/min.
	p2 := newPWAEnv(t)
	_, tok2 := p2.enroll()
	for i := range 10 {
		if w := p2.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok2}, "remote", "192.0.2.50:1"); w.Code != 200 {
			t.Fatalf("p2 session %d: %d", i, w.Code)
		}
	}
	w = p2.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": tok2}, "remote", "192.0.2.50:1")
	wantAPIErr(t, w, http.StatusTooManyRequests, codeRateLimited)
}

func TestDeviceConfirmLimit(t *testing.T) {
	p := newPWAEnv(t)
	_, tok := p.enroll()
	for i := range 60 {
		if w := p.do(http.MethodPost, dev+"confirm", "", map[string]string{"device_token": tok}); w.Code != 200 {
			t.Fatalf("confirm %d: %d", i, w.Code)
		}
	}
	w := p.do(http.MethodPost, dev+"confirm", "", map[string]string{"device_token": tok})
	wantAPIErr(t, w, http.StatusTooManyRequests, codeRateLimited)
}

// ---- DialpadLinks ----

func TestIssueChecks(t *testing.T) {
	p := newPWAEnv(t)
	links := p.s.DialpadLinks()
	p.set(func() { p.members = append(p.members, bob) })
	_, err := links.Issue(context.Background(), alice, dsLogin, roomMgmt, "Pixel")
	if !errors.Is(err, hostkit.ErrDialpadRoomNotPrivate) {
		t.Fatalf("shared room: %v", err)
	}
	p.set(func() { p.members = []id.UserID{alice, "@sipbot:hs.example"} })
	for range 5 {
		p.issue("")
	}
	_, err = links.Issue(context.Background(), alice, dsLogin, roomMgmt, "Pixel")
	if !errors.Is(err, hostkit.ErrDialpadLinkLimit) {
		t.Fatalf("limit: %v", err)
	}
	list, _ := links.List(context.Background(), alice)
	if len(list) != 5 || list[0].State != "pending" || !strings.HasPrefix(list[0].Name, "device-") {
		t.Errorf("list %+v", list)
	}
}

func TestClientFromUA(t *testing.T) {
	for ua, want := range map[string]string{
		uaCA: ClientChromeAndroid,
		"Mozilla/5.0 (Android 14; Mobile; rv:131.0) Gecko/131.0 Firefox/131.0":                                                                    ClientFirefoxAndroid,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": ClientSafariIOS,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0 Mobile/15E148 Safari/604.1":  ClientOther,
		uaFD: ClientFirefoxDesktop,
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36": ClientChromeDesktop,
		"curl/8.0": ClientOther,
		"":         ClientOther,
	} {
		if got := clientFromUA(ua); got != want {
			t.Errorf("%q: %s, want %s", ua, got, want)
		}
	}
}

// ---- janitor ----

func TestDeviceJanitorRedactsAndPurges(t *testing.T) {
	ot := obstest.Setup(t)
	p := newPWAEnv(t)
	ctx := context.Background()
	p.s.deviceJanitor(ctx, true)
	if ot.SpanByName(spanDevJanitor) != nil {
		t.Fatal("janitor span without work")
	}
	li, _ := p.issue("Pixel")
	_ = p.s.DialpadLinks().AttachNotice(ctx, li.ID, roomMgmt, "$expiring")
	p.s.deviceJanitor(ctx, false)
	if len(p.redacted()) != 0 {
		t.Fatal("redacted before expiry")
	}
	p.clk.Advance(16 * time.Minute)
	p.s.deviceJanitor(ctx, false)
	if r := p.redacted(); len(r) != 1 || r[0] != "$expiring" {
		t.Fatalf("redactions %v", r)
	}
	sp := ot.SpanByName(spanDevJanitor)
	if sp == nil || sp.Parent().IsValid() || obstest.Attr(sp, "notices.redacted").AsInt64() != 1 {
		t.Fatalf("janitor span %+v", sp)
	}
	// Failures are retried at most 3 times.
	li2, _ := p.issue("Tab")
	_ = p.s.DialpadLinks().AttachNotice(ctx, li2.ID, roomMgmt, "$fails")
	p.set(func() { p.failRedact = true })
	p.clk.Advance(16 * time.Minute)
	for range 5 {
		p.s.deviceJanitor(ctx, false)
	}
	n := 0
	for _, e := range p.redacted() {
		if e == "$fails" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("failed redaction attempts: %d", n)
	}
	// Orphans: the owner check fails → devices deleted, sessions killed.
	p.set(func() { p.failRedact = false })
	_, tok := p.enroll()
	s := p.exchange(tok).Session
	p.set(func() { p.ownerOK = false })
	p.s.deviceJanitor(ctx, true)
	if list, _ := p.s.DialpadLinks().List(ctx, alice); len(list) != 0 {
		t.Errorf("orphans kept: %+v", list)
	}
	if w := p.do(http.MethodGet, api+"state", s, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("orphan session survived: %d", w.Code)
	}
}

func TestJanitorScheduling(t *testing.T) {
	p := newPWAEnv(t)
	li, _ := p.issue("Pixel")
	_ = p.s.DialpadLinks().AttachNotice(context.Background(), li.ID, roomMgmt, "$x")
	p.s.Start()
	p.clk.Advance(16 * time.Minute)
	deadline := time.Now().Add(3 * time.Second)
	for len(p.redacted()) == 0 && time.Now().Before(deadline) {
		p.clk.Advance(time.Minute)
		time.Sleep(5 * time.Millisecond)
	}
	if len(p.redacted()) == 0 {
		t.Fatal("janitor did not redact the expired link message")
	}
	p.s.Stop() // the installer and janitor goroutines log
	if !strings.Contains(p.logs(), "dedicated hostname") {
		t.Error("no shared-hostname warning at start")
	}
}

// ---- telemetry and privacy ----

func TestDeviceSpans(t *testing.T) {
	ot := obstest.Setup(t)
	p := newPWAEnv(t)
	devID, tok := p.enroll()
	r := p.exchange(tok)
	p.confirmTok(r.NextToken)
	n := len(ot.Spans())
	p.do(http.MethodGet, api+"state", r.Session, nil)
	if len(ot.Spans()) != n {
		t.Error("state from the PWA produced a span")
	}
	p.do(http.MethodPost, api+"call", r.Session, map[string]string{"number": "+17377273232"})
	p.do(http.MethodPost, dev+"forget", r.Session, map[string]any{})
	for name, want := range map[string]string{spanDevEnroll: outcomeOK, spanDevSession: outcomeOK, spanDevConfirm: outcomeOK,
		spanDevForget: outcomeOK, spanCall: outcomeOK} {
		sp := ot.SpanByName(name)
		if sp == nil || sp.Parent().IsValid() || sp.SpanKind() != trace.SpanKindServer {
			t.Fatalf("%s: %+v", name, sp)
		}
		if obstest.Attr(sp, "outcome").AsString() != want || obstest.Attr(sp, "widget.device.id").AsString() != devID {
			t.Errorf("%s attrs %v", name, sp.Attributes())
		}
	}
	if obstest.Attr(ot.SpanByName(spanCall), "widget.client").AsString() != clientPWA {
		t.Error("widget.call lacks widget.client=pwa")
	}
	if obstest.Attr(ot.SpanByName(spanDevSession), "widget.device.token_state").AsString() != TokenStateCurrent {
		t.Error("token_state")
	}
	if got := ot.Sum(t, metricRequests, "widget.action", actDevSession, "widget.client", clientPWA, "outcome", mOK); got != 1 {
		t.Errorf("device_session requests %v", got)
	}
	if got := ot.Sum(t, metricDeviceEvents, "event", evEnrolled); got != 1 {
		t.Errorf("enrolled events %v", got)
	}
	_ = p.st.RefreshCounts(context.Background())
	if pts := ot.MetricPoints(t, metricDeviceLinks); len(pts) != 2 {
		t.Errorf("device_links points %v", pts)
	}
	obstest.AssertMetricAttrsAllowed(t, ot.Reader, widgetAllowlist())
}

func TestPWAPrivacy(t *testing.T) {
	ot := obstest.Setup(t)
	p := newPWAEnv(t)
	li, code := p.issue("Pixel")
	_ = p.s.DialpadLinks().AttachNotice(context.Background(), li.ID, roomMgmt, "$l")
	w := p.do(http.MethodPost, dev+"enroll", "", map[string]any{"code": code, "confirm": true}, "User-Agent", uaCA)
	var en enrollResp
	_ = json.Unmarshal(w.Body.Bytes(), &en)
	r := p.exchange(en.DeviceToken)
	p.confirmTok(r.NextToken)
	p.do(http.MethodPost, api+"call", r.Session, map[string]string{"number": "+17377273232"})
	p.do(http.MethodPost, api+"dtmf", r.Session, map[string]string{"digits": "159#"})
	r2 := p.exchange(r.NextToken)
	p.clk.Advance(5 * time.Minute)
	p.confirmTok(r2.NextToken)
	p.clk.Advance(5 * time.Minute)
	p.do(http.MethodPost, dev+"session", "", map[string]string{"device_token": en.DeviceToken}) // reuse
	_, _ = p.s.DialpadLinks().Revoke(context.Background(), alice, "all")
	p.s.bg.Wait()
	if !strings.Contains(p.logs(), "reuse detected") {
		t.Fatal("flow did not reach reuse")
	}
	secrets := []string{code, en.DeviceToken, r.NextToken, r2.NextToken, r.Session, r2.Session, "159#", uaCA}
	// The store's own query trace (args are hashes) counts too; the device name may only appear there.
	obstest.AssertNoSecrets(t, ot.Rec, bytes.NewBufferString(p.logs()+p.dbLog.String()), secrets...)
	obstest.AssertNoSecrets(t, ot.Rec, bytes.NewBufferString(p.logs()), "Pixel")
	for _, n := range p.sentNotices() {
		for _, s := range secrets[:6] {
			if strings.Contains(n.md, s) {
				t.Errorf("notice contains a secret: %q", n.md)
			}
		}
	}
	obstest.AssertMetricAttrsAllowed(t, ot.Reader, widgetAllowlist())
}

func TestSessionStoreDeviceRevocation(t *testing.T) {
	p := newEnv(t)
	st := p.s.sessions
	a, _, _ := st.createDevice(alice, dsLogin, "aaaaaa", time.Hour, time.Time{})
	b, _, _ := st.createDevice(alice, dsLogin, "bbbbbb", time.Hour, time.Time{})
	c, _, _ := st.create(alice, roomMgmt, time.Hour)
	d, _, _ := st.create(bob, roomBobMgmt, time.Hour)
	if st.revokeDevice("aaaaaa") != 1 || st.get(a) != nil || st.get(b) == nil || st.revokeDevice("") != 0 {
		t.Fatal("revokeDevice")
	}
	if st.revokeUser(alice) != 2 || st.get(b) != nil || st.get(c) != nil || st.get(d) == nil {
		t.Fatal("revokeUser")
	}
	// A session for a device revoked at or after notBefore is refused; later ones are not.
	before := p.clk.Now()
	p.clk.Advance(time.Second)
	st.revokeDevice("cccccc")
	if _, _, err := st.createDevice(alice, dsLogin, "cccccc", time.Hour, before); !errors.Is(err, errSessionDeviceRevoked) {
		t.Fatalf("created after revoke: %v", err)
	}
	if _, _, err := st.createDevice(alice, dsLogin, "cccccc", time.Hour, p.clk.Now()); !errors.Is(err, errSessionDeviceRevoked) {
		t.Fatalf("created at the revoke instant: %v", err)
	}
	p.clk.Advance(time.Millisecond)
	if _, _, err := st.createDevice(alice, dsLogin, "cccccc", time.Hour, p.clk.Now()); err != nil {
		t.Fatalf("created later: %v", err)
	}
	// The revocation record is purged after maxSessionTTL.
	p.clk.Advance(maxSessionTTL + time.Second)
	st.purge()
	st.mu.Lock()
	_, kept := st.revoked["cccccc"]
	st.mu.Unlock()
	if kept {
		t.Fatal("revocation record not purged")
	}
}
