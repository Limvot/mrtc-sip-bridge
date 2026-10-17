// Package widget is the Dialpad room widget (docs/WIDGET-SPEC.md): an HTTP server mounted on the
// appservice listener under the widget.public_url path, OpenID + room-key authentication, a small JSON
// API over the shared hostkit call actions, and the installer that adds the widget state event to
// portals and management rooms. Host-class (it imports bridgev2); core packages must not import it.
//
// Host wiring:
//
//	srv, err := widget.New(cfg.Widget, widget.Deps{Bridge: br, Calls: ..., ResolveRoom: ..., EligibleRooms: ...})
//	widget.Mount(br, srv)        // AS.Router.Handle(prefix, srv.Handler())
//	srv.Start()                  // installer worker, startup sweep, periodic sweep, janitor
//	commandDeps.Widget = srv.Installer()
//	commandDeps.Dialpad = srv.DialpadLinks() // nil unless widget.pwa.enabled
//
// Dialpad PWA (docs/PWA-SPEC.md), only when widget.pwa.enabled: before New, open the device store with
// OpenDeviceStore (or NewDeviceStore + Upgrade; an error should fail the start) and pass it as
// Deps.Devices, plus Deps.ResolveDevice. The server then also serves <prefix>app/ and
// <prefix>api/v1/device/..., and its janitor redacts link messages and purges devices.
//
//	...
//	srv.Stop()
//
// With widget.enabled false and remove_when_disabled true, run widget.Remove once at startup instead.
package widget

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

//go:embed static
var staticFS embed.FS

// Room kinds of RoomContext.Kind.
const (
	KindPortal     = "portal"
	KindManagement = "management"
	KindDevice     = "device" // a Dialpad PWA device session (management-room scope, PWA-SPEC §3.2)
)

// RoomContext is what a room means for a user (WIDGET-SPEC §5.4).
type RoomContext struct {
	Kind    string              // KindPortal or KindManagement (ResolveRoom), KindDevice (ResolveDevice)
	Login   *bridgev2.UserLogin // nil when not ready (then Refusal says why)
	Number  string              // the portal's E.164 ("" in the management room)
	Refusal string              // user-facing text, set when Login is nil
}

// RoomRef is one eligible room for the installer sweeps.
type RoomRef struct {
	RoomID id.RoomID
	Title  string // "Call <E.164>" in a number portal, "" in the management room
}

// RefusalError is a user-facing refusal returned by Deps.ResolveRoom for rooms that are not eligible
// (the API answers 403 with Message). Any other ResolveRoom error is an internal error.
type RefusalError struct{ Message string }

func (e *RefusalError) Error() string { return e.Message }

// Refuse returns a *RefusalError with msg.
func Refuse(msg string) error { return &RefusalError{Message: msg} }

// MsgNotBridgeRoom is the standard refusal for rooms that are neither an owned portal nor the
// management room.
const MsgNotBridgeRoom = "This room is not a bridge room you own."

// StateAPI reads and writes the widget state event (type im.vector.modular.widgets) as the bridge bot.
type StateAPI interface {
	// GetWidgetState returns the content of the state event (nil, nil when absent).
	GetWidgetState(ctx context.Context, roomID id.RoomID, stateKey string) (map[string]any, error)
	// SetWidgetState sends the state event; an M_FORBIDDEN failure must satisfy
	// errors.Is(err, mautrix.MForbidden).
	SetWidgetState(ctx context.Context, roomID id.RoomID, stateKey string, content map[string]any) error
}

