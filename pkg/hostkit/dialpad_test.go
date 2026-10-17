package hostkit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const testCode = "mrtcsip_dpc_SECRETSECRETSECRETSECRETSECRETSECRETSECRE"

type fakeLinks struct {
	issueErr error
	revokeN  int
	links    []LinkInfo
	issued   []string // names
	attached []id.EventID
	revoked  []string
}

func (f *fakeLinks) Issue(_ context.Context, _ id.UserID, _ networkid.UserLoginID, _ id.RoomID, name string) (LinkIssue, error) {
	if f.issueErr != nil {
		return LinkIssue{}, f.issueErr
	}
	f.issued = append(f.issued, name)
	return LinkIssue{ID: "k3m9qa", Name: name, Link: "https://x.example/app/#c=" + testCode,
		AppURL: "https://x.example/app/", ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}
func (f *fakeLinks) AttachNotice(_ context.Context, _ string, _ id.RoomID, evt id.EventID) error {
	f.attached = append(f.attached, evt)
	return nil
}
func (f *fakeLinks) List(context.Context, id.UserID) ([]LinkInfo, error) { return f.links, nil }
func (f *fakeLinks) Revoke(_ context.Context, _ id.UserID, linkID string) (int, error) {
	f.revoked = append(f.revoked, linkID)
	return f.revokeN, nil
}

// ctxLogBot logs every message body at Debug to the ctx logger, as mautrix does.
type ctxLogBot struct{ fakeBot }

func (b *ctxLogBot) SendMessage(ctx context.Context, room id.RoomID, t event.Type, c *event.Content, x *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	if m, ok := c.Parsed.(*event.MessageEventContent); ok {
		zerolog.Ctx(ctx).Debug().Str("req_body", m.Body).Msg("sending")
	}
	return b.fakeBot.SendMessage(ctx, room, t, c, x)
}

func dialpadSetup(f *fakeLinks) (*callCommands, *bridgev2.User) {
	d := fakeLoginDeps(&fakeCalls{}, "")
	d.Dialpad = f
	user := &bridgev2.User{User: &database.User{MXID: "@alice:x", ManagementRoom: "!mgmt:example.org"}}
	return &callCommands{d: d}, user
}

func TestDialpadLinkSendsQuietly(t *testing.T) {
	var logBuf bytes.Buffer
	log := zerolog.New(&logBuf).Level(zerolog.DebugLevel)
	f := &fakeLinks{}
	h, user := dialpadSetup(f)
	bot := &ctxLogBot{}
	ce := newEvent(&bot.fakeBot, user, &log, "$cmd", "Pixel", "7!")
	ce.Bot = bot
	ce.Ctx = log.WithContext(context.Background())
	h.cmdDialpadLink(ce)
	if len(f.issued) != 1 || f.issued[0] != "Pixel 7" {
		t.Fatalf("issued %v", f.issued)
	}
	msg := bot.lastReply()
	if !strings.Contains(msg, "`https://x.example/app/#c="+testCode+"`") || !strings.Contains(msg, "**Pixel 7**") || !strings.Contains(msg, "!jc dialpad-links") {
		t.Fatalf("message: %q", msg)
	}
	if len(f.attached) != 1 || f.attached[0] != "$reply" {
		t.Fatalf("attached %v", f.attached)
	}
	if strings.Contains(logBuf.String(), "mrtcsip_dpc_") || strings.Contains(logBuf.String(), "SECRET") {
		t.Fatalf("code logged: %s", logBuf.String())
	}
}

func TestDialpadLinkRefusals(t *testing.T) {
	log := zerolog.Nop()
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"private", ErrDialpadRoomNotPrivate, "other members"},
		{"limit", ErrDialpadLinkLimit, "too many"},
		{"other", errors.New("boom"), "Could not create"},
	} {
		f := &fakeLinks{issueErr: tt.err}
		h, user := dialpadSetup(f)
		bot := &fakeBot{}
		h.cmdDialpadLink(newEvent(bot, user, &log, "$c"))
		if !strings.Contains(bot.lastReply(), tt.want) || len(f.attached) != 0 {
			t.Errorf("%s: reply %q", tt.name, bot.lastReply())
		}
	}
	// Not the management room, and PWA disabled.
	f := &fakeLinks{}
	h, user := dialpadSetup(f)
	bot := &fakeBot{}
	ce := newEvent(bot, user, &log, "$c")
	ce.RoomID = "!other:x"
	h.cmdDialpadLink(ce)
	h.cmdDialpadLinks(ce)
	h.cmdDialpadRevoke(ce)
	if len(f.issued) != 0 || len(f.revoked) != 0 || !strings.Contains(bot.lastReply(), "management room") {
		t.Fatalf("non-management: %v %q", f.issued, bot.lastReply())
	}
	portal := portalEvent(newEvent(bot, user, &log, "$p"), "+1555")
	portal.User.ManagementRoom = "!portal:x" // even if equal, a portal room is refused
	h.cmdDialpadLink(portal)
	if len(f.issued) != 0 {
		t.Fatal("issued in portal room")
	}
	h2 := &callCommands{d: fakeLoginDeps(&fakeCalls{}, "")}
	h2.cmdDialpadLink(newEvent(bot, user, &log, "$d"))
	if !strings.Contains(bot.lastReply(), "not enabled") {
		t.Fatalf("disabled: %q", bot.lastReply())
	}
}

