package widget

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Widget actions (the widget.action metric attribute).
const (
	actSession   = "session"
	actState     = "state"
	actNormalize = "normalize"
	actCall      = "call"
	actDTMF      = "dtmf"
	actHangup    = "hangup"
	actPrefs     = "prefs"    // path prefs/roster_only
	actChat      = "chat"     // optional host feature (hostkit.ChatOpener)
	actContact   = "contact"  // optional host feature (hostkit.ContactSaver)
	actContacts  = "contacts" // optional host feature (hostkit.ContactLister), GET, no span

	// Dialpad PWA endpoints: the path is api/v1/device/<x>, the metric action is actDev*.
	actDevEnroll  = "enroll"
	actDevSession = "device_session"
	actDevConfirm = "device_confirm"
	actDevForget  = "forget"
)

// serveAPI dispatches <prefix>api/v1/<action>. HTTP status mapping: 200 ok; 400 bad_request and
// validation results (invalid_number, not_allowed, self_dial, invalid_digits); 401 unauthorized;
// 403 forbidden and login_required; 409 call-state results (line_busy, no_active_call, not_connected,
// dtmf_unsupported, dtmf_busy); 413 body too large; 415 wrong content type; 429 rate_limited
// (Retry-After); 503 not_ready, outbound_disabled and unavailable; 500 internal. The optional host
// features add 400 invalid_name and 404 not_supported (the host lacks the feature).
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, action string) {
	var method string
	var fn func(http.ResponseWriter, *http.Request) (string, *apiError)
	switch action {
	case actSession:
		method, fn = http.MethodPost, s.handleSession
	case actState:
		method, fn = http.MethodGet, s.handleState
	case actNormalize:
		method, fn = http.MethodPost, s.handleNormalize
	case actCall:
		method, fn = http.MethodPost, s.handleCall
	case actDTMF:
		method, fn = http.MethodPost, s.handleDTMF
	case actHangup:
		method, fn = http.MethodPost, s.handleHangup
	case actChat:
		method, fn = http.MethodPost, s.handleChat
	case actContacts:
		method, fn = http.MethodGet, s.handleContacts
	case actContact:
		method, fn = http.MethodPost, s.handleContact
	case "prefs/roster_only":
		action, method, fn = actPrefs, http.MethodPost, s.handlePrefs
	case "device/enroll", "device/session", "device/confirm", "device/forget":
		if !s.pwa {
			writeErr(w, &apiError{Status: http.StatusNotFound, Code: codeBadRequest, Message: "Unknown endpoint."})
			return
		}
		method = http.MethodPost
		switch action {
		case "device/enroll":
			action, fn = actDevEnroll, s.handleEnroll
		case "device/session":
			action, fn = actDevSession, s.handleDeviceSession
		case "device/confirm":
			action, fn = actDevConfirm, s.handleDeviceConfirm
		default:
			action, fn = actDevForget, s.handleForget
		}
	default:
		writeErr(w, &apiError{Status: http.StatusNotFound, Code: codeBadRequest, Message: "Unknown endpoint."})
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeErr(w, &apiError{Status: http.StatusMethodNotAllowed, Code: codeBadRequest, Message: "Method not allowed."})
		return
	}
	client := s.requestClient(r, action)
	if method == http.MethodPost {
		if e := s.checkPost(r); e != nil {
			recordRequest(r.Context(), action, client, e.Code)
			writeErr(w, e)
			return
		}
	}
	code, e := fn(w, r)
	if e != nil {
		code = e.Code
		if e.Reason == RevokedWhyReuse {
			code = mReuse
		}
		writeErr(w, e)
	}
	recordRequest(r.Context(), action, client, code)
}

// requestClient is the widget.client metric attribute of a request: pwa for the device endpoints and
// device sessions, widget otherwise.
func (s *Server) requestClient(r *http.Request, action string) string {
	switch action {
	case actDevEnroll, actDevSession, actDevConfirm, actDevForget:
		return clientPWA
	case actSession:
		return clientWidget
	}
	if ss := s.sessions.get(bearer(r)); ss != nil && ss.device != "" {
		return clientPWA
	}
	return clientWidget
}