// Deps adapts the widget to a host. Bridge, Calls, ResolveRoom and EligibleRooms are required for New;
// empty strings are derived from Bridge when it has a *matrix.Connector.
type Deps struct {
	Bridge *bridgev2.Bridge
	// Calls returns the call manager, or nil while starting.
	Calls func() hostkit.CallAPI
	// ResolveRoom maps (user, room) to a RoomContext; a *RefusalError (see Refuse) means the room is not
	// eligible. Called on every authenticated request.
	ResolveRoom func(ctx context.Context, user *bridgev2.User, roomID id.RoomID) (RoomContext, error)
	// EligibleRooms lists the rooms that should have the widget (sweep source).
	EligibleRooms func(ctx context.Context) ([]RoomRef, error)

	// RoomOwners optionally lists the users allowed in a room for the installer's privacy check (see
	// PrivateRoom). nil: any one bridge user (LookupUser finds them) plus the bot and ghosts.
	RoomOwners func(ctx context.Context, room id.RoomID) []id.UserID
	// RoomMembers optionally overrides the member list (default: the bot's state store of the bridge's
	// matrix connector; without either the installer cannot check and allows the room).
	RoomMembers func(ctx context.Context, room id.RoomID) ([]id.UserID, error)

	HomeserverURL      string    // homeserver.address (local OpenID check); default from Bridge
	ServerName         string    // homeserver.domain; default from Bridge
	AppserviceID       string    // the state key is "<AppserviceID>_dialpad"; default from Bridge
	BotMXID            id.UserID // creatorUserId; default from Bridge
	DefaultCountryCode string    // for normalize; default "1"
	Log                zerolog.Logger

	// Optional seams (tests). nil = derived from Bridge or a default.
	LookupUser    func(ctx context.Context, mxid id.UserID) (*bridgev2.User, error) // never creates users
	State         StateAPI
	HTTPTransport http.RoundTripper // base transport of the local userinfo client
	RemoteVerify  func(ctx context.Context, serverName, token string) (id.UserID, error)
	Clock         clock.Clock

	// ---- Dialpad PWA (docs/PWA-SPEC.md §8.2); used only when widget.pwa.enabled ----

	// Devices is the device store (required when the PWA is enabled; see OpenDeviceStore).
	Devices *DeviceStore
	// ResolveDevice maps a device session's (user, login) to a RoomContext with Kind KindDevice, on every
	// request. Login set when the login exists, belongs to user and is ready; otherwise Login nil and
	// Refusal = the commands' refusal text. Return ErrDeviceLoginGone when the login no longer exists or
	// belongs to someone else (the API answers 401 device_revoked and the janitor purges the device).
	// Required when the PWA is enabled.
	ResolveDevice func(ctx context.Context, user *bridgev2.User, login networkid.UserLoginID) (RoomContext, error)
	// DeviceOwner is the janitor's orphan check (DeviceStore.PurgeOrphans): false deletes the user's
	// devices of that login. Default (with Bridge): the login exists in br.DB.UserLogin, belongs to user,
	// and user still exists with command permission.
	DeviceOwner DeviceOwnerCheck
	// Notify posts a markdown notice in room as the bridge bot (enrollment, client change, reuse
	// notices). Default (with Bridge): br.Bot.SendMessage as m.notice with logging suppressed.
	Notify func(ctx context.Context, room id.RoomID, markdown string) (id.EventID, error)
	// Redact redacts the bot's link message evt in room. Default (with Bridge): br.Bot redaction.
	Redact func(ctx context.Context, room id.RoomID, evt id.EventID) error
	// CommandPrefix is used in notices ("`!sip dialpad-revoke k3m9qa`"); default from Bridge.Config.
	CommandPrefix string
}

// ErrDeviceLoginGone is returned by Deps.ResolveDevice when the device's login no longer exists or
// belongs to another user.
var ErrDeviceLoginGone = errors.New("widget: dialpad device login gone")

const (
	maxBody        = 4 << 10
	requestTimeout = 10 * time.Second
	janitorEvery   = time.Minute
	msgNotReady    = "The bridge is still starting. Try again in a moment."
)