func TestDialpadLinkRateLimit(t *testing.T) {
	log := zerolog.Nop()
	f := &fakeLinks{}
	h, user := dialpadSetup(f)
	bot := &fakeBot{}
	for i := 0; i < 6; i++ {
		h.cmdDialpadLink(newEvent(bot, user, &log, "$c"))
	}
	if len(f.issued) != 5 || !strings.Contains(bot.lastReply(), "Too many") {
		t.Fatalf("issued %d reply %q", len(f.issued), bot.lastReply())
	}
}

func TestDialpadLinksAndRevoke(t *testing.T) {
	log := zerolog.Nop()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := &fakeLinks{}
	h, user := dialpadSetup(f)
	bot := &fakeBot{}
	h.cmdDialpadLinks(newEvent(bot, user, &log, "$c"))
	if !strings.Contains(bot.lastReply(), "No Dialpad links. Use `!jc dialpad-link`") {
		t.Fatalf("empty: %q", bot.lastReply())
	}
	f.links = []LinkInfo{{ID: "abc123", Name: "a|b", State: "pending", Client: "Android Chrome", Created: now, Expires: now}}
	h.cmdDialpadLinks(newEvent(bot, user, &log, "$c"))
	r := bot.lastReply()
	if !strings.Contains(r, "abc123") || !strings.Contains(r, "waiting for setup") || !strings.Contains(r, "2026-01-02") || strings.Contains(r, "a|b") {
		t.Fatalf("table: %q", r)
	}
	f.revokeN = 1
	h.cmdDialpadRevoke(newEvent(bot, user, &log, "$c", "abc123"))
	if !strings.Contains(bot.lastReply(), "Revoked 1 link. That device is signed out now.") || f.revoked[0] != "abc123" {
		t.Fatalf("revoke: %q", bot.lastReply())
	}
	f.revokeN = 0
	h.cmdDialpadRevoke(newEvent(bot, user, &log, "$c", "zzz"))
	if !strings.Contains(bot.lastReply(), "No link `zzz` of yours") {
		t.Fatalf("unknown: %q", bot.lastReply())
	}
	h.cmdDialpadRevoke(newEvent(bot, user, &log, "$c"))
	if !strings.Contains(bot.lastReply(), "Usage") {
		t.Fatalf("usage: %q", bot.lastReply())
	}
}

func TestDialpadHandlersRegisteredOnlyWhenEnabled(t *testing.T) {
	if n := len(callHandlers(fakeLoginDeps(&fakeCalls{}, ""))); n != 4 {
		t.Fatalf("disabled: %d handlers", n)
	}
	d := fakeLoginDeps(&fakeCalls{}, "")
	d.Dialpad = &fakeLinks{}
	if n := len(callHandlers(d)); n != 7 {
		t.Fatalf("enabled: %d handlers", n)
	}
}

func TestPWAConfig(t *testing.T) {
	base := func() Config {
		c := Config{Widget: WidgetConfig{Enabled: true, PublicURL: "https://x.example/dp/", Secret: strings.Repeat("s", 40)}}
		c.Widget.PWA.Enabled = true
		c.ApplyDefaults()
		return c
	}
	c := base()
	if p := c.Widget.PWA; p.InactivityDays != 90 || p.RotationGraceSeconds != 120 || p.EnrollMinutes != 15 || p.MaxLinksPerUser != 5 || !p.NotifyOnEnrollEnabled() {
		t.Fatalf("defaults %+v", p)
	}
	if err := c.Validate("network."); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*PWAConfig){
		"inactivity_days":        func(p *PWAConfig) { p.InactivityDays = 3651 },
		"max_lifetime_days":      func(p *PWAConfig) { p.MaxLifetimeDays = 30 },
		"rotation_grace_seconds": func(p *PWAConfig) { p.RotationGraceSeconds = 5 },
		"enroll_minutes":         func(p *PWAConfig) { p.EnrollMinutes = 61 },
		"max_links_per_user":     func(p *PWAConfig) { p.MaxLinksPerUser = 51 },
	} {
		c := base()
		mut(&c.Widget.PWA)
		if err := c.Validate("network."); err == nil || !strings.Contains(err.Error(), "network.widget.pwa."+name) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	c = base()
	c.Widget.PWA.MaxLifetimeDays = 90
	if err := c.Validate("network."); err != nil {
		t.Errorf("lifetime == inactivity: %v", err)
	}
	c = base()
	c.Widget.Enabled = false
	if err := c.Validate("network."); err == nil || !strings.Contains(err.Error(), "pwa.enabled") {
		t.Errorf("pwa without widget: %v", err)
	}
	c = base()
	c.Widget.PWA.Enabled = false
	c.Widget.PWA.InactivityDays = 0
	if err := c.Validate("network."); err != nil {
		t.Errorf("disabled pwa validated: %v", err)
	}
}
