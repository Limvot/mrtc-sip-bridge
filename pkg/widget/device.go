package widget

// Dialpad PWA server side (docs/PWA-SPEC.md §3, §5, §7, §8.2, §10): the device API handlers
// (POST api/v1/device/{enroll,session,confirm,forget}), reuse handling, the DialpadLinks implementation
// used by the hostkit commands, the bot notices and link-message redaction, and the device janitor.
//
// Privacy: codes, device tokens and session tokens are never logged, traced or put on metrics; neither
// are device names or the User-Agent (only the closed client enum). Link IDs and MXIDs may appear on
// spans and logs, never on metrics.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Device API error codes (PWA-SPEC §7).
const (
	codeLinkInvalid   = "link_invalid"
	codeLinkUsed      = "link_used"
	codeLinkExpired   = "link_expired"
	codeDeviceInvalid = "device_invalid"
	codeDeviceExpired = "device_expired"
	codeDeviceRevoked = "device_revoked"
)

// Span outcomes of the device spans (beyond outcomeOK, outcomeRateLimited, outcomeError, outcomeForbidden).
const (
	outcomePeek    = "peek"
	outcomeInvalid = "invalid"
	outcomeUsed    = "used"
	outcomeExpired = "expired"
	outcomeRevoked = "revoked"
	outcomeReuse   = "reuse"
	outcomeRepeat  = "repeat"
)

const (
	deviceJanitorEvery = time.Hour
	asyncTimeout       = 30 * time.Second
	maxTokenCache      = 8192
	msgSignedOut       = "This device is signed out. Get a new link with `dialpad-link` in your management room."
)

var (
	errLinkInvalid = &apiError{Status: http.StatusUnauthorized, Code: codeLinkInvalid,
		Message: "This link is not valid. Request a new one with `dialpad-link` in your management room."}
	errLinkUsed = &apiError{Status: http.StatusGone, Code: codeLinkUsed,
		Message: "This link was already used. Request a new one."}
	errLinkExpired = &apiError{Status: http.StatusGone, Code: codeLinkExpired,
		Message: "This link has expired. Request a new one."}
	errDeviceInvalid = &apiError{Status: http.StatusUnauthorized, Code: codeDeviceInvalid, Message: msgSignedOut}
	errDeviceExpired = &apiError{Status: http.StatusUnauthorized, Code: codeDeviceExpired,
		Message: "This device was signed out because it was not used for a long time. Get a new link with `dialpad-link` in your management room."}
)

func errDeviceRevoked(reason string) *apiError {
	e := &apiError{Status: http.StatusUnauthorized, Code: codeDeviceRevoked, Reason: reason,
		Message: "This device was signed out. Get a new link with `dialpad-link` in your management room."}
	if reason == RevokedWhyReuse {
		e.Message = "Signed out for safety: an old copy of this app's sign-in was used somewhere else. Get a new link with `dialpad-link` in your management room."
	}
	return e
}

// ---- host helpers ----

// DeviceStoreConfigFrom maps the widget.pwa config block to the store config.
func DeviceStoreConfigFrom(p hostkit.PWAConfig) DeviceStoreConfig {
	return DeviceStoreConfig{
		InactivityDays:  p.InactivityDays,
		MaxLifetimeDays: p.MaxLifetimeDays,
		RotationGrace:   time.Duration(p.RotationGraceSeconds) * time.Second,
		EnrollTTL:       time.Duration(p.EnrollMinutes) * time.Minute,
		MaxLinksPerUser: p.MaxLinksPerUser,
	}
}

