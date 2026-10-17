package hostkit

import (
	"context"
	"errors"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
)

// CallAPI is the part of *calls.Manager the commands use (a seam for host tests).
type CallAPI interface {
	PlaceCall(ctx context.Context, login core.LoginRef, req calls.PlaceCallRequest) (callID, e164 string, err error)
	CheckDestination(login core.LoginRef, e164 string) error
	Hangup(ctx context.Context, login core.LoginRef, roomID id.RoomID) error
	SendDTMF(ctx context.Context, login core.LoginRef, roomID id.RoomID, digits string) (int, error)
	// Snapshot returns the login's active call (WIDGET-SPEC §8.2).
	Snapshot(login core.LoginRef) (calls.Snapshot, bool)
}

var _ CallAPI = (*calls.Manager)(nil)

// CommandDeps adapts the call commands to a host.
type CommandDeps struct {
	// Calls returns the manager, or nil while starting ("The bridge is still starting...").
	Calls func() CallAPI
	// Login picks the login a command acts on; on refusal returns nil and the reply to send.
	Login func(ce *commands.Event) (*bridgev2.UserLogin, string)
	// RoomNumber returns the E.164 of a 1:1 number portal, ok=false for any other room.
	RoomNumber func(p *bridgev2.Portal) (e164 string, ok bool)
	// RedactDTMF redacts dtmf command messages (Config.RedactDTMFCommand()).
	RedactDTMF bool
	// Widget installs the Dialpad widget (WIDGET-SPEC §6). nil = the widget is disabled on this host.
	Widget WidgetInstaller
	// Dialpad issues and manages PWA links (docs/PWA-SPEC.md §4). nil = the PWA is disabled on this host.
	Dialpad DialpadLinks
}

// WidgetInstaller is the part of the widget installer the commands use (implemented by pkg/widget).
type WidgetInstaller interface {
	// Ensure installs or updates the widget in roomID synchronously. title is "Call <number>" in a
	// number room and "" in the management room. An M_FORBIDDEN failure must be returned so that
	// errors.Is(err, mautrix.MForbidden) holds.
	Ensure(ctx context.Context, roomID id.RoomID, title string) error
	// Enqueue schedules Ensure on the worker; it never blocks.
	Enqueue(roomID id.RoomID, title string)
}

// MsgWidgetNotPrivate is the refusal for rooms with other members (the widget URL carries the room
// key); widget.MsgNotPrivateRoom is the same text.
const MsgWidgetNotPrivate = "This room has other members; the Dialpad works only in private bridge rooms."

// ErrWidgetRoomNotPrivate is what WidgetInstaller.Ensure returns for a room that is not private.
var ErrWidgetRoomNotPrivate = errors.New(MsgWidgetNotPrivate)

const msgNotReady = "The bridge is still starting. Try again in a moment."

// RegisterCallCommands adds call, dtmf, hangup and widget (each wrapped in WithSpan, which records only the
// command name, never its arguments) to proc. Call from PostInit (after the bridge is initialised, so
// br.Bridge.Commands exists). The help texts name the bridge's command prefix through `$cmdprefix`,
// which the help command replaces with br.Config.CommandPrefix when it renders them.
func RegisterCallCommands(proc *commands.Processor, d CommandDeps) {
	for _, fh := range callHandlers(d) {
		proc.AddHandler(fh)
	}
}

// callHandlers returns the call, dtmf and hangup handlers of RegisterCallCommands.
func callHandlers(d CommandDeps) []*commands.FullHandler {
	h := &callCommands{d: d}
	hs := []*commands.FullHandler{{
		Func:          WithSpan("call", h.cmdCall),
		Name:          "call",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Call a phone number: your devices ring first, answer to connect the call. Send it without a number inside a number's room to call that number. Usage: `$cmdprefix call <number>`",
			Args:        "[number]",
		},
	}, {
		Func:          WithSpan("dtmf", h.cmdDTMF),
		Name:          "dtmf",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Send keypad tones (0-9 * # A-D) on the active call. Usage: `$cmdprefix dtmf <digits>`",
			Args:        "<digits>",
		},
	}, {
		Func:          WithSpan("hangup", h.cmdHangup),
		Name:          "hangup",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Hang up the active call. Usage: `$cmdprefix hangup`",
		},
	}, {
		Func:          WithSpan("widget", h.cmdWidget),
		Name:          "widget",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Add the Dialpad widget to this room's widget list. Usage: `$cmdprefix widget`",
		},
	}}
	if d.Dialpad != nil {
		hs = append(hs, dialpadHandlers(h)...)
	}
	return hs
}

