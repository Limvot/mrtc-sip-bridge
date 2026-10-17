package matrixrtc

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

const (
	ghost  = id.UserID("@sip_15551234567:example.org")
	owner  = id.UserID("@owner:example.org")
	device = "SIPAB12CD34"
)

var testNow = time.UnixMilli(1792160000000)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMemberStateKey(t *testing.T) {
	if got, want := MemberStateKey(ghost, device), "_@sip_15551234567:example.org_SIPAB12CD34_m.call"; got != want {
		t.Fatalf("MemberStateKey = %q, want %q", got, want)
	}
}

func TestParseMemberStateKey(t *testing.T) {
	tests := []struct {
		name, key string
		user      id.UserID
		member    string
		ok        bool
	}{
		{"round trip", MemberStateKey(ghost, device), ghost, device + "_m.call", true},
		{"underscores in localpart", "_@a_b_c:example.org_DEV_m.call", "@a_b_c:example.org", "DEV_m.call", true},
		{"no leading underscore", "@owner:example.org_DEV_m.call", owner, "DEV_m.call", true},
		{"server with port", "_@u:example.org:8448_DEV_m.call", "@u:example.org:8448", "DEV_m.call", true},
		{"garbage", "garbage", "", "", false},
		{"no colon", "_foo_bar", "", "", false},
		{"no underscore after colon", "_@u:example.org", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, m, ok := ParseMemberStateKey(tt.key)
			if ok != tt.ok || u != tt.user || m != tt.member {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", u, m, ok, tt.user, tt.member, tt.ok)
			}
		})
	}
}

func TestJoinContentGolden(t *testing.T) {
	c := JoinContent(JoinParams{
		UserID:     ghost,
		DeviceID:   device,
		RoomID:     "!portalroom:example.org",
		ServiceURL: "https://matrix-rtc.example.org/livekit/jwt",
		CreatedTS:  testNow,
		Expires:    DefaultMembershipExpiry,
	})
	want := `{
	  "application": "m.call",
	  "call_id": "",
	  "scope": "m.room",
	  "device_id": "SIPAB12CD34",
	  "membershipID": "@sip_15551234567:example.org:SIPAB12CD34",
	  "created_ts": 1792160000000,
	  "expires": 14400000,
	  "m.call.intent": "audio",
	  "focus_active": { "type": "livekit", "focus_selection": "multi_sfu" },
	  "foci_preferred": [
	    {
	      "type": "livekit",
	      "livekit_service_url": "https://matrix-rtc.example.org/livekit/jwt",
	      "livekit_alias": "!portalroom:example.org"
	    }
	  ]
	}`
	assertJSONEqual(t, mustJSON(t, c), want)
	if _, has := c["url"]; has {
		t.Error("content has a url key")
	}
	if f := c[FieldFociPreferred].([]any)[0].(map[string]any); f["url"] != nil {
		t.Error("focus has a url key")
	}
}

func TestLeaveContent(t *testing.T) {
	if got := mustJSON(t, LeaveContent()); got != "{}" {
		t.Fatalf("LeaveContent = %s", got)
	}
}

func TestRingContentGolden(t *testing.T) {
	c := RingContent(RingParams{
		Owner:             "@owner:example.org",
		MembershipEventID: "$ghostMembershipEventId",
		Lifetime:          30 * time.Second,
		SenderTS:          time.UnixMilli(1792160000123),
	})
	want := `{
	  "notification_type": "ring",
	  "sender_ts": 1792160000123,
	  "lifetime": 30000,
	  "m.mentions": { "user_ids": ["@owner:example.org"] },
	  "m.relates_to": { "rel_type": "m.reference", "event_id": "$ghostMembershipEventId" },
	  "m.call.intent": "audio",
	  "slot_id": "m.call#ROOM"
	}`
	assertJSONEqual(t, mustJSON(t, c), want)
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("json mismatch\n got: %s\nwant: %s", gb, wb)
	}
}

func memberEvent(sender id.UserID, stateKey, content string, ts time.Time) Event {
	return Event{
		Kind: KindCallMember, Type: CallMember.Type, RoomID: "!r:example.org", EventID: "$e",
		Sender: sender, StateKey: stateKey, IsState: true, Timestamp: ts, Content: json.RawMessage(content),
	}
}

