package widget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

const (
	testServer   = "hs.example"
	testSecret   = "0123456789abcdef0123456789abcdef0123456789abcdef"
	testPublic   = "https://dev.example/sip-dialpad/"
	testOrigin   = "https://dev.example"
	alice        = id.UserID("@alice:hs.example")
	bob          = id.UserID("@bob:hs.example")
	carol        = id.UserID("@carol:hs.example") // exists, no command permission
	roomPortal   = id.RoomID("!portal:hs.example")
	roomMgmt     = id.RoomID("!mgmt:hs.example")
	roomBobMgmt  = id.RoomID("!bobmgmt:hs.example") // bob has no login
	roomOther    = id.RoomID("!other:hs.example")
	portalNumber = "+15551234567"
	tokAlice     = "openid-token-alice-0001"
	tokBob       = "openid-token-bob-0001"
	tokCarol     = "openid-token-carol-0001"
	tokDave      = "openid-token-dave-0001" // valid on the HS, but no bridge user
	tokOther     = "openid-token-other-server"
	tokSlow      = "openid-token-slow"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// fakeHS serves the openid/userinfo endpoint.
type fakeHS struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  int
	users map[string]id.UserID
	slow  chan struct{}
}

func newFakeHS(t *testing.T) *fakeHS {
	h := &fakeHS{users: map[string]id.UserID{
		tokAlice: alice, tokBob: bob, tokCarol: carol, tokDave: "@dave:hs.example",
		tokOther: "@mallory:evil.example",
	}, slow: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits++
		h.mu.Unlock()
		if r.URL.Path != userinfoPath {
			http.NotFound(w, r)
			return
		}
		tok := r.URL.Query().Get("access_token")
		if tok == tokSlow {
			select {
			case <-h.slow:
			case <-r.Context().Done():
			}
			return
		}
		sub, ok := h.users[tok]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"bad"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": string(sub)})
	}))
	t.Cleanup(func() { close(h.slow); h.srv.Close() })
	return h
}

func (h *fakeHS) hitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

// fakeCalls is a hostkit.CallAPI.
type fakeCalls struct {
	mu       sync.Mutex
	placed   []calls.PlaceCallRequest
	placeErr error
	checkErr error
	hangups  []id.RoomID
	hangErr  error
	dtmf     []string
	dtmfRoom []id.RoomID
	dtmfErr  error
	snap     *calls.Snapshot
	order    *orderLog // optional: records "place" in sequence with fakeFeatures
}

func (f *fakeCalls) PlaceCall(_ context.Context, _ core.LoginRef, req calls.PlaceCallRequest) (string, string, error) {
	f.order.add("place")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.placeErr != nil {
		return "", "", f.placeErr
	}
	f.placed = append(f.placed, req)
	if req.Number != "" {
		return "c1", "+1" + strings.TrimLeft(strings.ReplaceAll(req.Number, " ", ""), "+1"), nil
	}
	return "c1", "", nil
}

func (f *fakeCalls) CheckDestination(_ core.LoginRef, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkErr
}

func (f *fakeCalls) Hangup(_ context.Context, _ core.LoginRef, room id.RoomID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangups = append(f.hangups, room)
	return f.hangErr
}

func (f *fakeCalls) SendDTMF(_ context.Context, _ core.LoginRef, room id.RoomID, digits string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dtmfErr != nil {
		return 0, f.dtmfErr
	}
	f.dtmf = append(f.dtmf, digits)
	f.dtmfRoom = append(f.dtmfRoom, room)
	return len(digits), nil
}

func (f *fakeCalls) Snapshot(_ core.LoginRef) (calls.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snap == nil {
		return calls.Snapshot{}, false
	}
	return *f.snap, true
}

// fakeState is a StateAPI.
type fakeState struct {
	mu      sync.Mutex
	state   map[id.RoomID]map[string]any
	gets    int
	sets    int
	forbid  map[id.RoomID]bool
	block   chan struct{} // when set, SetWidgetState waits on it
	setRoom []id.RoomID
}

func newFakeState() *fakeState {
	return &fakeState{state: map[id.RoomID]map[string]any{}, forbid: map[id.RoomID]bool{}}
}

func (f *fakeState) GetWidgetState(_ context.Context, room id.RoomID, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	c := f.state[room]
	if c == nil {
		return nil, nil
	}
	// Round-trip through JSON like a homeserver would.
	b, _ := json.Marshal(c)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

func (f *fakeState) SetWidgetState(ctx context.Context, room id.RoomID, _ string, content map[string]any) error {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.setRoom = append(f.setRoom, room)
	if f.forbid[room] {
		return mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: mautrix.MForbidden.ErrCode, StatusCode: 403}}
	}
	f.state[room] = content
	return nil
}