// callCommands holds the handlers of RegisterCallCommands.
type callCommands struct {
	d       CommandDeps
	limiter dialpadLimiter
}

func (h *callCommands) calls() CallAPI {
	if h.d.Calls == nil {
		return nil
	}
	return h.d.Calls()
}

func (h *callCommands) cmdCall(ce *commands.Event) {
	ul, refusal := h.d.Login(ce)
	if ul == nil {
		ce.Reply("%s", refusal)
		return
	}
	api := h.calls()
	if api == nil {
		ce.Reply("%s", msgNotReady)
		return
	}
	h.enqueueWidget(ce)
	req := calls.PlaceCallRequest{}
	roomNumber := ""
	switch {
	case len(ce.Args) > 0:
		req.Number = strings.Join(ce.Args, " ")
	case ce.Portal != nil && ce.Portal.MXID != "" && h.roomNumber(ce.Portal) != "":
		req.RoomID = ce.Portal.MXID
		roomNumber = h.roomNumber(ce.Portal)
	default:
		ce.Reply("Usage: `$cmdprefix call <number>` (or send `$cmdprefix call` inside a number's room)")
		return
	}
	ce.Reply("%s", callReply(ce.Ctx, api, core.LoginRef(ul.ID), req, roomNumber))
}

// roomNumber returns the E.164 of p if it is a 1:1 number portal, else "".
func (h *callCommands) roomNumber(p *bridgev2.Portal) string {
	if h.d.RoomNumber == nil {
		return ""
	}
	if e164, ok := h.d.RoomNumber(p); ok {
		return e164
	}
	return ""
}

func (h *callCommands) cmdHangup(ce *commands.Event) {
	ul, refusal := h.d.Login(ce)
	if ul == nil {
		ce.Reply("%s", refusal)
		return
	}
	api := h.calls()
	if api == nil {
		ce.Reply("%s", msgNotReady)
		return
	}
	h.enqueueWidget(ce)
	ce.Reply("%s", hangupReply(ce.Ctx, api, core.LoginRef(ul.ID), commandRoom(ce)))
}

func (h *callCommands) cmdDTMF(ce *commands.Event) {
	// Whatever happens below, the command message (which holds the digits) is redacted when configured.
	if h.d.RedactDTMF {
		defer redactCommand(ce)
	}
	ul, refusal := h.d.Login(ce)
	if ul == nil {
		ce.Reply("%s", refusal)
		return
	}
	api := h.calls()
	if api == nil {
		ce.Reply("%s", msgNotReady)
		return
	}
	h.enqueueWidget(ce)
	// Join the args so "1 2 3" and "123" both work (RawArgs breaks on capitalized command names).
	ce.Reply("%s", dtmfReply(ce.Ctx, api, core.LoginRef(ul.ID), commandRoom(ce), strings.Join(ce.Args, "")))
}

const (
	msgWidgetDisabled = "The Dialpad widget is not enabled on this bridge."
	msgWidgetOK       = "The Dialpad widget is in this room's widget list (Sable, gomuks). Element X does not show widgets: use `$cmdprefix call`, `$cmdprefix dtmf` and `$cmdprefix hangup`."
	msgWidgetForbid   = "I could not add the widget: give the bridge bot power level 50 in this room, then send `$cmdprefix widget` again."
	msgWidgetFailed   = "Could not add the widget."
)

