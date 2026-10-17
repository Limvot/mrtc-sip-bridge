package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// newMatrixClient returns an appservice client masquerading as userID
// (AS token + ?user_id=). It never needs a /sync loop or an HTTP listener.
func newMatrixClient(cfg *envConfig, userID id.UserID) (*mautrix.Client, error) {
	cli, err := mautrix.NewClient(cfg.HomeserverURL, userID, cfg.ASToken)
	if err != nil {
		return nil, err
	}
	cli.SetAppServiceUserID = true
	return cli, nil
}

func stateType(name string) event.Type { return event.Type{Type: name, Class: event.StateEventType} }
func msgType(name string) event.Type   { return event.Type{Type: name, Class: event.MessageEventType} }

// ensureGhost registers the ghost (ignoring M_USER_IN_USE) and sets its display name.
func ensureGhost(ctx context.Context, cfg *envConfig, ghost id.UserID, displayName string, log *slog.Logger) error {
	// Registration must not carry ?user_id=, so use a non-masquerading client.
	reg, err := mautrix.NewClient(cfg.HomeserverURL, "", cfg.ASToken)
	if err != nil {
		return err
	}
	_, _, err = reg.Register(ctx, &mautrix.ReqRegister[any]{
		Username:     ghost.Localpart(),
		Type:         mautrix.AuthTypeAppservice,
		InhibitLogin: true,
	})
	switch {
	case err == nil:
		log.Info("ghost registered", "ghost", ghost)
	case errors.Is(err, mautrix.MUserInUse):
		log.Info("ghost already registered", "ghost", ghost)
	default:
		return fmt.Errorf("register ghost (is the registration installed and does the ghost match its namespace?): %w", err)
	}
	gc, err := newMatrixClient(cfg, ghost)
	if err != nil {
		return err
	}
	if err := gc.SetDisplayName(ctx, displayName); err != nil {
		return fmt.Errorf("set displayname: %w", err)
	}
	log.Info("ghost displayname set", "displayname", displayName)
	return nil
}

// ensureCallPowerLevels makes call.member (both names) sendable at level 0 (spec §1.5),
// keeping the rest of the room's power levels untouched.
func ensureCallPowerLevels(ctx context.Context, ghost *mautrix.Client, room id.RoomID, log *slog.Logger) error {
	var pl map[string]any
	if err := ghost.StateEvent(ctx, room, event.StatePowerLevels, "", &pl); err != nil {
		return fmt.Errorf("read power levels: %w", err)
	}
	events, _ := pl["events"].(map[string]any)
	if events == nil {
		events = map[string]any{}
	}
	changed := false
	for _, t := range callMemberPLEvents {
		if v, ok := events[t].(float64); !ok || v != 0 {
			events[t] = 0
			changed = true
		}
	}
	if !changed {
		log.Info("power levels already allow call.member at 0")
		return nil
	}
	pl["events"] = events
	if _, err := ghost.SendStateEvent(ctx, room, event.StatePowerLevels, "", pl); err != nil {
		return fmt.Errorf("update power levels (ghost needs enough power in a reused room): %w", err)
	}
	log.Info("power levels updated: call.member at 0 for users")
	return nil
}

// waitOwnerJoined polls joined members until the owner is in the room.
func waitOwnerJoined(ctx context.Context, ghost *mautrix.Client, room id.RoomID, owner id.UserID, log *slog.Logger) error {
	hinted := false
	for {
		resp, err := ghost.JoinedMembers(ctx, room)
		if err != nil {
			return fmt.Errorf("joined members: %w", err)
		}
		if _, ok := resp.Joined[owner]; ok {
			log.Info("owner has joined the room", "owner", owner)
			return nil
		}
		if !hinted {
			log.Info("waiting for owner to join: ACCEPT THE INVITE in Element now", "owner", owner, "room", room)
			hinted = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// liveMember is an owner membership event considered valid and unexpired.
type liveMember struct {
	StateKey string
	EventID  id.EventID
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// memberIsLive applies the "valid, non-empty, unexpired" test (spec §1.3/§1.4):
// application m.call, scope m.room (when present), and base+expires > now where
// base is created_ts or the event's origin_server_ts, expires defaulting to 4 h.
func memberIsLive(ev *event.Event, now time.Time) bool {
	c := ev.Content.Raw
	if len(c) == 0 {
		return false
	}
	if app, _ := c[fieldApplication].(string); app != appCall {
		return false
	}
	if sc, ok := c[fieldScope].(string); ok && sc != scopeRoom {
		return false
	}
	base := ev.Timestamp
	if v, ok := asInt64(c[fieldCreatedTS]); ok {
		base = v
	}
	exp := membershipTTL.Milliseconds()
	if v, ok := asInt64(c[fieldExpires]); ok {
		exp = v
	}
	return base+exp > now.UnixMilli()
}

// liveMembersOf returns all live call.member events of userID from the room state.
func liveMembersOf(ctx context.Context, cli *mautrix.Client, room id.RoomID, userID id.UserID, now time.Time) ([]liveMember, error) {
	st, err := cli.State(ctx, room)
	if err != nil {
		return nil, err
	}
	var out []liveMember
	for typ, byKey := range st {
		if typ.Type != EventCallMember && typ.Type != EventCallMemberAlias {
			continue
		}
		for key, ev := range byKey {
			if ev == nil || !stateKeyIsUser(key, userID.String()) {
				continue
			}
			if ev.Sender != userID {
				continue // spec §1.5: sender must be the member
			}
			if memberIsLive(ev, now) {
				out = append(out, liveMember{StateKey: key, EventID: ev.ID})
			}
		}
	}
	return out, nil
}

// findDecline scans recent room messages for an owner decline of notifID newer than since.
func findDecline(ctx context.Context, cli *mautrix.Client, room id.RoomID, owner id.UserID, notifID id.EventID, since time.Time) (bool, error) {
	resp, err := cli.Messages(ctx, room, "", "", mautrix.DirectionBackward, nil, 50)
	if err != nil {
		return false, err
	}
	for _, ev := range resp.Chunk {
		if ev.Timestamp < since.UnixMilli() {
			continue
		}
		if ev.Type.Type != EventRTCDecline && ev.Type.Type != EventRTCDeclineAlias {
			continue
		}
		if ev.Sender != owner { // only honour the owner's declines (spec §3.1)
			continue
		}
		rel, _ := ev.Content.Raw[fieldRelatesTo].(map[string]any)
		if got, _ := rel[fieldEventID].(string); got == notifID.String() {
			return true, nil
		}
	}
	return false, nil
}
