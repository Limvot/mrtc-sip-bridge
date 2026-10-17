package hostkit

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
)

// ResolveRTCLogin returns the core.LoginResolver of the S3 hook: drop ghost and bot senders (bridgev2's
// own filtering does not apply to raw handlers), look up the portal by room ID, return its Receiver if
// accept(receiver) (accept nil = all). Register it with core.RegisterMatrixRTCHandlers in PostInit,
// before the event processor starts.
func ResolveRTCLogin(br *bridgev2.Bridge, accept func(networkid.UserLoginID) bool) core.LoginResolver {
	return resolveRTCLogin(
		func(sender id.UserID) bool { return br.IsGhostMXID(sender) || sender == br.Bot.GetMXID() },
		br.GetPortalByMXID,
		accept)
}

// resolveRTCLogin is ResolveRTCLogin with the bridge lookups injected (unit-tested).
func resolveRTCLogin(dropSender func(id.UserID) bool, lookup func(context.Context, id.RoomID) (*bridgev2.Portal, error),
	accept func(networkid.UserLoginID) bool) core.LoginResolver {
	return func(ctx context.Context, evt *event.Event) (core.LoginRef, bool) {
		if dropSender(evt.Sender) {
			return "", false
		}
		portal, err := lookup(ctx, evt.RoomID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", evt.RoomID).Msg("Failed to look up portal for MatrixRTC event")
			return "", false
		}
		if portal == nil || portal.Receiver == "" {
			return "", false
		}
		if accept != nil && !accept(portal.Receiver) {
			return "", false
		}
		return core.LoginRef(portal.Receiver), true
	}
}