func (f *fakeState) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.sets
}

// env is a server under test with its fakes.
type env struct {
	t      *testing.T
	s      *Server
	hs     *fakeHS
	calls  *fakeCalls
	state  *fakeState
	clk    *clock.Fake
	logBuf *bytes.Buffer
	rooms  []RoomRef
	roomMu sync.Mutex
	noMgr  bool
	users  map[id.UserID]*bridgev2.User
	login  *bridgev2.UserLogin
	// loggedOut makes alice's rooms report no login (refusal).
	loggedOut bool
}

func mkUser(mxid id.UserID, mgmt id.RoomID, cmds bool) *bridgev2.User {
	return &bridgev2.User{
		User:        &database.User{MXID: mxid, ManagementRoom: mgmt},
		Permissions: bridgeconfig.Permissions{Commands: cmds},
	}
}

func testCfg() hostkit.WidgetConfig {
	return hostkit.WidgetConfig{
		Enabled:        true,
		PublicURL:      testPublic,
		Secret:         testSecret,
		FrameAncestors: []string{"*"},
	}
}

func newEnv(t *testing.T, mod ...func(*hostkit.WidgetConfig, *Deps)) *env {
	t.Helper()
	e := &env{t: t, hs: newFakeHS(t), calls: &fakeCalls{}, state: newFakeState(), clk: clock.NewFake(t0),
		logBuf: &bytes.Buffer{}}
	e.login = &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: networkid.UserLoginID("login-alice"), UserMXID: alice}}
	e.users = map[id.UserID]*bridgev2.User{
		alice: mkUser(alice, roomMgmt, true),
		bob:   mkUser(bob, roomBobMgmt, true),
		carol: mkUser(carol, "", false),
	}
	e.rooms = []RoomRef{{RoomID: roomPortal, Title: "Call " + portalNumber}, {RoomID: roomMgmt}}
	cfg := testCfg()
	var logMu sync.Mutex
	d := Deps{
		Calls: func() hostkit.CallAPI {
			if e.noMgr {
				return nil
			}
			return e.calls
		},
		ResolveRoom:   e.resolve,
		EligibleRooms: e.eligible,
		HomeserverURL: e.hs.srv.URL,
		ServerName:    testServer,
		AppserviceID:  "sipbridge",
		BotMXID:       "@sipbot:hs.example",
		Log:           zerolog.New(zerolog.SyncWriter(lockedWriter{&logMu, e.logBuf})).Level(zerolog.DebugLevel),
		LookupUser: func(_ context.Context, mxid id.UserID) (*bridgev2.User, error) {
			return e.users[mxid], nil
		},
		State:         e.state,
		HTTPTransport: e.hs.srv.Client().Transport,
		RemoteVerify: func(_ context.Context, server, token string) (id.UserID, error) {
			if token == "remote-ok" {
				return id.UserID("@rem:" + server), nil
			}
			return "", ErrOpenIDInvalid
		},
		Clock: e.clk,
	}
	for _, m := range mod {
		m(&cfg, &d)
	}
	s, err := New(cfg, d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Stop)
	e.s = s
	return e
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (e *env) logs() string {
	return e.logBuf.String()
}

func (e *env) resolve(_ context.Context, user *bridgev2.User, room id.RoomID) (RoomContext, error) {
	switch {
	case room == roomPortal && user.MXID == alice:
		if e.loggedOut {
			return RoomContext{Kind: KindPortal, Number: portalNumber, Refusal: "You are not logged in."}, nil
		}
		return RoomContext{Kind: KindPortal, Login: e.login, Number: portalNumber}, nil
	case room == user.ManagementRoom && room != "":
		if user.MXID == alice && !e.loggedOut {
			return RoomContext{Kind: KindManagement, Login: e.login}, nil
		}
		return RoomContext{Kind: KindManagement, Refusal: "No SIP account is set up for this login."}, nil
	default:
		return RoomContext{}, Refuse(MsgNotBridgeRoom)
	}
}

func (e *env) eligible(context.Context) ([]RoomRef, error) {
	e.roomMu.Lock()
	defer e.roomMu.Unlock()
	return append([]RoomRef(nil), e.rooms...), nil
}

