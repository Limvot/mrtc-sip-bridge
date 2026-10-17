package hostkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// DialpadLinks is implemented by pkg/widget (nil = the PWA is disabled on this host); docs/PWA-SPEC.md §4.
type DialpadLinks interface {
	Issue(ctx context.Context, user id.UserID, login networkid.UserLoginID, room id.RoomID, name string) (LinkIssue, error)
	AttachNotice(ctx context.Context, linkID string, room id.RoomID, evt id.EventID) error
	List(ctx context.Context, user id.UserID) ([]LinkInfo, error)
	Revoke(ctx context.Context, user id.UserID, linkID string) (int, error) // linkID "all" = every link of user
}

// LinkIssue is a freshly issued link. Link carries the one-time code and must never be logged.
type LinkIssue struct {
	ID, Name, Link, AppURL string
	ExpiresAt              time.Time
}

// LinkInfo describes one link for `dialpad-links`. State is "pending", "active" or "expired".
type LinkInfo struct {
	ID, Name, State, Client    string
	Created, LastUsed, Expires time.Time
}

// Errors DialpadLinks.Issue returns (compare with errors.Is).
var (
	ErrDialpadLinkLimit      = errors.New("dialpad link limit reached")
	ErrDialpadRoomNotPrivate = errors.New(MsgWidgetNotPrivate)
)

const (
	msgDialpadMgmtOnly = "Run `$cmdprefix %s` in your management room (your DM with the bridge bot)."
	msgPWADisabled     = "The Dialpad app is not enabled on this bridge."
	msgLinkLimit       = "You have too many Dialpad links. Remove one with `$cmdprefix dialpad-revoke <id>` (see `$cmdprefix dialpad-links`)."
	msgLinkRate        = "Too many links requested. Try again later."
	msgLinkFailed      = "Could not create a Dialpad link."
	maxLinkName        = 32
	linkRatePerHour    = 5
)

// dialpadLimiter allows linkRatePerHour dialpad-link commands per user and hour.
type dialpadLimiter struct {
	mu  sync.Mutex
	now func() time.Time
	hit map[id.UserID][]time.Time
}

func (l *dialpadLimiter) allow(u id.UserID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	t := now()
	if l.hit == nil {
		l.hit = map[id.UserID][]time.Time{}
	}
	kept := l.hit[u][:0]
	for _, x := range l.hit[u] {
		if t.Sub(x) < time.Hour {
			kept = append(kept, x)
		}
	}
	if len(kept) >= linkRatePerHour {
		l.hit[u] = kept
		return false
	}
	l.hit[u] = append(kept, t)
	return true
}

func dialpadHandlers(h *callCommands) []*commands.FullHandler {
	return []*commands.FullHandler{{
		Func:          WithSpan("dialpad_link", h.cmdDialpadLink),
		Name:          "dialpad-link",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Get a one-time link that sets up the Dialpad phone app (management room only). Usage: `$cmdprefix dialpad-link [name]`",
			Args:        "[name]",
		},
	}, {
		Func:          WithSpan("dialpad_links", h.cmdDialpadLinks),
		Name:          "dialpad-links",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "List your Dialpad phone app links (management room only). Usage: `$cmdprefix dialpad-links`",
		},
	}, {
		Func:          WithSpan("dialpad_revoke", h.cmdDialpadRevoke),
		Name:          "dialpad-revoke",
		RequiresLogin: true,
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionChats,
			Description: "Sign out a Dialpad phone app link, or all of them (management room only). Usage: `$cmdprefix dialpad-revoke <id|all>`",
			Args:        "<id|all>",
		},
	}}
}

// dialpadGate runs the checks common to the three commands; false means a reply was sent.
func (h *callCommands) dialpadGate(ce *commands.Event, cmd string) bool {
	if h.d.Dialpad == nil {
		ce.Reply("%s", msgPWADisabled)
		return false
	}
	if ce.Portal != nil || ce.User == nil || ce.RoomID == "" || ce.RoomID != ce.User.ManagementRoom {
		ce.Reply(msgDialpadMgmtOnly, cmd)
		return false
	}
	if ul, refusal := h.d.Login(ce); ul == nil {
		ce.Reply("%s", refusal)
		return false
	}
	return true
}

// linkName keeps letters, digits, space and ._- (markdown-safe) and caps the length.
func linkName(args []string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.Join(args, " ") {
		if n >= maxLinkName {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			n++
		}
	}
	return strings.TrimSpace(b.String())
}

