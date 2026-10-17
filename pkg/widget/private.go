package widget

import (
	"context"
	"errors"
	"slices"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

// MsgNotPrivateRoom is the refusal text for rooms with other members (hosts' ResolveRoom should
// return Refuse(MsgNotPrivateRoom) for them): the widget URL carries the room key, so anyone who can
// read the room's state could use it (WIDGET-SPEC §5.2).
const MsgNotPrivateRoom = hostkit.MsgWidgetNotPrivate

// ErrNotPrivateRoom is returned by Installer.Ensure for rooms that are not private.
var ErrNotPrivateRoom = hostkit.ErrWidgetRoomNotPrivate

// PrivateRoom reports whether room has no joined or invited member besides the bridge bot, this
// bridge's ghosts and the allowed users. It reads only the bot's state store (no network I/O), so it
// reflects the membership events the appservice has seen. An error means it could not tell.
func PrivateRoom(ctx context.Context, br *bridgev2.Bridge, room id.RoomID, allowed ...id.UserID) (bool, error) {
	members, err := storeMembers(ctx, br, room)
	if err != nil {
		return false, err
	}
	var bot id.UserID
	if mc, ok := br.Matrix.(*matrix.Connector); ok && mc.Bot != nil {
		bot = mc.Bot.UserID
	}
	return privateAmong(members, bot, allowed, func(u id.UserID) bool { return isGhost(br, u) }), nil
}

// privateAmong reports whether every member is the bot, a ghost or in allowed.
func privateAmong(members []id.UserID, bot id.UserID, allowed []id.UserID, ghost func(id.UserID) bool) bool {
	for _, m := range members {
		if m == bot || ghost(m) {
			continue
		}
		if !slices.Contains(allowed, m) {
			return false
		}
	}
	return true
}

func isGhost(br *bridgev2.Bridge, u id.UserID) bool {
	_, ok := br.Matrix.ParseGhostMXID(u)
	return ok
}

// storeMembers lists the joined and invited members from the bot's state store.
func storeMembers(ctx context.Context, br *bridgev2.Bridge, room id.RoomID) ([]id.UserID, error) {
	if br == nil {
		return nil, errors.New("widget: no bridge")
	}
	mc, ok := br.Matrix.(*matrix.Connector)
	if !ok || mc == nil || mc.Bot == nil || mc.Bot.StateStore == nil {
		return nil, errors.New("widget: the bridge has no matrix state store")
	}
	return mc.Bot.StateStore.GetRoomJoinedOrInvitedMembers(ctx, room)
}

// roomPrivate is the installer's privacy check. With neither a RoomMembers seam nor a real matrix
// connector it cannot tell and allows the room (host tests). Otherwise every joined or invited member
// must be the bot, a ghost, or a bridge user (at most one: the owner).
func (in *Installer) roomPrivate(ctx context.Context, room id.RoomID) (bool, error) {
	var members []id.UserID
	var err error
	switch {
	case in.d.RoomMembers != nil:
		members, err = in.d.RoomMembers(ctx, room)
	case in.d.Bridge != nil:
		if _, ok := in.d.Bridge.Matrix.(*matrix.Connector); !ok {
			return true, nil
		}
		members, err = storeMembers(ctx, in.d.Bridge, room)
	default:
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owners := 0
	for _, m := range members {
		if m == in.creator || (in.d.Bridge != nil && isGhost(in.d.Bridge, m)) {
			continue
		}
		if in.d.RoomOwners != nil {
			listed := false
			for _, o := range in.d.RoomOwners(ctx, room) {
				listed = listed || o == m
			}
			if !listed {
				return false, nil
			}
			continue
		}
		u, err := in.d.LookupUser(ctx, m)
		if err != nil {
			return false, err
		}
		if u == nil {
			return false, nil
		}
		if owners++; owners > 1 {
			return false, nil
		}
	}
	return true, nil
}
