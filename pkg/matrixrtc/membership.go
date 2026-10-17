package matrixrtc

import (
	"encoding/json"
	"strings"
	"time"

	"maunium.net/go/mautrix/id"
)

// MemberStateKey builds the per-device membership state key (wire §1.2):
// "_" + userID + "_" + deviceID + "_" + ApplicationCall + CallIDRoom.
func MemberStateKey(userID id.UserID, deviceID string) string {
	return "_" + string(userID) + "_" + deviceID + "_" + ApplicationCall + CallIDRoom
}

// ParseMemberStateKey reverses MemberStateKey the way ruma does (wire §1.2): strip one leading "_"
// (accept keys without it too), the user ID runs up to the first "_" after the first ":", the rest is
// the member id ("<device>_m.call"). ok=false if there is no ":" or no "_" after it.
func ParseMemberStateKey(stateKey string) (userID id.UserID, memberID string, ok bool) {
	s := strings.TrimPrefix(stateKey, "_")
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return "", "", false
	}
	us := strings.IndexByte(s[colon:], '_')
	if us < 0 {
		return "", "", false
	}
	us += colon
	return id.UserID(s[:us]), s[us+1:], true
}

// JoinParams describes the ghost's membership.
type JoinParams struct {
	UserID     id.UserID     // the ghost
	DeviceID   string        // invented, [A-Z0-9]
	RoomID     id.RoomID     // also used as livekit_alias (value ignored by clients, required by ruma)
	ServiceURL string        // lk-jwt-service base URL, goes into foci_preferred[0].livekit_service_url
	CreatedTS  time.Time     // created_ts (UnixMilli)
	Expires    time.Duration // expires (ms), relative to CreatedTS
}

// JoinContent returns exactly the wire §1.3 content (keys: application, call_id, scope, device_id,
// membershipID = "<user>:<device>", created_ts, expires, m.call.intent = "audio", focus_active
// {type: livekit, focus_selection: multi_sfu}, foci_preferred [{type: livekit, livekit_service_url,
// livekit_alias}]). Never a "url" key (wire §1.3).
func JoinContent(p JoinParams) map[string]any {
	return map[string]any{
		FieldApplication:  ApplicationCall,
		FieldCallID:       CallIDRoom,
		FieldScope:        ScopeRoom,
		FieldDeviceID:     p.DeviceID,
		FieldMembershipID: string(p.UserID) + ":" + p.DeviceID,
		FieldCreatedTS:    p.CreatedTS.UnixMilli(),
		FieldExpires:      p.Expires.Milliseconds(),
		FieldCallIntent:   IntentAudio,
		FieldFocusActive: map[string]any{
			FieldType:           FocusTypeLiveKit,
			FieldFocusSelection: FocusSelectionMultiSFU,
		},
		FieldFociPreferred: []any{
			map[string]any{
				FieldType:              FocusTypeLiveKit,
				FieldLiveKitServiceURL: p.ServiceURL,
				FieldLiveKitAlias:      string(p.RoomID),
			},
		},
	}
}

// LeaveContent is the empty membership content (wire §1.4).
func LeaveContent() map[string]any { return map[string]any{} }

// RingParams describes the MSC4075 ring.
type RingParams struct {
	Owner             id.UserID
	MembershipEventID id.EventID    // the ghost's call.member event (m.reference target)
	Lifetime          time.Duration // = ring timeout
	SenderTS          time.Time     // set right before sending
}

// RingContent returns exactly the wire §2.2 content: notification_type "ring", sender_ts, lifetime (ms),
// m.mentions.user_ids [owner], m.relates_to {rel_type m.reference, event_id}, m.call.intent "audio",
// slot_id "m.call#ROOM".
func RingContent(p RingParams) map[string]any {
	return map[string]any{
		FieldNotificationType: NotificationTypeRing,
		FieldSenderTS:         p.SenderTS.UnixMilli(),
		FieldLifetime:         p.Lifetime.Milliseconds(),
		FieldMentions:         map[string]any{FieldUserIDs: []any{string(p.Owner)}},
		FieldRelatesTo: map[string]any{
			FieldRelType: RelTypeReference,
			FieldEventID: string(p.MembershipEventID),
		},
		FieldCallIntent: IntentAudio,
		FieldSlotID:     SlotIDRoom,
	}
}

// Membership is a parsed call.member event.
type Membership struct {
	UserID   id.UserID // from the state key
	DeviceID string    // content device_id, else the member id without the "_m.call" suffix
	Live     bool      // non-empty, application m.call, scope m.room (or absent), base+expires > now
	// Created is created_ts if present, else the event timestamp (the base used for Live). Zero for
	// non-live (empty or foreign) content.
	Created time.Time
}