// ---- session ----

type openIDCreds struct {
	AccessToken      string `json:"access_token"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int64  `json:"expires_in"`
	TokenType        string `json:"token_type,omitempty"` // ignored (Widget API sends "Bearer")
}

type sessionReq struct {
	OpenID   openIDCreds `json:"openid"`
	RoomID   id.RoomID   `json:"room_id"`
	Key      string      `json:"key"`
	WidgetID string      `json:"widget_id"`
}

type roomView struct {
	Kind    string       `json:"kind"`
	Number  string       `json:"number"`
	Contact *contactInfo `json:"contact,omitempty"` // state only, portal rooms with ContactLookup
}

// contactInfo is the saved-contact status of a portal's number.
type contactInfo struct {
	Name  string `json:"name"`
	Saved bool   `json:"saved"`
}

type sessionResp struct {
	Session   string   `json:"session"`
	ExpiresIn int64    `json:"expires_in"`
	Room      roomView `json:"room"`
}

// sessionFail ends a failed session exchange: counts the failure for the IP lockout, sets the span
// outcome and returns e.
func (s *Server) sessionFail(ctx context.Context, span trace.Span, ip, outcome string, e *apiError) (string, *apiError) {
	if outcome != outcomeRateLimited && outcome != outcomeOpenIDError {
		s.limits.sessionFail.fail(ip)
	}
	span.SetAttributes(obs.AttrOutcome.String(outcome))
	s.logFor(ctx).Info().Str("action", actSession).Str("outcome", outcome).Msg("Widget session refused")
	return "", e
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	ip := clientIP(r, s.trusted)
	if locked, retry := s.limits.sessionFail.locked(ip); locked {
		return "", errRateLimited(retry)
	}
	if ok, retry := s.limits.sessionIP.allow(ip); !ok {
		return "", errRateLimited(retry)
	}
	var req sessionReq
	if e := decodeJSON(w, r, &req); e != nil {
		s.limits.sessionFail.fail(ip)
		return "", e
	}
	ctx, span := obs.Start(r.Context(), spanSession, trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	if validRoomID(req.RoomID) {
		span.SetAttributes(obs.AttrMatrixRoomID.String(string(req.RoomID)))
	}
	// 1. Room key (constant time). Malformed input is a bad key too.
	if !validRoomID(req.RoomID) || len(req.Key) != keyLen || !keyMatches(s.secret, req.RoomID, req.Key) {
		return s.sessionFail(ctx, span, ip, outcomeBadKey, errUnauthorized)
	}
	// 2. OpenID token.
	tok, srv := req.OpenID.AccessToken, req.OpenID.MatrixServerName
	if tok == "" || len(tok) > 2048 || !id.ValidateServerName(srv) {
		return s.sessionFail(ctx, span, ip, outcomeOpenIDInvalid, errUnauthorized)
	}
	sub, outcome := s.verifyOpenID(ctx, srv, tok)
	switch outcome {
	case "":
	case outcomeForbidden:
		return s.sessionFail(ctx, span, ip, outcome, &apiError{Status: http.StatusForbidden, Code: codeForbidden,
			Message: "Users of other homeservers cannot use this widget."})
	case outcomeOpenIDError:
		return s.sessionFail(ctx, span, ip, outcome, &apiError{Status: http.StatusServiceUnavailable, Code: codeInternal,
			Message: "Could not check your sign-in with the homeserver. Try again."})
	default:
		return s.sessionFail(ctx, span, ip, outcome, errUnauthorized)
	}
	span.SetAttributes(obs.AttrMatrixSender.String(string(sub)))
	if ok, retry := s.limits.sessionUser.allow(string(sub)); !ok {
		return s.sessionFail(ctx, span, ip, outcomeRateLimited, errRateLimited(retry))
	}
	// 3. Existing user with command permission (never create users).
	user, err := s.lookup(ctx, sub)
	switch {
	case err != nil:
		s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Widget user lookup failed")
		span.SetAttributes(obs.AttrOutcome.String(outcomeError))
		return "", errInternal
	case user == nil:
		return s.sessionFail(ctx, span, ip, outcomeNoUser, errForbidden)
	case !user.Permissions.Commands:
		return s.sessionFail(ctx, span, ip, outcomeForbidden, errForbidden)
	}
	// 4. Room scoping. A valid room without a ready login still gets a session (state reports why).
	rc, e := s.resolve(ctx, user, req.RoomID)
	if e != nil {
		if e.Code == codeInternal {
			span.SetAttributes(obs.AttrOutcome.String(outcomeError))
			return "", e
		}
		return s.sessionFail(ctx, span, ip, outcomeForbidden, e)
	}
	span.SetAttributes(attrRoomKind.String(rc.Kind))
	ttl := time.Duration(req.OpenID.ExpiresIn) * time.Second
	stok, ss, err := s.sessions.create(sub, req.RoomID, ttl)
	if err != nil {
		obs.Fail(span, err)
		return "", errInternal
	}
	span.SetAttributes(obs.AttrOutcome.String(outcomeOK))
	s.logFor(ctx).Info().Str("action", actSession).Stringer("user", sub).Stringer("room_id", req.RoomID).
		Str("outcome", outcomeOK).Msg("Widget session created")
	writeJSON(w, http.StatusOK, sessionResp{
		Session:   stok,
		ExpiresIn: int64(ss.expires.Sub(ss.created) / time.Second),
		Room:      roomView{Kind: rc.Kind, Number: rc.Number},
	})
	return "", nil
}

func validRoomID(r id.RoomID) bool {
	return len(r) > 1 && len(r) <= 255 && strings.HasPrefix(string(r), "!") &&
		!strings.ContainsAny(string(r), " \t\r\n\x00")
}

// user returns the existing bridge user with command permission (errForbidden otherwise).
func (s *Server) user(ctx context.Context, mxid id.UserID) (*bridgev2.User, *apiError) {
	u, err := s.lookup(ctx, mxid)
	if err != nil {
		s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Widget user lookup failed")
		return nil, errInternal
	}
	if u == nil || !u.Permissions.Commands {
		return nil, errForbidden
	}
	return u, nil
}

// resolve runs Deps.ResolveRoom: a refusal is 403 with its text, other errors are internal.
func (s *Server) resolve(ctx context.Context, user *bridgev2.User, room id.RoomID) (RoomContext, *apiError) {
	rc, err := s.d.ResolveRoom(ctx, user, room)
	if err != nil {
		var ref *RefusalError
		if errors.As(err, &ref) {
			return rc, &apiError{Status: http.StatusForbidden, Code: codeForbidden, Message: s.expand(ref.Message)}
		}
		s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Widget room resolution failed")
		return rc, errInternal
	}
	if rc.Kind != KindPortal && rc.Kind != KindManagement {
		return rc, errInternal
	}
	return rc, nil
}

// ---- authenticated requests ----

// authed is the context of an authenticated request: the session's user and room, resolved again.
type authed struct {
	ss   *session
	user *bridgev2.User
	rc   RoomContext
}

// authenticate checks the bearer session, the user's permission and the room (every request: a logout
// takes effect immediately). The room always comes from the session, never from the request.
func (s *Server) authenticate(r *http.Request) (*authed, *apiError) {
	ss := s.sessions.get(bearer(r))
	if ss == nil {
		return nil, errUnauthorized
	}
	ctx := r.Context()
	user, e := s.user(ctx, ss.user)
	if e != nil {
		return nil, e
	}
	if ss.device != "" {
		rc, e := s.resolveDevice(ctx, user, ss.login, ss.device)
		if e != nil {
			return nil, e
		}
		return &authed{ss: ss, user: user, rc: rc}, nil
	}
	rc, e := s.resolve(ctx, user, ss.room)
	if e != nil {
		return nil, e
	}
	return &authed{ss: ss, user: user, rc: rc}, nil
}

// resolveDevice runs Deps.ResolveDevice for a device session or exchange: a gone login is 401
// device_revoked (and the device's sessions are killed), a refusal error is 403, other errors internal.
func (s *Server) resolveDevice(ctx context.Context, user *bridgev2.User, login networkid.UserLoginID, devID string) (RoomContext, *apiError) {
	if s.d.ResolveDevice == nil {
		return RoomContext{}, errInternal
	}
	rc, err := s.d.ResolveDevice(ctx, user, login)
	if err != nil {
		var ref *RefusalError
		switch {
		case errors.Is(err, ErrDeviceLoginGone):
			s.sessions.revokeDevice(devID)
			return rc, errDeviceRevoked("")
		case errors.As(err, &ref):
			return rc, &apiError{Status: http.StatusForbidden, Code: codeForbidden, Message: s.expand(ref.Message)}
		}
		s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Dialpad device resolution failed")
		return rc, errInternal
	}
	if rc.Kind != KindDevice {
		return rc, errInternal
	}
	return rc, nil
}

// ready returns the call API and login of a, or the not_ready / login_required error.
func (s *Server) ready(a *authed) (hostkit.CallAPI, core.LoginRef, *apiError) {
	if a.rc.Login == nil {
		msg := s.expand(a.rc.Refusal)
		if msg == "" {
			msg = "You are not logged in."
		}
		return nil, "", &apiError{Status: http.StatusForbidden, Code: codeLoginRequired, Message: msg}
	}
	api := s.d.Calls()
	if api == nil {
		return nil, "", errNotReady
	}
	return api, core.LoginRef(a.rc.Login.ID), nil
}

// ---- state ----

// CallView is the call shown by the widget (WIDGET-SPEC §7).
type CallView struct {
	ID         string    `json:"id"`
	Direction  string    `json:"direction"`
	Phase      string    `json:"phase"`
	Number     string    `json:"number"`
	RoomID     id.RoomID `json:"room_id"`
	InThisRoom bool      `json:"in_this_room"`
	StartedAt  *string   `json:"started_at"`  // RFC 3339, null when unknown
	AnsweredAt *string   `json:"answered_at"` // RFC 3339, null until answered
	CanDTMF    bool      `json:"can_dtmf"`
}

type loginView struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
}

// featuresView tells the UI which optional host features exist (all false unless the login is ready).
type featuresView struct {
	RosterOnly  bool `json:"roster_only"`
	Chat        bool `json:"chat"`
	SaveContact bool `json:"save_contact"`
	Contacts    bool `json:"contacts"`
}

type prefsView struct {
	RosterOnly bool   `json:"roster_only"`
	Warning    string `json:"warning"`
}

type stateResp struct {
	Room     roomView     `json:"room"`
	Login    loginView    `json:"login"`
	Call     *CallView    `json:"call"`
	Features featuresView `json:"features"`
	Prefs    *prefsView   `json:"prefs"` // null unless features.roster_only
}

// phaseOf maps an actor state to the UI phase enum.
func phaseOf(snap calls.Snapshot) string {
	switch snap.State {
	case "setup", "ringing":
		if snap.Outbound {
			return "ringing_you"
		}
		return "incoming"
	case "answering":
		return "incoming"
	case "joining":
		return "connecting"
	case "dialing":
		return "calling"
	case "early":
		return "ringing_them"
	case "active":
		return "connected"
	default: // ending, ended, unknown
		return "ending"
	}
}

func timePtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

// callView builds the CallView of snap for a session in room.
func callView(snap calls.Snapshot, room id.RoomID) *CallView {
	dir := snap.Direction
	if dir == "" {
		dir = "inbound"
		if snap.Outbound {
			dir = "outbound"
		}
	}
	return &CallView{
		ID:         snap.ID,
		Direction:  dir,
		Phase:      phaseOf(snap),
		Number:     snap.Remote,
		RoomID:     snap.RoomID,
		InThisRoom: snap.RoomID != "" && snap.RoomID == room,
		StartedAt:  timePtr(snap.StartedAt),
		AnsweredAt: timePtr(snap.AnsweredAt),
		CanDTMF:    snap.State == "active",
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	a, e := s.authenticate(r)
	if e != nil {
		return "", e
	}
	if ok, retry := s.limits.state.allow(string(a.ss.hash[:])); !ok {
		return "", errRateLimited(retry)
	}
	resp := stateResp{Room: roomView{Kind: a.rc.Kind, Number: a.rc.Number}}
	api, login, e := s.ready(a)
	switch {
	case e != nil:
		resp.Login = loginView{Message: e.Message}
	default:
		resp.Login = loginView{Ready: true}
		resp.Features = featuresView{RosterOnly: s.feat.prefs != nil, Chat: s.feat.chat != nil, SaveContact: s.feat.saver != nil,
			Contacts: s.feat.lister != nil}
		// The host methods below read memory only (hostkit contract): state is polled every 1-5 s.
		if s.feat.prefs != nil {
			st := s.feat.prefs.RosterOnly(a.rc.Login)
			resp.Prefs = &prefsView{RosterOnly: st.On, Warning: s.expand(st.Warning)}
		}
		if s.feat.lookup != nil && a.ss.device == "" && a.rc.Kind == KindPortal && a.rc.Number != "" {
			name, saved := s.feat.lookup.ContactName(a.rc.Login, a.rc.Number)
			resp.Room.Contact = &contactInfo{Name: name, Saved: saved || name != ""}
		}
		if snap, ok := api.Snapshot(login); ok {
			resp.Call = callView(snap, a.ss.room)
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return "", nil
}

// ---- normalize ----

type numberReq struct {
	Number string `json:"number"`
}

type normalizeResp struct {
	OK      bool   `json:"ok"`
	E164    string `json:"e164"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	// ChatOK: the number may be texted or saved although the call policy may refuse it. Present only
	// when the host offers Message or Save contact.
	ChatOK *bool `json:"chat_ok,omitempty"`
}

