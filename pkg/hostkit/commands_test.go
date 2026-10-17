package hostkit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

type fakeCalls struct {
	e164      string
	err       error
	n         int
	gotReq    calls.PlaceCallRequest
	gotRoom   id.RoomID
	gotDigits string
	checkErr  error
}

func (f *fakeCalls) PlaceCall(_ context.Context, _ core.LoginRef, req calls.PlaceCallRequest) (string, string, error) {
	f.gotReq = req
	return "id", f.e164, f.err
}

func (f *fakeCalls) CheckDestination(_ core.LoginRef, _ string) error { return f.checkErr }

func (f *fakeCalls) Snapshot(core.LoginRef) (calls.Snapshot, bool) { return calls.Snapshot{}, false }

func (f *fakeCalls) Hangup(_ context.Context, _ core.LoginRef, room id.RoomID) error {
	f.gotRoom = room
	return f.err
}

func (f *fakeCalls) SendDTMF(_ context.Context, _ core.LoginRef, room id.RoomID, digits string) (int, error) {
	f.gotRoom, f.gotDigits = room, digits
	return f.n, f.err
}

func TestCallReply(t *testing.T) {
	tests := []struct {
		name       string
		e164, room string
		err        error
		want       string
	}{
		{"number", "+15551234567", "", nil, "Calling +15551234567."},
		{"room form known", "", "+15559876543", nil, "Calling +15559876543."},
		{"room form unknown", "", "", nil, "Calling. Answer"},
		{"invalid", "", "", calls.ErrInvalidNumber, "not a phone number"},
		{"not allowed", "", "", calls.ErrNotAllowed, "dial_allowed_prefixes"},
		{"self", "", "", calls.ErrSelfDial, "own number"},
		{"busy", "", "", calls.ErrLineBusy, "already in progress"},
		{"disabled", "", "", calls.ErrOutboundDisabled, "not available"},
		{"other", "", "", errors.New("boom"), "Could not place"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCalls{e164: tt.e164, err: tt.err}
			got := callReply(context.Background(), f, "login", calls.PlaceCallRequest{Number: "x"}, tt.room)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestHangupReply(t *testing.T) {
	f := &fakeCalls{}
	if got := hangupReply(context.Background(), f, "l", "!room:x"); got != "Hanging up." || f.gotRoom != "!room:x" {
		t.Fatalf("got %q room %q", got, f.gotRoom)
	}
	f.err = calls.ErrNoActiveCall
	if got := hangupReply(context.Background(), f, "l", ""); got != "No call in progress." {
		t.Fatal(got)
	}
	f.err = errors.New("x")
	if got := hangupReply(context.Background(), f, "l", ""); got != "Could not hang up." {
		t.Fatal(got)
	}
}

func TestDTMFReply(t *testing.T) {
	tests := []struct {
		err  error
		n    int
		want string
	}{
		{nil, 5, "Sent 5 tones."},
		{nil, 1, "Sent 1 tone."},
		{calls.ErrNoActiveCall, 0, "No call in progress."},
		{calls.ErrNotConnected, 0, "The call is not connected yet."},
		{media.ErrDTMFUnsupported, 0, "DTMF is not supported on this call."},
		{calls.ErrInvalidDigits, 0, "Use 1-32 of 0-9 * # A-D."},
		{media.ErrDTMFBusy, 0, "Too many tones"},
		{errors.New("x"), 0, "Could not send"},
	}
	for _, tt := range tests {
		f := &fakeCalls{err: tt.err, n: tt.n}
		got := dtmfReply(context.Background(), f, "l", "", "12#")
		if !strings.Contains(got, tt.want) {
			t.Errorf("err %v: got %q want %q", tt.err, got, tt.want)
		}
		if strings.Contains(got, "12#") {
			t.Errorf("reply echoes the digits: %q", got)
		}
	}
}

// TestDTMFNeverRecorded runs a dtmf handler under WithSpan and checks that neither the digits nor the
// number leak into spans or logs, and that only the digit count is recorded.
func TestDTMFNeverRecorded(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	base := zerolog.New(&logBuf)
	f := &fakeCalls{n: 5}
	ce := &commands.Event{Ctx: context.Background(), Log: &base, RawArgs: "9876#", Args: []string{"9876#"}}
	WithSpan("dtmf", func(ce *commands.Event) {
		_ = dtmfReply(ce.Ctx, f, "l", "", ce.RawArgs)
	})(ce)
	s := env.SpanByName("cmd.dtmf")
	if s == nil {
		t.Fatal("no cmd.dtmf span")
	}
	if got := obstest.Attr(s, "dtmf.digit_count"); got.AsInt64() != 5 {
		t.Errorf("dtmf.digit_count = %v", got)
	}
	if f.gotDigits != "9876#" {
		t.Errorf("digits not forwarded: %q", f.gotDigits)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "9876")
}

func TestPlaceErrReplyDestination(t *testing.T) {
	if got := placeErrReply(calls.ErrNotAllowed); !strings.Contains(got, "dial_allowed_prefixes") {
		t.Fatalf("got %q", got)
	}
}

func TestWidgetTargetNonPortalOnlyManagementRoom(t *testing.T) {
	h := &callCommands{}
	user := &bridgev2.User{User: &database.User{MXID: "@a:example.org", ManagementRoom: "!mgmt:example.org"}}
	cases := []struct {
		name string
		ce   *commands.Event
		want bool
	}{
		{"management room", &commands.Event{RoomID: "!mgmt:example.org", User: user}, true},
		{"other room", &commands.Event{RoomID: "!other:example.org", User: user}, false},
		{"no room", &commands.Event{User: user}, false},
		{"no management room set", &commands.Event{RoomID: "!x:example.org", User: &bridgev2.User{User: &database.User{}}}, false},
	}
	for _, c := range cases {
		room, _, ok := h.widgetTarget(c.ce)
		if ok != c.want || (ok && room != c.ce.RoomID) {
			t.Errorf("%s: ok=%v room=%q", c.name, ok, room)
		}
	}
}