// OpenDeviceStore creates the device store on the bridge's database (bridge_id = br.ID) and upgrades
// it. Call it in the host's Start, after the bridge's own DB upgrade and before New, only when
// cfg.PWA.Enabled; an error should fail the start.
func OpenDeviceStore(ctx context.Context, br *bridgev2.Bridge, cfg hostkit.WidgetConfig, log zerolog.Logger) (*DeviceStore, error) {
	if br == nil || br.DB == nil || br.DB.Database == nil {
		return nil, errors.New("widget: OpenDeviceStore needs a bridge with a database")
	}
	st := NewDeviceStore(br.DB.Database, br.ID, DeviceStoreConfigFrom(cfg.PWA), nil,
		log.With().Str("component", "dialpad_devices").Logger())
	if err := st.Upgrade(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

// quietCtx returns ctx with a logger that discards everything. It must not be a disabled logger:
// mautrix falls back to the client's own logger (which logs request bodies at Debug) when the ctx
// logger is disabled.
func quietCtx(ctx context.Context) context.Context {
	return zerolog.New(io.Discard).WithContext(ctx)
}

// bridgeNotify is the default Deps.Notify: an m.notice from the bridge bot, logging suppressed.
func bridgeNotify(br *bridgev2.Bridge) func(ctx context.Context, room id.RoomID, md string) (id.EventID, error) {
	return func(ctx context.Context, room id.RoomID, md string) (id.EventID, error) {
		content := format.RenderMarkdown(md, true, false)
		content.MsgType = event.MsgNotice
		resp, err := br.Bot.SendMessage(quietCtx(ctx), room, event.EventMessage, &event.Content{Parsed: &content}, nil)
		if err != nil {
			return "", err
		}
		if resp == nil {
			return "", errors.New("widget: empty send response")
		}
		return resp.EventID, nil
	}
}

// bridgeRedact is the default Deps.Redact: a redaction by the bridge bot.
func bridgeRedact(br *bridgev2.Bridge) func(ctx context.Context, room id.RoomID, evt id.EventID) error {
	return func(ctx context.Context, room id.RoomID, evt id.EventID) error {
		_, err := br.Bot.SendMessage(quietCtx(ctx), room, event.EventRedaction,
			&event.Content{Parsed: &event.RedactionEventContent{Redacts: evt}}, nil)
		return err
	}
}

// bridgeDeviceOwner is the default Deps.DeviceOwner.
func bridgeDeviceOwner(br *bridgev2.Bridge, lookup func(context.Context, id.UserID) (*bridgev2.User, error)) DeviceOwnerCheck {
	return func(ctx context.Context, user id.UserID, login networkid.UserLoginID) (bool, error) {
		ul, err := br.DB.UserLogin.GetByID(ctx, login)
		if err != nil {
			return false, err
		}
		if ul == nil || ul.UserMXID != user {
			return false, nil
		}
		if lookup == nil {
			return true, nil
		}
		u, err := lookup(ctx, user)
		if err != nil {
			return false, err
		}
		return u != nil && u.Permissions.Commands, nil
	}
}

// ---- client enum (§2.6) ----

// clientFromUA maps a User-Agent to the closed client enum. The UA itself is never stored or logged.
func clientFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		if strings.Contains(ua, "CriOS") || strings.Contains(ua, "FxiOS") || strings.Contains(ua, "EdgiOS") {
			return ClientOther
		}
		return ClientSafariIOS
	case strings.Contains(ua, "Android"):
		switch {
		case strings.Contains(ua, "Firefox/"):
			return ClientFirefoxAndroid
		case strings.Contains(ua, "Chrome/"):
			return ClientChromeAndroid
		}
		return ClientOther
	case strings.Contains(ua, "Firefox/"):
		return ClientFirefoxDesktop
	case strings.Contains(ua, "Chrome/"):
		return ClientChromeDesktop
	}
	return ClientOther
}

// clientLabel is the human form of a client enum value.
func clientLabel(c string) string {
	switch c {
	case ClientChromeAndroid:
		return "Chrome Android"
	case ClientFirefoxAndroid:
		return "Firefox Android"
	case ClientSafariIOS:
		return "Safari iOS"
	case ClientChromeDesktop:
		return "Chrome Desktop"
	case ClientFirefoxDesktop:
		return "Firefox Desktop"
	}
	return "another browser"
}

// ---- token -> device cache (per-device limits before the DB is touched) ----

// tokenDevices remembers which device a token hash belongs to, so the per-device limits can refuse a
// request before the store rotates anything. Unknown tokens (after a restart) are charged afterwards.
type tokenDevices struct {
	mu sync.Mutex
	m  map[[32]byte]string
}

func newTokenDevices() *tokenDevices { return &tokenDevices{m: make(map[[32]byte]string)} }

func (c *tokenDevices) get(tok string) string {
	h := sha256.Sum256([]byte(tok))
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[h]
}

func (c *tokenDevices) put(devID string, toks ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range toks {
		if t == "" {
			continue
		}
		if len(c.m) >= maxTokenCache {
			c.m = make(map[[32]byte]string)
		}
		c.m[sha256.Sum256([]byte(t))] = devID
	}
}

// ---- background work ----

var asyncMu sync.Mutex // guards Server.bg Add against Stop's Wait

// async runs fn in a goroutine with a bounded context that Stop cancels and waits for. Used for bot
// notices and redactions, so HTTP handlers never wait for Matrix I/O.
func (s *Server) async(fn func(ctx context.Context)) {
	asyncMu.Lock()
	if s.stopped.Load() {
		asyncMu.Unlock()
		return
	}
	s.bg.Add(1)
	asyncMu.Unlock()
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(s.bgCtx, asyncTimeout)
		defer cancel()
		fn(ctx)
	}()
}