func (s *Server) handleNormalize(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req numberReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	a, e := s.authenticate(r)
	if e != nil {
		return "", e
	}
	if ok, retry := s.limits.normalize.allow(string(a.ss.user)); !ok {
		return "", errRateLimited(retry)
	}
	api, login, e := s.ready(a)
	if e != nil {
		return "", e
	}
	if len(req.Number) > 64 {
		req.Number = req.Number[:64]
	}
	res := hostkit.CheckNumberAction(api, login, req.Number, s.defaultCC)
	var chatOK *bool
	if s.feat.chat != nil || s.feat.saver != nil {
		ok := res.OK || hostkit.CheckChatNumberAction(api, login, req.Number, s.defaultCC).OK
		chatOK = &ok
	}
	if res.OK {
		writeJSON(w, http.StatusOK, normalizeResp{OK: true, E164: res.Number, Message: "Will call " + res.Number, ChatOK: chatOK})
		return "", nil
	}
	writeJSON(w, http.StatusOK, normalizeResp{E164: res.Number, Error: res.Code, Message: res.Message, ChatOK: chatOK})
	return res.Code, nil
}

// ---- actions ----

// resultErr maps a failed hostkit.Result to an API error with its user-facing message.
func resultErr(res hostkit.Result) *apiError {
	st := http.StatusInternalServerError
	switch res.Code {
	case hostkit.CodeInvalidNumber, hostkit.CodeNotAllowed, hostkit.CodeSelfDial, hostkit.CodeInvalidDigits,
		hostkit.CodeInvalidName:
		st = http.StatusBadRequest
	case hostkit.CodeLineBusy, hostkit.CodeNoActiveCall, hostkit.CodeNotConnected, hostkit.CodeDTMFUnsupported,
		hostkit.CodeDTMFBusy:
		st = http.StatusConflict
	case hostkit.CodeOutboundDisabled, hostkit.CodeUnavailable:
		st = http.StatusServiceUnavailable
	case hostkit.CodeNotSupported:
		st = http.StatusNotFound
	}
	code := res.Code
	if code == "" {
		code = codeInternal
	}
	return &apiError{Status: st, Code: code, Message: res.Message}
}

