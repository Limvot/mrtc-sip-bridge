package main

// All MatrixRTC event type names, state-key and content field names live here.
// Spec: docs/spikes/S1-matrixrtc-wire-spec.md (cited as "spec §N"). These are
// unstable identifiers; if a client/server stops honouring them, this is the
// one file to change.

import (
	"strings"
	"time"
)

const (
	// Spec §1.1: legacy per-device membership state event. The only membership
	// type Element X understands. Do NOT send the sticky msc4143 rtc.member.
	EventCallMember = "org.matrix.msc3401.call.member"
	// Spec §1.5/§2.3: ruma alias; power level override is set for both names.
	EventCallMemberAlias = "m.call.member"

	// Spec §2.1: ring notification (MSC4075 unstable name; NOT m.rtc.notification).
	EventRTCNotification = "org.matrix.msc4075.rtc.notification"

	// Spec §3.1: decline (MSC4310, folded into MSC4075). Listen for both names.
	EventRTCDecline      = "org.matrix.msc4310.rtc.decline"
	EventRTCDeclineAlias = "m.rtc.decline"
)

// Spec §1.3 constant content values.
const (
	appCall       = "m.call"
	scopeRoom     = "m.room"
	callIDRoom    = ""            // "" = the room call (slot m.call#ROOM)
	slotIDRoom    = "m.call#ROOM" // spec §2.2 / §4.5
	intentAudio   = "audio"
	focusTypeLK   = "livekit"
	focusSelMulti = "multi_sfu"
	membershipTTL = 4 * time.Hour // spec §1.3 `expires`, a duration relative to created_ts
	ringTypeRing  = "ring"
)

// memberStateKey builds the spec §1.2 state key: "_" + user + "_" + device + "_" + application + call_id.
func memberStateKey(userID, deviceID string) string {
	return "_" + userID + "_" + deviceID + "_" + appCall + callIDRoom
}

// memberStateKeyUser extracts nothing fancy: reports whether a state key belongs to userID (spec §1.2).
func stateKeyIsUser(stateKey, userID string) bool {
	return strings.HasPrefix(stateKey, "_"+userID+"_")
}

// joinContent is the spec §1.3 membership content (SessionMembershipData union).
// roomID goes into foci_preferred[].livekit_alias (required by ruma, value ignored).
// No `url` field on purpose (spec §1.3: it would switch to MSC4195 hashed identity).
func joinContent(userID, deviceID, roomID, serviceURL string, now time.Time) map[string]any {
	return map[string]any{
		"application":   appCall,
		"call_id":       callIDRoom,
		"scope":         scopeRoom,
		"device_id":     deviceID,
		"membershipID":  userID + ":" + deviceID, // spec §1.3/§5: matches LiveKit identity
		"created_ts":    now.UnixMilli(),
		"expires":       membershipTTL.Milliseconds(),
		"m.call.intent": intentAudio,
		"focus_active":  map[string]any{"type": focusTypeLK, "focus_selection": focusSelMulti},
		"foci_preferred": []any{map[string]any{
			"type":                focusTypeLK,
			"livekit_service_url": serviceURL,
			"livekit_alias":       roomID,
		}},
	}
}

// leaveContent is the spec §1.4 leave: empty content.
func leaveContent() map[string]any { return map[string]any{} }

// ringContent is the spec §2.2 notification content. lifetime must equal the ring timeout.
func ringContent(ownerMXID, membershipEventID string, lifetime time.Duration, now time.Time) map[string]any {
	return map[string]any{
		"notification_type": ringTypeRing, // required by ruma/Element X, spec §2.2
		"sender_ts":         now.UnixMilli(),
		"lifetime":          lifetime.Milliseconds(), // spec §2.2: Element X has no cap
		"m.mentions":        map[string]any{"user_ids": []string{ownerMXID}},
		"m.relates_to":      map[string]any{"rel_type": "m.reference", "event_id": membershipEventID},
		"m.call.intent":     intentAudio,
		"slot_id":           slotIDRoom,
	}
}

// Power level entries required for call.member at level 0 (spec §1.5).
var callMemberPLEvents = []string{EventCallMember, EventCallMemberAlias}

// Field names read when parsing other people's membership/decline events.
const (
	fieldApplication = "application"
	fieldScope       = "scope"
	fieldCreatedTS   = "created_ts"
	fieldExpires     = "expires"
	fieldRelatesTo   = "m.relates_to"
	fieldEventID     = "event_id"
)

// OpenID / lk-jwt-service legacy endpoint (spec §4.4).
const sfuGetPath = "/sfu/get"
