package hostkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

func TestPlaceCallAction(t *testing.T) {
	ctx := context.Background()
	f := &fakeCalls{e164: "+15551234567"}
	r := PlaceCallAction(ctx, f, "l", calls.PlaceCallRequest{Number: "555"}, "")
	if !r.OK || r.Number != "+15551234567" || r.Code != "" || !strings.HasPrefix(r.Message, "Calling +15551234567.") {
		t.Fatalf("number: %+v", r)
	}
	f = &fakeCalls{}
	r = PlaceCallAction(ctx, f, "l", calls.PlaceCallRequest{RoomID: "!r:x"}, "+15559876543")
	if !r.OK || r.Number != "+15559876543" || f.gotReq.RoomID != "!r:x" {
		t.Fatalf("room: %+v", r)
	}
	f = &fakeCalls{checkErr: calls.ErrNotAllowed}
	r = PlaceCallAction(ctx, f, "l", calls.PlaceCallRequest{RoomID: "!r:x"}, "+445551234567")
	if r.OK || r.Code != CodeNotAllowed || f.gotReq != (calls.PlaceCallRequest{}) {
		t.Fatalf("precheck: %+v req %+v", r, f.gotReq)
	}
	for err, code := range map[error]string{
		calls.ErrInvalidNumber: CodeInvalidNumber, calls.ErrSelfDial: CodeSelfDial, calls.ErrLineBusy: CodeLineBusy,
		calls.ErrOutboundDisabled: CodeOutboundDisabled, errors.New("x"): CodeInternal,
	} {
		r = PlaceCallAction(ctx, &fakeCalls{err: err}, "l", calls.PlaceCallRequest{Number: "1"}, "")
		if r.OK || r.Code != code || r.Message != placeErrReply(err) {
			t.Errorf("%v: %+v", err, r)
		}
	}
}

func TestHangupAndDTMFAction(t *testing.T) {
	ctx := context.Background()
	if r := HangupAction(ctx, &fakeCalls{}, "l", "!r:x"); !r.OK || r.Message != "Hanging up." {
		t.Fatalf("%+v", r)
	}
	if r := HangupAction(ctx, &fakeCalls{err: calls.ErrNoActiveCall}, "l", ""); r.Code != CodeNoActiveCall || r.Message != "No call in progress." {
		t.Fatalf("%+v", r)
	}
	if r := HangupAction(ctx, &fakeCalls{err: errors.New("x")}, "l", ""); r.Code != CodeInternal {
		t.Fatalf("%+v", r)
	}
	f := &fakeCalls{n: 3}
	r := DTMFAction(ctx, f, "l", "!r:x", "1#2")
	if !r.OK || r.Count != 3 || r.Message != "Sent 3 tones." || f.gotDigits != "1#2" || strings.Contains(r.Message, "#") {
		t.Fatalf("%+v", r)
	}
	if r = DTMFAction(ctx, &fakeCalls{n: 1}, "l", "", "5"); r.Message != "Sent 1 tone." {
		t.Fatalf("%+v", r)
	}
	for err, code := range map[error]string{
		calls.ErrNoActiveCall: CodeNoActiveCall, calls.ErrNotConnected: CodeNotConnected, media.ErrDTMFNotConnected: CodeNotConnected,
		media.ErrDTMFUnsupported: CodeDTMFUnsupported, calls.ErrInvalidDigits: CodeInvalidDigits, media.ErrDTMFInvalid: CodeInvalidDigits,
		media.ErrDTMFBusy: CodeDTMFBusy, errors.New("x"): CodeInternal,
	} {
		if r = DTMFAction(ctx, &fakeCalls{err: err}, "l", "", "1"); r.OK || r.Code != code {
			t.Errorf("%v: %+v", err, r)
		}
	}
}

func TestCheckNumberAction(t *testing.T) {
	r := CheckNumberAction(&fakeCalls{}, "l", "(555) 234-5678", "1")
	if !r.OK || r.Number != "+15552345678" {
		t.Fatalf("%+v", r)
	}
	if r = CheckNumberAction(&fakeCalls{}, "l", "abc", "1"); r.OK || r.Code != CodeInvalidNumber {
		t.Fatalf("%+v", r)
	}
	if r = CheckNumberAction(&fakeCalls{}, "l", "611", "1"); r.OK || r.Code != CodeInvalidNumber {
		t.Fatalf("short code: %+v", r)
	}
	if r = CheckNumberAction(&fakeCalls{checkErr: calls.ErrSelfDial}, "l", "+15552345678", "1"); r.OK || r.Code != CodeSelfDial || r.Number != "+15552345678" {
		t.Fatalf("%+v", r)
	}
}