func TestParseMembership(t *testing.T) {
	key := MemberStateKey(owner, "OWNERDEV")
	live := fmt.Sprintf(`{"application":"m.call","scope":"m.room","device_id":"OWNERDEV","created_ts":%d,"expires":14400000}`,
		testNow.Add(-time.Minute).UnixMilli())
	tests := []struct {
		name     string
		evt      Event
		ok, live bool
		device   string
	}{
		{"live", memberEvent(owner, key, live, testNow), true, true, "OWNERDEV"},
		{"empty means left", memberEvent(owner, key, `{}`, testNow), true, false, "OWNERDEV"},
		{"expired", memberEvent(owner, key,
			fmt.Sprintf(`{"application":"m.call","scope":"m.room","created_ts":%d,"expires":1000}`, testNow.Add(-time.Hour).UnixMilli()),
			testNow), true, false, "OWNERDEV"},
		{"wrong application", memberEvent(owner, key, `{"application":"m.other","scope":"m.room","expires":14400000}`, testNow), true, false, "OWNERDEV"},
		{"non-empty call_id is another slot", memberEvent(owner, key, `{"application":"m.call","call_id":"other","scope":"m.room","expires":14400000}`, testNow), true, false, "OWNERDEV"},
		{"explicit empty call_id is live", memberEvent(owner, key, `{"application":"m.call","call_id":"","scope":"m.room","expires":14400000}`, testNow), true, true, "OWNERDEV"},
		{"scope m.user", memberEvent(owner, key, `{"application":"m.call","scope":"m.user","expires":14400000}`, testNow), true, false, "OWNERDEV"},
		{"missing scope is live", memberEvent(owner, key, `{"application":"m.call","expires":14400000}`, testNow), true, true, "OWNERDEV"},
		{"missing created_ts uses event timestamp (fresh)", memberEvent(owner, key,
			`{"application":"m.call","scope":"m.room","expires":60000}`, testNow.Add(-30*time.Second)), true, true, "OWNERDEV"},
		{"missing created_ts uses event timestamp (stale)", memberEvent(owner, key,
			`{"application":"m.call","scope":"m.room","expires":60000}`, testNow.Add(-2*time.Minute)), true, false, "OWNERDEV"},
		{"missing expires defaults to 4h", memberEvent(owner, key,
			`{"application":"m.call","scope":"m.room"}`, testNow.Add(-3*time.Hour)), true, true, "OWNERDEV"},
		{"sender differs from state key user", memberEvent(ghost, key, live, testNow), false, false, ""},
		{"bad state key", memberEvent(owner, "garbage", live, testNow), false, false, ""},
		{"not a member event", Event{Kind: KindRTCNotification, Sender: owner, StateKey: key, Content: json.RawMessage(live)}, false, false, ""},
		{"alias type accepted", func() Event {
			e := memberEvent(owner, key, live, testNow)
			e.Type = CallMemberAlias.Type
			e.Kind = KindOf(CallMemberAlias)
			return e
		}(), true, true, "OWNERDEV"},
		{"device from content", memberEvent(owner, key,
			`{"application":"m.call","scope":"m.room","device_id":"FROMCONTENT","expires":14400000}`, testNow), true, true, "FROMCONTENT"},
		{"device from state key", memberEvent(owner, key,
			`{"application":"m.call","scope":"m.room","expires":14400000}`, testNow), true, true, "OWNERDEV"},
		{"invalid json is left", memberEvent(owner, key, `not json`, testNow), true, false, "OWNERDEV"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := ParseMembership(tt.evt, testNow)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if m.Live != tt.live || m.DeviceID != tt.device || m.UserID != owner {
				t.Fatalf("got %+v, want live=%v device=%q", m, tt.live, tt.device)
			}
		})
	}
}

func TestDeclineTarget(t *testing.T) {
	good := `{"m.relates_to":{"rel_type":"m.reference","event_id":"$ring"}}`
	for _, typ := range []string{RTCDecline.Type, RTCDeclineAlias.Type} {
		e := Event{Kind: KindRTCDecline, Type: typ, Content: json.RawMessage(good)}
		if got, ok := DeclineTarget(e); !ok || got != "$ring" {
			t.Errorf("%s: got (%q, %v)", typ, got, ok)
		}
	}
	for name, e := range map[string]Event{
		"missing relates_to": {Kind: KindRTCDecline, Content: json.RawMessage(`{}`)},
		"wrong kind":         {Kind: KindRTCNotification, Content: json.RawMessage(good)},
		"bad json":           {Kind: KindRTCDecline, Content: json.RawMessage(`x`)},
		"other rel type":     {Kind: KindRTCDecline, Content: json.RawMessage(`{"m.relates_to":{"rel_type":"m.replace","event_id":"$x"}}`)},
	} {
		if got, ok := DeclineTarget(e); ok {
			t.Errorf("%s: unexpected ok (%q)", name, got)
		}
	}
}

