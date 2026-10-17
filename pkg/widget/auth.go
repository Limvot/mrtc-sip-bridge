package widget

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/exhttp"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// ---- Room key (WIDGET-SPEC §5.2) ----

const (
	keyLabel = "mrtcsip-dialpad-v1\x00"
	keyLen   = 22 // base64url chars, 132 bits (>= 128)
)

// roomKey returns k(room) = base64url(HMAC-SHA256(secret, label || roomID))[:22].
func roomKey(secret []byte, room id.RoomID) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(keyLabel))
	m.Write([]byte(room))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:keyLen]
}

// keyMatches compares key with k(room) in constant time (only the public length can leak).
func keyMatches(secret []byte, room id.RoomID, key string) bool {
	want := roomKey(secret, room)
	return subtle.ConstantTimeCompare([]byte(want), []byte(key)) == 1
}

// ---- OpenID verification (WIDGET-SPEC §5.3 step 2) ----

const (
	verifyTimeoutDefault = 5 * time.Second
	userinfoPath         = "/_matrix/federation/v1/openid/userinfo"
	maxUserinfoBody      = 16 << 10
	negCacheTTL          = time.Minute
	negCacheMax          = 4096
)

// verifyTimeout bounds the userinfo request (a var so tests can shorten it).
var verifyTimeout = verifyTimeoutDefault

// ErrOpenIDInvalid is returned by a RemoteVerify func when the homeserver rejected the token (or its
// sub belongs to another server). Any other error counts as a transient openid_error.
var ErrOpenIDInvalid = errors.New("widget: openid token rejected")

// errOpenIDTransient is a sanitized transient failure (never carries the request URL or token).
type errOpenIDTransient struct{ kind string }

func (e errOpenIDTransient) Error() string { return "widget: openid verification failed: " + e.kind }

// userinfoTemplate is the fixed url.template of the userinfo client span (the token never reaches it).
func userinfoTemplate(string) string { return userinfoPath }