// Server is the widget HTTP server plus its installer.
type Server struct {
	cfg        hostkit.WidgetConfig
	d          Deps
	log        zerolog.Logger
	clk        clock.Clock
	prefix     string
	origin     string
	secret     []byte
	csp        string
	pwa        bool   // widget.pwa.enabled
	publicURL  string // widget.public_url with trailing slash
	hsURL      string
	serverName string
	defaultCC  string

	sessions *sessionStore
	neg      *negCache
	limits   limits
	hc       *http.Client
	remote   func(ctx context.Context, serverName, token string) (id.UserID, error)
	lookup   func(ctx context.Context, mxid id.UserID) (*bridgev2.User, error)
	static   fs.FS
	inst     *Installer
	handler  http.Handler

	trusted []netip.Prefix // widget.trusted_proxies
	stopped atomic.Bool

	devTokens *tokenDevices  // token hash -> device ID, for the per-device limits
	bg        sync.WaitGroup // async notices and redactions
	bgCtx     context.Context
	bgCancel  context.CancelFunc

	runMu   sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// fillFromBridge derives empty Deps fields from the bridge's matrix connector.
func (d *Deps) fillFromBridge() {
	if d.Bridge == nil {
		return
	}
	if mc, ok := d.Bridge.Matrix.(*matrix.Connector); ok && mc != nil {
		if mc.Config != nil {
			if d.HomeserverURL == "" {
				d.HomeserverURL = mc.Config.Homeserver.Address
			}
			if d.AppserviceID == "" {
				d.AppserviceID = mc.Config.AppService.ID
			}
		}
		if d.BotMXID == "" && mc.Bot != nil {
			d.BotMXID = mc.Bot.UserID
		}
	}
	if d.ServerName == "" && d.Bridge.Matrix != nil {
		d.ServerName = d.Bridge.Matrix.ServerName()
	}
	if d.BotMXID == "" && d.Bridge.Bot != nil {
		d.BotMXID = d.Bridge.Bot.GetMXID()
	}
	if d.LookupUser == nil {
		br := d.Bridge
		d.LookupUser = br.GetExistingUserByMXID
	}
	if d.State == nil {
		d.State = bridgeState{br: d.Bridge}
	}
	if d.CommandPrefix == "" && d.Bridge.Config != nil {
		d.CommandPrefix = d.Bridge.Config.CommandPrefix
	}
	if d.Notify == nil && d.Bridge.Bot != nil {
		d.Notify = bridgeNotify(d.Bridge)
	}
	if d.Redact == nil && d.Bridge.Bot != nil {
		d.Redact = bridgeRedact(d.Bridge)
	}
	if d.DeviceOwner == nil && d.Bridge.DB != nil && d.Bridge.DB.UserLogin != nil {
		d.DeviceOwner = bridgeDeviceOwner(d.Bridge, d.LookupUser)
	}
}

// New builds the widget server. cfg must be enabled and valid (hostkit.WidgetConfig.Validate).
func New(cfg hostkit.WidgetConfig, d Deps) (*Server, error) {
	if !cfg.Enabled {
		return nil, errors.New("widget: not enabled")
	}
	if err := cfg.Validate("widget."); err != nil {
		return nil, err
	}
	d.fillFromBridge()
	switch {
	case d.Calls == nil || d.ResolveRoom == nil || d.EligibleRooms == nil:
		return nil, errors.New("widget: Calls, ResolveRoom and EligibleRooms are required")
	case d.LookupUser == nil || d.State == nil:
		return nil, errors.New("widget: Bridge (or LookupUser and State) is required")
	case d.ServerName == "" || !id.ValidateServerName(d.ServerName):
		return nil, errors.New("widget: invalid server name")
	case d.AppserviceID == "":
		return nil, errors.New("widget: appservice id is required")
	case cfg.PWA.Enabled && (d.Devices == nil || d.ResolveDevice == nil):
		return nil, errors.New("widget: the PWA needs Devices and ResolveDevice")
	}
	hs, err := url.Parse(d.HomeserverURL)
	if err != nil || (hs.Scheme != "http" && hs.Scheme != "https") || hs.Host == "" {
		return nil, errors.New("widget: homeserver URL must be an absolute http(s) URL")
	}
	pub, _ := url.Parse(cfg.PublicURL)
	trusted, err := cfg.ParseTrustedProxies()
	if err != nil {
		return nil, fmt.Errorf("widget: trusted_proxies: %w", err)
	}
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	if d.DefaultCountryCode == "" {
		d.DefaultCountryCode = "1"
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		d:          d,
		log:        d.Log.With().Str("component", "widget").Logger(),
		clk:        d.Clock,
		prefix:     cfg.PublicPrefix(),
		origin:     normalizeOrigin(pub),
		trusted:    trusted,
		secret:     []byte(cfg.Secret),
		csp:        buildCSP(cfg.FrameAncestors),
		pwa:        cfg.PWA.Enabled,
		publicURL:  strings.TrimRight(cfg.PublicURL, "/") + "/",
		devTokens:  newTokenDevices(),
		hsURL:      strings.TrimRight(d.HomeserverURL, "/"),
		serverName: d.ServerName,
		defaultCC:  d.DefaultCountryCode,
		sessions:   newSessionStore(d.Clock),
		neg:        newNegCache(d.Clock),
		limits:     newLimits(d.Clock),
		hc:         newUserinfoClient(d.HTTPTransport),
		remote:     d.RemoteVerify,
		lookup:     d.LookupUser,
		static:     sub,
	}
	if s.remote == nil {
		s.remote = defaultRemoteVerifier(d.Clock)
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.inst = newInstaller(cfg, d, s.log)
	s.handler = http.HandlerFunc(s.serveHTTP)
	liveMu.Lock()
	live[s] = struct{}{}
	liveMu.Unlock()
	return s, nil
}

// normalizeOrigin returns scheme://host[:port] in lower case without the scheme's default port (a
// browser's Origin header never contains it).
func normalizeOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// Prefix returns the mount path (the path of widget.public_url, with trailing slash).
func (s *Server) Prefix() string { return s.prefix }

// Handler returns the HTTP handler for the prefix. Requests outside the prefix get 404.
func (s *Server) Handler() http.Handler { return s.handler }

// Installer returns the installer (implements hostkit.WidgetInstaller).
func (s *Server) Installer() *Installer { return s.inst }

// Mount registers the handler on the bridge's appservice router (br.Matrix.(*matrix.Connector).AS.Router,
// an *http.ServeMux; safe after the server started). Never use GetRouter (nil without public_address).
func Mount(br *bridgev2.Bridge, s *Server) error {
	mc, ok := br.Matrix.(*matrix.Connector)
	if !ok || mc == nil || mc.AS == nil || mc.AS.Router == nil {
		return errors.New("widget: the bridge has no appservice router")
	}
	return MountOn(mc.AS.Router, s)
}

// MountOn registers the handler on mux at the server's prefix.
func MountOn(mux *http.ServeMux, s *Server) (err error) {
	defer func() {
		if r := recover(); r != nil { // ServeMux.Handle panics on conflicting patterns
			err = fmt.Errorf("widget: mount %s: %v", s.prefix, r)
		}
	}()
	mux.Handle(s.prefix, s.handler)
	return nil
}

// Start starts the installer worker (with the startup sweep) and the periodic sweep and janitor. It is
// idempotent; Stop ends them.
func (s *Server) Start() {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.inst.start(ctx, &s.wg)
	s.inst.RequestSweep(context.Background(), TriggerStartup)
	s.wg.Add(1)
	go s.janitor(ctx)
	if s.pwa && s.prefix != "/" {
		s.log.Warn().Str("public_url", s.publicURL).
			Msg("Dialpad app (widget.pwa) shares its hostname with other content: the app keeps a long-lived sign-in in that origin's browser storage. Use a dedicated hostname for widget.public_url in production.")
	}
}

// Stop stops the background goroutines and waits for them. Afterwards the handler answers 503
// not_ready.
func (s *Server) Stop() {
	asyncMu.Lock()
	s.stopped.Store(true)
	asyncMu.Unlock()
	s.runMu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.runMu.Unlock()
	s.wg.Wait()
	s.bgCancel()
	s.bg.Wait()
	liveMu.Lock()
	delete(live, s)
	liveMu.Unlock()
}

// janitor purges expired sessions and idle rate-limit buckets every minute and, with the PWA, redacts
// due link messages (every minute) and purges expired and orphaned devices (hourly, first on the first
// tick).
func (s *Server) janitor(ctx context.Context) {
	defer s.wg.Done()
	t := s.clk.NewTicker(janitorEvery)
	defer t.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			s.sessions.purge()
			s.limits.evict()
			if s.pwa {
				now := s.clk.Now()
				hourly := lastPurge.IsZero() || now.Sub(lastPurge) >= deviceJanitorEvery
				if hourly {
					lastPurge = now
				}
				s.deviceJanitor(ctx, hourly)
			}
		}
	}
}

