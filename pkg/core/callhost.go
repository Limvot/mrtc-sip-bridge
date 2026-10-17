package core

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// LoginRef identifies one SIP account login inside a host. The host chooses the value
// (the standalone bridge uses its bridgev2 UserLoginID). Core treats it as opaque.
type LoginRef string

// OpenIDToken is the Matrix OpenID token returned by
// POST /_matrix/client/v3/user/{userId}/openid/request_token, minted for a ghost.
type OpenIDToken struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int    `json:"expires_in"`
}

// GhostIntent acts as the ghost user that stands for the remote phone number.
// Content maps are sent as raw JSON event content.
type GhostIntent interface {
	UserID() id.UserID
	SendState(ctx context.Context, roomID id.RoomID, evtType event.Type, stateKey string, content map[string]any) (id.EventID, error)
	SendMessage(ctx context.Context, roomID id.RoomID, evtType event.Type, content map[string]any) (id.EventID, error)
	Redact(ctx context.Context, roomID id.RoomID, eventID id.EventID, reason string) error
	RequestOpenIDToken(ctx context.Context) (*OpenIDToken, error)
}

// CallRoom is the Matrix room in which calls with one remote number ring, plus the ghost for that number.
type CallRoom struct {
	RoomID     id.RoomID
	RemoteE164 string
	Ghost      GhostIntent
	// Encrypted is true if the room has m.room.encryption. Calls refuse to ring in encrypted rooms
	// (Element Call would use SFrame E2EE and the bridged audio would decode to silence, ADR-002).
	Encrypted bool
	// Created is true when ResolveCallRoom created the Matrix room in this call (clients may not have
	// synced it yet). Always false from LookupCallRoom.
	Created bool
}

// CallHost is everything the call core needs from the bridge that embeds it.
type CallHost interface {
	// ResolveCallRoom returns the room and ghost for e164 (E.164, e.g. "+15551234567"),
	// creating the portal room if needed.
	ResolveCallRoom(ctx context.Context, login LoginRef, e164 string) (*CallRoom, error)
	// LookupCallRoom returns the CallRoom for an existing room, or ErrNotCallRoom if roomID is not a
	// 1:1 call portal of login (unknown room, other login, group chat). Must not create anything.
	LookupCallRoom(ctx context.Context, login LoginRef, roomID id.RoomID) (*CallRoom, error)
	// SendCallNotice posts a short text notice ("Missed call", "Call · 1m20s") in the room.
	SendCallNotice(ctx context.Context, room *CallRoom, text string) error
	// OwnerMXID returns the Matrix user who owns the login, or "" if the login is unknown.
	OwnerMXID(login LoginRef) id.UserID
	// MarkReadAsOwner sends a read receipt for eventID as the owner (double puppet).
	// Returns ErrNoDoublePuppet if the owner has no double puppet.
	MarkReadAsOwner(ctx context.Context, login LoginRef, roomID id.RoomID, eventID id.EventID) error
}

var (
	// ErrNotCallRoom is returned by LookupCallRoom for rooms that cannot carry calls.
	ErrNotCallRoom = errors.New("core: not a call room")
	// ErrUnknownLogin is returned by hosts when a LoginRef does not belong to any known login.
	ErrUnknownLogin = errors.New("core: unknown login")
	// ErrNoDoublePuppet is returned by MarkReadAsOwner when the owner has no double puppet.
	ErrNoDoublePuppet = errors.New("core: owner has no double puppet")
)