// actionSpan starts the root SERVER span of an action and annotates it with the user and room.
func (s *Server) actionSpan(r *http.Request, name string, a *authed) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{obs.AttrMatrixSender.String(string(a.ss.user))}
	if a.ss.device != "" {
		attrs = append(attrs, attrClient.String(clientPWA), attrDeviceID.String(a.ss.device))
	} else {
		attrs = append(attrs, attrClient.String(clientWidget), obs.AttrMatrixRoomID.String(string(a.ss.room)))
	}
	return obs.Start(r.Context(), name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
}

// finish sets the span outcome and writes the action log line (never digits or tokens).
func (s *Server) finish(ctx context.Context, span trace.Span, action string, a *authed, code, to string) {
	o := code
	if o == "" {
		o = outcomeOK
	}
	span.SetAttributes(obs.AttrOutcome.String(o))
	ev := s.logFor(ctx).Info().Str("action", action).Stringer("user", a.ss.user).Str("outcome", o)
	if a.ss.device != "" {
		ev = ev.Str("device_id", a.ss.device)
	} else {
		ev = ev.Stringer("room_id", a.ss.room)
	}
	if to != "" {
		ev = ev.Str("to", to)
	}
	ev.Msg("Widget action")
}

// commandRoom is the room a hangup/dtmf targets: the session's portal, or "" (any room) in the
// management room and for device sessions.
func commandRoom(a *authed) id.RoomID {
	if a.rc.Kind == KindPortal {
		return a.ss.room
	}
	return ""
}

