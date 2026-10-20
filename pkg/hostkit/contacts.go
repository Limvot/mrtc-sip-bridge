package hostkit

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"
)

// Optional host features of the Dialpad widget (WIDGET-SPEC §7.5). A host passes one value as
// widget.Deps.Features; the widget type-asserts it against each interface once, in widget.New. A
// feature is offered only when the value implements its interface. All methods act for ul, which the
// widget has already verified belongs to the session's user and is ready (WIDGET-SPEC §5.4).

// RosterOnlyPrefs is the "only ring for contacts" preference (roster-only ringing).
type RosterOnlyPrefs interface {
	// RosterOnly returns ul's current preference. It is called on every GET state poll: it must read
	// memory only (no network, no database) and never block.
	RosterOnly(ul *bridgev2.UserLogin) RosterOnlyState
	// SetRosterOnly persists the preference and returns the new state. It must return before ctx ends.
	SetRosterOnly(ctx context.Context, ul *bridgev2.UserLogin, on bool) (RosterOnlyState, error)
}

// RosterOnlyState is the preference plus an optional user-facing warning.
type RosterOnlyState struct {
	On bool
	// Warning is user-facing text ("" for none), e.g. that the contact list is empty so every call
	// goes to voicemail, or not loaded yet so calls ring for everyone. May contain $cmdprefix.
	Warning string
}

// ChatOpener opens the text chat of a phone number (same room as the host's start-chat/pm command).
type ChatOpener interface {
	// OpenChat returns the chat room of e164 (valid E.164, already checked by the widget) for ul,
	// creating it if needed. It must return before ctx's deadline: when creation takes longer it
	// returns ChatRoom{Pending: true} and finishes in the background. It must not run history
	// backfill inline.
	OpenChat(ctx context.Context, ul *bridgev2.UserLogin, e164 string) (ChatRoom, error)
}

// ChatRoom is the result of OpenChat.
type ChatRoom struct {
	RoomID  id.RoomID // "" when Pending
	Created bool      // the room was created by this call (clients may not have synced it yet)
	Pending bool      // creation is still running; the room appears (as an invite) shortly
}

// ContactSaver saves a phone number to ul's contacts (XMPP roster) under a name.
type ContactSaver interface {
	// SaveContact adds e164 under name, or renames it when already saved under another name (the
	// host's save-contact semantics). name has passed CleanContactName. Must return before ctx ends.
	SaveContact(ctx context.Context, ul *bridgev2.UserLogin, e164, name string) (SavedContact, error)
}

// SavedContact is the result of SaveContact.
type SavedContact struct {
	Name         string // the name now stored
	PreviousName string // the name before ("" when the number was not named)
	Changed      bool   // false when it was already saved under Name (nothing was sent)
}

// ContactLookup reports whether a number is a saved contact (portal view: "Saved as ...").
type ContactLookup interface {
	// ContactName returns the saved name of e164 ("" when none) and whether e164 is in the contacts at
	// all (an entry may have no name). Called on GET state polls: memory only, never blocks.
	ContactName(ul *bridgev2.UserLogin, e164 string) (name string, saved bool)
}

// ContactLister lists the user's named phone contacts for the Dialpad Contacts tab.
type ContactLister interface {
	// ListContacts returns one page of ul's named phone contacts. It is called on a request path (rate
	// limited): it must read memory only (no network, no database) and never block. offset and limit are
	// already validated by the widget (0 <= offset <= 10000, 1 <= limit <= 50).
	//
	// The list holds named phone contacts only: a number saved without a name, and contacts that are not
	// phone numbers, are not listed. Every Name must have passed DisplayContactName and be non-empty; the
	// host applies it BEFORE filtering and sorting so Total and the order match what is shown. Sort order:
	// strings.ToLower(Name) ascending, ties by E164 ascending (no Unicode normalization or accent folding).
	//
	// query "" lists all. Otherwise an entry matches when strings.Contains(lower(Name), lower(query)), or
	// when the query has at least 2 digits, consists only of digits and the characters + - ( ) . and space,
	// and its digits are a substring of the digits of E164. Otherwise (no digits, or one digit, or other
	// characters) only the name is matched. The widget passes the query trimmed, without control
	// characters and of at most 64 runes.
	//
	// An offset at or beyond the number of matches yields no Items but the correct Total. A host whose
	// contact list has not loaded yet returns ContactPage{Loaded: false}. The list may still be returned
	// while the host is offline. Errors: *ActionError like the other features.
	ListContacts(ul *bridgev2.UserLogin, query string, offset, limit int) (ContactPage, error)
}

// ContactEntry is one listed contact.
type ContactEntry struct {
	Name string // never empty; has passed DisplayContactName
	E164 string // valid E.164
}

// ContactPage is the result of ListContacts.
type ContactPage struct {
	Items  []ContactEntry // at most limit
	Total  int            // matches of the query over all pages
	Loaded bool           // false: the host's contact list has not loaded yet (Items and Total are then empty)
}

// Codes of ActionError (and of the widget API errors) for the optional features.
const (
	CodeInvalidName  = "invalid_name"  // 400
	CodeUnavailable  = "unavailable"   // 503: the host side is temporarily unable (e.g. XMPP offline)
	CodeNotSupported = "not_supported" // 404: produced by the widget when the host lacks the feature
)

// ActionError is a user-facing failure of an optional feature. Code is CodeInvalidNumber,
// CodeInvalidName or CodeUnavailable; Message is shown as is ($cmdprefix is expanded). Any other error
// returned by a feature method is reported as "internal" with a generic message (and logged scrubbed).
type ActionError struct{ Code, Message string }

func (e *ActionError) Error() string { return e.Code + ": " + e.Message }

// NewActionError returns an *ActionError.
func NewActionError(code, msg string) error { return &ActionError{Code: code, Message: msg} }

// MaxContactNameRunes bounds contact names (same limit as jmpchat-matrix-bridge's save-contact).
const MaxContactNameRunes = 64

// CleanContactName trims s and checks it is a usable contact name: 1..MaxContactNameRunes runes, valid
// UTF-8, no control characters (unicode.IsControl), no line/paragraph separators (U+2028, U+2029) and
// no bidi embedding/override/isolate controls (U+202A..U+202E, U+2066..U+2069). ok=false otherwise.
func CleanContactName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxContactNameRunes {
		return "", false
	}
	for _, r := range s {
		switch {
		case unicode.IsControl(r), r == ' ', r == ' ',
			r >= '‪' && r <= '‮', r >= '⁦' && r <= '⁩':
			return "", false
		}
	}
	return s, true
}

// DisplayContactName makes s safe and short for display: it drops the runes CleanContactName forbids
// (control characters, U+2028/U+2029, bidi embedding/override/isolate controls), maps every run of other
// whitespace to one space, trims, replaces invalid UTF-8 with nothing, and truncates to MaxContactNameRunes
// runes. It returns "" when nothing is left. It is idempotent. Hosts apply it to roster names (which come
// from arbitrary servers) before listing them; the widget applies it again defensively.
func DisplayContactName(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range strings.ToValidUTF8(s, "") {
		switch {
		case r == '\u2028' || r == '\u2029':
			continue
		case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r):
			continue
		}
		if space && b.Len() > 0 {
			if n++; n > MaxContactNameRunes {
				break
			}
			b.WriteByte(' ')
		}
		space = false
		if n++; n > MaxContactNameRunes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