// logFor returns the server logger with the trace fields of ctx's span.
func (s *Server) logFor(ctx context.Context) *zerolog.Logger { return obs.Logger(ctx, &s.log) }

// buildCSP returns the HTML Content-Security-Policy (WIDGET-SPEC §5.5).
func buildCSP(ancestors []string) string {
	fa := strings.Join(ancestors, " ")
	if fa == "" {
		fa = "'none'"
	}
	return "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors " + fa
}

// appCSP is the Content-Security-Policy of the Dialpad app routes (PWA-SPEC §8.3): never framed.
const appCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var staticNameRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// serveHTTP routes <prefix> (index), <prefix>static/<name> and <prefix>api/v1/<action>.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if s.stopped.Load() {
		h.Set("Cache-Control", "no-store")
		writeErr(w, errNotReady)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, s.prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "":
		s.serveStatic(w, r, "index.html")
	case strings.HasPrefix(rest, "static/"):
		name := strings.TrimPrefix(rest, "static/")
		if !staticNameRE.MatchString(name) || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		s.serveStatic(w, r, name)
	case rest == "app" && s.pwa:
		h.Set("Cache-Control", "no-store")
		http.Redirect(w, r, s.prefix+"app/", http.StatusMovedPermanently)
	case strings.HasPrefix(rest, "app/") && s.pwa:
		name := strings.TrimPrefix(rest, "app/")
		if name == "" {
			name = "index.html"
		}
		if !staticNameRE.MatchString(name) || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		s.serveApp(w, r, name)
	case strings.HasPrefix(rest, "api/v1/"):
		h.Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		s.serveAPI(w, r.WithContext(ctx), strings.TrimPrefix(rest, "api/v1/"))
	default:
		http.NotFound(w, r)
	}
}