// escapeMD escapes the markdown characters a device name could contain.
func escapeMD(s string) string {
	return strings.NewReplacer(`\`, `\\`, "_", `\_`, "*", `\*`, "`", "'", "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;").Replace(s)
}

func (s *Server) cmd(args string) string {
	return strings.TrimSpace(s.d.CommandPrefix + " " + args)
}

// notify posts md in room (async). Failures are logged at Debug (never the text).
func (s *Server) notify(room id.RoomID, devID, kind, md string) {
	if s.d.Notify == nil || room == "" {
		return
	}
	s.async(func(ctx context.Context) {
		if _, err := s.d.Notify(ctx, room, md); err != nil {
			s.log.Debug().Str("device_id", devID).Str("notice", kind).Str("error", obs.Scrub(err.Error())).
				Msg("Failed to send Dialpad notice")
		}
	})
}

// redactNotice redacts a link message and records the result in the store. It reports success.
func (s *Server) redactNotice(ctx context.Context, devID string, room id.RoomID, evt id.EventID) bool {
	st := s.d.Devices
	if s.d.Redact == nil || st == nil || evt == "" {
		return false
	}
	if err := s.d.Redact(ctx, room, evt); err != nil {
		gaveUp, ferr := st.NoticeFailed(ctx, devID, evt)
		s.log.Debug().Str("device_id", devID).Bool("gave_up", gaveUp).Str("error", obs.Scrub(err.Error())).
			AnErr("store_error", ferr).Msg("Failed to redact Dialpad link message")
		return false
	}
	if err := st.ClearNotice(ctx, devID, evt); err != nil {
		s.log.Debug().Str("device_id", devID).Err(err).Msg("Failed to clear Dialpad link message")
	}
	recordDeviceEvent(ctx, evNoticeRedacted, 1)
	return true
}

// dropDevices kills the sessions of deleted devices and redacts their pending link messages (async).
func (s *Server) dropDevices(devs []Device) {
	for _, d := range devs {
		s.sessions.revokeDevice(d.ID)
		if d.NoticeEvent != "" {
			d := d
			s.async(func(ctx context.Context) {
				if s.d.Redact != nil {
					if err := s.d.Redact(ctx, d.NoticeRoom, d.NoticeEvent); err != nil {
						s.log.Debug().Str("device_id", d.ID).Str("error", obs.Scrub(err.Error())).
							Msg("Failed to redact Dialpad link message of a removed device")
					} else {
						recordDeviceEvent(ctx, evNoticeRedacted, 1)
					}
				}
			})
		}
	}
}

// ---- handlers ----

type deviceView struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	User             id.UserID `json:"user"`
	InactiveExpiryAt *string   `json:"inactive_expiry_at,omitempty"`
}

type enrollReq struct {
	Code    string `json:"code"`
	Confirm bool   `json:"confirm"`
	// PreviousToken optionally names the device this app was enrolled as before: after a successful
	// enrollment that family is forgotten when it belongs to the same user (a replaced sign-in).
	PreviousToken string `json:"previous_token,omitempty"`
}

type enrollPeekResp struct {
	User      id.UserID `json:"user"`
	Name      string    `json:"name"`
	ExpiresIn int64     `json:"expires_in"`
}

type enrollResp struct {
	DeviceToken string     `json:"device_token"`
	Device      deviceView `json:"device"`
}

type deviceTokenReq struct {
	DeviceToken string `json:"device_token"`
}

type deviceSessionResp struct {
	Session   string     `json:"session"`
	ExpiresIn int64      `json:"expires_in"`
	NextToken string     `json:"next_token"`
	Room      roomView   `json:"room"`
	Device    deviceView `json:"device"`
}

// devFail ends a failed device request: counts it for the IP lockout when count is set, sets the span
// outcome, logs one line (no secrets) and returns e.
func (s *Server) devFail(ctx context.Context, span trace.Span, action, ip, outcome string, count bool, e *apiError) (string, *apiError) {
	if count {
		s.limits.sessionFail.fail(ip)
	}
	span.SetAttributes(obs.AttrOutcome.String(outcome))
	s.logFor(ctx).Info().Str("action", action).Str("outcome", outcome).Msg("Dialpad device request refused")
	return "", e
}

