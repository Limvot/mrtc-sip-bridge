# S3: receiving non-routed Matrix events in a bridgev2 bridge

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Checked against maunium.net/go/mautrix v0.32.0 (`~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0`, paths below relative to it).
Result: **works**. Register extra handlers on `*matrix.Connector.EventProcessor`. No fork needed.

## 1. Dispatch path
- Transactions: `appservice/http.go:148` `handleEvents(txn.Events, UnknownEventType)`. For events with a state key it sets
  `Type.Class = StateEventType` (`http.go:198-200`), otherwise `MessageEventType` (`:202`). Nothing filters by type.
- `ParseRaw` on an unregistered type returns `ErrUnsupportedContentType` (`event/content.go:222-224`); `http.go:206-208` logs at
  debug and carries on. `Content.Parsed` stays nil; use `Content.VeryRaw` / `Content.Raw`.
- `appservice/eventprocessor.go:128-132` `Dispatch` looks up `ep.handlers[evt.Type]` (exact struct incl. Class) and returns silently if
  none. `On` (`:63`) and `PrependHandler` (`:73`) accept any type; no whitelist.
- bridgev2 only registers a fixed list (`bridgev2/matrix/connector.go:126-163`: message, sticker, reaction, redaction, member,
  power levels, name/avatar/topic/tombstone, etc.). `call.member`, `rtc.decline`, `rtc.notification` are simply unregistered, so
  `Dispatch` drops them. Our own `On` calls add to the map, so both coexist.
- `handlers` is a plain map with no lock: **register before `EventProcessor.Start`** (`connector.go:229`). Registering in the
  connector's `Start` is too late if Start runs after the matrix connector's Start; use `PostInit` (runs after `Init`, before start:
  `mxmain/main.go:270`) or the connector's `Init`/`Start` (the network connector `Start` runs from `Bridge.StartConnectors`, which in
  v0.32 is after `Matrix.Start`; lhns does it in connector `Start`, working in practice, but PostInit is the safe place).
- Handler type: `func(ctx context.Context, evt *event.Event)`.
- Permission/ghost filtering lives only in bridgev2's own `handleRoomEvent` (`bridgev2/matrix/matrix.go:26-53`, `shouldIgnoreEvent`
  `:195`: drops events from bridge bot and ghosts). Our raw handler bypasses it: filter ghosts yourself
  (`br.Bridge.IsGhostMXID(evt.Sender)`), and resolve rooms via `br.Bridge.GetPortalByMXID`.
- Encrypted rooms: decrypted events re-enter via `postDecrypt` -> `EventProcessor.Dispatch` (`matrix.go`), so same handlers fire (moot
  for state events).

## 2. Does the appservice receive them?
Yes. The homeserver sends every event in rooms where an appservice user (bot or ghost) is joined (plus namespace-matching
events). No type filtering in mautrix. Needs the room to contain a ghost/bot member, which a portal room does.

## 3. lhns/matrix-sip-bridge (Apache-2.0)
- `pkg/connector/connector.go` `eventRegistrar()` (~l.210-222): `sc.br.Matrix.(*matrix.Connector)`; on type-assert failure returns
  `noopRegistrar{}`, else returns `mx.EventProcessor`.
- `pkg/calls/subsystem.go` `Start` (~l.255-264): `reg.On(CallMemberEventType, s.handleCallMember)`,
  `reg.On(RtcDeclineEventType, ...)`, `reg.On(RtcDeclineStableEventType, ...)`, with a comment about exact `event.Type` incl. Class.
- `pkg/calls/notification.go`: types `m.rtc.notification`, `org.matrix.msc4310.rtc.decline`, `m.rtc.decline` defined with
  `Class: event.MessageEventType`; content built as raw JSON.
- `docs/adr/0006-observing-inbound-rtc-membership.md` documents the same conclusion (checked there on v0.30.0).
- `EventRegistrar` interface = `On(event.Type, func(ctx, *event.Event))` keeps `bridgev2/matrix` out of the core package.

## 4. Sending / OpenID as a ghost
- `br.Bridge.Matrix.GhostIntent(userID)` returns `bridgev2.MatrixAPI` (`connector.go:510`) wrapping `*ASIntent{Matrix *appservice.IntentAPI}`.
  `MatrixAPI.SendState(ctx, roomID, eventType, stateKey, *event.Content, ts)` (`bridgev2/matrixinterface.go:204`, impl
  `bridgev2/matrix/intent.go:128-132`) accepts arbitrary types; use `&event.Content{Raw: map[string]any{...}}`. `SendMessage` likewise
  via `event.Type` custom with raw content.
- For OpenID, drop to the raw intent: `mx.AS.Intent(ghostMXID).RequestOpenIDToken(ctx)` (`client.go:1995`, POST
  `/_matrix/client/v3/user/{userID}/openid/request_token`, appservice masquerade via `user_id` query param is set by IntentAPI).
  `br.Matrix.FormatGhostMXID(networkid.UserID)` yields the MXID. Alternatively type-assert `GhostIntent(...).(*matrix.ASIntent).Matrix`.
  Result `*mautrix.RespOpenIDToken` goes to lk-jwt-service. (Not exercised against a live homeserver in this spike.)

## 5. mautrix-jabber (Phase J)
`cmd/mautrix-jabber/main.go` uses `mxmain.BridgeMain` with `br.PostInit = func(){...}` (already uses `br.Matrix.AS.HTTPClient`),
and `br.Matrix` is `*matrix.Connector` (`mxmain/main.go:88`). Same hook works there, no type assertion needed.

## Recommended sketch
```go
var (
    CallMember = event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}
    RTCDecline = event.Type{Type: "org.matrix.msc4310.rtc.decline", Class: event.MessageEventType}
    RTCNotify  = event.Type{Type: "org.matrix.msc4075.rtc.notification", Class: event.MessageEventType}
    // m.rtc.member / m.rtc.decline / m.rtc.notification: register the stable names too (state vs message class as per spec).
)

type Registrar interface{ On(event.Type, appservice.EventHandler) }

br.PostInit = func() { // before EventProcessor.Start
    var reg Registrar = br.Matrix.EventProcessor
    for _, t := range []event.Type{CallMember, RTCDecline, RTCNotify} {
        reg.On(t, func(ctx context.Context, evt *event.Event) {
            if br.Bridge.IsGhostMXID(evt.Sender) || evt.Sender == br.Matrix.Bot.UserID { return }
            portal, _ := br.Bridge.GetPortalByMXID(ctx, evt.RoomID)
            if portal == nil { return }
            raw := evt.Content.VeryRaw // Parsed is nil for these types
            _ = raw
        })
    }
}
// Standalone connector w/o mxmain: sc.br.Matrix.(*matrix.Connector).EventProcessor, noop fallback as in lhns.

// Ghost send + OpenID
intent := br.Matrix.AS.Intent(br.Matrix.FormatGhostMXID(ghostID))
_, _ = intent.SendStateEvent(ctx, roomID, CallMember, string(intent.UserID)+"_"+deviceID, rawContent)
tok, _ := intent.RequestOpenIDToken(ctx)
```

## Caveats
- Class matters: a type registered with the wrong Class never matches. Unknown whether Element sends `rtc.notification` /
  `rtc.decline` as message events (assumed; lhns uses MessageEventType).
- `Sync` exec mode (default when `async_transactions` false) runs handlers inline in the transaction path: do not block; spawn goroutines.
- Portal power levels must let real users send `call.member` state (state_default / events override) or the homeserver rejects it.