type callResp struct {
	OK      bool         `json:"ok"`
	Number  string       `json:"number"`
	Message string       `json:"message"`
	Contact *contactView `json:"contact,omitempty"`
	Warning string       `json:"warning,omitempty"`
}

// callReq is the call body: an optional number (else the portal room) and an optional contact name to
// save before the call.
type callReq struct {
	Number string `json:"number"`
	Name   string `json:"name"`
}

// contactView is the result of saving a contact name.
type contactView struct {
	Name         string `json:"name"`
	PreviousName string `json:"previous_name"`
	Changed      bool   `json:"changed"`
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req callReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	a, e := s.authenticate(r)
	if e != nil {
		return "", e
	}
	ctx, span := s.actionSpan(r, spanCall, a)
	defer span.End()
	form := "number"
	if strings.TrimSpace(req.Number) == "" {
		form = "room"
	}
	span.SetAttributes(attrCallForm.String(form))
	fail := func(e *apiError, to string) (string, *apiError) {
		s.finish(ctx, span, actCall, a, e.Code, to)
		return "", e
	}
	if ok, retry := s.limits.call.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry), "")
	}
	api, login, e := s.ready(a)
	if e != nil {
		return fail(e, "")
	}
	var creq calls.PlaceCallRequest
	roomNumber := ""
	if form == "room" {
		if a.rc.Kind != KindPortal || a.rc.Number == "" {
			return fail(errBadRequest("Enter a number to call."), "")
		}
		creq.RoomID, roomNumber = a.ss.room, a.rc.Number
	} else {
		if len(req.Number) > 64 {
			return fail(&apiError{Status: http.StatusBadRequest, Code: hostkit.CodeInvalidNumber,
				Message: "That is not a phone number I can call."}, "")
		}
		creq.Number = req.Number
	}
	var contact *contactView
	warning := ""
	if req.Name != "" {
		name, ok := hostkit.CleanContactName(req.Name)
		if !ok {
			return fail(errInvalidName, "")
		}
		if s.feat.saver == nil {
			return fail(errNotSupported, "")
		}
		// The call must be refused exactly as PlaceCall would refuse it, before anything is saved.
		target := roomNumber
		if form == "number" {
			target = req.Number
		}
		chk := hostkit.CheckNumberAction(api, login, target, s.defaultCC)
		if !chk.OK {
			return fail(resultErr(chk), chk.Number)
		}
		sctx, cancel := budget(ctx, 4*time.Second)
		sc, err := s.feat.saver.SaveContact(sctx, a.rc.Login, chk.Number, name)
		cancel()
		span.SetAttributes(attrContactSaved.Bool(err == nil))
		if err != nil {
			// Not fatal: a call is time-critical. Never log the name.
			fe := s.featureErr(ctx, err)
			warning = "Could not save the contact: " + fe.Message
			s.logFor(ctx).Warn().Str("number", chk.Number).Str("code", fe.Code).Msg("Contact save before call failed")
		} else {
			contact = &contactView{Name: sc.Name, PreviousName: sc.PreviousName, Changed: sc.Changed}
		}
	}
	// The call actor outlives the request: PlaceCall keeps only the span context, not ctx.
	res := hostkit.PlaceCallAction(ctx, api, login, creq, roomNumber)
	if res.Number != "" {
		span.SetAttributes(obs.AttrCallRemoteE164.String(res.Number))
	}
	if a.ss.room != "" { // device sessions have no room: no installer enqueue
		s.inst.Enqueue(a.ss.room, titleFor(a.rc))
	}
	if !res.OK {
		return fail(resultErr(res), res.Number)
	}
	s.finish(ctx, span, actCall, a, "", res.Number)
	writeJSON(w, http.StatusOK, callResp{OK: true, Number: res.Number, Message: res.Message, Contact: contact, Warning: warning})
	return "", nil
}

