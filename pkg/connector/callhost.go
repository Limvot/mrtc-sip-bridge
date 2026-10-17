package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// callHost is the standalone core.CallHost.
type callHost struct{ c *SIPConnector }

var _ core.CallHost = (*callHost)(nil)

// CallHost returns the standalone core.CallHost (used by the call manager).
func (c *SIPConnector) CallHost() core.CallHost { return &callHost{c: c} }

func (h *callHost) login(ref core.LoginRef) (*bridgev2.UserLogin, error) {
	ul := h.c.br.GetCachedUserLoginByID(networkid.UserLoginID(ref))
	if ul == nil {
		return nil, core.ErrUnknownLogin
	}
	return ul, nil
}

// ResolveCallRoom returns the portal room and ghost for e164, creating the room if needed.
func (h *callHost) ResolveCallRoom(ctx context.Context, ref core.LoginRef, e164 string) (*core.CallRoom, error) {
	if !phone.IsE164(e164) {
		return nil, fmt.Errorf("%q is not an E.164 number", e164)
	}
	ul, err := h.login(ref)
	if err != nil {
		return nil, err
	}
	meta, ok := ul.Metadata.(*UserLoginMetadata)
	if !ok {
		return nil, fmt.Errorf("login %s has unexpected metadata type", ul.ID)
	}
	portal, err := h.c.br.GetPortalByKey(ctx, MakePortalKey(e164, ul.ID))
	if err != nil {
		return nil, fmt.Errorf("get portal: %w", err)
	}
	created := false
	if portal.MXID == "" {
		if err = portal.CreateMatrixRoom(ctx, ul, buildChatInfo(ul.ID, meta.DisplayNumber, e164)); err != nil {
			return nil, fmt.Errorf("create room: %w", err)
		}
		created = true
	}
	room, err := h.roomFor(ctx, portal.MXID, e164)
	if err != nil {
		return nil, err
	}
	room.Created = created
	if created {
		h.c.enqueueWidget(room.RoomID, e164)
	}
	return room, nil
}

// roomFor builds the CallRoom (ghost intent, encryption state) of an existing portal room.
func (h *callHost) roomFor(ctx context.Context, roomID id.RoomID, e164 string) (*core.CallRoom, error) {
	ghost, err := h.c.br.GetGhostByID(ctx, MakeUserID(e164))
	if err != nil {
		return nil, fmt.Errorf("get ghost: %w", err)
	}
	gi, err := hostkit.NewGhostIntent(ghost)
	if err != nil {
		return nil, err
	}
	encrypted := hostkit.RoomEncrypted(ctx, h.c.br, roomID)
	return &core.CallRoom{RoomID: roomID, RemoteE164: e164, Ghost: gi, Encrypted: encrypted}, nil
}

// SendCallNotice posts a notice in the room as the ghost.
func (h *callHost) SendCallNotice(ctx context.Context, room *core.CallRoom, text string) error {
	_, err := room.Ghost.SendMessage(ctx, room.RoomID, event.EventMessage, map[string]any{
		"msgtype": "m.notice",
		"body":    text,
	})
	return err
}

// LookupCallRoom returns the call room for an existing 1:1 number portal of ref. It creates nothing.
func (h *callHost) LookupCallRoom(ctx context.Context, ref core.LoginRef, roomID id.RoomID) (*core.CallRoom, error) {
	portal, err := h.c.br.GetPortalByMXID(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("get portal: %w", err)
	}
	if portal == nil || portal.Receiver != networkid.UserLoginID(ref) {
		return nil, core.ErrNotCallRoom
	}
	e164 := string(portal.ID)
	if !phone.IsE164(e164) {
		return nil, core.ErrNotCallRoom
	}
	return h.roomFor(ctx, roomID, e164)
}

// OwnerMXID returns the Matrix user who owns the login, or "" if unknown.
func (h *callHost) OwnerMXID(ref core.LoginRef) id.UserID {
	ul, err := h.login(ref)
	if err != nil {
		return ""
	}
	return ul.UserMXID
}

// MarkReadAsOwner sends a read receipt as the owner's double puppet.
func (h *callHost) MarkReadAsOwner(ctx context.Context, ref core.LoginRef, roomID id.RoomID, eventID id.EventID) error {
	ul, err := h.login(ref)
	if err != nil {
		return err
	}
	return hostkit.MarkReadAsOwner(ctx, ul, roomID, eventID)
}
