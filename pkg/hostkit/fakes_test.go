package hostkit

import (
	"context"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// sentEvent is one event sent through fakeBot.
type sentEvent struct {
	room    id.RoomID
	typ     event.Type
	body    string     // m.notice body for messages
	redacts id.EventID // for redactions
}

// fakeBot is the bridge bot of command tests: it records replies and redactions.
type fakeBot struct {
	bridgev2.MatrixAPI
	mu   sync.Mutex
	sent []sentEvent
}

func (b *fakeBot) SendMessage(_ context.Context, roomID id.RoomID, evtType event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	se := sentEvent{room: roomID, typ: evtType}
	switch c := content.Parsed.(type) {
	case *event.MessageEventContent:
		se.body = c.Body
	case *event.RedactionEventContent:
		se.redacts = c.Redacts
	}
	b.mu.Lock()
	b.sent = append(b.sent, se)
	b.mu.Unlock()
	return &mautrix.RespSendEvent{EventID: "$reply"}, nil
}

func (b *fakeBot) replies() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, s := range b.sent {
		if s.typ == event.EventMessage {
			out = append(out, s.body)
		}
	}
	return out
}

func (b *fakeBot) lastReply() string {
	r := b.replies()
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1]
}

func (b *fakeBot) redacted(evt id.EventID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sent {
		if s.typ == event.EventRedaction && s.redacts == evt {
			return true
		}
	}
	return false
}

// newEvent returns a command event in the management room with the given message.
func newEvent(bot *fakeBot, user *bridgev2.User, log *zerolog.Logger, eventID id.EventID, args ...string) *commands.Event {
	raw := ""
	for i, a := range args {
		if i > 0 {
			raw += " "
		}
		raw += a
	}
	return &commands.Event{
		Bot:        bot,
		Bridge:     &bridgev2.Bridge{Config: &bridgeconfig.BridgeConfig{CommandPrefix: "!jc"}},
		RoomID:     "!mgmt:example.org",
		OrigRoomID: "!mgmt:example.org",
		EventID:    eventID,
		User:       user,
		Args:       args,
		RawArgs:    raw,
		Ctx:        context.Background(),
		Log:        log,
	}
}