type dtmfReq struct {
	Digits string `json:"digits"`
}

func (s *Server) handleDTMF(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req dtmfReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	a, e := s.authenticate(r)
	if e != nil {
		return "", e
	}
	ctx, span := s.actionSpan(r, spanDTMF, a)
	defer span.End()
	fail := func(e *apiError) (string, *apiError) {
		s.finish(ctx, span, actDTMF, a, e.Code, "")
		return "", e
	}
	if ok, retry := s.limits.dtmf.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry))
	}
	api, login, e := s.ready(a)
	if e != nil {
		return fail(e)
	}
	if !validDigits(req.Digits) {
		return fail(&apiError{Status: http.StatusBadRequest, Code: hostkit.CodeInvalidDigits, Message: "Use 1-32 of 0-9 * # A-D."})
	}
	// DTMFAction sets dtmf.digit_count on the span; the digits are never logged or traced.
	res := hostkit.DTMFAction(ctx, api, login, commandRoom(a), req.Digits)
	if !res.OK {
		return fail(resultErr(res))
	}
	s.finish(ctx, span, actDTMF, a, "", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": res.Count})
	return "", nil
}

// validDigits accepts 1-32 of 0-9 * # A-D (A-D case-insensitive, like the call manager).
func validDigits(d string) bool {
	if len(d) < 1 || len(d) > 32 {
		return false
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !(c >= '0' && c <= '9' || c == '*' || c == '#' || c >= 'A' && c <= 'D' || c >= 'a' && c <= 'd') {
			return false
		}
	}
	return true
}

func (s *Server) handleHangup(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req struct{}
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	a, e := s.authenticate(r)
	if e != nil {
		return "", e
	}
	ctx, span := s.actionSpan(r, spanHangup, a)
	defer span.End()
	fail := func(e *apiError) (string, *apiError) {
		s.finish(ctx, span, actHangup, a, e.Code, "")
		return "", e
	}
	if ok, retry := s.limits.hangup.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry))
	}
	api, login, e := s.ready(a)
	if e != nil {
		return fail(e)
	}
	res := hostkit.HangupAction(ctx, api, login, commandRoom(a))
	if !res.OK {
		return fail(resultErr(res))
	}
	s.finish(ctx, span, actHangup, a, "", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return "", nil
}

// titleFor returns the widget title of a room context.
func titleFor(rc RoomContext) string {
	if rc.Kind == KindPortal && rc.Number != "" {
		return "Call " + rc.Number
	}
	return ""
}

// expand replaces $cmdprefix in host-supplied user-facing text (refusals) with the command prefix.
func (s *Server) expand(msg string) string {
	return strings.ReplaceAll(msg, "$cmdprefix", s.d.CommandPrefix)
}