// devPrelude runs the per-IP checks shared by enroll/session/confirm: lockout, then the IP bucket.
func (s *Server) devPrelude(r *http.Request, ipLimit *limiter) (string, *apiError) {
	ip := clientIP(r, s.trusted)
	if locked, retry := s.limits.sessionFail.locked(ip); locked {
		return ip, errRateLimited(retry)
	}
	if ipLimit != nil {
		if ok, retry := ipLimit.allow(ip); !ok {
			return ip, errRateLimited(retry)
		}
	}
	return ip, nil
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	ip, e := s.devPrelude(r, s.limits.enrollIP)
	if e != nil {
		return "", e
	}
	var req enrollReq
	if e := decodeJSON(w, r, &req); e != nil {
		s.limits.sessionFail.fail(ip)
		return "", e
	}
	ctx, span := obs.Start(r.Context(), spanDevEnroll, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrDeviceConfirm.Bool(req.Confirm)))
	defer span.End()
	if !ValidCode(req.Code) { // malformed input never reaches the DB
		return s.devFail(ctx, span, actDevEnroll, ip, outcomeInvalid, true, errLinkInvalid)
	}
	st := s.d.Devices
	dev, err := st.Peek(ctx, req.Code)
	if err != nil {
		return s.enrollErr(ctx, span, ip, err)
	}
	span.SetAttributes(obs.AttrMatrixSender.String(string(dev.User)), attrDeviceID.String(dev.ID))
	if _, e := s.user(ctx, dev.User); e != nil {
		if e.Code == codeInternal {
			span.SetAttributes(obs.AttrOutcome.String(outcomeError))
			return "", e
		}
		return s.devFail(ctx, span, actDevEnroll, ip, outcomeForbidden, false, e)
	}
	if !req.Confirm {
		span.SetAttributes(obs.AttrOutcome.String(outcomePeek))
		exp := int64(dev.CodeExpires.Sub(s.clk.Now()) / time.Second)
		writeJSON(w, http.StatusOK, enrollPeekResp{User: dev.User, Name: dev.Name, ExpiresIn: max(exp, 0)})
		return "", nil
	}
	res, err := st.Enroll(ctx, req.Code, clientFromUA(r.UserAgent()))
	if err != nil {
		return s.enrollErr(ctx, span, ip, err)
	}
	dev = res.Device
	s.devTokens.put(dev.ID, res.Token)
	span.SetAttributes(obs.AttrOutcome.String(outcomeOK))
	recordDeviceEvent(ctx, evEnrolled, 1)
	s.logFor(ctx).Info().Str("action", actDevEnroll).Stringer("user", dev.User).Str("device_id", dev.ID).
		Str("client", dev.Client).Str("outcome", outcomeOK).Msg("Dialpad device enrolled")
	writeJSON(w, http.StatusOK, enrollResp{DeviceToken: res.Token,
		Device: deviceView{ID: dev.ID, Name: dev.Name, User: dev.User, InactiveExpiryAt: timePtr(dev.ExpiresAt)}})

	// After the response: redact the link message, post the enrollment notice, forget a replaced device.
	if dev.NoticeEvent != "" {
		s.async(func(ctx context.Context) { s.redactNotice(ctx, dev.ID, dev.NoticeRoom, dev.NoticeEvent) })
	}
	if s.cfg.PWA.NotifyOnEnrollEnabled() {
		s.notify(dev.Room, dev.ID, "enrolled", fmt.Sprintf(
			"Dialpad link `%s` (%s) was set up on %s just now. Not you? `%s`.",
			dev.ID, escapeMD(dev.Name), clientLabel(dev.Client), s.cmd("dialpad-revoke "+dev.ID)))
	}
	if ValidDeviceToken(req.PreviousToken) {
		prev := req.PreviousToken
		s.async(func(ctx context.Context) { s.forgetPrevious(ctx, dev, prev) })
	}
	return "", nil
}

// forgetPrevious forgets the device of prev when it is a different device of the same user as the new
// device dev, whatever the state of the token. It only looks the device up (DeviceOfToken has no side
// effects): previous_token is client-supplied, so it must not confirm, rotate or trip reuse detection,
// and a token of another user's device changes nothing.
func (s *Server) forgetPrevious(ctx context.Context, dev Device, prev string) {
	st := s.d.Devices
	old, err := st.DeviceOfToken(ctx, prev)
	if err != nil || old.ID == "" || old.ID == dev.ID || old.User != dev.User {
		return
	}
	gone, err := st.Revoke(ctx, dev.User, old.ID)
	if err != nil {
		return
	}
	s.dropDevices(gone)
	recordDeviceEvent(ctx, evRevoked, len(gone))
	s.log.Info().Stringer("user", dev.User).Str("device_id", old.ID).Str("new_device_id", dev.ID).
		Msg("Dialpad device replaced by a new enrollment")
}