func TestParseRTCFoci(t *testing.T) {
	tests := []struct {
		name, body, want string
		ok               bool
	}{
		{"wire 8.2 example", `{"m.homeserver":{"base_url":"https://matrix.example.org"},"org.matrix.msc4143.rtc_foci":[{"type":"livekit","livekit_service_url":"https://matrix-rtc.example.org/livekit/jwt"}]}`,
			"https://matrix-rtc.example.org/livekit/jwt", true},
		{"other type first", `{"org.matrix.msc4143.rtc_foci":[{"type":"other","livekit_service_url":"https://x"},{"type":"livekit","livekit_service_url":"https://lk.example.org/jwt/"}]}`,
			"https://lk.example.org/jwt/", true},
		{"livekit without url skipped", `{"org.matrix.msc4143.rtc_foci":[{"type":"livekit"},{"type":"livekit","livekit_service_url":"https://b"}]}`, "https://b", true},
		{"empty list", `{"org.matrix.msc4143.rtc_foci":[]}`, "", false},
		{"missing key", `{"m.homeserver":{}}`, "", false},
		{"invalid json", `{`, "", false},
		{"wrong shape", `{"org.matrix.msc4143.rtc_foci":"x"}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRTCFoci([]byte(tt.body))
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestJoinContentKeepsServiceURLVerbatim(t *testing.T) {
	const raw = "https://lk.example.org/jwt/"
	c := JoinContent(JoinParams{UserID: ghost, DeviceID: "DEV", RoomID: "!r:example.org", ServiceURL: raw})
	foci := c[FieldFociPreferred].([]any)
	got := foci[0].(map[string]any)[FieldLiveKitServiceURL]
	if got != raw {
		t.Fatalf("livekit_service_url = %v, want %q", got, raw)
	}
}

func TestParseMembershipCreated(t *testing.T) {
	key := MemberStateKey(owner, "OWNERDEV")
	ts := testNow.Add(-time.Minute)
	m, ok := ParseMembership(memberEvent(owner, key,
		fmt.Sprintf(`{"application":"m.call","scope":"m.room","created_ts":%d,"expires":14400000}`, ts.UnixMilli()), testNow), testNow)
	if !ok || !m.Created.Equal(ts) {
		t.Fatalf("Created from created_ts: got (%v, %v), want %v", m.Created, ok, ts)
	}
	evTS := testNow.Add(-2 * time.Minute)
	m, ok = ParseMembership(memberEvent(owner, key, `{"application":"m.call","scope":"m.room","expires":14400000}`, evTS), testNow)
	if !ok || !m.Created.Equal(evTS) {
		t.Fatalf("Created from event timestamp: got (%v, %v), want %v", m.Created, ok, evTS)
	}
}

func notifEvent(kind EventKind, content string) Event {
	return Event{Kind: kind, Sender: owner, RoomID: "!r:example.org", Timestamp: testNow, Content: json.RawMessage(content)}
}

func TestParseNotification(t *testing.T) {
	t.Run("wire 2.2 ring", func(t *testing.T) {
		c := RingContent(RingParams{Owner: owner, MembershipEventID: "$m", Lifetime: 30 * time.Second, SenderTS: testNow})
		n, ok := ParseNotification(notifEvent(KindRTCNotification, mustJSON(t, c)))
		if !ok || n.Type != NotificationTypeRing || !n.SenderTS.Equal(testNow) || n.Lifetime != 30*time.Second ||
			n.Reference != "$m" || len(n.Mentions) != 1 || n.Mentions[0] != owner || n.RoomMention {
			t.Fatalf("got (%+v, %v)", n, ok)
		}
	})
	t.Run("element call style", func(t *testing.T) {
		n, ok := ParseNotification(notifEvent(KindRTCNotification,
			`{"notification_type":"ring","m.mentions":{"user_ids":[],"room":true},"m.relates_to":{"rel_type":"m.reference","event_id":"$x"},"sender_ts":1792160000000,"lifetime":30000}`))
		if !ok || n.Type != "ring" || !n.RoomMention || len(n.Mentions) != 0 || n.Reference != "$x" {
			t.Fatalf("got (%+v, %v)", n, ok)
		}
	})
	t.Run("missing fields", func(t *testing.T) {
		n, ok := ParseNotification(notifEvent(KindRTCNotification, `{}`))
		if !ok || n.Type != "" || !n.SenderTS.IsZero() || n.Lifetime != 0 || n.Reference != "" || n.Mentions != nil {
			t.Fatalf("got (%+v, %v)", n, ok)
		}
	})
	t.Run("foreign rel_type", func(t *testing.T) {
		n, ok := ParseNotification(notifEvent(KindRTCNotification, `{"m.relates_to":{"rel_type":"m.thread","event_id":"$x"}}`))
		if !ok || n.Reference != "" {
			t.Fatalf("got (%+v, %v)", n, ok)
		}
	})
	t.Run("wrong kind", func(t *testing.T) {
		if _, ok := ParseNotification(notifEvent(KindCallMember, `{}`)); ok {
			t.Fatal("ok for wrong kind")
		}
	})
	t.Run("unparsable", func(t *testing.T) {
		if _, ok := ParseNotification(notifEvent(KindRTCNotification, `[`)); ok {
			t.Fatal("ok for invalid JSON")
		}
	})
}

func TestDeclineContentGolden(t *testing.T) {
	got := mustJSON(t, DeclineContent("$n:example.org"))
	const want = `{"m.relates_to":{"event_id":"$n:example.org","rel_type":"m.reference"}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	target, ok := DeclineTarget(Event{Kind: KindRTCDecline, Content: json.RawMessage(got)})
	if !ok || target != "$n:example.org" {
		t.Fatalf("DeclineTarget round trip: (%q, %v)", target, ok)
	}
}