// ParseMembership parses a KindCallMember event. Live uses S1's memberIsLive rule: base = created_ts if
// present else evt.Timestamp; expires defaults to DefaultMembershipExpiry. ok=false if evt is not a
// call member event, the state key does not parse, or the state key user differs from evt.Sender
// (wire §1.5: the sender is the member).
func ParseMembership(evt Event, now time.Time) (Membership, bool) {
	if evt.Kind != KindCallMember {
		return Membership{}, false
	}
	user, memberID, ok := ParseMemberStateKey(evt.StateKey)
	if !ok || user != evt.Sender {
		return Membership{}, false
	}
	m := Membership{
		UserID:   user,
		DeviceID: strings.TrimSuffix(memberID, "_"+ApplicationCall+CallIDRoom),
	}
	var c map[string]json.RawMessage
	if err := json.Unmarshal(evt.Content, &c); err != nil || len(c) == 0 {
		return m, true // empty or unparseable content means "left"
	}
	if dev, ok := rawString(c[FieldDeviceID]); ok && dev != "" {
		m.DeviceID = dev
	}
	if app, _ := rawString(c[FieldApplication]); app != ApplicationCall {
		return m, true
	}
	if cid, present := c[FieldCallID]; present {
		if v, ok := rawString(cid); !ok || v != CallIDRoom {
			return m, true // another slot (non-empty call_id) is not the room call
		}
	}
	if raw, present := c[FieldScope]; present {
		if sc, ok := rawString(raw); !ok || sc != ScopeRoom {
			return m, true
		}
	}
	var base int64
	if !evt.Timestamp.IsZero() {
		base = evt.Timestamp.UnixMilli()
	}
	if v, ok := rawInt(c[FieldCreatedTS]); ok {
		base = v
	}
	exp := DefaultMembershipExpiry.Milliseconds()
	if v, ok := rawInt(c[FieldExpires]); ok {
		exp = v
	}
	m.Live = base+exp > now.UnixMilli()
	if base != 0 {
		m.Created = time.UnixMilli(base)
	}
	return m, true
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func rawInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	if f, err := n.Float64(); err == nil {
		return int64(f), true
	}
	return 0, false
}

// Notification is a parsed MSC4075 rtc.notification (wire §2.2).
type Notification struct {
	Type        string        // notification_type ("ring", "notification", or "" if absent)
	SenderTS    time.Time     // sender_ts, zero if absent
	Lifetime    time.Duration // lifetime (ms), 0 if absent
	Reference   id.EventID    // m.relates_to.event_id when rel_type is m.reference (or absent), "" otherwise
	Mentions    []id.UserID   // m.mentions.user_ids
	RoomMention bool          // m.mentions.room
}

// ParseNotification parses a KindRTCNotification event. ok=false for any other kind or unparsable
// content. Never fails on missing optional fields.
func ParseNotification(evt Event) (Notification, bool) {
	if evt.Kind != KindRTCNotification {
		return Notification{}, false
	}
	var c struct {
		Type     string          `json:"notification_type"`
		SenderTS json.RawMessage `json:"sender_ts"`
		Lifetime json.RawMessage `json:"lifetime"`
		Mentions struct {
			UserIDs []string `json:"user_ids"`
			Room    bool     `json:"room"`
		} `json:"m.mentions"`
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if json.Unmarshal(evt.Content, &c) != nil {
		return Notification{}, false
	}
	n := Notification{Type: c.Type, RoomMention: c.Mentions.Room}
	if v, ok := rawInt(c.SenderTS); ok && v > 0 {
		n.SenderTS = time.UnixMilli(v)
	}
	if v, ok := rawInt(c.Lifetime); ok && v > 0 {
		n.Lifetime = time.Duration(v) * time.Millisecond
	}
	if c.RelatesTo.RelType == "" || c.RelatesTo.RelType == RelTypeReference {
		n.Reference = id.EventID(c.RelatesTo.EventID)
	}
	for _, u := range c.Mentions.UserIDs {
		n.Mentions = append(n.Mentions, id.UserID(u))
	}
	return n, true
}

// DeclineContent returns the MSC4310 decline content referencing notificationID (wire §3.1):
// {"m.relates_to": {"rel_type": "m.reference", "event_id": notificationID}}. Sent as RTCDecline
// (unstable name), never the alias.
func DeclineContent(notificationID id.EventID) map[string]any {
	return map[string]any{
		FieldRelatesTo: map[string]any{
			FieldRelType: RelTypeReference,
			FieldEventID: string(notificationID),
		},
	}
}

// DeclineTarget returns the m.reference event_id of a KindRTCDecline event. ok=false otherwise.
func DeclineTarget(evt Event) (id.EventID, bool) {
	if evt.Kind != KindRTCDecline {
		return "", false
	}
	var c struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if json.Unmarshal(evt.Content, &c) != nil || c.RelatesTo.EventID == "" {
		return "", false
	}
	if c.RelatesTo.RelType != "" && c.RelatesTo.RelType != RelTypeReference {
		return "", false
	}
	return id.EventID(c.RelatesTo.EventID), true
}

// ParseRTCFoci extracts the first livekit_service_url from a .well-known/matrix/client body
// (WellKnownRTCFociKey array, first entry with type "livekit" and a non-empty livekit_service_url,
// returned byte-for-byte: Element Call keys connections by that string). ok=false if absent or malformed.
func ParseRTCFoci(wellKnown []byte) (serviceURL string, ok bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(wellKnown, &body) != nil {
		return "", false
	}
	var foci []struct {
		Type       string `json:"type"`
		ServiceURL string `json:"livekit_service_url"`
	}
	if json.Unmarshal(body[WellKnownRTCFociKey], &foci) != nil {
		return "", false
	}
	for _, f := range foci {
		if f.Type == FocusTypeLiveKit && f.ServiceURL != "" {
			return f.ServiceURL, true
		}
	}
	return "", false
}