// widgetTarget returns the room and widget title of the command's room, ok=false if the room is not
// eligible (neither a number room nor the management room).
func (h *callCommands) widgetTarget(ce *commands.Event) (id.RoomID, string, bool) {
	if ce.Portal != nil && ce.Portal.MXID != "" {
		if n := h.roomNumber(ce.Portal); n != "" {
			return ce.Portal.MXID, "Call " + n, true
		}
		return "", "", false
	}
	// Outside a portal only the user's own management room is eligible (any other room could hold
	// other members, who would then see the room key).
	return ce.RoomID, "", ce.RoomID != "" && ce.User != nil && ce.RoomID == ce.User.ManagementRoom
}

// enqueueWidget asks the installer to (idempotently) add the widget to the command's room (trigger 4).
func (h *callCommands) enqueueWidget(ce *commands.Event) {
	if h.d.Widget == nil {
		return
	}
	if room, title, ok := h.widgetTarget(ce); ok {
		h.d.Widget.Enqueue(room, title)
	}
}

func (h *callCommands) cmdWidget(ce *commands.Event) {
	if ul, refusal := h.d.Login(ce); ul == nil {
		ce.Reply("%s", refusal)
		return
	}
	if h.d.Widget == nil {
		ce.Reply("%s", msgWidgetDisabled)
		return
	}
	room, title, ok := h.widgetTarget(ce)
	if !ok {
		ce.Reply("%s", "The Dialpad widget is only available in a number's room or the management room.")
		return
	}
	ctx, cancel := context.WithTimeout(ce.Ctx, 5*time.Second)
	defer cancel()
	switch err := h.d.Widget.Ensure(ctx, room, title); {
	case err == nil:
		ce.Reply("%s", msgWidgetOK)
	case errors.Is(err, ErrWidgetRoomNotPrivate):
		ce.Reply("%s", MsgWidgetNotPrivate)
	case errors.Is(err, mautrix.MForbidden):
		ce.Reply("%s", msgWidgetForbid)
	default:
		ce.Reply("%s", msgWidgetFailed)
	}
}

// commandRoom is the portal room the command was sent in, or "" for the management room.
func commandRoom(ce *commands.Event) id.RoomID {
	if ce.Portal != nil && ce.Portal.MXID != "" {
		return ce.Portal.MXID
	}
	return ""
}

// redactCommand redacts the command message as the bridge bot (best effort). It never logs the message.
func redactCommand(ce *commands.Event) {
	if ce.EventID == "" || ce.Bot == nil {
		return
	}
	_, err := ce.Bot.SendMessage(ce.Ctx, ce.OrigRoomID, event.EventRedaction,
		&event.Content{Parsed: &event.RedactionEventContent{Redacts: ce.EventID}}, nil)
	if err != nil && ce.Log != nil {
		ce.Log.Warn().Err(err).Msg("Failed to redact dtmf command message")
	}
}

// callReply runs PlaceCallAction and returns its reply. roomNumber is the room's number for the room
// form when known ("" otherwise).
func callReply(ctx context.Context, api CallAPI, login core.LoginRef, req calls.PlaceCallRequest, roomNumber string) string {
	return PlaceCallAction(ctx, api, login, req, roomNumber).Message
}

// placeErrReply maps a PlaceCall/CheckDestination error to the user-facing reply.
func placeErrReply(err error) string {
	switch {
	case errors.Is(err, calls.ErrInvalidNumber):
		return "That is not a phone number I can call. Use a full number such as +15551234567."
	case errors.Is(err, calls.ErrNotAllowed):
		return "Calls to that number are not allowed by the bridge's dial_allowed_prefixes setting."
	case errors.Is(err, calls.ErrSelfDial):
		return "That is this account's own number."
	case errors.Is(err, calls.ErrLineBusy):
		return "Another call is already in progress."
	case errors.Is(err, calls.ErrOutboundDisabled):
		return "Outgoing calls are not available right now."
	default:
		return "Could not place the call."
	}
}

// hangupReply runs HangupAction and returns its reply.
func hangupReply(ctx context.Context, api CallAPI, login core.LoginRef, room id.RoomID) string {
	return HangupAction(ctx, api, login, room).Message
}

// dtmfReply runs DTMFAction and returns its reply (never containing the digits).
func dtmfReply(ctx context.Context, api CallAPI, login core.LoginRef, room id.RoomID, digits string) string {
	return DTMFAction(ctx, api, login, room, digits).Message
}
