package widget

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Optional host features of the Dialpad (WIDGET-SPEC §7.5): chat, contact and prefs/roster_only. Each
// endpoint exists only when Deps.Features implements the matching hostkit interface (else 404
// not_supported). Contact names are never logged or put on spans or metrics.

// featureErr maps an error of a host feature method to an API error: *hostkit.ActionError by its code,
// a deadline or cancellation as 503 unavailable, anything else as internal (logged scrubbed).
func (s *Server) featureErr(ctx context.Context, err error) *apiError {
	var ae *hostkit.ActionError
	switch {
	case errors.As(err, &ae):
		return resultErr(hostkit.Result{Code: ae.Code, Message: s.expand(ae.Message)})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &apiError{Status: http.StatusServiceUnavailable, Code: hostkit.CodeUnavailable,
			Message: "The bridge took too long. Try again."}
	}
	s.logFor(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("Widget host feature failed")
	return errInternal
}

// featureAction is the common prologue of the feature handlers after the body is decoded: it
// authenticates, starts the span, applies the limiter and checks readiness. The returned fail closure
// ends the action with an error.
type featureAction struct {
	a    *authed
	ctx  context.Context
	span trace.Span
	fail func(e *apiError, to string) (string, *apiError)
}

func (s *Server) beginFeature(r *http.Request, spanName, act string) (*featureAction, *apiError) {
	a, e := s.authenticate(r)
	if e != nil {
		return nil, e
	}
	ctx, span := s.actionSpan(r, spanName, a)
	fa := &featureAction{a: a, ctx: ctx, span: span}
	fa.fail = func(e *apiError, to string) (string, *apiError) {
		s.finish(ctx, span, act, a, e.Code, to)
		return "", e
	}
	return fa, nil
}

// ---- chat ----

type chatReq struct {
	Number string `json:"number"`
	Name   string `json:"name"`
}

type chatResp struct {
	OK      bool         `json:"ok"`
	Number  string       `json:"number"`
	RoomID  string       `json:"room_id"`
	Created bool         `json:"created"`
	Pending bool         `json:"pending"`
	Contact *contactView `json:"contact,omitempty"`
	Message string       `json:"message"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req chatReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	fa, e := s.beginFeature(r, spanChat, actChat)
	if e != nil {
		return "", e
	}
	defer fa.span.End()
	a, ctx, span, fail := fa.a, fa.ctx, fa.span, fa.fail
	if ok, retry := s.limits.chat.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry), "")
	}
	api, login, e := s.ready(a)
	if e != nil {
		return fail(e, "")
	}
	if s.feat.chat == nil {
		return fail(errNotSupported, "")
	}
	if len(req.Number) > 64 || strings.TrimSpace(req.Number) == "" {
		return fail(&apiError{Status: http.StatusBadRequest, Code: hostkit.CodeInvalidNumber,
			Message: "That is not a phone number I can text."}, "")
	}
	name := ""
	if req.Name != "" {
		var ok bool
		if name, ok = hostkit.CleanContactName(req.Name); !ok {
			return fail(errInvalidName, "")
		}
		if s.feat.saver == nil {
			return fail(errNotSupported, "")
		}
	}
	res := hostkit.CheckChatNumberAction(api, login, req.Number, s.defaultCC)
	if !res.OK {
		e := resultErr(res)
		e.Message = s.expand(e.Message)
		return fail(e, res.Number)
	}
	e164 := res.Number
	span.SetAttributes(obs.AttrCallRemoteE164.String(e164))
	var contact *contactView
	if name != "" {
		sctx, cancel := budget(ctx, 4*time.Second)
		sc, err := s.feat.saver.SaveContact(sctx, a.rc.Login, e164, name)
		cancel()
		span.SetAttributes(attrContactSaved.Bool(err == nil))
		if err != nil {
			return fail(s.featureErr(ctx, err), e164) // abort: no room is created
		}
		contact = &contactView{Name: sc.Name, PreviousName: sc.PreviousName, Changed: sc.Changed}
	}
	octx, cancel := budget(ctx, requestTimeout)
	room, err := s.feat.chat.OpenChat(octx, a.rc.Login, e164)
	cancel()
	if err != nil {
		return fail(s.featureErr(ctx, err), e164)
	}
	span.SetAttributes(attrChatCreated.Bool(room.Created), attrChatPending.Bool(room.Pending))
	var msg string
	switch {
	case room.Pending:
		msg = "Creating the chat with " + e164 + ". It will appear in your room list shortly."
	case room.Created:
		msg = "Chat with " + e164 + " created. It is in your room list (accept the invite if asked)."
	default:
		msg = "Chat with " + e164 + " is ready."
	}
	if contact != nil && contact.Changed {
		msg = "Saved as " + contact.Name + ". " + msg
	}
	s.finish(ctx, span, actChat, a, "", e164)
	writeJSON(w, http.StatusOK, chatResp{OK: true, Number: e164, RoomID: string(room.RoomID), Created: room.Created,
		Pending: room.Pending, Contact: contact, Message: msg})
	return "", nil
}

// ---- contact ----

type contactReq struct {
	Number string `json:"number"`
	Name   string `json:"name"`
}

type contactResp struct {
	OK           bool   `json:"ok"`
	Number       string `json:"number"`
	Name         string `json:"name"`
	PreviousName string `json:"previous_name"`
	Changed      bool   `json:"changed"`
	Message      string `json:"message"`
}

func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req contactReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	fa, e := s.beginFeature(r, spanContact, actContact)
	if e != nil {
		return "", e
	}
	defer fa.span.End()
	a, ctx, span, fail := fa.a, fa.ctx, fa.span, fa.fail
	if ok, retry := s.limits.contact.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry), "")
	}
	api, login, e := s.ready(a)
	if e != nil {
		return fail(e, "")
	}
	if s.feat.saver == nil {
		return fail(errNotSupported, "")
	}
	name, ok := hostkit.CleanContactName(req.Name)
	if !ok {
		return fail(errInvalidName, "")
	}
	var e164 string
	if strings.TrimSpace(req.Number) == "" {
		span.SetAttributes(attrContactForm.String("room"))
		if a.rc.Kind != KindPortal || a.rc.Number == "" {
			return fail(errBadRequest("Enter a number to save."), "")
		}
		e164 = a.rc.Number // from the session's room, never from the body
	} else {
		span.SetAttributes(attrContactForm.String("number"))
		if len(req.Number) > 64 {
			return fail(&apiError{Status: http.StatusBadRequest, Code: hostkit.CodeInvalidNumber,
				Message: "That is not a phone number I can save."}, "")
		}
		res := hostkit.CheckChatNumberAction(api, login, req.Number, s.defaultCC)
		if !res.OK {
			e := resultErr(res)
			e.Message = s.expand(e.Message)
			return fail(e, res.Number)
		}
		e164 = res.Number
	}
	span.SetAttributes(obs.AttrCallRemoteE164.String(e164))
	sctx, cancel := budget(ctx, requestTimeout)
	sc, err := s.feat.saver.SaveContact(sctx, a.rc.Login, e164, name)
	cancel()
	if err != nil {
		return fail(s.featureErr(ctx, err), e164)
	}
	span.SetAttributes(attrContactChanged.Bool(sc.Changed))
	var msg string
	switch {
	case !sc.Changed:
		msg = e164 + " is already saved as " + sc.Name + "."
	case sc.PreviousName != "":
		msg = "Renamed " + sc.PreviousName + " to " + sc.Name + " (" + e164 + ")."
	default:
		msg = "Saved " + e164 + " as " + sc.Name + "."
	}
	s.finish(ctx, span, actContact, a, "", e164)
	writeJSON(w, http.StatusOK, contactResp{OK: true, Number: e164, Name: sc.Name, PreviousName: sc.PreviousName,
		Changed: sc.Changed, Message: msg})
	return "", nil
}

// ---- prefs ----

type prefsReq struct {
	On *bool `json:"on"`
}

type prefsResp struct {
	OK         bool   `json:"ok"`
	RosterOnly bool   `json:"roster_only"`
	Warning    string `json:"warning"`
	Message    string `json:"message"`
}

func (s *Server) handlePrefs(w http.ResponseWriter, r *http.Request) (string, *apiError) {
	var req prefsReq
	if e := decodeJSON(w, r, &req); e != nil {
		return "", e
	}
	fa, e := s.beginFeature(r, spanPrefs, actPrefs)
	if e != nil {
		return "", e
	}
	defer fa.span.End()
	a, ctx, span, fail := fa.a, fa.ctx, fa.span, fa.fail
	if ok, retry := s.limits.prefs.allow(string(a.ss.user)); !ok {
		return fail(errRateLimited(retry), "")
	}
	if _, _, e := s.ready(a); e != nil {
		return fail(e, "")
	}
	if s.feat.prefs == nil {
		return fail(errNotSupported, "")
	}
	if req.On == nil {
		return fail(errBadRequest("Malformed request."), "")
	}
	pctx, cancel := budget(ctx, requestTimeout)
	st, err := s.feat.prefs.SetRosterOnly(pctx, a.rc.Login, *req.On)
	cancel()
	if err != nil {
		return fail(s.featureErr(ctx, err), "")
	}
	span.SetAttributes(attrPrefsRosterOnly.Bool(st.On))
	msg := "Incoming calls now ring for everyone."
	if st.On {
		msg = "Incoming calls now ring only for your contacts. Other callers go to voicemail."
	}
	s.finish(ctx, span, actPrefs, a, "", "")
	writeJSON(w, http.StatusOK, prefsResp{OK: true, RosterOnly: st.On, Warning: s.expand(st.Warning), Message: msg})
	return "", nil
}