// newUserinfoClient returns the local-homeserver userinfo client: 5 s timeout, no redirects, traced
// through obs with a fixed template.
func newUserinfoClient(base http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: obs.WrapHTTPTransport(base, userinfoTemplate),
		Timeout:   verifyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// verifyLocal checks token against the bridge's own homeserver (homeserver.address, no federation
// discovery) and returns the token's user, which must be on serverName.
func verifyLocal(ctx context.Context, hc *http.Client, hsURL, serverName, token string) (id.UserID, error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	u := hsURL + userinfoPath + "?access_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errOpenIDTransient{"request"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		// err is a *url.Error carrying the URL (and so the token): never return or log it.
		if ctx.Err() != nil {
			return "", errOpenIDTransient{"timeout"}
		}
		return "", errOpenIDTransient{"transport"}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUserinfoBody))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return "", ErrOpenIDInvalid
	default:
		return "", errOpenIDTransient{fmt.Sprintf("status %d", resp.StatusCode)}
	}
	var body struct {
		Sub id.UserID `json:"sub"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxUserinfoBody)).Decode(&body); err != nil {
		return "", errOpenIDTransient{"decode"}
	}
	return checkSub(body.Sub, serverName)
}

// checkSub validates sub and requires it to be on serverName.
func checkSub(sub id.UserID, serverName string) (id.UserID, error) {
	_, hs, err := sub.Parse()
	if err != nil || hs != serverName {
		return "", ErrOpenIDInvalid
	}
	return sub, nil
}

const (
	maxRemoteServers  = 256 // distinct remote server names resolved per remoteNamesWindow
	remoteNamesWindow = time.Hour
)

// cgnatRange (100.64.0.0/10) is shared address space that DefaultAllowIP lets through.
var cgnatRange = netip.MustParsePrefix("100.64.0.0/10")

// remoteAllowIP is the federation dial filter: DefaultAllowIP minus carrier-grade NAT space.
func remoteAllowIP(ip net.IP) bool {
	if !federation.DefaultAllowIP(ip) {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	return !ok || !cgnatRange.Contains(a.Unmap())
}

// newFederationClient builds the federation client of remote verification. Every connection (the
// userinfo request and the .well-known fetch of server discovery) goes through the client's
// IP-filtering dialer; the discovery client has timeouts and follows no redirects.
func newFederationClient() (*federation.Client, *federation.ServerResolvingTransport) {
	fc := federation.NewClient("", nil, federation.NewInMemoryCache(), exhttp.ClientSettings{
		DialTimeout:           verifyTimeout,
		GlobalTimeout:         verifyTimeout,
		TLSHandshakeTimeout:   verifyTimeout,
		ResponseHeaderTimeout: verifyTimeout,
		IdleConnTimeout:       time.Minute,
	})
	fc.AllowIP = remoteAllowIP // read by the dialer's control func at connect time
	srt, _ := fc.HTTP.Transport.(*federation.ServerResolvingTransport)
	if srt != nil {
		srt.ResolveOpts = &federation.ResolveServerNameOpts{HTTPClient: newDiscoveryClient(fc.Dialer)}
	}
	fc.HTTP.Transport = obs.WrapHTTPTransport(fc.HTTP.Transport, userinfoTemplate)
	fc.ResponseSizeLimit = maxUserinfoBody
	return fc, srt
}

// newDiscoveryClient is the HTTP client of .well-known fetches: it dials through d (whose control
// func filters addresses), times out and never follows a redirect.
func newDiscoveryClient(d *net.Dialer) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   verifyTimeout,
			ResponseHeaderTimeout: verifyTimeout,
			IdleConnTimeout:       time.Minute,
			MaxIdleConns:          16,
		},
		Timeout:       verifyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// serverNames bounds how many distinct remote server names are resolved per window, so a stream of
// tokens naming made-up servers cannot drive unlimited DNS and connection attempts.
type serverNames struct {
	mu   sync.Mutex
	clk  clock.Clock
	max  int
	seen map[string]time.Time
}

func newServerNames(clk clock.Clock, max int) *serverNames {
	return &serverNames{clk: clk, max: max, seen: make(map[string]time.Time)}
}

// admit reports whether serverName may be resolved now (known names always may).
func (n *serverNames) admit(serverName string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.clk.Now()
	if _, ok := n.seen[serverName]; ok {
		n.seen[serverName] = now
		return true
	}
	if len(n.seen) >= n.max {
		for k, t := range n.seen {
			if now.Sub(t) >= remoteNamesWindow {
				delete(n.seen, k)
			}
		}
		if len(n.seen) >= n.max {
			return false
		}
	}
	n.seen[serverName] = now
	return true
}

// subMismatchRE matches GetOpenIDUserInfo's own error for a sub on another server (mautrix v0.32.0).
var subMismatchRE = regexp.MustCompile(`^user ID \S+ does not belong to server `)

// defaultRemoteVerifier verifies tokens of other homeservers over federation (mautrix
// federation.Client.GetOpenIDUserInfo, which resolves the server and rejects a sub from a different
// server). Resolution and connections are IP-filtered, see newFederationClient.
func defaultRemoteVerifier(clk clock.Clock) func(ctx context.Context, serverName, token string) (id.UserID, error) {
	var once sync.Once
	var fc *federation.Client
	names := newServerNames(clk, maxRemoteServers)
	return func(ctx context.Context, serverName, token string) (id.UserID, error) {
		if !names.admit(serverName) {
			return "", errOpenIDTransient{"too many servers"}
		}
		once.Do(func() { fc, _ = newFederationClient() })
		ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
		defer cancel()
		resp, err := fc.GetOpenIDUserInfo(ctx, serverName, token)
		if err != nil {
			// The error text may contain the request URL with the token: map it, never return it.
			var he mautrix.HTTPError
			if errors.As(err, &he) && he.Response != nil {
				if sc := he.Response.StatusCode; sc >= 400 && sc < 500 && sc != http.StatusTooManyRequests {
					return "", ErrOpenIDInvalid
				}
				return "", errOpenIDTransient{fmt.Sprintf("status %d", he.Response.StatusCode)}
			}
			if subMismatchRE.MatchString(err.Error()) {
				return "", ErrOpenIDInvalid
			}
			// Resolution, IP-filter, transport and decode failures are transient: they must not be
			// negative-cached against the token.
			return "", errOpenIDTransient{"transport"}
		}
		if resp == nil {
			return "", ErrOpenIDInvalid
		}
		return checkSub(resp.Sub, serverName)
	}
}

// negCache remembers rejected tokens (by hash) briefly, so a replayed bad token costs no HS request.
type negCache struct {
	mu  sync.Mutex
	clk clock.Clock
	m   map[[32]byte]time.Time
}

func newNegCache(clk clock.Clock) *negCache {
	return &negCache{clk: clk, m: make(map[[32]byte]time.Time)}
}

func tokenHash(serverName, token string) [32]byte {
	return sha256.Sum256([]byte(serverName + "\x00" + token))
}

func (c *negCache) has(h [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.m[h]
	if !ok {
		return false
	}
	if !c.clk.Now().Before(exp) {
		delete(c.m, h)
		return false
	}
	return true
}

func (c *negCache) add(h [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.Now()
	if len(c.m) >= negCacheMax {
		for k, exp := range c.m {
			if !now.Before(exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= negCacheMax {
			c.m = make(map[[32]byte]time.Time)
		}
	}
	c.m[h] = now.Add(negCacheTTL)
}

// verifyOutcome is the widget.session outcome of a verification.
const (
	outcomeBadKey        = "bad_key"
	outcomeOpenIDInvalid = "openid_invalid"
	outcomeOpenIDError   = "openid_error"
	outcomeNoUser        = "no_user"
	outcomeForbidden     = "forbidden"
	outcomeRateLimited   = "rate_limited"
	outcomeError         = "error" // internal failure (user lookup, room resolution)
)

// verifyOpenID returns the token's user and "" or the failure outcome (openid_invalid, openid_error,
// forbidden for remote users when they are not allowed). It runs under the widget.session span and
// starts the child-only widget.openid.verify span.
func (s *Server) verifyOpenID(ctx context.Context, serverName, token string) (id.UserID, string) {
	local := serverName == s.serverName
	if !local && !s.cfg.AllowRemoteUsers {
		return "", outcomeForbidden
	}
	h := tokenHash(serverName, token)
	if s.neg.has(h) {
		return "", outcomeOpenIDInvalid
	}
	ctx, span := obs.StartChild(ctx, spanVerify, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(obs.AttrServerAddress.String(serverName)))
	defer span.End()
	var sub id.UserID
	var err error
	if local {
		sub, err = verifyLocal(ctx, s.hc, s.hsURL, serverName, token)
	} else {
		sub, err = s.remote(ctx, serverName, token)
	}
	switch {
	case err == nil:
		span.SetAttributes(obs.AttrOutcome.String(outcomeOK))
		return sub, ""
	case errors.Is(err, ErrOpenIDInvalid):
		s.neg.add(h)
		span.SetAttributes(obs.AttrOutcome.String(outcomeOpenIDInvalid))
		return "", outcomeOpenIDInvalid
	default:
		span.SetAttributes(obs.AttrOutcome.String(outcomeOpenIDError))
		// Never record a raw error: a RemoteVerify implementation might include the URL.
		obs.Fail(span, errOpenIDTransient{"error"})
		s.logFor(ctx).Warn().Str("server", serverName).Str("error", obs.Scrub(err.Error())).
			Msg("Widget OpenID verification failed")
		return "", outcomeOpenIDError
	}
}

// ---- Sessions (WIDGET-SPEC §5.3 step 4) ----

const (
	sessionTokenBytes = 32 // 43 base64url chars
	sessionTokenLen   = 43
	maxSessionTTL     = time.Hour
	maxSessionsUser   = 8
	maxSessionsTotal  = 1000
)

// session is a widget session bound to one user and one room (whose key was checked at creation), or
// a Dialpad PWA device session (device != ""): bound to the user, the login the link was issued for
// and the link ID, with room "" (management-room scope, PWA-SPEC §3.1).
type session struct {
	hash    [32]byte
	user    id.UserID
	room    id.RoomID
	device  string                // link ID of a PWA device session; "" for widget sessions
	login   networkid.UserLoginID // the device's login (device sessions only)
	created time.Time
	expires time.Time
}

// sessionStore keeps sessions in memory, keyed by sha256(token): the token itself is never stored.
type sessionStore struct {
	mu  sync.Mutex
	clk clock.Clock
	m   map[[32]byte]*session
	// revoked is when each PWA device's sessions were last revoked (revokeDevice), so createDevice can
	// refuse a session for a device revoked while its exchange was in flight. Entries are purged
	// maxSessionTTL after the revoke.
	revoked map[string]time.Time
}

// errSessionDeviceRevoked: createDevice refused because the device was revoked at or after notBefore.
var errSessionDeviceRevoked = errors.New("widget: dialpad device revoked during session creation")

func newSessionStore(clk clock.Clock) *sessionStore {
	return &sessionStore{clk: clk, m: make(map[[32]byte]*session), revoked: make(map[string]time.Time)}
}

// create returns a new session token for user in room, living ttl (capped at maxSessionTTL). It evicts
// the user's oldest session beyond maxSessionsUser and the overall oldest beyond maxSessionsTotal.
func (st *sessionStore) create(user id.UserID, room id.RoomID, ttl time.Duration) (string, *session, error) {
	return st.add(&session{user: user, room: room}, ttl, time.Time{})
}

// createDevice returns a new device session for link devID of (user, login); caps as create. It
// returns errSessionDeviceRevoked when devID's sessions were revoked at or after notBefore (the time
// the caller took before validating the device).
func (st *sessionStore) createDevice(user id.UserID, login networkid.UserLoginID, devID string, ttl time.Duration, notBefore time.Time) (string, *session, error) {
	return st.add(&session{user: user, device: devID, login: login}, ttl, notBefore)
}

// add stores ss (user, room, device and login set) under a fresh token. A device session is refused
// with errSessionDeviceRevoked when its device was revoked at or after notBefore.
func (st *sessionStore) add(ss *session, ttl time.Duration, notBefore time.Time) (string, *session, error) {
	user := ss.user
	if ttl <= 0 || ttl > maxSessionTTL {
		ttl = maxSessionTTL
	}
	var raw [sessionTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.clk.Now()
	st.purgeLocked(now)
	if ss.device != "" {
		if at, ok := st.revoked[ss.device]; ok && !at.Before(notBefore) {
			return "", nil, errSessionDeviceRevoked
		}
	}
	var mine []*session
	for _, x := range st.m {
		if x.user == user {
			mine = append(mine, x)
		}
	}
	if len(mine) >= maxSessionsUser {
		sort.Slice(mine, func(i, j int) bool { return mine[i].created.Before(mine[j].created) })
		for _, x := range mine[:len(mine)-maxSessionsUser+1] {
			delete(st.m, x.hash)
		}
	}
	for len(st.m) >= maxSessionsTotal {
		var oldest *session
		for _, x := range st.m {
			if oldest == nil || x.created.Before(oldest.created) {
				oldest = x
			}
		}
		delete(st.m, oldest.hash)
	}
	ss.hash, ss.created, ss.expires = sha256.Sum256([]byte(tok)), now, now.Add(ttl)
	st.m[ss.hash] = ss
	return tok, ss, nil
}

// get returns the live session of tok, or nil. The map lookup is on sha256(tok), so lookup timing
// reveals nothing usable about valid tokens.
func (st *sessionStore) get(tok string) *session {
	if len(tok) != sessionTokenLen || !isB64URL(tok) {
		return nil
	}
	h := sha256.Sum256([]byte(tok))
	st.mu.Lock()
	defer st.mu.Unlock()
	ss := st.m[h]
	if ss == nil {
		return nil
	}
	if !st.clk.Now().Before(ss.expires) {
		delete(st.m, h)
		return nil
	}
	cp := *ss
	return &cp
}

// revokeDevice deletes every session of PWA device devID and returns how many there were.
func (st *sessionStore) revokeDevice(devID string) int {
	if devID == "" {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.revoked[devID] = st.clk.Now()
	n := 0
	for h, x := range st.m {
		if x.device == devID {
			delete(st.m, h)
			n++
		}
	}
	return n
}

// revokeUser deletes every session (widget and device) of user and returns how many there were.
func (st *sessionStore) revokeUser(user id.UserID) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for h, x := range st.m {
		if x.user == user {
			delete(st.m, h)
			n++
		}
	}
	return n
}

func (st *sessionStore) purge() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.purgeLocked(st.clk.Now())
}

func (st *sessionStore) purgeLocked(now time.Time) {
	for h, x := range st.m {
		if !now.Before(x.expires) {
			delete(st.m, h)
		}
	}
	for dev, at := range st.revoked {
		if now.Sub(at) > maxSessionTTL {
			delete(st.revoked, dev)
		}
	}
}

// count returns the number of unexpired sessions.
func (st *sessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.clk.Now()
	n := 0
	for _, x := range st.m {
		if now.Before(x.expires) {
			n++
		}
	}
	return n
}

func isB64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// bearer returns the token of an "Authorization: Bearer <tok>" header ("" if absent or malformed).
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