func (e *env) setRooms(r []RoomRef) {
	e.roomMu.Lock()
	e.rooms = r
	e.roomMu.Unlock()
}

// do runs a request against the handler.
func (e *env) do(method, path, auth string, body any, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	r := httptest.NewRequest(method, "http://dev.example"+path, rd)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "remote" {
			r.RemoteAddr = hdr[i+1]
		} else if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r)
	return w
}

func sessBody(tok, server string, room id.RoomID, key string) map[string]any {
	return map[string]any{
		"openid":    map[string]any{"access_token": tok, "matrix_server_name": server, "expires_in": 3600},
		"room_id":   room,
		"key":       key,
		"widget_id": "sipbridge_dialpad",
	}
}

func (e *env) key(room id.RoomID) string { return roomKey([]byte(testSecret), room) }

// session exchanges a session for user token tok in room (with the right key) and returns the token.
func (e *env) session(tok string, room id.RoomID, hdr ...string) string {
	e.t.Helper()
	w := e.do(http.MethodPost, "/sip-dialpad/api/v1/session", "", sessBody(tok, testServer, room, e.key(room)), hdr...)
	if w.Code != http.StatusOK {
		e.t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	var resp sessionResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp.Session
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

var errBoom = errors.New("boom")

// orderLog records the order of host calls across fakes (nil-safe).
type orderLog struct {
	mu sync.Mutex
	l  []string
}

func (o *orderLog) add(s string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.l = append(o.l, s)
	o.mu.Unlock()
}

func (o *orderLog) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.l...)
}

// fakeFeatures implements all four optional host interfaces.
type fakeFeatures struct {
	order *orderLog

	mu        sync.Mutex
	on        bool
	warning   string
	setErr    error
	saveErr   error
	saveDelay time.Duration
	saved     []string // "<e164>|<name>"
	prev      string
	unchanged bool
	openErr   error
	openDelay time.Duration
	openRoom  hostkit.ChatRoom
	opened    []string
	cName     string
	cSaved    bool
}

func (f *fakeFeatures) RosterOnly(*bridgev2.UserLogin) hostkit.RosterOnlyState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return hostkit.RosterOnlyState{On: f.on, Warning: f.warning}
}

func (f *fakeFeatures) SetRosterOnly(_ context.Context, _ *bridgev2.UserLogin, on bool) (hostkit.RosterOnlyState, error) {
	f.order.add("prefs")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return hostkit.RosterOnlyState{}, f.setErr
	}
	f.on = on
	return hostkit.RosterOnlyState{On: on, Warning: f.warning}, nil
}

func (f *fakeFeatures) OpenChat(ctx context.Context, _ *bridgev2.UserLogin, e164 string) (hostkit.ChatRoom, error) {
	f.order.add("open")
	f.mu.Lock()
	f.opened = append(f.opened, e164)
	delay, err, room := f.openDelay, f.openErr, f.openRoom
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return hostkit.ChatRoom{}, ctx.Err()
		}
	}
	return room, err
}

func (f *fakeFeatures) SaveContact(ctx context.Context, _ *bridgev2.UserLogin, e164, name string) (hostkit.SavedContact, error) {
	f.order.add("save")
	f.mu.Lock()
	f.saved = append(f.saved, e164+"|"+name)
	delay, err, prev, unch := f.saveDelay, f.saveErr, f.prev, f.unchanged
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return hostkit.SavedContact{}, ctx.Err()
		}
	}
	if err != nil {
		return hostkit.SavedContact{}, err
	}
	return hostkit.SavedContact{Name: name, PreviousName: prev, Changed: !unch}, nil
}

func (f *fakeFeatures) ContactName(*bridgev2.UserLogin, string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cName, f.cSaved
}

// prefsOnly implements only hostkit.RosterOnlyPrefs.
type prefsOnly struct{}

func (prefsOnly) RosterOnly(*bridgev2.UserLogin) hostkit.RosterOnlyState {
	return hostkit.RosterOnlyState{On: true}
}

func (prefsOnly) SetRosterOnly(_ context.Context, _ *bridgev2.UserLogin, on bool) (hostkit.RosterOnlyState, error) {
	return hostkit.RosterOnlyState{On: on}, nil
}

// withFeatures sets Deps.Features and shares the order log with the call fake.
func withFeatures(f any) func(*hostkit.WidgetConfig, *Deps) {
	return func(_ *hostkit.WidgetConfig, d *Deps) { d.Features = f }
}