// enrollErr maps a Peek/Enroll error.
func (s *Server) enrollErr(ctx context.Context, span trace.Span, ip string, err error) (string, *apiError) {
	switch {
	case errors.Is(err, ErrLinkInvalid):
		return s.devFail(ctx, span, actDevEnroll, ip, outcomeInvalid, true, errLinkInvalid)
	case errors.Is(err, ErrLinkUsed):
		return s.devFail(ctx, span, actDevEnroll, ip, outcomeUsed, true, errLinkUsed)
	case errors.Is(err, ErrLinkExpired):
		return s.devFail(ctx, span, actDevEnroll, ip, outcomeExpired, true, errLinkExpired)
	}
	obs.Fail(span, err)
	span.SetAttributes(obs.AttrOutcome.String(outcomeError))
	s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Dialpad enrollment failed")
	return "", errInternal
}

// tokenErr maps a Rotate/Confirm error (dev is the token's device when it was found; reuse is set when
// this call detected reuse).
func (s *Server) tokenErr(ctx context.Context, span trace.Span, action, ip string, dev Device, reuse bool, err error) (string, *apiError) {
	switch {
	case errors.Is(err, ErrDeviceInvalid):
		return s.devFail(ctx, span, action, ip, outcomeInvalid, true, errDeviceInvalid)
	case errors.Is(err, ErrDeviceExpired):
		recordDeviceEvent(ctx, evExpired, 1)
		return s.devFail(ctx, span, action, ip, outcomeExpired, true, errDeviceExpired)
	case errors.Is(err, ErrDeviceRevoked):
		if dev.ID != "" {
			span.SetAttributes(attrDeviceID.String(dev.ID), obs.AttrMatrixSender.String(string(dev.User)))
		}
		reason := ""
		if reuse || dev.RevokedWhy == RevokedWhyReuse {
			reason = RevokedWhyReuse
		}
		if reuse {
			s.handleReuse(ctx, dev)
			return s.devFail(ctx, span, action, ip, outcomeReuse, true, errDeviceRevoked(reason))
		}
		return s.devFail(ctx, span, action, ip, outcomeRevoked, true, errDeviceRevoked(reason))
	}
	obs.Fail(span, err)
	span.SetAttributes(obs.AttrOutcome.String(outcomeError))
	s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Dialpad device token operation failed")
	return "", errInternal
}

// handleReuse reacts to a detected token reuse (the store already revoked the family): Warn log, kill
// the device's sessions, management-room notice.
func (s *Server) handleReuse(ctx context.Context, dev Device) {
	s.logFor(ctx).Warn().Stringer("user", dev.User).Str("device_id", dev.ID).Str("client", dev.Client).
		Msg("Dialpad device token reuse detected; device revoked")
	s.sessions.revokeDevice(dev.ID)
	recordDeviceEvent(ctx, evReuseRevoked, 1)
	s.notify(dev.Room, dev.ID, "reuse", fmt.Sprintf(
		"Dialpad `%s` (%s) was signed out: an old copy of its sign-in was used, which can mean it was copied from your phone. Set it up again with `%s` if this was you.",
		dev.ID, escapeMD(dev.Name), s.cmd("dialpad-link")))
}

// devLimit applies a per-device limit before the store call when the token's device is known.
func (s *Server) devLimitBefore(l *limiter, tok string) *apiError {
	if devID := s.devTokens.get(tok); devID != "" {
		if over, retry := l.exhausted(devID); over {
			return errRateLimited(retry)
		}
	}
	return nil
}