func (h *callCommands) cmdDialpadLink(ce *commands.Event) {
	if !h.dialpadGate(ce, "dialpad-link") {
		return
	}
	ul, _ := h.d.Login(ce)
	if !h.limiter.allow(ce.User.MXID) {
		ce.Reply("%s", msgLinkRate)
		return
	}
	name := linkName(ce.Args)
	iss, err := h.d.Dialpad.Issue(ce.Ctx, ce.User.MXID, ul.ID, ce.RoomID, name)
	switch {
	case err == nil:
	case errors.Is(err, ErrDialpadRoomNotPrivate):
		ce.Reply("%s", MsgWidgetNotPrivate)
		return
	case errors.Is(err, ErrDialpadLinkLimit):
		ce.Reply("%s", msgLinkLimit)
		return
	default:
		ce.Reply("%s", msgLinkFailed)
		return
	}
	prefix := ""
	if ce.Bridge != nil && ce.Bridge.Config != nil {
		prefix = ce.Bridge.Config.CommandPrefix
	}
	text := linkMessage(iss, prefix)
	content := format.RenderMarkdown(text, true, false)
	content.MsgType = event.MsgNotice
	// mautrix logs message bodies at Debug: send with a logger that drops everything (the span stays).
	// A disabled logger would make mautrix fall back to the client's own logger, which logs request
	// bodies (the one-time code) at Debug; an enabled logger writing nowhere suppresses it.
	quiet := zerolog.New(io.Discard).WithContext(ce.Ctx)
	resp, err := ce.Bot.SendMessage(quiet, ce.RoomID, event.EventMessage, &event.Content{Parsed: &content}, nil)
	if err != nil || resp == nil {
		// The code never reached the user: drop it.
		_, _ = h.d.Dialpad.Revoke(ce.Ctx, ce.User.MXID, iss.ID)
		ce.Reply("%s", msgLinkFailed)
		return
	}
	if err := h.d.Dialpad.AttachNotice(ce.Ctx, iss.ID, ce.RoomID, resp.EventID); err != nil && ce.Log != nil {
		ce.Log.Warn().Err(err).Msg("Failed to attach dialpad link notice for redaction")
	}
}

// linkMessage renders the one link message (the only place a code is ever shown).
func linkMessage(iss LinkIssue, cmdPrefix string) string {
	label := "Dialpad link"
	if iss.Name != "" {
		label += " for **" + iss.Name + "**"
	}
	mins := int(time.Until(iss.ExpiresAt).Round(time.Minute) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("%s (`%s`). It works **once** and expires in **%d minutes**:\n\n`%s`\n\n"+
		"On your phone, open <%s>, tap **Paste link**, and paste this message (or copy the link above into the phone's browser). "+
		"Then install it: Chrome: ⋮ → *Add to Home screen* / *Install app*; Firefox: ⋮ → *Install*. "+
		"iPhone: add the app to the Home Screen **first**, open it from there, then paste. "+
		"The bridge rings your Matrix app for each call; answer there. "+
		"I will delete this message once the link is used or expires. Devices: `%s dialpad-links`.",
		label, iss.ID, mins, iss.Link, iss.AppURL, cmdPrefix)
}

func (h *callCommands) cmdDialpadLinks(ce *commands.Event) {
	if !h.dialpadGate(ce, "dialpad-links") {
		return
	}
	links, err := h.d.Dialpad.List(ce.Ctx, ce.User.MXID)
	if err != nil {
		ce.Reply("%s", "Could not list Dialpad links.")
		return
	}
	if len(links) == 0 {
		ce.Reply("No Dialpad links. Use `$cmdprefix dialpad-link`.")
		return
	}
	var b strings.Builder
	b.WriteString("| id | name | state | client | created | last used | expires |\n|---|---|---|---|---|---|---|\n")
	for _, l := range links {
		state := l.State
		switch state {
		case "pending":
			state = "waiting for setup"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s |\n", l.ID, cell(l.Name), cell(state), cell(l.Client),
			day(l.Created), day(l.LastUsed), day(l.Expires))
	}
	ce.Reply("%s", b.String())
}

// cell makes s safe for a markdown table cell.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.NewReplacer("|", "/", "\n", " ", "\r", " ", "`", "'").Replace(s)
}

func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

func (h *callCommands) cmdDialpadRevoke(ce *commands.Event) {
	if !h.dialpadGate(ce, "dialpad-revoke") {
		return
	}
	if len(ce.Args) != 1 {
		ce.Reply("Usage: `$cmdprefix dialpad-revoke <id|all>`")
		return
	}
	linkID := ce.Args[0]
	n, err := h.d.Dialpad.Revoke(ce.Ctx, ce.User.MXID, linkID)
	switch {
	case err != nil:
		ce.Reply("%s", "Could not revoke.")
	case n == 0 && linkID == "all":
		ce.Reply("No Dialpad links to revoke.")
	case n == 0:
		ce.Reply("No link `%s` of yours.", cell(linkID))
	case n == 1:
		ce.Reply("Revoked 1 link. That device is signed out now.")
	default:
		ce.Reply("Revoked %d links. Those devices are signed out now.", n)
	}
}