type fakeWidget struct {
	err      error
	ensured  []id.RoomID
	title    string
	enqueued []id.RoomID
}

func (w *fakeWidget) Ensure(_ context.Context, room id.RoomID, title string) error {
	w.ensured, w.title = append(w.ensured, room), title
	return w.err
}
func (w *fakeWidget) Enqueue(room id.RoomID, _ string) { w.enqueued = append(w.enqueued, room) }

func TestWidgetCommand(t *testing.T) {
	log := zerolog.Nop()
	user := &bridgev2.User{User: &database.User{MXID: "@alice:x", ManagementRoom: "!mgmt:example.org"}}

	// Disabled.
	bot := &fakeBot{}
	h := &callCommands{d: fakeLoginDeps(&fakeCalls{}, "")}
	h.cmdWidget(newEvent(bot, user, &log, "$w0"))
	if bot.lastReply() != msgWidgetDisabled {
		t.Fatalf("disabled: %q", bot.lastReply())
	}

	// Management room.
	w := &fakeWidget{}
	bot = &fakeBot{}
	d := fakeLoginDeps(&fakeCalls{}, "")
	d.Widget = w
	h = &callCommands{d: d}
	h.cmdWidget(newEvent(bot, user, &log, "$w1"))
	if len(w.ensured) != 1 || w.ensured[0] != "!mgmt:example.org" || w.title != "" || !strings.Contains(bot.lastReply(), "widget list") {
		t.Fatalf("mgmt: %+v %q", w, bot.lastReply())
	}

	// Number portal: title; non-number portal refused.
	w = &fakeWidget{}
	bot = &fakeBot{}
	d.Widget = w
	h = &callCommands{d: d}
	h.cmdWidget(portalEvent(newEvent(bot, user, &log, "$w2"), "+15559876543"))
	if len(w.ensured) != 1 || w.ensured[0] != "!portal:x" || w.title != "Call +15559876543" {
		t.Fatalf("portal: %+v", w)
	}
	bot = &fakeBot{}
	h.cmdWidget(portalEvent(newEvent(bot, user, &log, "$w3"), "room@conference.example.org"))
	if len(w.ensured) != 1 || !strings.Contains(bot.lastReply(), "only available") {
		t.Fatalf("muc: %+v %q", w, bot.lastReply())
	}

	// Forbidden and other failures.
	for err, want := range map[error]string{
		mautrix.HTTPError{RespError: &mautrix.MForbidden}: "power level 50",
		errors.New("boom"): msgWidgetFailed,
	} {
		w = &fakeWidget{err: err}
		d.Widget = w
		bot = &fakeBot{}
		h = &callCommands{d: d}
		h.cmdWidget(newEvent(bot, user, &log, "$w4"))
		if !strings.Contains(bot.lastReply(), want) {
			t.Errorf("%v: %q", err, bot.lastReply())
		}
	}

	// Login refusal.
	bot = &fakeBot{}
	h = &callCommands{d: fakeLoginDeps(&fakeCalls{}, "nope")}
	h.cmdWidget(newEvent(bot, user, &log, "$w5"))
	if bot.lastReply() != "nope" {
		t.Fatalf("refusal %q", bot.lastReply())
	}

	// Trigger 4: call/hangup/dtmf enqueue the room.
	w = &fakeWidget{}
	d.Widget = w
	h = &callCommands{d: d}
	h.cmdHangup(portalEvent(newEvent(&fakeBot{}, user, &log, "$w6"), "+15559876543"))
	h.cmdDTMF(newEvent(&fakeBot{}, user, &log, "$w7", "1"))
	h.cmdCall(portalEvent(newEvent(&fakeBot{}, user, &log, "$w8"), "+15559876543"))
	if len(w.enqueued) != 3 || w.enqueued[0] != "!portal:x" || w.enqueued[1] != "!mgmt:example.org" {
		t.Fatalf("enqueued %v", w.enqueued)
	}
}