func (s *Server) handleDeviceSession(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	ip, e := s.devPrelude(r, s.limits.devSessionIP)
	if e != nil {
		return "", e
	}
	var req deviceTokenReq
	if e := decodeJSON(w, r, &req); e != nil {
		s.limits.sessionFail.fail(ip)
		return "", e
	}
	ctx, span := obs.Start(r.Context(), spanDevSession, trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	if !ValidDeviceToken(req.DeviceToken) {
		return s.devFail(ctx, span, actDevSession, ip, outcomeInvalid, true, errDeviceInvalid)
	}
	if e := s.devLimitBefore(s.limits.devSession, req.DeviceToken); e != nil {
		return s.devFail(ctx, span, actDevSession, ip, outcomeRateLimited, false, e)
	}
	// Taken before the rotation: a revocation of the device at or after this instant (another request's
	// reuse detection, dialpad-revoke) refuses the session below, even if it raced past Rotate.
	started := s.clk.Now()
	res, err := s.d.Devices.Rotate(ctx, req.DeviceToken, clientFromUA(r.UserAgent()))
	if err != nil {
		return s.tokenErr(ctx, span, actDevSession, ip, res.Device, res.Reuse, err)
	}
	dev := res.Device
	s.devTokens.put(dev.ID, req.DeviceToken, res.Next)
	span.SetAttributes(obs.AttrMatrixSender.String(string(dev.User)), attrDeviceID.String(dev.ID),
		attrTokenState.String(res.TokenState))
	recordDeviceEvent(ctx, evRotated, 1)
	if res.TokenState == TokenStateGrace {
		recordDeviceEvent(ctx, evGraceUsed, 1)
	}
	if res.ClientChanged {
		s.notify(dev.Room, dev.ID, "client_changed", fmt.Sprintf(
			"Dialpad `%s` (%s) was used from %s, set up on %s. Not you? `%s`.",
			dev.ID, escapeMD(dev.Name), clientLabel(res.Client), clientLabel(dev.Client), s.cmd("dialpad-revoke "+dev.ID)))
	}
	// Charged after the rotation (the successor stays unconfirmed when refused here, so nothing breaks).
	if ok, retry := s.limits.devSession.allow(dev.ID); !ok {
		return s.devFail(ctx, span, actDevSession, ip, outcomeRateLimited, false, errRateLimited(retry))
	}
	if ok, retry := s.limits.sessionUser.allow(string(dev.User)); !ok {
		return s.devFail(ctx, span, actDevSession, ip, outcomeRateLimited, false, errRateLimited(retry))
	}
	user, e := s.user(ctx, dev.User)
	if e != nil {
		if e.Code == codeInternal {
			span.SetAttributes(obs.AttrOutcome.String(outcomeError))
			return "", e
		}
		return s.devFail(ctx, span, actDevSession, ip, outcomeForbidden, false, e)
	}
	rc, e := s.resolveDevice(ctx, user, dev.Login, dev.ID)
	if e != nil {
		switch e.Code {
		case codeInternal:
			span.SetAttributes(obs.AttrOutcome.String(outcomeError))
			return "", e
		case codeDeviceRevoked:
			return s.devFail(ctx, span, actDevSession, ip, outcomeRevoked, false, e)
		}
		return s.devFail(ctx, span, actDevSession, ip, outcomeForbidden, false, e)
	}
	ttl := maxSessionTTL
	if !dev.ExpiresAt.IsZero() {
		if left := dev.ExpiresAt.Sub(s.clk.Now()); left < ttl {
			ttl = left
		}
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	stok, ss, err := s.sessions.createDevice(dev.User, dev.Login, dev.ID, ttl, started)
	if errors.Is(err, errSessionDeviceRevoked) {
		return s.devFail(ctx, span, actDevSession, ip, outcomeRevoked, false, errDeviceRevoked(""))
	}
	if err != nil {
		obs.Fail(span, err)
		span.SetAttributes(obs.AttrOutcome.String(outcomeError))
		return "", errInternal
	}
	span.SetAttributes(obs.AttrOutcome.String(outcomeOK))
	s.logFor(ctx).Info().Str("action", actDevSession).Stringer("user", dev.User).Str("device_id", dev.ID).
		Str("token_state", res.TokenState).Str("outcome", outcomeOK).Msg("Dialpad device session created")
	writeJSON(w, http.StatusOK, deviceSessionResp{
		Session:   stok,
		ExpiresIn: int64(ss.expires.Sub(ss.created) / time.Second),
		NextToken: res.Next,
		Room:      roomView{Kind: rc.Kind},
		Device:    deviceView{ID: dev.ID, Name: dev.Name, User: dev.User, InactiveExpiryAt: timePtr(dev.ExpiresAt)},
	})
	return "", nil
}

func (s *Server) handleDeviceConfirm(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	ip, e := s.devPrelude(r, nil)
	if e != nil {
		return "", e
	}
	var req deviceTokenReq
	if e := decodeJSON(w, r, &req); e != nil {
		s.limits.sessionFail.fail(ip)
		return "", e
	}
	ctx, span := obs.Start(r.Context(), spanDevConfirm, trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	if !ValidDeviceToken(req.DeviceToken) {
		return s.devFail(ctx, span, actDevConfirm, ip, outcomeInvalid, true, errDeviceInvalid)
	}
	if e := s.devLimitBefore(s.limits.devConfirm, req.DeviceToken); e != nil {
		return s.devFail(ctx, span, actDevConfirm, ip, outcomeRateLimited, false, e)
	}
	res, err := s.d.Devices.Confirm(ctx, req.DeviceToken)
	if err != nil {
		return s.tokenErr(ctx, span, actDevConfirm, ip, res.Device, res.Reuse, err)
	}
	dev := res.Device
	s.devTokens.put(dev.ID, req.DeviceToken)
	span.SetAttributes(obs.AttrMatrixSender.String(string(dev.User)), attrDeviceID.String(dev.ID))
	if ok, retry := s.limits.devConfirm.allow(dev.ID); !ok {
		return s.devFail(ctx, span, actDevConfirm, ip, outcomeRateLimited, false, errRateLimited(retry))
	}
	o := outcomeOK
	if res.Repeat {
		o = outcomeRepeat
	}
	span.SetAttributes(obs.AttrOutcome.String(o))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return "", nil
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req struct{}
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	ss := s.sessions.get(bearer(r))
	if ss == nil || ss.device == "" {
		return "", errUnauthorized
	}
	ctx, span := obs.Start(r.Context(), spanDevForget, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(obs.AttrMatrixSender.String(string(ss.user)), attrDeviceID.String(ss.device)))
	defer span.End()
	gone, err := s.d.Devices.Revoke(ctx, ss.user, ss.device)
	if err != nil && !errors.Is(err, ErrDeviceNotFound) {
		obs.Fail(span, err)
		span.SetAttributes(obs.AttrOutcome.String(outcomeError))
		s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Dialpad forget failed")
		return "", errInternal
	}
	s.sessions.revokeDevice(ss.device)
	s.dropDevices(gone)
	recordDeviceEvent(ctx, evRevoked, len(gone))
	span.SetAttributes(obs.AttrOutcome.String(outcomeOK))
	s.logFor(ctx).Info().Str("action", actDevForget).Stringer("user", ss.user).Str("device_id", ss.device).
		Str("outcome", outcomeOK).Msg("Dialpad device forgotten")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return "", nil
}

// ---- DialpadLinks (hostkit commands) ----

// DialpadLinks returns the hostkit.DialpadLinks implementation for CommandDeps.Dialpad, or nil when
// widget.pwa is disabled.
func (s *Server) DialpadLinks() hostkit.DialpadLinks {
	if s == nil || !s.pwa || s.d.Devices == nil {
		return nil
	}
	return dialpadLinks{s: s}
}

type dialpadLinks struct{ s *Server }

var _ hostkit.DialpadLinks = dialpadLinks{}

// mgmtPrivate reports whether room's joined or invited members are only user, the bot, this bridge's
// ghosts and Deps.RoomOwners. Without a member source it cannot tell and returns an error (links are
// refused rather than risked).
func (s *Server) mgmtPrivate(ctx context.Context, room id.RoomID, user id.UserID) (bool, error) {
	var members []id.UserID
	var err error
	if s.d.RoomMembers != nil {
		members, err = s.d.RoomMembers(ctx, room)
	} else {
		members, err = storeMembers(ctx, s.d.Bridge, room)
	}
	if err != nil {
		return false, err
	}
	allowed := []id.UserID{user}
	if s.d.RoomOwners != nil {
		allowed = append(allowed, s.d.RoomOwners(ctx, room)...)
	}
	ghost := func(id.UserID) bool { return false }
	if s.d.Bridge != nil && s.d.Bridge.Matrix != nil {
		br := s.d.Bridge
		ghost = func(u id.UserID) bool { return isGhost(br, u) }
	}
	return privateAmong(members, s.d.BotMXID, allowed, ghost), nil
}

func linkEvent(ctx context.Context, name, devID, outcome string) {
	attrs := []attribute.KeyValue{obs.AttrOutcome.String(outcome)}
	if devID != "" {
		attrs = append(attrs, attrDeviceID.String(devID))
	}
	trace.SpanFromContext(ctx).AddEvent(name, trace.WithAttributes(attrs...))
}

// Issue creates a pending link for (user, login) after checking that the management room is private.
func (l dialpadLinks) Issue(ctx context.Context, user id.UserID, login networkid.UserLoginID, room id.RoomID, name string) (hostkit.LinkIssue, error) {
	s := l.s
	ok, err := s.mgmtPrivate(ctx, room, user)
	if err != nil {
		linkEvent(ctx, "dialpad.link.issue", "", outcomeError)
		return hostkit.LinkIssue{}, fmt.Errorf("widget: management room privacy check: %w", err)
	}
	if !ok {
		linkEvent(ctx, "dialpad.link.issue", "", "not_private")
		return hostkit.LinkIssue{}, hostkit.ErrDialpadRoomNotPrivate
	}
	res, err := s.d.Devices.Issue(ctx, IssueParams{User: user, Login: login, Room: room, Name: name})
	if err != nil {
		if errors.Is(err, ErrDeviceLimit) {
			linkEvent(ctx, "dialpad.link.issue", "", "limit")
			return hostkit.LinkIssue{}, fmt.Errorf("%w: %w", hostkit.ErrDialpadLinkLimit, err)
		}
		linkEvent(ctx, "dialpad.link.issue", "", outcomeError)
		return hostkit.LinkIssue{}, err
	}
	recordDeviceEvent(ctx, evIssued, 1)
	linkEvent(ctx, "dialpad.link.issue", res.Device.ID, outcomeOK)
	s.logFor(ctx).Info().Stringer("user", user).Str("device_id", res.Device.ID).Msg("Dialpad link issued")
	app := s.publicURL + "app/"
	return hostkit.LinkIssue{
		ID:        res.Device.ID,
		Name:      res.Device.Name,
		Link:      app + "#c=" + res.Code,
		AppURL:    app,
		ExpiresAt: res.Device.CodeExpires,
	}, nil
}

// AttachNotice records the bot's link message for redaction.
func (l dialpadLinks) AttachNotice(ctx context.Context, linkID string, room id.RoomID, evt id.EventID) error {
	return l.s.d.Devices.AttachNotice(ctx, linkID, room, evt)
}

// List returns user's links. Reuse-revoked links show as "expired".
func (l dialpadLinks) List(ctx context.Context, user id.UserID) ([]hostkit.LinkInfo, error) {
	devs, err := l.s.d.Devices.List(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]hostkit.LinkInfo, 0, len(devs))
	for _, d := range devs {
		state := string(d.State)
		exp := d.ExpiresAt
		switch {
		case d.State == DeviceStateRevoked:
			state, exp = "expired", d.RevokedAt
		case d.Expired:
			state = "expired"
		}
		out = append(out, hostkit.LinkInfo{ID: d.ID, Name: d.Name, State: state, Client: d.Client,
			Created: d.Created, LastUsed: d.LastUsed, Expires: exp})
	}
	return out, nil
}

// Revoke deletes user's link linkID (or all of them), kills their sessions and redacts pending link
// messages. An unknown or foreign link is (0, nil).
func (l dialpadLinks) Revoke(ctx context.Context, user id.UserID, linkID string) (int, error) {
	s := l.s
	gone, err := s.d.Devices.Revoke(ctx, user, linkID)
	if errors.Is(err, ErrDeviceNotFound) {
		linkEvent(ctx, "dialpad.link.revoke", "", "not_found")
		return 0, nil
	}
	if err != nil {
		linkEvent(ctx, "dialpad.link.revoke", "", outcomeError)
		return 0, err
	}
	s.dropDevices(gone)
	recordDeviceEvent(ctx, evRevoked, len(gone))
	for _, d := range gone {
		linkEvent(ctx, "dialpad.link.revoke", d.ID, outcomeOK)
		s.logFor(ctx).Info().Stringer("user", user).Str("device_id", d.ID).Msg("Dialpad link revoked")
	}
	return len(gone), nil
}

// ---- janitor ----

// deviceJanitor redacts due link messages and, when hourly, purges expired and orphaned devices. It
// records a bridge.widget.device_janitor span only when it did something.
func (s *Server) deviceJanitor(ctx context.Context, hourly bool) {
	st := s.d.Devices
	if st == nil {
		return
	}
	start := time.Now()
	var redacted, failed, purged int
	if s.d.Redact != nil {
		notices, err := st.DueNotices(ctx)
		if err != nil {
			s.log.Warn().Err(err).Msg("Dialpad janitor: listing due link messages failed")
		}
		for _, n := range notices {
			rctx, cancel := context.WithTimeout(ctx, requestTimeout)
			if s.redactNotice(rctx, n.DeviceID, n.Room, n.Event) {
				redacted++
			} else {
				failed++
			}
			cancel()
		}
	}
	if hourly {
		stats, err := st.PurgeExpired(ctx)
		if err != nil {
			s.log.Warn().Err(err).Msg("Dialpad janitor: purging expired devices failed")
		}
		purged += stats.Devices
		recordDeviceEvent(ctx, evExpired, stats.Devices)
		if s.d.DeviceOwner != nil {
			gone, err := st.PurgeOrphans(ctx, s.d.DeviceOwner)
			if err != nil {
				s.log.Warn().Err(err).Msg("Dialpad janitor: purging orphaned devices failed")
			}
			for _, d := range gone {
				s.sessions.revokeDevice(d.ID)
				if d.NoticeEvent != "" && s.d.Redact != nil {
					rctx, cancel := context.WithTimeout(ctx, requestTimeout)
					if err := s.d.Redact(rctx, d.NoticeRoom, d.NoticeEvent); err == nil {
						redacted++
						recordDeviceEvent(ctx, evNoticeRedacted, 1)
					} else {
						failed++
					}
					cancel()
				}
				s.log.Info().Stringer("user", d.User).Str("device_id", d.ID).Msg("Dialpad device of a removed login purged")
			}
			purged += len(gone)
			recordDeviceEvent(ctx, evOrphanPurged, len(gone))
		}
	}
	if redacted+failed+purged == 0 {
		return
	}
	_, span := obs.Start(ctx, spanDevJanitor, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(start),
		trace.WithAttributes(attrDevicesPurged.Int(purged), attrNoticesRedact.Int(redacted), attrNoticesFailed.Int(failed)))
	span.End()
}
