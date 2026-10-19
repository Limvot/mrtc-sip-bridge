package hostkit

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// Result codes of Result.Code (empty when OK). They are stable: the widget API maps them to its
// error codes. Message is always the user-facing reply the bot commands send.
const (
	CodeInvalidNumber    = "invalid_number"
	CodeNotAllowed       = "not_allowed"
	CodeSelfDial         = "self_dial"
	CodeLineBusy         = "line_busy"
	CodeOutboundDisabled = "outbound_disabled"
	CodeNoActiveCall     = "no_active_call"
	CodeNotConnected     = "not_connected"
	CodeDTMFUnsupported  = "dtmf_unsupported"
	CodeInvalidDigits    = "invalid_digits"
	CodeDTMFBusy         = "dtmf_busy"
	CodeInternal         = "internal"
)

// Result is the outcome of an action, shared by the bot commands (which send Message) and the widget
// API (which uses OK and Code). Message never contains DTMF digits.
type Result struct {
	OK      bool
	Code    string // "" when OK, else one of the Code* constants
	Message string // the user-facing reply
	Number  string // E.164 of the call or checked number, when known
	Count   int    // tones sent (DTMFAction)
}

// PlaceCallAction places a call by room (req.RoomID, with roomNumber the room's E.164 for the reply and
// the destination pre-check) or by number (req.Number, roomNumber ""). With a non-empty roomNumber it
// first runs CheckDestination(roomNumber), exactly as the call command always did.
func PlaceCallAction(ctx context.Context, api CallAPI, login core.LoginRef, req calls.PlaceCallRequest, roomNumber string) Result {
	if roomNumber != "" {
		if err := api.CheckDestination(login, roomNumber); err != nil {
			return placeErrResult(err)
		}
	}
	_, e164, err := api.PlaceCall(ctx, login, req)
	if err != nil {
		return placeErrResult(err)
	}
	if e164 == "" {
		e164 = roomNumber
	}
	msg := "Calling. Answer the incoming call on any of your devices to connect."
	if e164 != "" {
		msg = fmt.Sprintf("Calling %s. Answer the incoming call on any of your devices to connect.", e164)
	}
	return Result{OK: true, Message: msg, Number: e164}
}

// HangupAction hangs up the login's active call (room "" = the management room form).
func HangupAction(ctx context.Context, api CallAPI, login core.LoginRef, room id.RoomID) Result {
	switch err := api.Hangup(ctx, login, room); {
	case err == nil:
		return Result{OK: true, Message: "Hanging up."}
	case errors.Is(err, calls.ErrNoActiveCall):
		return Result{Code: CodeNoActiveCall, Message: "No call in progress."}
	default:
		return Result{Code: CodeInternal, Message: "Could not hang up."}
	}
}

// DTMFAction sends digits on the active call. The result never contains the digits, and only their
// count is put on the span.
func DTMFAction(ctx context.Context, api CallAPI, login core.LoginRef, room id.RoomID, digits string) Result {
	n, err := api.SendDTMF(ctx, login, room, digits)
	switch {
	case err == nil:
		trace.SpanFromContext(ctx).SetAttributes(obs.AttrDTMFDigitCount.Int(n))
		msg := fmt.Sprintf("Sent %d tones.", n)
		if n == 1 {
			msg = "Sent 1 tone."
		}
		return Result{OK: true, Message: msg, Count: n}
	case errors.Is(err, calls.ErrNoActiveCall):
		return Result{Code: CodeNoActiveCall, Message: "No call in progress."}
	case errors.Is(err, calls.ErrNotConnected), errors.Is(err, media.ErrDTMFNotConnected):
		return Result{Code: CodeNotConnected, Message: "The call is not connected yet."}
	case errors.Is(err, media.ErrDTMFUnsupported):
		return Result{Code: CodeDTMFUnsupported, Message: "DTMF is not supported on this call."}
	case errors.Is(err, calls.ErrInvalidDigits), errors.Is(err, media.ErrDTMFInvalid):
		return Result{Code: CodeInvalidDigits, Message: "Use 1-32 of 0-9 * # A-D."}
	case errors.Is(err, media.ErrDTMFBusy):
		return Result{Code: CodeDTMFBusy, Message: "Too many tones are queued. Try again in a moment."}
	default:
		return Result{Code: CodeInternal, Message: "Could not send the tones."}
	}
}

// CheckNumberAction normalizes number (phone.Normalize with defaultCC) and runs CheckDestination on it.
// On success Result.Number is the E.164 form; Message is empty.
func CheckNumberAction(api CallAPI, login core.LoginRef, number, defaultCC string) Result {
	e164, err := phone.Normalize(number, defaultCC)
	if err != nil {
		return placeErrResult(calls.ErrInvalidNumber)
	}
	if err = api.CheckDestination(login, e164); err != nil {
		r := placeErrResult(err)
		r.Number = e164
		return r
	}
	return Result{OK: true, Number: e164}
}

// CheckChatNumberAction normalizes number for texting/saving: phone.Normalize with defaultCC, then
// CheckDestination, where only ErrInvalidNumber fails (ErrNotAllowed and ErrSelfDial are call policy).
// Short codes fail with CodeInvalidNumber and a hint to use the pm command. On success Number is E.164.
func CheckChatNumberAction(api CallAPI, login core.LoginRef, number, defaultCC string) Result {
	e164, err := phone.Normalize(number, defaultCC)
	if err != nil {
		if errors.Is(err, phone.ErrShortCode) {
			return Result{Code: CodeInvalidNumber,
				Message: "Short codes can't be texted from the Dialpad. Use `$cmdprefix pm <code>`."}
		}
		return placeErrResult(calls.ErrInvalidNumber)
	}
	if err = api.CheckDestination(login, e164); err != nil && errors.Is(err, calls.ErrInvalidNumber) {
		r := placeErrResult(err)
		r.Number = e164
		return r
	}
	return Result{OK: true, Number: e164}
}

// placeErrResult maps a PlaceCall/CheckDestination error to a failed Result.
func placeErrResult(err error) Result {
	r := Result{Message: placeErrReply(err)}
	switch {
	case errors.Is(err, calls.ErrInvalidNumber):
		r.Code = CodeInvalidNumber
	case errors.Is(err, calls.ErrNotAllowed):
		r.Code = CodeNotAllowed
	case errors.Is(err, calls.ErrSelfDial):
		r.Code = CodeSelfDial
	case errors.Is(err, calls.ErrLineBusy):
		r.Code = CodeLineBusy
	case errors.Is(err, calls.ErrOutboundDisabled):
		r.Code = CodeOutboundDisabled
	default:
		r.Code = CodeInternal
	}
	return r
}