// contentTypes of the static assets we serve (anything else is application/octet-stream).
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".json": "application/json",
	// The app's manifest (PWA-SPEC §6.1).
	".webmanifest": "application/manifest+json",
}

// serveStatic writes an embedded asset (GET/HEAD only). Static requests are never logged or traced.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeAsset(w, r, name, data, s.csp)
}

// serveApp writes an asset of the Dialpad app (static/app/) with the app headers (PWA-SPEC §8.3).
func (s *Server) serveApp(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := fs.ReadFile(s.static, "app/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	if name == "sw.js" {
		// The worker's scope is its own directory, <prefix>app/ (no wider scope is ever allowed).
		h.Set("Service-Worker-Allowed", s.prefix+"app/")
	}
	s.writeAsset(w, r, name, data, appCSP)
}

// writeAsset writes data with its content type, no-cache and the given CSP.
func (s *Server) writeAsset(w http.ResponseWriter, r *http.Request, name string, data []byte, csp string) {
	ct, ok := contentTypes[path.Ext(name)]
	if !ok {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", csp)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// ---- JSON helpers ----

// API error codes beyond the hostkit Result codes (WIDGET-SPEC §7).
const (
	codeNotReady      = "not_ready"
	codeLoginRequired = "login_required"
	codeUnauthorized  = "unauthorized"
	codeForbidden     = "forbidden"
	codeRateLimited   = "rate_limited"
	codeBadRequest    = "bad_request"
	codeInternal      = "internal"
)

// apiError is an error response.
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Reason     string // device_revoked: "reuse" after a reuse revoke
}

func (e *apiError) Error() string { return e.Code }

func errBadRequest(msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: codeBadRequest, Message: msg}
}

var (
	errUnauthorized = &apiError{Status: http.StatusUnauthorized, Code: codeUnauthorized, Message: "Sign-in failed or expired."}
	errForbidden    = &apiError{Status: http.StatusForbidden, Code: codeForbidden, Message: "You are not allowed to use this widget here."}
	errInternal     = &apiError{Status: http.StatusInternalServerError, Code: codeInternal, Message: "Something went wrong."}
	errNotReady     = &apiError{Status: http.StatusServiceUnavailable, Code: codeNotReady, Message: msgNotReady}
)

func errRateLimited(retry time.Duration) *apiError {
	return &apiError{Status: http.StatusTooManyRequests, Code: codeRateLimited, Message: "Slow down: too many requests.", RetryAfter: retry}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	if e.RetryAfter > 0 {
		secs := int((e.RetryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	body := map[string]string{"error": e.Code, "message": e.Message}
	if e.Reason != "" {
		body["reason"] = e.Reason
	}
	writeJSON(w, e.Status, body)
}

// checkPost enforces the state-changing request rules: POST, Content-Type application/json, and a
// same-origin Origin when one is sent (WIDGET-SPEC §5.5).
func (s *Server) checkPost(r *http.Request) *apiError {
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, s.origin) {
		return &apiError{Status: http.StatusForbidden, Code: codeForbidden, Message: "Cross-origin requests are not allowed."}
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: codeBadRequest, Message: "Send application/json."}
	}
	return nil
}

// decodeJSON strictly decodes a bounded JSON object body into v (unknown fields and trailing data are
// refused; an empty body decodes as {}). Errors never echo the body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) *apiError {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &apiError{Status: http.StatusRequestEntityTooLarge, Code: codeBadRequest, Message: "Request too large."}
		}
		return errBadRequest("Could not read the request.")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return errBadRequest("Malformed request.")
	}
	if _, err = dec.Token(); err != io.EOF {
		return errBadRequest("Malformed request.")
	}
	return nil
}

// clientIP returns the rate-limit key of the client: the peer, or the last X-Forwarded-For hop (the
// address our reverse proxy saw) when the peer is a trusted proxy. With no trusted proxies configured
// every private or loopback peer counts as one. IPv6 clients are keyed by their /64.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	isProxy := peer.IsPrivate() || peer.IsLoopback()
	if len(trusted) > 0 {
		isProxy = false
		for _, p := range trusted {
			if p.Contains(peer) {
				isProxy = true
				break
			}
		}
	}
	if isProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return limiterKey(a.Unmap())
			}
		}
	}
	return limiterKey(peer)
}

// limiterKey is the string form of a (IPv6: its /64 prefix, so one host cannot rotate addresses).
func limiterKey(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}
