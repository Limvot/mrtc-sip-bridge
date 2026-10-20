# Phase 3 implementation spec: outbound calls

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Status: **ready for implementation** (written 2026-10-17, branch `phase3-outbound`).
Audience: implementer agents. Follow it literally. Where this spec and your own taste differ, this spec
wins. If something here does not compile against the pinned versions, stop and report it rather than
redesigning.

Inputs: `docs/PLAN.md` (Phase 3, ADR-008), `docs/DECISIONS.md`, `docs/PHASE2-SPEC.md` (cited as "P2 §N";
this spec extends that architecture and keeps every rule of it that is not explicitly changed here,
including its §16 deviations), live-test findings (the owner client's own
`rtc_notification` when the owner starts a call; client quirks),
`docs/TELEMETRY-SPEC.md` (cited as "TS §N"), `docs/spikes/S1-matrixrtc-wire-spec.md` ("wire §N"),
`docs/spikes/S2-sip-ua-design.md`, `spikes/s2-sip/main.go` (`outbound`), and the Phase 2 code.

Library facts were checked against the module cache (line numbers refer to these versions):
`~/go/pkg/mod/github.com/emiago/diago@v0.41.0` (`diago.go`, `dialog_client_session.go`,
`dialog_media.go`, `dialog_server_session.go`, `digest_auth.go`, `media/rtp_packet_writer.go`,
`media/rtp_dtmf*.go`), `.../emiago/sipgo@v1.6.0` (`dialog_client.go`),
`~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0` (`bridgev2/queue.go`, `bridgev2/commands/*`).
LiveKit (`server-sdk-go/v2` v2.18.1) needs no new API in this phase.

---

## 0. Scope

In scope:

1. **Owner-initiated call** in an existing portal room: the owner presses the call button in a portal
   (their client publishes a live `call.member` and an MSC4075 notification). The bridge publishes the
   ghost membership, joins LiveKit as the ghost, then places a SIP INVITE (digest auth, From = SIP
   username). Ringback: relay 183 early media, otherwise a generated ringback tone. 200 OK → ACK →
   media bridge. Failure → busy/reorder tone, decline of the owner's notification, notice with the
   reason, ghost leaves.
2. **`!sip call <number>`** bot command (management room, or in a portal room without argument): the
   portal is created or found, the ghost **rings the owner's devices** (exactly like an inbound call),
   and when the owner answers the bridge dials ("callback ring", D6). The `start-chat` path
   (`ResolveIdentifier` / `CreateChatWithGhost`, already implemented) creates the portal; the owner then
   presses call (path 1).
3. **Teardown both directions** (owner leaves → CANCEL before answer, BYE after; remote BYE → leave +
   clear membership), owner cancel while dialing, CANCEL/200 OK race, `!sip hangup`, one active call per
   login across directions (busy either way).
4. **DTMF out** (RFC 4733 telephone-event) with `!sip dtmf <digits>` in the portal or management room
   while a call is connected (outbound **and** inbound calls). Digits are never logged or traced.
5. Telemetry per TS §5.7, §5.8 and §8.2 Phase 3; notices ("Outgoing call · 1m 20s", "Call failed:
   486 Busy Here", ...).
6. Small hardening that falls out of the work: brand-new portal ring delay for Element X (D18), the
   pinned registrar IP also used for INVITE (D9), the sipgo 10-response limit (D12).

Out of scope (do **not** implement): PSTN third-party protocol lookup (ADR-008 0b), dialpad widgets,
in-band DTMF fallback, receiving DTMF, re-INVITE / hold / session timers (Phase 4), concurrent calls,
persistence, call transfer, video, outbound calls through ATAs (ADR-007), plain-digit messages as DTMF
(§15 Q4), embedded (mautrix-jabber) wiring beyond the `CallHost` contract change.

---

## 1. Decisions (and why)

| # | Decision | Why |
|---|---|---|
| D1 | **Same per-call actor** (`pkg/calls.call`) with a `direction` field and new states `stJoining`, `stDialing`, `stEarly`. Inbound and outbound share `stRinging` (owner ring, command path only), `stActive`, `stEnding`, the answered-call teardown, owner/LiveKit presence tracking, stats, membership refresh and `call.session`. Setup and the unanswered failure branch get outbound variants in new files. | The answered phase (P2 §8.5 Active rows, §8.7) is ~60 % of the actor and identical for both directions. A second actor type would duplicate it and its tests. Inbound-only SIP commands move behind `c.in` (inbound) / `c.out` (outbound) fields; the shared code calls `c.sipHangup()` and reads `c.sipEvents`. |
| D2 | `pkg/sipua` gains `UA.Dial(DialRequest) (OutboundCall, error)`. `OutboundCall` mirrors `InboundCall`: non-blocking `Hangup()`, ordered `Events()` reusing `CallEvent` with two new kinds (`EventProvisional`, `EventAuthChallenged`) and two new causes (`EndLocalCancel`, `EndRemoteRejected`). No diago/sipgo type is exported. The CANCEL-vs-200 race, ACK, BYE and digest auth are resolved entirely inside sipua. | Same model the actor already consumes (P2 D7). sipua is the only place that can see the raw 200 that raced a CANCEL (`sipgo dialog_client.go:337-390` stores it in `InviteResponse` and returns an error without ACKing). |
| D3 | Outbound INVITE uses diago's low-level `NewDialog` + `DialogClientSession.Invite(ctx, InviteClientOptions{Username, Password, EarlyMediaDetect: true, OnResponse})`, then `WaitAnswer` after `ErrClientEarlyMedia`, then `Ack`. Digest 401/407 is handled by sipgo (`dialog_client.go:281-321`, `maxAuthAttempts = 2`). | Verified in source: `Invite` returns `(med, ErrClientEarlyMedia)` on 183+SDP with the RTP session already set up (`dialog_client_session.go:298-344`), and the final 200 re-applies the SDP to the **same** RTP session (`applyRemoteSDP` → `checkEarlyMedia`, `dialog_media.go:249`), so one `media.SIPLeg` serves early media and the call. The high-level `diago.Invite` has no early-media hook and ACKs immediately. |
| D4 | **Join before dial**: the ghost membership is published and LiveKit joined **before** the INVITE is sent (owner-initiated path); on the command path the pre-join runs while the owner's devices ring (as inbound). If LiveKit cannot be joined, nothing is dialled. | No PSTN party is ever called into dead air; ringback/early media is audible from the first provisional; a fast answer (IVR) loses no audio. Cost: ~0.2-2 s before the INVITE (S1: 0.15 s warm, 2.2 s cold), invisible because the owner already hears nothing until 180. |
| D5 | **Ghost membership is published immediately on trigger** (not at 200 OK), and the ghost plays ringback into LiveKit. | The owner sees the ghost tile within ~1-2 s and hears call progress. Waiting for 200 OK was rejected: Element Call's caller-side waiting UI gives up after the owner's own notification `lifetime` if nobody joins (UNVERIFIED but likely, and PSTN rings often exceed 30 s), early media (IVR announcements, "number not in service") would be lost, and the first ~1 s of the answer would be clipped. |
| D6 | **`!sip call` = callback ring**: the bridge rings the owner's devices from the ghost (P2 setup steps 9-11: membership, MSC4075 ring, pre-join) and dials when the owner answers. Declining or letting it ring out places no call. | Best behaviour on Element X: it is the one flow Element X already does perfectly (full-screen ring, one tap opens the call with the ghost present, live-tested in Phase 2), it needs no navigation to a new room, and it works on every client that rings. Alternatives rejected: "reply and wait for the owner to press call" (Element X needs the user to find the new room, and the trigger depends on the client sending a notification); "dial first, ring owner in parallel" (the callee may answer into silence). |
| D7 | **Owner-initiated trigger** (default `outbound_trigger: ring`): an outbound call starts when, for a login with **no** active call, the **owner** sends an MSC4075 notification (`org.matrix.msc4075.rtc.notification` or alias, any `notification_type`) in a portal room, the event is fresh (origin ts within 30 s of now), and the owner has a **freshly joined** live `call.member` in that room (membership `created` within 60 s of the notification; if the notification arrives first, a fresh membership within 5 s completes it). Alternative `membership`: a fresh owner membership in a room where no owner device was live before. `off`: only `!sip call`. A 5 s per-room cooldown follows every call end and every refused trigger. | Dialling a phone number by mistake calls a real person, so false triggers matter more than missed ones. Element Call sends the notification only when the user **starts** a call as the first participant (never when joining an existing call, never on answer, never on membership refresh), so it separates "start a call" from "my other device joined", "Sable stayed in the call after hang-up" and "client re-published its membership". Freshness drops appservice backlog after a restart (P2 §16). The membership mode exists for clients that do not send notifications. |
| D8 | Destination policy: `phone.Normalize(_, default_country_code)` must yield E.164 (short codes and N11, including 911, are refused by `Normalize`: `ErrShortCode`); the number must start with one of `network.dial_allowed_prefixes` (default `["+1"]`; empty list = any E.164); the login's own number is refused unless `network.allow_self_dial: true` (§12.3). | Toll and emergency safety for a bridge that dials from Matrix events. Emergency calls must use a real phone; JMP's own guidance is the same. International from NANP via Bandwidth is unverified (format and cost). |
| D9 | INVITE addressing: Request-URI `sip:<user>@<AOR host>` (AOR host = `Domain` or registrar host, no port), `user` = E.164 digits without `+` (`network.dial_number_format: digits`, default) or with `+` (`e164`); **destination = the registrar IP currently pinned by the register loop** (`SetDestination`), sent from the UA's bound SIP socket. From = sipgo default `<sip:<SIP username>@<AOR host>>` (the UA name is the username, `diago.go:713`). | JMP/Bandwidth requires the SIP username as caller ID and swaps in the real number (RESEARCH, Marier 2021 config: `CALLERID(all)=<username>`); the same Asterisk config dials 11-digit `1NXXNXXXXXX` (verified 2021, hence `digits` default; `e164` is UNVERIFIED). Using the pinned IP and the same socket keeps the one NAT mapping S2 proved stable. |
| D10 | Ringback: if a 180/181/182/183-without-SDP arrives and no early media is flowing, `pkg/media` plays a generated **US ringback** (440+480 Hz, 2 s on / 4 s off) into LiveKit. On 183 with SDP the media bridge starts in **early mode**: SIP audio is relayed to LiveKit; while no SIP RTP has arrived for 1 s (or yet), ringback fills in; owner audio is **not** sent to the carrier (µ-law silence keeps the NAT pinhole open). On 200 OK the bridge leaves early mode. | Carriers often send 183 with SDP and then nothing for a while; the owner must never hear dead silence. Not leaking the owner's audio before answer avoids surprises with IVR-like early media. Starting RTP toward the carrier at 183 opens the NAT pinhole before answer (S2 §3.7). |
| D11 | Failure audio: on a remote final rejection or dial timeout while the LiveKit session is up, the ghost plays **busy** (480+620 Hz, 0.5 s on/off; for 486/600) or **reorder** (480+620 Hz, 0.25 s on/off; everything else) for 2 s before leaving. Then the ghost sends an MSC4310 **decline** referencing the owner's notification (owner-initiated path) and clears its membership. | Phone-like feedback, and a decline lets Element Call end the caller's call screen instead of leaving the owner alone in an empty call (UNVERIFIED that Element Call/Element X react to a callee decline on the caller side; §12 checks it). |
| D12 | sipgo `WaitAnswer` returns `"more than 10 responses received"` after 11 responses (`dialog_client.go:246`). sipua re-enters `WaitAnswer` on that error (at most 10 times) instead of failing the call. | Long PSTN rings with repeated 180/183 would otherwise fail a ringing call. |
| D13 | DTMF: `pkg/media` sends RFC 4733 events itself on the rtcToSIP tick (no diago `DTMFWriter`, which sleeps on its own ticker and writes the same RTP writer from another goroutine). Per digit: 5 event packets (duration 160..800, marker on the first) + 3 end packets (duration 800), all with the digit's start timestamp, then the RTP timestamp advances by 160 × 8; then 5 ticks (100 ms) of normal audio before the next digit. Payload type = the negotiated `telephone-event/8000` PT. Supported only when telephone-event was negotiated (JMP offers PT 101); otherwise the command answers "DTMF is not supported on this call". | One writer per RTP stream (the rtcToSIP goroutine) keeps sequence numbers and timestamps monotonic. 100 ms tone / 100 ms gap is accepted by every IVR. In-band fallback is out of scope. |
| D14 | `!sip dtmf` command messages are **redacted by the bridge bot** after the digits are queued (best effort, `network.dtmf_redact_command: true` default). Digits never appear in logs, spans, span events, errors or metrics; only counts. | DTMF is often a PIN. The redaction is cheap and the bot is room admin in portals; in the management room the bot is also admin. |
| D15 | Notices: answered `Outgoing call · <duration>`; owner cancelled before answer `Cancelled call`; remote rejection/timeout/error `Call failed: <reason>` where `<reason>` comes from a fixed table (§6.6), never from the remote reason phrase; command ring not answered/declined `Call not placed` variants. | Remote reason phrases are untrusted text; the table keeps notices stable and short. |
| D16 | `CallHost` gains `LookupCallRoom(ctx, login, roomID)` (existing portal room → `CallRoom`, `ErrNotCallRoom` for anything that is not a 1:1 number portal) and `CallRoom.Created` (the host just created the room). | The owner trigger knows only the room; the embedded host (mautrix-jabber) must be able to refuse group-MMS portals (PLAN Phase J). |
| D17 | One active call per login across directions: an outbound call occupies `Manager.active` from trigger to end exactly like an inbound call. Inbound INVITE during an outbound call → `486` + `Missed call (line busy)` (unchanged P2 D19). Owner trigger during any active call in another room → refused: decline of the owner's notification + `Call not placed: another call is in progress` (short-lived non-active actor, outcome `rejected`). `!sip call` during a call → command reply only (no actor). A notification/membership in the active call's own room is routed to that call (or ignored), never a new trigger. | Same mechanism as P2; a refused owner trigger still needs a decline so the owner's call screen ends. |
| D18 | When `ResolveCallRoom` reports `Created` on the command path, the ghost waits 3 s before publishing its membership and ring. | Element X treats a ring as a plain notification if it cannot see the call in the room within 3 s (wire §2.2); a brand-new room may not be synced yet. Cheap; UNVERIFIED that 3 s is enough. |
| D19 | New config keys only (§9.1): `outbound_trigger`, `dial_timeout_seconds` (60), `dial_allowed_prefixes` (`["+1"]`), `dial_number_format` (`digits`), `allow_self_dial` (false), `dtmf_redact_command` (true). | Small, each tied to an unverified provider or client behaviour. |
| D20 | Telemetry: `call.outbound` (CLIENT, linked root to the trigger span) ends at ACK sent or after the failure branch; `sip.invite` (CLIENT, child) from INVITE to final response with events `sip.response.received` and `sip.auth.challenge`; `call.ringback` (INTERNAL, child) from the first 18x to 200/failure with `sip.early_media`; `matrixrtc.decline.send` (CLIENT, child) is new; the command path adds `matrixrtc.ring.send` and `call.ring` as inbound. All spans are emitted by `pkg/calls` (sipua stays uninstrumented for calls, as in P2). TELEMETRY-SPEC edits are listed in §13. | Matches TS §5.7 with the additions the design needs. |

---

## 2. Package layout and file ownership

Tasks T1-T5 are defined in §14. Files marked **(API)** are written by T1 as exact contracts with stub
bodies; implementers must not change an API file's exported surface (stop and report instead).

```
pkg/matrixrtc/membership.go               T1  Membership.Created; ParseNotification; DeclineContent
pkg/matrixrtc/events.go                   T1  FieldMentionsRoom; NotificationTypeNotification
pkg/matrixrtc/membership_test.go          T1  (extend)
pkg/core/callhost.go                      T1  CallRoom.Created; CallHost.LookupCallRoom; ErrNotCallRoom
pkg/obs/attrs.go, pkg/obs/metrics.go      T1  new keys; DTMFSent instrument
pkg/obs/obs_test.go                       T1  (extend)
internal/obstest/obstest.go               T1  allowlist: mrtcsip.dtmf.sent
pkg/sipua/outbound.go           (API)     T1  DialRequest, NumberFormat, DialInfo, OutboundCall, ErrNotRegistered
pkg/sipua/inbound.go            (API)     T1  new CallEventKinds, EndCauses, CallEvent/AnswerInfo/Offer fields
pkg/sipua/diago.go                        T1  UA.Dial in the interface + stub method on diagoUA
pkg/media/media.go              (API)     T1  Config.Early, Bridge.Connect/SendDTMF, Stats.DTMFSent,
                                              SIPEventLeg, Tone API, DTMF errors (stubs)
pkg/calls/manager.go            (API part) T1 Config fields, PlaceCall/Hangup/SendDTMF signatures (stubs),
                                              exported errors
fakes that must keep compiling            T1  pkg/calls/fakes_test.go (fakeBridge, fakeHost stub methods),
                                              pkg/connector/client_lifecycle_test.go (fakeUA.Dial)
pkg/media/tone.go                         T2  tone generator (ringback, busy, reorder) + Tone player
pkg/media/dtmf.go                         T2  RFC 4733 event encoder and per-digit schedule
pkg/media/bridge.go                       T2  early mode, Connect, SendDTMF, DTMFSent
pkg/media/{tone,dtmf,bridge,bench}_test.go T2
pkg/sipua/outbound_loop.go                T3  per-call loop (diago-free, dialogClient seam)
pkg/sipua/outbound_diago.go               T3  diago adapter, R-URI/From/destination, answer parsing
pkg/sipua/diago.go                        T3  keep *diago.Diago on diagoUA; Dial; pinned destination atomic
pkg/sipua/inbound_diago.go                T3  diagoLeg implements media.SIPEventLeg; parseOffer PT
pkg/sipua/{outbound_loop,outbound_loopback,offer}_test.go  T3
pkg/calls/state.go, call.go, setup.go,
  owner.go, teardown.go, telemetry.go,
  notices.go, manager.go                  T4  refactor c.sip -> c.in/c.out, outbound states, busy rules
pkg/calls/outbound.go                     T4  outbound setup, dial, early/answer, failure branch
pkg/calls/detect.go                       T4  owner-trigger detector (manager side)
pkg/calls/commands.go                     T4  PlaceCall, Hangup, SendDTMF implementation
pkg/calls/{outbound,detect,commands}_test.go, fakes_test.go, telemetry_test.go,
  integration_test.go                     T4
pkg/connector/commands.go                 T5  !sip call / dtmf / hangup handlers (WithSpan)
cmd/mrtc-sip-bridge/main.go               T5  register the handlers
pkg/connector/callhost.go                 T5  LookupCallRoom, CallRoom.Created
pkg/connector/start.go, client.go         T5  calls.Config.Dial/LoginNumber/..., SIPClient.dial
pkg/connector/config.go, example-config.yaml, config_test.go   T5
pkg/connector/{commands,client_lifecycle}_test.go              T5
README.md, CLAUDE.md, docs/PLAN.md        T5
docs/TELEMETRY-SPEC.md                    (this spec's author; flagged edits §13, already applied)
```

No new module dependencies. `go.mod` must not change (T5 runs `go mod tidy` and verifies no diff other
than possibly go.sum ordering).

Acceptance commands everywhere (cgo and `pkg-config opus` required):

```bash
go build -tags "nocrypto nolibopusfile" ./...
go vet   -tags "nocrypto nolibopusfile" ./...
go test  -race -count=1 -tags "nocrypto nolibopusfile" ./...
```

---

## 3. Shared contracts (T1)

### 3.1 `pkg/matrixrtc`

`events.go` additions (the only file with MatrixRTC names):

```go
const (
	NotificationTypeNotification = "notification" // non-ringing notification (group rooms)
	FieldMentionsRoom            = "room"          // m.mentions.room
)
```

`membership.go` additions:

```go
// Membership gains:
//	Created time.Time // created_ts if present, else the event timestamp (the base used for Live)

// Notification is a parsed MSC4075 rtc.notification (wire §2.2).
type Notification struct {
	Type       string     // notification_type ("ring", "notification", or "" if absent)
	SenderTS   time.Time  // sender_ts, zero if absent
	Lifetime   time.Duration // lifetime (ms), 0 if absent
	Reference  id.EventID // m.relates_to.event_id when rel_type is m.reference (or absent), "" otherwise
	Mentions   []id.UserID // m.mentions.user_ids
	RoomMention bool      // m.mentions.room
}

// ParseNotification parses a KindRTCNotification event. ok=false for any other kind or unparsable
// content. Never fails on missing optional fields.
func ParseNotification(evt Event) (Notification, bool)

// DeclineContent returns the MSC4310 decline content referencing notificationID (wire §3.1):
// {"m.relates_to": {"rel_type": "m.reference", "event_id": notificationID}}. Sent as RTCDecline
// (unstable name), never the alias.
func DeclineContent(notificationID id.EventID) map[string]any
```

Tests: `Created` from `created_ts` and from the event timestamp; `ParseNotification` on the wire §2.2
example, an Element Call style `{notification_type: "ring", m.mentions: {user_ids: [], room: true},
m.relates_to: {...}}`, missing fields, wrong kind; `DeclineContent` golden JSON. The `org.matrix.msc`
grep rule of P2 §4.3 still holds.

### 3.2 `pkg/core/callhost.go`

```go
// CallRoom gains:
//	// Created is true when ResolveCallRoom created the Matrix room in this call (clients may not have
//	// synced it yet). Always false from LookupCallRoom.
//	Created bool

// CallHost gains:
//	// LookupCallRoom returns the CallRoom for an existing room, or ErrNotCallRoom if roomID is not a
//	// 1:1 call portal of login (unknown room, other login, group chat). Must not create anything.
//	LookupCallRoom(ctx context.Context, login LoginRef, roomID id.RoomID) (*CallRoom, error)

// ErrNotCallRoom is returned by LookupCallRoom for rooms that cannot carry calls.
var ErrNotCallRoom = errors.New("core: not a call room")
```

### 3.3 `pkg/obs`

`attrs.go`:

```go
AttrSIPEarlyMedia   = attribute.Key("sip.early_media")    // call.ringback: early media was relayed
AttrDTMFDigitCount  = attribute.Key("dtmf.digit_count")   // cmd.dtmf: number of digits (never the digits)
AttrDTMFMethod      = attribute.Key("dtmf.method")        // metric: rfc4733
```

`metrics.go` (TS §4.3 row, already in the spec):

```go
DTMFSent metric.Int64Counter // mrtcsip.dtmf.sent {digit}, attribute dtmf.method=rfc4733
```

`internal/obstest` allowlist: `"mrtcsip.dtmf.sent": {"dtmf.method": {"rfc4733"}}`.

### 3.4 `pkg/sipua` API (exact)

`inbound.go` changes (additive; existing values keep their numbers):

```go
const (
	EventAnswered CallEventKind = iota + 1
	EventAnswerFailed
	EventEnded
	EventProvisional    // outbound only: 1xx received (StatusCode; EarlyMedia+Media on the first 183 with SDP)
	EventAuthChallenged // outbound only: the INVITE was challenged (401/407) and re-sent with credentials
)

const (
	EndCancelled   EndCause = iota + 1 // inbound: caller sent CANCEL
	EndRemoteBye                       // the remote sent BYE after answer
	EndLocalHangup                     // we sent BYE (Hangup after answer)
	EndRejected                        // inbound: we sent a final non-2xx
	EndFailed                          // timeout, transport error, no ACK, protocol error
	EndLocalCancel                     // outbound: Hangup before answer (CANCEL sent; or 200 raced it: ACK+BYE sent)
	EndRemoteRejected                  // outbound: remote final non-2xx (StatusCode, Reason)
)
// String adds "local_cancel" and "remote_rejected".

// AnswerInfo gains:
//	TelephoneEventPT uint8      // negotiated telephone-event payload type (valid if TelephoneEvent)
//	OKReceivedAt     time.Time  // outbound: 200 OK received
//	AckSentAt        time.Time  // outbound: ACK sent
// (OKSentAt/AckAt stay inbound-only.)

// CallEvent gains:
//	EarlyMedia bool   // EventProvisional: Media is the early-media leg (first 183 with SDP only)
//	Reason     string // EventEnded/EndRemoteRejected: remote reason phrase (logs/spans only, scrubbed)
// CallEvent.StatusCode doc: inbound EventEnded = final status WE sent; outbound EventProvisional = the
// 1xx; outbound EventEnded = final status RECEIVED (200 for answered or raced calls, 487 for a completed
// CANCEL, 0 if none).

// Offer gains:
//	TelephoneEventPT uint8 // PT of telephone-event/8000 (valid if TelephoneEvent)
```

`outbound.go` (new, exact):

```go
package sipua

// ErrNotRegistered is returned by Dial when the account is not currently registered (no pinned
// registrar destination or State != StateRegistered).
var ErrNotRegistered = errors.New("sipua: not registered")

// NumberFormat selects the Request-URI user part for a dialled E.164 number.
type NumberFormat int

const (
	FormatDigits NumberFormat = iota // "15551234567" (E.164 without "+"; JMP/Bandwidth default)
	FormatE164                       // "+15551234567"
)

// DialRequest describes one outbound call.
type DialRequest struct {
	Target   string        // E.164, validated by the caller (phone.IsE164 is re-checked)
	Format   NumberFormat
	CallerID string        // From user part; "" = Config.Username (required by JMP/Bandwidth)
	Timeout  time.Duration // safety bound until a final response; 0 = 5 min (the core cancels earlier)
}

// DialInfo holds facts about an outbound call. Nothing in it is secret.
type DialInfo struct {
	CallID      string    // SIP Call-ID
	Target      string    // E.164
	RequestUser string    // Request-URI user part actually sent
	StartedAt   time.Time // Dial called
}

// OutboundCall is one outbound INVITE dialog. Every method is non-blocking and safe for concurrent use.
type OutboundCall interface {
	Info() DialInfo
	// Hangup ends the call: before a final response it cancels the INVITE (CANCEL is sent once a
	// provisional response has arrived, per RFC 3261 §9.1; a 200 that races the CANCEL is ACKed and
	// immediately BYEd); after answer it sends BYE. Idempotent; ignored after EventEnded.
	Hangup()
	// Events delivers, in order: zero or more EventAuthChallenged/EventProvisional, at most one
	// EventAnswered, then exactly one EventEnded, then the channel is closed. Capacity 16; provisional
	// events are dropped (never Answered/Ended) when fewer than 2 slots are free.
	Events() <-chan CallEvent
}

// UA gains (diago.go):
//	// Dial starts an outbound call from this account and returns immediately (the INVITE is sent on
//	// the call's own goroutine). Errors: ErrNotRegistered; invalid Target; UA not started or stopped.
//	Dial(req DialRequest) (OutboundCall, error)
```

T1 adds `func (u *diagoUA) Dial(DialRequest) (OutboundCall, error) { return nil, errors.New("not implemented: T3") }`.

### 3.5 `pkg/media` API (exact)

`media.go` additions:

```go
// Config gains:
//	// Early starts the bridge in early-media mode (D10): SIP audio is relayed to LiveKit with ringback
//	// filling silence, nothing from the owner is sent to SIP (µ-law silence instead), the RTC receiver is
//	// not attached and the RTP timeout is disabled. Connect leaves early mode.
//	Early bool
//	// EarlyFillAfter is how long without SIP RTP (or since Start) before ringback fills in [1 s].
//	EarlyFillAfter time.Duration

// Stats gains:
//	DTMFSent uint64 // DTMF digits fully sent (RFC 4733)

// SIPEventLeg is implemented by SIP legs that negotiated telephone-event (RFC 4733).
type SIPEventLeg interface {
	// TelephoneEventPT returns the negotiated payload type; ok=false if not negotiated.
	TelephoneEventPT() (pt uint8, ok bool)
	// WriteEvent sends one telephone-event packet now with that PT. samples is the RTP timestamp increment
	// applied after this packet (0 for every packet of an event except the last).
	WriteEvent(payload []byte, samples uint32, marker bool) error
}

// Bridge gains:
//	// Connect leaves early mode (no-op otherwise): attaches the RTC receiver (owner audio flows to SIP),
//	// stops the ringback fill and starts the RTP timeout from now. Idempotent, non-blocking.
//	Connect()
//	// SendDTMF queues digits (0-9, *, #, A-D) for RFC 4733 transmission (D13). Non-blocking. Errors:
//	// ErrDTMFUnsupported (leg is not a SIPEventLeg or PT not negotiated), ErrDTMFNotConnected (early mode),
//	// ErrDTMFBusy (more than 32 digits queued), ErrDTMFInvalid (other characters).
//	SendDTMF(digits string) error

var (
	ErrDTMFUnsupported  = errors.New("media: DTMF (telephone-event) not negotiated on this call")
	ErrDTMFNotConnected = errors.New("media: call not connected")
	ErrDTMFBusy         = errors.New("media: too many DTMF digits queued")
	ErrDTMFInvalid      = errors.New("media: invalid DTMF digit")
)

// TonePattern selects a generated call-progress tone (North American values, ITU-T E.180 supplement).
type TonePattern int

const (
	ToneRingback TonePattern = iota + 1 // 440+480 Hz, 2 s on / 4 s off
	ToneBusy                            // 480+620 Hz, 0.5 s on / 0.5 s off
	ToneReorder                         // 480+620 Hz, 0.25 s on / 0.25 s off
)

// Tone plays a call-progress tone into an RTCLeg on its own 20 ms ticker.
type Tone interface {
	Stop() // idempotent; waits for the goroutine; the RTCLeg is free for another writer afterwards
}

// StartTone starts playing p into rtc (Opus 8 kHz mono, same encoder settings as the bridge). Errors only
// on encoder creation. Only one writer may use an RTCLeg at a time: stop the Tone before Start'ing a
// bridge on the same leg.
func StartTone(cfg Config, rtc RTCLeg, p TonePattern) (Tone, error)
```

T1 stubs: `Connect` and `SendDTMF` on the real bridge return/do nothing (`SendDTMF` returns
`ErrDTMFUnsupported`), `StartTone` panics `"not implemented: T2"`.

### 3.6 `pkg/calls` exported API (exact; bodies T4)

```go
// Config gains:
//	// Dial places an outbound SIP call from login's account; nil disables outbound calls.
//	Dial func(login core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error)
//	// LoginNumber returns login's own E.164 number (span attribute, self-dial guard); nil = unknown.
//	LoginNumber func(login core.LoginRef) string
//	DialTimeout         time.Duration // INVITE sent -> 200 OK, then CANCEL [60 s]
//	OutboundTrigger     string        // "ring" (default), "membership", "off"
//	DialAllowedPrefixes []string      // E.164 prefixes; empty = any
//	DialNumberFormat    sipua.NumberFormat
//	AllowSelfDial       bool

// PlaceCallRequest is a command-initiated outbound call. Exactly one of Number and RoomID is set.
type PlaceCallRequest struct {
	Number string    // any form phone.Normalize accepts
	RoomID id.RoomID // an existing portal room of the login (the command was sent there)
}

// PlaceCall validates the request (number, allowed prefix, self-dial, outbound enabled, line free),
// reserves the login's call slot and starts a callback-ring call actor (D6). It does no network I/O and
// returns the call id and the normalized number. ctx carries the cmd.call span (linked from call.outbound).
func (m *Manager) PlaceCall(ctx context.Context, login core.LoginRef, req PlaceCallRequest) (callID, e164 string, err error)

// Hangup ends the login's active call (any state). roomID "" = whatever room it is in; otherwise it must
// match. Non-blocking.
func (m *Manager) Hangup(ctx context.Context, login core.LoginRef, roomID id.RoomID) error

// SendDTMF queues digits on the login's active, connected call (roomID as for Hangup). It waits up to 2 s
// for the actor's answer. Spaces are ignored; 1..32 digits of 0-9 * # A-D (case-insensitive).
// Returns the digit count queued.
func (m *Manager) SendDTMF(ctx context.Context, login core.LoginRef, roomID id.RoomID, digits string) (int, error)

var (
	ErrOutboundDisabled = errors.New("calls: outbound calls are disabled")
	ErrInvalidNumber    = errors.New("calls: not a dialable phone number")
	ErrNotAllowed       = errors.New("calls: destination not allowed by dial_allowed_prefixes")
	ErrSelfDial         = errors.New("calls: that is this account's own number")
	ErrLineBusy         = errors.New("calls: another call is in progress")
	ErrNoActiveCall     = errors.New("calls: no active call")
	ErrNotConnected     = errors.New("calls: the call is not connected yet")
	ErrInvalidDigits    = errors.New("calls: digits must be 1-32 of 0-9 * # A-D")
)
// SendDTMF also returns media.ErrDTMFUnsupported and media.ErrDTMFBusy as-is.
```

---

## 4. SIP outbound (`pkg/sipua`, T3)

### 4.1 UA changes (`diago.go`)

- `diagoUA` keeps `dg *diago.Diago`, `aorHost string`, `transport Transport` after `start`.
- Pinned destination: `diagoTxn.SetDestination(hostport)` also stores it in `u.dest atomic.Pointer[string]`
  (the `diagoTxn` gets a back pointer or a callback). `Dial` reads it lock-free.
- `Dial(req)`: under `u.mu` check started and not stopped; `phone.IsE164(req.Target)` else error;
  `u.tracker.state().State == StateRegistered && dest != ""` else `ErrNotRegistered`; build the dialog
  (§4.3), `c := newOutboundCall(dlg, log, clock.Real(), req)`, `go c.run()`, return `c`.
- Unexported test seam: `(*diagoUA).dialTo(req DialRequest, dest string) (OutboundCall, error)` skips the
  registration check (used only by the loopback test).
- Acceptance (loopback test asserts it): the INVITE's UDP source port equals the UA's `BindPort` (same
  socket as REGISTER, D9). If diago's `createClient` does not bind the client to `BindPort` for the
  `"udp4"` transport name (`diago.go:866` compares `tran.Transport == "udp"`), stop and report; do not
  work around it silently.

### 4.2 Per-call loop (`outbound_loop.go`, no diago imports)

```go
// dialogClient is the subset of the diago client dialog the loop uses (outbound_diago.go adapts it).
type dialogClient interface {
	Info() DialInfo
	// Invite sends the INVITE and blocks until: 2xx (nil; Answer/Leg valid), 183 with SDP (errEarlyMedia;
	// Leg valid), final non-2xx (*finalError), or ctx cancellation (sipgo sent CANCEL; returns an error
	// wrapping context.Canceled). onResponse is called on this goroutine for every response.
	Invite(ctx context.Context, onResponse func(status int)) error
	// WaitAnswer continues after errEarlyMedia with the same result contract.
	WaitAnswer(ctx context.Context, onResponse func(status int)) error
	Leg() media.SIPLeg       // early-media or call leg (same object across 183 -> 200)
	Answer() AnswerInfo      // after 2xx: codec, telephone-event, RTP addrs, OKReceivedAt
	FinalStatus() (int, string) // last final response seen (incl. a 200 that raced CANCEL); 0 if none
	Ack(ctx context.Context) error
	Bye(ctx context.Context) error
	Done() <-chan struct{}   // dialog context done (remote BYE, ended)
	Close()
}

var errEarlyMedia = errors.New("sipua: early media")

type finalError struct{ Status int; Reason string }
```

Two goroutines per call: **run** (owns all state, emits events) and **invite** (the only goroutine that
calls `Invite`/`WaitAnswer`). Internal channel `msgs` (capacity 32) from invite to run; command channel
`cmds` (capacity 4); `events` (capacity 16).

invite goroutine:

1. `err := dlg.Invite(ictx, onResp)`.
2. While `err` is errEarlyMedia → post `msgEarly`; `err = dlg.WaitAnswer(ictx, onResp)`. While `err`
   message contains `"more than 10 responses received"` and fewer than 10 re-entries → `err =
   dlg.WaitAnswer(ictx, onResp)` (D12).
3. Post `msgResult{err}` and return.

`onResp(status)`: 401/407 → post `msgAuth` once; 100-199 → post `msgProvisional{status}` (non-blocking;
drop when full); 2xx → record `okAt = now` (read by run after `msgResult`).

run goroutine `select` over `msgs`, `cmds`, `dlg.Done()` (only after answered), safety timer
(`req.Timeout`, default 5 min):

| Input | State | Action |
|---|---|---|
| `msgAuth` | | emit `EventAuthChallenged` |
| `msgProvisional{s}` | not ended | emit `EventProvisional{StatusCode: s}` |
| `msgEarly` | first one | emit `EventProvisional{StatusCode: 183, EarlyMedia: true, Media: dlg.Leg()}`; later ones ignored |
| `msgResult{nil}` | | `Ack(ctx 5 s)` first (fresh context, never `ictx`); if `cancelRequested` → `Bye(5 s)`, finish(`EndLocalCancel`, 200); else `answered = true`, emit `EventAnswered{Media: dlg.Leg(), Answer: dlg.Answer() + AckSentAt}`; Ack error → `Bye`, finish(`EndFailed`, 200, err) |
| `msgResult{*finalError}` | | `cancelRequested && Status == 487` → finish(`EndLocalCancel`, 487); else finish(`EndRemoteRejected`, Status, Reason) |
| `msgResult{ctx error}` | cancelled | `s, _ := dlg.FinalStatus()`; `s` 2xx → `Ack`+`Bye` (5 s each), finish(`EndLocalCancel`, 200); else finish(`EndLocalCancel`, s) |
| `msgResult{other err}` | | `cancelRequested` → finish(`EndLocalCancel`, 0); safety fired → finish(`EndFailed`, 0, err) |
| cmd `hangup` | answered | `Bye(5 s)` (log Warn on error), finish(`EndLocalHangup`, 200) |
| cmd `hangup` | not answered, not yet requested | `cancelRequested = true`; `cancelInvite()` (sipgo waits for a provisional, sends CANCEL, waits for 487 up to 64×T1, `dialog_client.go:337-390`); keep looping until `msgResult` |
| `dlg.Done()` | answered | finish(`EndRemoteBye`, 200) |
| safety timer | not answered | `safety = true`; as cmd `hangup` |

`finish(cause, status, err)`: emit exactly one `EventEnded{Cause, StatusCode, Reason, Err, At}`, close
`events`, mark commands closed (later `Hangup` ignored), `dlg.Close()`. run returns only after the invite
goroutine posted its result (it never leaks; bounded by sipgo's own timers).

Logging (`component=sipua`, clean logger): Info "Outbound INVITE" (`target`, `request_user`,
`sip_call_id`); Debug per provisional status; Info final (`status`, `reason` scrubbed, `cause`). Never SDP,
headers, credentials or digest values.

### 4.3 diago adapter (`outbound_diago.go`)

- Request-URI: `sip:<user>@<aorHost>` (`user` per `Format`; `;transport=tcp` for TCP);
  `d, err := u.dg.NewDialog(ruri, diago.NewDialogOptions{})`; `d.InviteRequest.SetDestination(dest)`.
- Options: `diago.InviteClientOptions{Username, Password, Headers: []sip.Header{}, EarlyMediaDetect: true,
  OnResponse: func(r *sip.Response) error { onResponse(r.StatusCode); return nil }}`; if `CallerID != ""`
  `opts.WithCaller("", CallerID, aorHost)`.
- `Invite`: `med, err := d.Invite(ctx, opts)`; `errors.Is(err, diago.ErrClientEarlyMedia)` → keep `med`,
  leg = `&diagoLeg{med: med, buf: make([]byte, 1500)}`, return `errEarlyMedia`; `errors.As(err,
  &*sipgo.ErrDialogResponse)` → `&finalError{Res.StatusCode, Res.Reason}`; nil → keep `med`, build leg
  and `AnswerInfo` (parse `d.InviteResponse.Body()` with `parseOffer`: `PCMU`, `TelephoneEvent`,
  `TelephoneEventPT`, `MediaAddr` → `RemoteRTP`; `LocalRTP` from `med.MediaSession().Laddr`; `Codec:
  "PCMU"`, `PtimeMS: 20`; `OKReceivedAt` from `onResp`).
- `WaitAnswer`: `d.WaitAnswer(ctx, med, sipgo.AnswerOptions{Username, Password, OnResponse})`, same
  error mapping and the same `AnswerInfo` building on success.
- `FinalStatus`: `d.InviteResponse` status/reason if final (read only by run after `msgResult`).
- `Ack`: `d.Ack(ctx)`. `Bye`: `d.Hangup(ctx)`. `Done`: `d.Context().Done()`. `Close`: `d.Close()`, then
  `med.Close()` if non-nil.
- The leg's telephone-event PT is set from `AnswerInfo` once answered (atomic in `diagoLeg`).

`diagoLeg` (also used inbound) implements `media.SIPEventLeg`: `TelephoneEventPT()` returns the PT set at
creation (inbound: `Offer.TelephoneEventPT` when `Offer.TelephoneEvent`; outbound: from the answer);
`WriteEvent(p, samples, marker)` = `med.RTPPacketWriter.WriteSamples(p, samples, marker, pt)`
(`rtp_packet_writer.go:164`). `parseOffer` records the PT of `telephone-event/8000`.

### 4.4 What sipua deliberately does not do

No ringback, no DTMF generation (media does it), no PRACK/100rel, no re-INVITE handling for client
dialogs (Phase 4), no symmetric-RTP learning on outbound (diago client dialogs have `RTPNAT` 0; carrier
SDP addresses are public, and our early RTP opens our NAT, D10), no spans.

---

## 5. Media (`pkg/media`, T2)

### 5.1 Tone generator (`tone.go`)

- `toneGen` produces 160-sample PCM16 frames for a pattern with two phase accumulators (float64 phase,
  `math.Sin` per sample is fine: 8000 samples/s) at amplitude 0.12 of full scale per component (about
  -19 dBm0 each); cadence by sample counting; zero allocation per frame after construction.
- `StartTone(cfg, rtc, p)`: one goroutine with a `cfg.Clock` ticker (20 ms): frame → Opus encode (own
  encoder, settings of P2 §7.2) → `rtc.WriteOpus(out, 20 ms)`; errors logged at most once per 10 s.
  `Stop` closes a channel and waits.
- The bridge in early mode embeds a `toneGen` (ringback) for the fill.

### 5.2 Early mode (`bridge.go`)

- `Start` with `cfg.Early`: do **not** call `rtc.SetReceiver(b)`; `early` atomic = true.
- sipToRTC step while early: if `LastSIPRx` is zero or older than `EarlyFillAfter` (since Start for the
  zero case), encode a ringback frame instead of the jitter-buffer/concealment frame (do not count it as
  concealment); otherwise the normal path. The RTP-timeout check is skipped while early.
- rtcToSIP step while early: send one 160-byte µ-law silence frame (`0xFF`), marker on the first; never
  pop the RTC jitter buffer.
- `Connect()`: if early: `early = false`, record `connectedAt = clock.Now()` (RTP timeout measured from
  `max(LastSIPRx, connectedAt)`), `rtc.SetReceiver(b)`. Idempotent; safe from any goroutine.

### 5.3 DTMF (`dtmf.go` + rtcToSIP)

- `SendDTMF(digits)`: validate (D13 characters; uppercase letters), error if early (`ErrDTMFNotConnected`)
  or the SIP leg is not a `SIPEventLeg` with a PT (`ErrDTMFUnsupported`); push into a fixed 32-slot ring
  under a small mutex (`ErrDTMFBusy` if full). Never logs digits.
- rtcToSIP step: if a digit is in progress or queued, the tick sends the next event packet instead of an
  audio frame: event code (0-9 → 0-9, `*` → 10, `#` → 11, A-D → 12-15), volume 10, packets 1-5 not-end
  with duration 160×k (marker on packet 1), packets 6-8 end-bit with duration 800; samples = 0 except the
  8th packet with `samples = 160*8` (keeps the RTP clock in step with the 8 ticks used). Then 5 ticks of
  normal audio, then the next digit. `DTMFSent++` after the 8th packet. Owner audio decoded meanwhile is
  dropped (FIFO cleared at digit start).
- 4-byte payload encoded into a reused buffer (no allocation).

### 5.4 Tests (T2)

`tone_test.go`: each pattern's on/off cadence over 12 s of frames (energy per 20 ms frame), dominant
frequencies via Goertzel at 440/480/620 Hz; `StartTone` with a fake clock writes 50 frames/s, `Stop`
waits; zero allocations per generated frame. `dtmf_test.go`: digit validation; encoder bytes for `5`,
`#` (event, end bit, volume, duration) match RFC 4733 §2.3; schedule over ticks for "12": 8 event packets
+ 5 audio ticks + 8 event packets, timestamps (via a fake `SIPEventLeg`) constant within a digit and
advancing by 1280 after it, marker only on each digit's first packet; `ErrDTMFUnsupported` with a plain
`SIPLeg`; `ErrDTMFBusy` after 32; `DTMFSent == 2`. `bridge_test.go` additions: early mode sends 0xFF to SIP
and never attaches the receiver; ringback fill reaches LiveKit until SIP RTP arrives, then relayed audio;
no RTP timeout while early; `Connect` attaches the receiver and starts the timeout from Connect;
`SendDTMF` while early → `ErrDTMFNotConnected`. `bench_test.go`: hot path with a DTMF digit in progress and
in early mode stays at 0 allocs.

---

## 6. Call state machine (`pkg/calls`, T4)

### 6.1 Refactor of the existing actor

- `call.sip` (inbound) becomes `c.in sipua.InboundCall`; new `c.out sipua.OutboundCall`; `c.direction`
  (`"inbound"`/`"outbound"`); `c.sipEvents` is set from whichever exists. Shared code uses
  `c.sipHangup()` (= `c.in.Hangup()` or `c.out.Hangup()` once, `finalIssued` guard as today). `reject` and
  `Answer`/`Ringing` remain inbound-only.
- Metrics/telemetry helpers take the direction (`attrInbound` → per-call `c.dirAttr`, pre-built options
  for both directions).
- The answered-call teardown (P2 §8.7) is unchanged except the notice (`Outgoing call · %s` for outbound)
  and the stats/refresh/owner rules, which apply as-is.
- Everything in P2 §8.2 (concurrency rules) applies to outbound calls unchanged. Additional inputs on
  `c.events`: `evHangupCmd`, `evDTMF{digits, reply chan error}` (reply channel capacity 1; the actor always
  answers).

### 6.2 States and events

```go
const (
	stSetup state = iota
	stRinging   // inbound: ringing owner. outbound (command): ringing owner for the callback (D6)
	stAnswering // inbound only
	stJoining   // outbound: owner present, waiting for the LiveKit session before dialling
	stDialing   // outbound: INVITE sent, no 18x yet
	stEarly     // outbound: 18x received (ringback tone or early-media bridge)
	stActive
	stEnding
	stEnded
)

const (
	evOwnerMembership eventKind = iota + 1
	evDecline
	evShutdown
	evHangupCmd // Manager.Hangup
	evDTMF      // Manager.SendDTMF
)

// trigger values (call.trigger): "invite" (inbound), "command", "owner_membership" (both detector modes)
```

New constants (`state.go`):

```go
const (
	triggerFreshness   = 30 * time.Second // owner notification/membership origin ts vs now
	triggerPairWindow  = 5 * time.Second  // notification before membership: wait this long
	freshJoinWindow    = 60 * time.Second // membership created within this of the notification
	triggerCooldown    = 5 * time.Second  // per room, after a call ends or a trigger is refused
	newRoomRingDelay   = 3 * time.Second  // D18
	failureToneLength  = 2 * time.Second  // D11
	dtmfReplyTimeout   = 2 * time.Second
	defaultDialTimeout = 60 * time.Second
)
```

### 6.3 Owner-trigger detector (`detect.go`, manager side, appservice path: no I/O, no blocking)

Manager state (under `m.mu`, bounded): `ownerLive map[core.LoginRef]map[id.RoomID]map[string]memberSeen`
(owner device state key → `{created, eventID, seenAt}`; entries removed on non-live content; pruned when
older than 5 min on insert), `pendingNotif map[core.LoginRef]map[id.RoomID]notifSeen` (one per room,
expires after `triggerPairWindow`), `cooldown map[id.RoomID]time.Time` (pruned on insert).

`HandleRTCEvent` new order:

1. If the login has a routable call and `evt.RoomID == c.roomID` → existing routing (membership,
   decline); notifications to the active call's room are ignored. Return.
2. If `evt.Sender != Host.OwnerMXID(login)` → return (the resolver already drops ghosts and the bot).
3. `KindCallMember`: `ParseMembership`; update `ownerLive`. If live **and** fresh (`now - Created ≤
   triggerFreshness` and `now - evt.Timestamp ≤ triggerFreshness`, future skew ≤ 30 s allowed):
   - mode `ring`: if a pending notification for this room exists and is younger than `triggerPairWindow` →
     **trigger** (notification event id from the pending entry).
   - mode `membership`: if no **other** owner device was live in the room before this event → **trigger**
     (no notification id).
4. `KindRTCNotification`, mode `ring`: `ParseNotification` ok; `now - evt.Timestamp ≤ triggerFreshness`;
   if `ownerLive[login][room]` has a device whose `Created` is within `freshJoinWindow` of the
   notification's timestamp → **trigger** with that notification id; else store as pending.
5. **trigger**: skip if `m.stopped`, `cfg.Dial == nil`, mode `off`, or the room is in cooldown (log Debug).
   If the login has an active call (other room) → start a **busy actor** (not active, not routable) that
   runs: `LookupCallRoom`; decline the notification (if any); notice `Call not placed: another call is in
   progress`; outcome `rejected`, end_reason `rejected`; set cooldown. Otherwise create the outbound actor
   with `roomID` set, devices = snapshot of the owner's live state keys in that room, `ownerNotif` =
   notification id, register it as `active` **and** `routable` in the same critical section, then `go
   c.run()`. The `matrixrtc.ingest` span gets `matrixrtc.routed_to=call` and `call.id`; its span context
   is the `call.outbound` link.

### 6.4 Outbound setup (`outbound.go`, inline in `run`)

1. Start `call.outbound` (CLIENT, `obs.StartLinkedRoot(context.Background(), "call.outbound", triggerSC)`
   with timestamp = trigger time); attributes `call.id`, `call.direction=outbound`, `call.trigger`,
   `call.local.e164`, `matrix.owner_id` (when known). `call.started{outbound}` +1, `call.active{outbound}`
   +1.
2. `owner := Host.OwnerMXID(login)`; "" → fail(failed, error, no notice).
3. Room: owner trigger → `LookupCallRoom(ctx 15 s, login, roomID)` inside `call.resolve_room`
   (`ErrNotCallRoom` → fail(rejected, rejected, no notice); other error → fail(failed, error, no notice));
   command → `ResolveCallRoom(ctx 15 s, login, e164)`, then `c.roomID = room.RoomID`, `m.setRoutable(c)`.
   Set `call.remote.e164`, `matrix.room_id`, `matrix.ghost_id`.
4. Guards (owner-trigger path; the command path checked them in `PlaceCall` already, re-check is
   harmless): allowed prefix / self-dial (§6.6 notices, outcome `rejected`); `room.Encrypted` → `rejected`,
   notice `noticeOutEncrypted`, Error log.
5. Service URL (P2 step 7) → failure: fail(failed, error, `Call failed: could not connect audio`).
6. Command path: if `room.Created` → wait `newRoomRingDelay` (abort on quit). Publish the ghost
   membership (P2 step 9) → failure as 5. Send the ring (P2 step 10, lifetime = ring timeout) → failure as
   5. Start `call.ring`, `ringTimer`, keepalive is not used (no SIP leg yet), start join attempt 1,
   state `stRinging`. Log "Ringing owner for an outgoing call".
7. Owner-trigger path: publish the ghost membership → failure as 5; start join attempt 1; state
   `stJoining`. Log "Owner started a call" (`to`, `room_id`, `trigger`).

**dial()** (from `stJoining` when the session is ready, or from `stRinging` on the owner's answer when the
session is already ready):

- `out, err := cfg.Dial(login, sipua.DialRequest{Target: e164, Format: cfg.DialNumberFormat})`;
  `ErrNotRegistered` → fail(rejected, rejected, `Call failed: SIP account is not registered`); other →
  fail(failed, error, `Call failed: could not place the call`).
- `c.out = out`; `c.sipEvents = out.Events()`; start `sip.invite` (CLIENT, child of `call.outbound`) with
  `sip.call_id = out.Info().CallID`, `sip.method=INVITE`; `call.outbound` gets `sip.call_id`;
  `dialStartedAt = now`; `dialTimer = cfg.DialTimeout`; state `stDialing`; Log "Dialing" (`to`,
  `request_user`, `sip_call_id`).

### 6.5 Transitions (outbound)

"Fail(o, r, n)" = outbound failure branch (§6.7) with outcome, end reason, notice. "Cancel(o, r, n)" =
`c.sipHangup()` (if `c.out != nil`) then Fail. "Teardown" = P2 §8.7.

| State | Input | Guard | Actions | Next |
|---|---|---|---|---|
| Ringing (out) | `evOwnerMembership` | own, Live, not older than `ringSentAt - 5 s` | end `call.ring` (answered, link ingest); add device; stop ring timer; `markRingRead`; session ready → **dial()** else state Joining | Dialing / Joining |
| Ringing (out) | `evDecline` | target == ring, sender == owner | end `call.ring` (declined); Fail(declined, decline, `Call not placed`) | Ending |
| Ringing (out) | `ringTimer` | | end `call.ring` (timeout); Fail(timeout, ring_timeout, `Call not placed: not answered`) | Ending |
| Ringing / Joining | join result err | attempts remain | next attempt | same |
| Ringing / Joining | join result err | last | Fail(failed, error, `Call failed: could not connect audio`) | Ending |
| Ringing / Joining | rtc `EventDisconnected` | | as P2 (retry once, else the row above) | same / Ending |
| Joining | join result ok | | store session; **dial()** | Dialing |
| Joining | `evOwnerMembership` | own, not Live, last device | Fail(cancelled, cancel, `Cancelled call`) | Ending |
| Ringing / Joining | `evHangupCmd` | | Fail(cancelled, cancel, `Cancelled call`) | Ending |
| Ringing / Joining | `evShutdown` | | Fail(failed, bridge_stop, `Call not placed`) | Ending |
| Dialing / Early | SIP `EventAuthChallenged` | | `sip.invite` event `sip.auth.challenge`, attribute `sip.auth.challenged=true` | same |
| Dialing / Early | SIP `EventProvisional{100}` | | `sip.invite` event `sip.response.received{100}` | same |
| Dialing | SIP `EventProvisional{18x}` | | event; start `call.ringback`; `ringbackAt = now`; early media → **startEarlyMedia**; else **startRingback** | Early |
| Early | SIP `EventProvisional{18x}` | | event; early media and no bridge yet → **startEarlyMedia** | Early |
| Dialing / Early | SIP `EventAnswered` | | **connect** (below) | Active |
| Dialing / Early | SIP `EventEnded{EndRemoteRejected}` | | Fail(outcomeFor(status), remote_reject, `Call failed: ` + reasonText(status)) | Ending |
| Dialing / Early | SIP `EventEnded{EndFailed}` | | Fail(failed, error, `Call failed: the provider did not respond`) | Ending |
| Dialing / Early | SIP `EventEnded{other}` | | Fail(failed, error, `Call failed: could not place the call`) | Ending |
| Dialing / Early | `evOwnerMembership` | own, Live | add device | same |
| Dialing / Early | `evOwnerMembership` | own, not Live, last device | Cancel(cancelled, cancel, `Cancelled call`) | Ending |
| Dialing / Early | rtc `EventOwnerLeft` | `lkOwners` becomes 0 | `graceTimer = ownerRTCGrace` | same |
| Dialing / Early | rtc `EventOwnerJoined` | | stop grace timer | same |
| Dialing / Early | `graceTimer` | | Cancel(cancelled, rtc_lost, `Cancelled call`) | Ending |
| Dialing / Early | rtc `EventDisconnected` | | Cancel(failed, rtc_lost, `Call failed: could not connect audio`) | Ending |
| Dialing / Early | `dialTimer` | | Cancel(timeout, ring_timeout, `Call failed: no answer`) | Ending |
| Dialing / Early | `evHangupCmd` | | Cancel(cancelled, cancel, `Cancelled call`) | Ending |
| Dialing / Early | `evShutdown` | | Cancel(failed, bridge_stop, `Call failed: the bridge stopped`) | Ending |
| Dialing / Early | bridge `Done()` (early bridge died) | | Warn; stop it; **startRingback** | Early |
| any but Active | `evDTMF` | | reply `ErrNotConnected` | same |
| any | `evDecline` (not the Ringing row) | | ignore | same |
| Active | all P2 §8.5 Active rows | | unchanged (owner leave → BYE; remote BYE; rtc_lost; rtp_timeout; refresh; stats; shutdown) | Active / Ending |
| Active | `evHangupCmd` | | Teardown(owner, bye) | Ending |
| Active | `evDTMF` | | reply `bridge.SendDTMF(digits)` | Active |

Inbound calls additionally accept `evHangupCmd` and `evDTMF`:

| State | Input | Actions | Next |
|---|---|---|---|
| Ringing (in) | `evHangupCmd` | end `call.ring` (declined); `reject(603, "Decline")`; Fail(declined, decline, `Declined call`) (same as an MSC4310 decline) | Ending |
| Answering | `evHangupCmd` | `hangup()`; Fail(missed, cancel, `Missed call`) | Ending |
| Active (in) | `evHangupCmd` | Teardown(owner, bye) | Ending |
| Active (in) | `evDTMF` | reply `bridge.SendDTMF(digits)` | Active |
| other | `evDTMF` | reply `ErrNotConnected` | same |

**startRingback**: if no tone and no bridge: `c.tone, err = StartTone(mediaCfg, session.Audio(),
ToneRingback)` (error → Warn, continue silently).

**startEarlyMedia(ev)**: stop the tone (if any); `media.start` (child of `call.ringback`); `bridge, err :=
StartMedia(media.Config{..., Early: true}, ev.Media, session.Audio())`; error → Warn and startRingback;
`call.ringback` gets `sip.early_media=true`.

**connect(ev)** (on `EventAnswered`): end `call.ringback` (outcome answered) if open; `sip.invite` events
`sip.response.received{200}` at `OKReceivedAt`; end `sip.invite` (status 200); `call.outbound` event
`sip.ack.sent` at `AckSentAt`; stop the tone; if a bridge exists → `bridge.Connect()`, else `media.start`
(child of `call.outbound`) + `StartMedia(non-early)` (error → end spans, Teardown(bridge, error)); stop
`dialTimer`; end `call.outbound` (outcome answered, status 200, end time `AckSentAt`); `answered = true`,
`answeredAt = AckSentAt`; stats ticker, refresh timer, grace timer if `lkOwners == 0` (as P2 activate);
record setup duration (`dialStartedAt` → `OKReceivedAt`) and ring duration (`ringbackAt` →
`OKReceivedAt`, only if a provisional was seen). Log "Call connected".

Owner-trigger calls start with the snapshot devices, so the P2 "no device left → teardown" logic works
from the first moment. Owner memberships older than the trigger are fine (they are the snapshot).

### 6.6 Outcomes, reasons and notices (`notices.go`)

```go
const (
	noticeOutAnsweredFmt = "Outgoing call · %s"
	noticeOutCancelled   = "Cancelled call"
	noticeOutFailedFmt   = "Call failed: %s"
	noticeOutNotPlaced   = "Call not placed"
	noticeOutNoAnswer    = "Call not placed: not answered"
	noticeOutBusyLine    = "Call not placed: another call is in progress"
	noticeOutNotAllowed  = "Call not placed: the bridge configuration does not allow this destination"
	noticeOutSelf        = "Call not placed: that is your own number"
	noticeOutEncrypted   = "Call not placed: calls need an unencrypted room. This room is encrypted."
)

// reasonText(status): fixed table, never the remote reason phrase:
// 400 "400 Bad Request", 403 "403 Forbidden", 404 "404 Not Found", 408 "no answer (408)",
// 410 "410 Gone", 480 "480 Temporarily Unavailable", 484 "484 Address Incomplete",
// 486 "486 Busy Here", 487 "487 Request Terminated", 488 "488 Not Acceptable Here",
// 500 "500 Server Internal Error", 502 "502 Bad Gateway", 503 "503 Service Unavailable",
// 504 "504 Server Time-out", 600 "600 Busy Everywhere", 603 "603 Decline",
// 604 "604 Does Not Exist Anywhere"; anything else "SIP <code>".

// outcomeFor(status): 486, 600 -> busy; 603 -> declined; 408, 480 -> timeout; else failed.
```

| Situation | outcome | end_reason | Notice | Tone | Decline owner notif |
|---|---|---|---|---|---|
| Answered, any hangup | answered | bye / rtc_lost / rtp_timeout / bridge_stop / error | `Outgoing call · 1m 20s` | | |
| Owner left / `!sip hangup` / LiveKit grace before answer | cancelled | cancel / rtc_lost | `Cancelled call` | | no |
| Remote 486/600 | busy | remote_reject | `Call failed: 486 Busy Here` | busy | yes |
| Remote 603 | declined | remote_reject | `Call failed: 603 Decline` | reorder | yes |
| Remote 408/480 | timeout | remote_reject | `Call failed: no answer (408)` / `... 480 ...` | reorder | yes |
| Dial timeout (60 s) | timeout | ring_timeout | `Call failed: no answer` | reorder | yes |
| Other remote 3xx-6xx | failed | remote_reject | `Call failed: 404 Not Found` etc. | reorder | yes |
| Provider timeout / transport | failed | error | `Call failed: the provider did not respond` | reorder | yes |
| Not registered | rejected | rejected | `Call failed: SIP account is not registered` | | yes |
| Prefix / self / encrypted (owner trigger) | rejected | rejected | §6.6 constants | | yes |
| Busy line (owner trigger, other room) | rejected | rejected | `Call not placed: another call is in progress` | | yes |
| LiveKit join failed / no service URL / membership send failed | failed | error | `Call failed: could not connect audio` | | yes |
| Command ring declined | declined | decline | `Call not placed` | | n/a |
| Command ring timeout | timeout | ring_timeout | `Call not placed: not answered` | | n/a |
| Shutdown before answer | failed | bridge_stop | `Call not placed` / `Call failed: the bridge stopped` | | no |

`hangup.by` for answered outbound calls: as inbound (`remote`, `owner`, `bridge`).

### 6.7 Outbound failure branch (`failOutbound`, inside `call.outbound`)

Inline, each step best effort with `matrixOpTimeout` unless noted:

1. `state = stEnding`; `releaseActive`; stop timers; set the room's trigger cooldown.
2. SIP: if `c.out != nil` and not ended: `c.sipHangup()` (if the transition has not); `waitSIPEnd`
   (5 s). End `sip.invite` with the received final status (`sip.response.status_code`,
   `sip.response.reason` scrubbed) or Error status if none. If `call.ringback` is open, end it with the
   outcome.
3. `cancelJoinAndWait` (P2 §8.2 rule 7).
4. Command path with a ring sent: `markRingRead`; redact the ring unless outcome is declined (P2 §8.6).
5. Stop the early bridge and the ringback tone.
6. Failure tone (table above) if the session exists: `StartTone(busy|reorder)`, wait
   `failureToneLength` on the call clock (also ends early on quit), `Stop`.
7. If `ownerNotif != ""` and the table says so: `matrixrtc.decline.send` (CLIENT, child) →
   `Ghost.SendMessage(room, matrixrtc.RTCDecline, matrixrtc.DeclineContent(ownerNotif))`.
8. Clear the ghost membership (if published); leave LiveKit (if joined).
9. Notice (if a room exists and the table has one).
10. End `call.outbound` (outcome, end reason, final status; Error status only for `failed`); `finish()`
    (metrics, `call.session`, manager removal).

### 6.8 Command handling (`commands.go`)

- `PlaceCall`: `cfg.Dial == nil` → `ErrOutboundDisabled`; exactly one of Number/RoomID; Number →
  `phone.Normalize` (error → `ErrInvalidNumber`); RoomID → the actor resolves the number via
  `LookupCallRoom` (so the guards below run in the actor for this form); prefix → `ErrNotAllowed`; self →
  `ErrSelfDial`; under `m.mu`: stopped → `ErrOutboundDisabled`, `active[login] != nil` → `ErrLineBusy`;
  create the actor (`trigger=command`, `triggerSC` = span context of ctx), register active, `go run()`.
- `Hangup`: find `active[login]` (and room match when given) → non-blocking send `evHangupCmd` (a full
  queue logs Warn and still returns nil; the queue holds 32); none → `ErrNoActiveCall`.
- `SendDTMF`: normalise digits (strip spaces, uppercase; validate → `ErrInvalidDigits`); find the active
  call (room match) → send `evDTMF{digits, reply}`; wait reply or `dtmfReplyTimeout` (→ `ErrNotConnected`)
  or ctx done. Never logs digits.

### 6.9 Edge cases (normative)

1. **Owner hangs up while the INVITE has no provisional yet**: sipua waits for the first response before
   CANCEL (sipgo); the actor does not wait beyond 5 s; a later 200 is ACKed+BYEd by sipua.
2. **CANCEL vs 200 race**: sipua returns `EndLocalCancel` with status 200 (ACK+BYE sent). The actor
   already chose outcome `cancelled`; `call.ended` status class `2xx`. The callee experiences a call that
   connects and drops immediately: unavoidable in SIP.
3. **Owner on two devices**: the snapshot holds both; leaving one keeps dialing.
4. **Owner's own client times out its notification** (lifetime ~30 s): irrelevant once the ghost joined
   (D5).
5. **Element X sends the notification again** (retry/duplicate): routed to the active call's room →
   ignored.
6. **Inbound INVITE while dialing/connected**: 486 (busy actor). **Owner trigger while an inbound call
   rings in the same room**: the owner's membership is routed to the inbound call and answers it (P2);
   no outbound trigger.
7. **Bridge restart with the owner sitting in a call**: no fresh events → no trigger (freshness).
8. **Stale ghost membership from a crash** (Phase 4 cleanup): Element Call joins the existing "call" and
   sends no notification → no trigger in `ring` mode; the owner can use `!sip call`. Documented.
9. **Early media then 200 with a different SDP**: diago re-applies the SDP on the same session; the leg is
   unchanged.
10. **180 with SDP** (non-standard early media): treated as no early media (ringback tone); diago only
    sets up early media on 183 (`dialog_client_session.go:310`).
11. **Owner leaves during the failure tone**: ignored (stEnding).
12. **Brand-new portal on the command path**: D18 delay.

---

## 7. Standalone host (`pkg/connector`, `cmd`, T5)

### 7.1 Commands (`commands.go`, registered in `main.go` with `WithSpan`)

All `RequiresLogin: true`, help section `commands.HelpSectionChats`. The login is the portal's receiver
when the command is sent in a portal room (`ce.Portal != nil`, look up `GetCachedUserLoginByID(
portal.Receiver)` and require it to belong to `ce.User`), else `ce.User.GetDefaultLogin()`.

| Command | Args | Behaviour | Reply |
|---|---|---|---|
| `call` | `<number>` (management room) or none (portal room → that room) | `Calls.PlaceCall` | `Calling <e164>. Answer the incoming call on any of your devices to connect.` / errors mapped: invalid number, not allowed, own number, line busy, outbound disabled, not logged in |
| `dtmf` | `<digits>` | `Calls.SendDTMF`; then (whatever the result), if `dtmf_redact_command`, redact the command event as the bot: `ce.Bot.SendMessage(ctx, ce.OrigRoomID, event.EventRedaction, &event.Content{Parsed: &event.RedactionEventContent{Redacts: ce.EventID}}, nil)` (the pattern of `bridgev2/portal.go:1733`; best effort, Warn on error without the content) | `Sent <n> tones.` / `No call in progress.` / `The call is not connected yet.` / `DTMF is not supported on this call.` / `Use 1-32 of 0-9 * # A-D.` (never echoes the digits) |
| `hangup` | none | `Calls.Hangup` | `Hanging up.` / `No call in progress.` |

Span names `cmd.call`, `cmd.dtmf`, `cmd.hangup` (TS §5.8); `cmd.dtmf` gets `dtmf.digit_count` after
validation. Arguments are never recorded or logged (bridgev2 itself logs only the command name,
`commands/processor.go:165`).

### 7.2 Wiring

- `start.go` `startCalls`: `calls.Config` gains `Dial: c.dial`, `LoginNumber: c.loginNumber`,
  `DialTimeout`, `OutboundTrigger`, `DialAllowedPrefixes`, `DialNumberFormat`, `AllowSelfDial` from config.
- `c.dial(login, req)`: find the `SIPClient` of the cached login (`ul.Client.(*SIPClient)`), take `cl.ua`
  under `cl.mu`; nil → `sipua.ErrNotRegistered`; else `ua.Dial(req)`.
- `c.loginNumber(login)`: the login metadata's `DisplayNumber`.
- `callhost.go`: `ResolveCallRoom` sets `Created: true` when it called `CreateMatrixRoom`;
  `LookupCallRoom`: `GetPortalByMXID`; nil, `Receiver != login`, or `ParseE164(portal.ID)` fails →
  `ErrNotCallRoom`; else the same ghost/encryption code as `ResolveCallRoom` (factor a helper).
- Login completion text: `"Registered %s via %s. Incoming calls ring your Matrix clients; start a call in a
  number's room or use `$cmdprefix call <number>` to call out."`.

### 7.3 Config (§9.1) and docs

README: status (outbound works), "Calls" section (how to call out on Element X/Sable/Element Web, the
callback ring, triggers and their caveats, dial policy, DTMF command, notices), config table, "Tracing
and metrics" (every new span/metric/attribute of §10). CLAUDE.md status line. PLAN Phase 3 ticks and
Current state. No new dependency, CREDITS unchanged.

---

## 8. Sequence diagrams

### 8.1 Owner-initiated, success (with 183 early media)

```
Owner client          Synapse/AS          calls actor              sipua            Bandwidth      LiveKit
 | press call             |                    |                      |                  |              |
 |-- call.member (live) ->|-- ingest --------->| detector: ownerLive   |                  |              |
 |-- rtc.notification --->|-- ingest --------->| trigger -> new actor  |                  |              |
 |                        |                    | call.outbound (root)  |                  |              |
 |                        |<- ghost call.member| LookupCallRoom, guards|                  |              |
 |  (ghost tile appears)  |                    | rtc.join ------------------------------------------->| joined
 |                        |                    | Dial ---------------->| INVITE ---------->|              |
 |                        |                    |                       |<-- 407 ----------|              |
 |                        |                    |<- EventAuthChallenged |-- INVITE+auth -->|              |
 |                        |                    |<- Provisional 100     |<-- 100 ----------|              |
 |                        |                    |<- Provisional 183+SDP |<-- 183 (SDP) ----|              |
 |                        |                    | bridge(Early) <== early RTP =============|              |
 |  hears carrier ringback|<===================| (ringback fill if RTP silent) ========================>|
 |                        |                    |<- EventAnswered       |<-- 200 OK -------|              |
 |                        |                    |                       |-- ACK ---------->|              |
 |                        |                    | bridge.Connect; end call.outbound (at ACK)|              |
 |  <==================== two-way audio =================================================>|              |
```

### 8.2 Remote busy / decline / no answer

```
calls actor             sipua               Bandwidth          Owner client
 | Dial ------------->  | INVITE ----------> |                     |
 |<- Provisional 180    |<-- 180 ------------|                     |
 | Tone(ringback) ===============================================> | hears ringback
 |<- Ended{RemoteRejected 486}|<-- 486 --------|                     |
 | stop ringback; Tone(busy) 2 s ================================> | hears busy
 | rtc.decline(ref owner notif), membership {}, rtc.leave, notice "Call failed: 486 Busy Here"
 |                                                                  | EC ends call (UNVERIFIED)
No answer: dialTimer 60 s -> Hangup -> CANCEL -> 487 -> Ended{LocalCancel 487}; reorder tone;
decline; notice "Call failed: no answer".
```

### 8.3 Owner cancels while dialing (and the 200 race)

```
Owner client            calls actor               sipua                 Bandwidth
 | leave: call.member {} | own, last device       |                        |
 |---------------------->| Hangup --------------->| cancel ctx -> CANCEL ->|
 |                       | Fail(cancelled)         |<-- 200 (CANCEL) -------|
 |                       | (waits <= 5 s)          |<-- 487 (INVITE) -------|
 |                       |<- Ended{LocalCancel,487}|                        |
 | notice "Cancelled call"; membership {}; rtc.leave; no tone, no decline
Race: |<-- 200 OK (INVITE) instead of 487 -> sipua ACK + BYE -> Ended{LocalCancel, 200}
```

### 8.4 Remote BYE

```
Bandwidth -- BYE --> diago (200 OK) ; dialog ctx done -> sipua Ended{RemoteBye}
actor: Teardown(remote, bye): media.stop, rtc.leave, membership {}, notice "Outgoing call · 2m 5s"
```

### 8.5 Owner leaves (connected)

```
Owner client -- call.member {} --> actor (last device) -> Teardown(owner, bye):
media.stop, sip.bye -> sipua BYE -> Ended{LocalHangup 200}, rtc.leave, membership {}, notice
```

### 8.6 `!sip call` (callback ring)

```
Owner (mgmt room)   connector cmd.call      calls actor              Owner devices        Bandwidth
 | !sip call 5551234567 | PlaceCall -> id    |                         |                    |
 |<- "Calling +1555..." |                    | ResolveCallRoom (create)|                    |
 |                      |                    | (new room: wait 3 s)    |                    |
 |                      |                    | ghost call.member, ring --> all devices ring  |
 |                      |                    | rtc.join (pre-join)     |                    |
 |                      |                    |<- owner call.member (answer)                  |
 |                      |                    | read receipt; dial() -- INVITE ------------->|
 |                      |                    | ... as 8.1 from 100 Trying ...                |
```

### 8.7 DTMF

```
Owner -- "!sip dtmf 1#" --> cmd.dtmf (dtmf.digit_count=2) -> Manager.SendDTMF -> actor evDTMF
actor: bridge.SendDTMF("1#") -> nil -> reply -> "Sent 2 tones." ; bot redacts the command message
rtcToSIP ticks: [1: 8 x telephone-event PT 101] [5 x audio] [#: 8 x telephone-event] -> Bandwidth
stats tick: DTMFSent delta 2 -> mrtcsip.dtmf.sent{rfc4733} += 2
```

---

## 9. Configuration

### 9.1 New `network:` keys (T5)

```yaml
# How the bridge recognises that you started a call in a number's room:
#   ring        your client's call notification plus your fresh call membership (Element X, Element Call)
#   membership  your fresh call membership alone (for clients that send no notification; may misfire if
#               a second device of yours joins an empty call)
#   off         only the "call" command places calls
outbound_trigger: ring
# Cancel an outgoing call that is not answered within this many seconds (10-300).
dial_timeout_seconds: 60
# Only numbers starting with one of these E.164 prefixes can be called. Empty list = any number.
# Emergency and short codes can never be called.
dial_allowed_prefixes: ["+1"]
# Request-URI user format for dialled numbers: digits (15551234567, JMP/Bandwidth) or e164 (+15551234567).
dial_number_format: digits
# Allow calling the account's own number (testing only: the call loops back through the provider).
allow_self_dial: false
# Redact "dtmf" command messages after sending the tones (they may contain PINs).
dtmf_redact_command: true
```

Validation: trigger in {ring, membership, off}; timeout 10..300; each prefix matches `^\+[1-9][0-9]{0,14}$`;
format in {digits, e164}. Defaults applied when absent (upgrader copies user values; a nil list stays the
default `["+1"]`, an explicit `[]` means any). `config_test.go` rows for each.

---

## 10. Telemetry (TS §5.7, §5.8, §8.2)

### 10.1 Spans

| Span | Kind | Root? | Emitted by | Attributes / events |
|---|---|---|---|---|
| `cmd.call`, `cmd.dtmf`, `cmd.hangup` | CONSUMER | root | connector `WithSpan` | `cmd.name`; `cmd.dtmf`: `dtmf.digit_count` |
| `call.outbound` | CLIENT | **linked root** → `cmd.call` or `matrixrtc.ingest` | calls | `call.id`, `call.direction=outbound`, `call.trigger` (command, owner_membership), `call.remote.e164`, `call.local.e164`, `matrix.room_id`, `matrix.ghost_id`, `matrix.owner_id`, `sip.call_id`, final `sip.response.status_code`, `call.outcome`, `call.end_reason`; event `sip.ack.sent` |
| `call.resolve_room` | INTERNAL | child-only | calls | `call.remote.e164` (or `matrix.room_id` for lookups) |
| `matrixrtc.membership.publish` / `.clear` | CLIENT | child-only | calls | as P2 |
| `matrixrtc.ring.send`, `call.ring`, `matrixrtc.ring.redact` | | child-only | calls | command path only, as P2 |
| `rtc.join` (+ children), `rtc.leave` | | child-only | rtc / calls | as P2 |
| `sip.invite` | CLIENT | child-only | calls | `sip.method=INVITE`, `sip.call_id`, `sip.auth.challenged`, `sip.response.status_code`, `sip.response.reason` (scrubbed); events `sip.response.received{sip.response.status_code}` (100, 18x, final), `sip.auth.challenge` |
| `call.ringback` | INTERNAL | child-only | calls | `sip.early_media`, `call.outcome` |
| `media.start` | INTERNAL | child-only (of `call.ringback` for early media, else `call.outbound`) | calls | `network.peer.address` |
| `matrixrtc.decline.send` | CLIENT | child-only | calls | `matrix.event_id` (the owner's notification) |
| `call.notice.send` | INTERNAL | child-only | calls | `call.outcome` |
| `call.teardown`, `call.session`, `call.owner.left`, `call.media.degraded`, `call.membership.refresh` | | linked roots | calls | as P2, `call.direction=outbound` on `call.session` |

`call.outbound` duration bound: callback ring (ring timeout) + join (≤ 21 s) + dial timeout (≤ 300 s) +
failure branch (≤ ~20 s, including the 2 s tone): under the 10 min cap (TS §5.10) with the config limits.
The `matrixrtc.ingest` span of the trigger event carries `matrixrtc.routed_to=call` and `call.id`.

### 10.2 Metrics

| Instrument | When | Attributes |
|---|---|---|
| `call.started` +1, `call.active` ±1 | outbound actor start / end (not for `PlaceCall` refusals) | `call.direction=outbound` |
| `call.ended` | end | `call.direction`, `call.outcome`, `sip.response.status_class` (of the final status **received**; `2xx` answered or raced; `none` if no INVITE was sent) |
| `call.setup.duration` | end | dial start (`dialStartedAt`) → 200 OK received; unanswered: trigger → end |
| `call.ring.duration` | end, if a provisional arrived | first 18x → 200 OK / failure (TS §4.3 outbound definition); the callback ring of the command path is not recorded here |
| `call.duration` | answered | ACK sent → teardown start; `media.codec` |
| `media.*` | as P2 (stats ticker only while Active) | |
| `dtmf.sent` | stats tick + final: delta of `Stats.DTMFSent` | `dtmf.method=rfc4733` (pre-built option) |

### 10.3 Logs

Info: "Owner started a call" (`to`, `room_id`, `trigger`), "Ringing owner for an outgoing call", "Dialing"
(`to`, `request_user`, `sip_call_id`), "Call progress" (`status`, `early_media`) once per distinct status,
"Call connected", "Call ended" (P2 fields + `direction`). Warn: best-effort failures, ignored triggers at
Debug. Never digits, SDP, raw SIP, tokens, event content.

---

## 11. Privacy

- DTMF digits: not in logs (connector, calls, media, sipua), spans, span events, errors, metrics. `cmd.dtmf`
  records only `dtmf.digit_count`. The Matrix command message is redacted by default (D14).
- Remote SIP reason phrases: only scrubbed on `sip.invite`/logs, never in notices (D15).
- Phone numbers stay span/log-only (allowlist test). The SIP username appears only as `sip.user.masked`;
  the From header is never logged.
- Tests (`pkg/calls/telemetry_test.go`, `pkg/connector/commands_test.go`): run a DTMF command with digits
  `9876#` and a full outbound call whose fake SIP reason phrase is `Busy access_token=SECRETTOKEN`;
  `obstest.AssertNoSecrets(t, rec, logBuf, "9876", "SECRETTOKEN")` and
  `AssertMetricAttrsAllowed(DefaultMetricAllowlist)`.

---

## 12. Tests and live test plan

### 12.1 Unit and component tests (offline, `-race`)

| Package | Tests |
|---|---|
| `pkg/matrixrtc` | §3.1 |
| `pkg/media` | §5.4 |
| `pkg/sipua` | `outbound_loop_test.go` with a fake `dialogClient` (fake clock): 100/180 → `EventProvisional`s in order; 401 → `EventAuthChallenged` once; early media → one `EventProvisional{EarlyMedia, Media}` then 200 → `Ack` before `EventAnswered` (order recorded); 486 → `EventEnded{EndRemoteRejected, 486, reason}`; `Hangup` before answer → invite ctx cancelled, `EndLocalCancel` 487; `Hangup` then `FinalStatus` 200 → `Ack` + `Bye`, `EndLocalCancel` 200, no `EventAnswered`; `Hangup` after answer → one `Bye`, `EndLocalHangup`; remote BYE → `EndRemoteBye`; "more than 10 responses" error → `WaitAnswer` re-entered; safety timeout → `EndFailed`; events channel closed after `EventEnded`; `Hangup` after end never blocks; provisional flood never blocks and never drops `EventEnded`. `offer_test.go`: `TelephoneEventPT` = 101 for the JMP-like offer. `outbound_loopback_test.go` (skip if 127.0.0.1 bind fails): a diago UAS on 127.0.0.1 (`diago.NewDiago` + `ServeBackground`) and our UA via `dialTo`: (a) UAS challenges with `DigestAuthServer.AuthorizeDialog` (401), then `Ringing`, `ProgressMedia` (183+SDP) and writes PCMU early RTP, then `AnswerEarlyMedia`; assert `EventAuthChallenged`, `EventProvisional{180}`, `EventProvisional{183, EarlyMedia}` with a leg that reads the early RTP, `EventAnswered` with the same leg, RTP both ways for 200 ms, UAS `Hangup` → `EndRemoteBye`; INVITE source port == UA `BindPort`; From user == the configured username; Request-URI user `15551230000` for target `+15551230000`. (b) UAS responds 486 → `EndRemoteRejected 486`. (c) UAS rings and never answers; our `Hangup` → UAS sees CANCEL (its dialog context ends cancelled) → `EndLocalCancel 487`. (d) after answer, our leg's `WriteEvent` packets for one digit are decoded by the UAS's `AudioReaderDTMF().OnDTMF` as that digit. |
| `pkg/calls` | `detect_test.go`: `ring` mode: membership then notification → one trigger; notification then membership within 5 s → trigger; after 5 s → none; stale notification (ts − 31 s) → none; membership not fresh (created 2 min ago) + notification → none; notification from a non-owner → none; second owner device joining an empty-of-calls room → none; event in the active call's room → routed, no trigger; trigger while another room has an active call → busy actor (decline + notice, no `Dial`); cooldown after a call end; `membership` mode: fresh first device → trigger, second device → none; `off` → none; `Dial == nil` → none; `HandleRTCEvent` never blocks (actor blocked in a fake host). `outbound_test.go` (fake `OutboundCall`, fake joiner/host/ghost/bridge/tone, fake clock): 1 owner-trigger happy path with 183 early media → membership content, join before `Dial` (order asserted), early bridge started with `Early: true`, `Connect` on answer, `call.outbound` ends at `AckSentAt`, remote BYE → teardown, notice `Outgoing call · 1m 20s`; 2 180 without SDP → ringback `StartTone(ToneRingback)`, stopped before the bridge starts; 3 486 → busy tone 2 s (fake clock), decline content references the owner notification, membership `{}`, leave, notice `Call failed: 486 Busy Here`, outcome busy; 4 603 / 404 / dial timeout (CANCEL issued) mapping per §6.6; 5 owner leaves while Early → `Hangup`, `Cancelled call`, no tone, no decline; 6 `EndLocalCancel` with status 200 → outcome cancelled, status class 2xx; 7 LiveKit join fails twice → no `Dial`, `Call failed: could not connect audio`, decline; 8 `ErrNotRegistered` → rejected; 9 self number / disallowed prefix / encrypted (owner trigger) → rejected with the notices, no `Dial`; 10 command path: `PlaceCall` → ring content and pre-join; owner answers → `Dial` once; owner declines → no `Dial`, `Call not placed`; ring timeout → `Call not placed: not answered`; `Created` room → membership after 3 s; 11 `PlaceCall` refusals (invalid, prefix, self, busy, disabled) create no actor and no metrics; 12 inbound INVITE while an outbound call is active → 486, `Missed call (line busy)`; 13 `!sip hangup` in Dialing → CANCEL, in Active → BYE; 14 `SendDTMF` before answer → `ErrNotConnected`, while Active → `bridge.SendDTMF` called with the digits, count returned; invalid digits rejected before the actor; inbound Active call also accepts DTMF; 15 shutdown in Dialing → CANCEL, `bridge_stop`; 16 every outbound span ≤ 10 min of fake time including a 300 s dial timeout. `telemetry_test.go`: trace shapes for owner-trigger success, 486, owner cancel, command-path success (`call.outbound` linked to `cmd.call`; children in order; `sip.invite` events 100/180/183/200 and `sip.auth.challenge`; `call.ringback` with `sip.early_media`); metrics (`call.started/ended{outbound}`, setup and ring durations once, `dtmf.sent`); privacy assertions (§11). `integration_test.go`: real `media.Start` early mode + `Connect` with fake legs, real `core.Ingest` fed an Element X style owner membership and notification fixture → trigger → fake `OutboundCall` scripted 183 (early leg yields a tone) → 200 → both legs carry tone → owner leave fixture → BYE. |
| `pkg/connector` | `commands_test.go`: `call` in a portal room without args uses that room; in the management room requires a number; error replies per §7.1; `dtmf` reply never contains the digits, redaction requested; `cmd.dtmf` span has no digits; `hangup`. `config_test.go`: new keys. `client_lifecycle_test.go`: `c.dial` with no UA → `ErrNotRegistered`; `LookupCallRoom` for a non-portal room → `ErrNotCallRoom`. |

### 12.2 Live test plan (user)

Environment: as Phase 2 (bridge in the example.org compose stack, double puppeting, OTLP on). Owner
clients: Element X Android, Sable web, Sable Android.

**Safety rules for every outbound test (must be followed literally):**

1. The user's personal number **+15550001234** is never dialled without warning the user first and
   waiting for their explicit confirmation in the session. It is only a fallback (see destinations).
2. Before each test, check the bridge config `dial_allowed_prefixes` and that `allow_self_dial` is false.
3. Tests are started by the user pressing call or typing the command; the agent never triggers calls.

**Destinations:**

| Destination | Use | Status |
|---|---|---|
| **+17377273232** (preferred first destination) | Listed on testnumber.org as a "Test PSTN Number" backed by Twilio functions: on a voice call it reads back the calling number (caller ID), the STIR/SHAKEN attestation and robocall flagging, and has a DTMF check. The user has used it for SMS tests with the jabber bridge. | **UNVERIFIED for voice with this account** (the site says it answers calls; exact prompts, call length and whether it hangs up itself are unknown). Ideal for caller-ID (JMP number expected, not the SIP username), remote BYE and DTMF tests. |
| +15555550100 (fictional 555-01xx range) | Failure-notice test: expected a carrier error (404/484/503) or a 183 intercept message; no person can answer | UNVERIFIED |
| +15550001234 (user's personal mobile) | Fallback for answer/talk/hang-up-from-remote/busy tests only **after warning the user and waiting for confirmation** | allowed only with confirmation |
| +15550004321 (the JMP number itself) | Avoid. The call leaves via Bandwidth and comes back to JMP, which forks it to every registration: the bridge itself answers its own INVITE with 486 (one call per login), the ATA rings if registered, and JMP voicemail may answer. That is a loop that tests nothing reliably and burns minutes; refused unless `allow_self_dial: true`. Optional only to observe the busy path, with the user's consent. | not recommended |

| # | Action | Expect |
|---|---|---|
| 0 | Restart the bridge while the owner is in no call | No call starts (no stale trigger); Info log of config `outbound_trigger=ring`. |
| 1 | Element X: open the +17377273232 portal (create with `!sip start-chat +17377273232` first), press the call button | Within ~2 s the ghost tile appears; ringback (generated or carrier) audible; answer by the test service; caller ID read back is the **JMP number** (+15550004321), not the SIP username; two-way audio; the service hangs up (or the owner does) → notice `Outgoing call · …`. Trace: `call.outbound` (linked to `matrixrtc.ingest`), `sip.invite` with `sip.auth.challenge` if challenged, `call.ringback`, ending at ACK; `call.teardown`, `call.session`. If Bandwidth answers 403/404: record it and try `dial_number_format: e164`. |
| 2 | Same, use the service's DTMF check with `!sip dtmf 123#` in the portal | The service recognises the digits; the bot replies `Sent 4 tones.` and the command message is redacted; logs and traces contain no digits; `mrtcsip.dtmf.sent` += 4. |
| 3 | Sable web: press call in the same portal, hang up after 3 s of ringback | CANCEL sent; notice `Cancelled call`; no decline; `call.ended{outcome=cancelled}`. |
| 4 | `!sip call +17377273232` in the management room (Element X on the phone) | Reply `Calling …`; the phone rings with an incoming call from the number's ghost; answer → ringback → connected. Repeat with decline → `Call not placed`, nothing dialled (no `sip.invite`). |
| 5 | `!sip call +15555550100` | Callback ring, answer → carrier failure or intercept recording (early media audible), then `Call failed: <reason>` with reorder tone; Element X call screen: record whether it closes on the ghost's decline (D11 UNVERIFIED). |
| 6 | Owner leaves while connected (hang up in Element X) | BYE sent immediately; notice. |
| 7 | While connected outbound, call the JMP number from another phone (only if a second phone is available) | Second caller gets busy (486); `Missed call (line busy)` in that room. |
| 8 | During a ringing inbound call to room A, press call in room B | No dial; `Call not placed: another call is in progress` in room B; the owner's call in B ends (decline) or is left alone (record). |
| 9 | (Fallback, **only after warning the user and receiving confirmation**) call +15550001234 and let the user reject it on the mobile | `Call failed: 486 Busy Here` or `603 Decline` with busy/reorder tone; record what Bandwidth returns for a rejected mobile call and whether voicemail answers instead (then it is an answered call). |
| 10 | Membership-mode check (optional): set `outbound_trigger: membership`, press call | Same as test 1; then join the empty call from a second device after it ended → no new call. |
| 11 | Metrics backend | `mrtcsip.call.*{call.direction=outbound}`, `mrtcsip.dtmf.sent` present; no numbers in metric attributes. |

Record the outcome in PLAN and tick PLAN Phase 3.

---

## 13. Edits to `docs/TELEMETRY-SPEC.md` (flagged, already applied with this spec)

1. §5.2: `call.end_reason` adds `remote_reject` (outbound: the remote answered the INVITE with a final
   non-2xx); new keys `sip.early_media`, `dtmf.digit_count`, `dtmf.method`.
2. §5.7: the tree shows the ghost membership publish, the command-path ring (`matrixrtc.ring.send`,
   `call.ring`), `sip.response.received` events, and the failure branch with `matrixrtc.decline.send` and
   `matrixrtc.membership.clear`; `call.outbound` ends at ACK **sent**.
3. §8.2 Phase 3 bullet: adds `matrixrtc.decline.send`.

No existing span or metric name changes.

---

## 14. Work split

```
T1 (contracts: matrixrtc, core, obs, obstest, sipua/media/calls API stubs, fakes compile)
 ├── T2 (pkg/media: tones, early mode, DTMF)          ┐
 ├── T3 (pkg/sipua: outbound dial, SIPEventLeg)       ├─ parallel, all code against T1's API
 └── T4 (pkg/calls: outbound actor, detector, cmds)   ┘  (fakes only; integration test needs T2)
T5 (connector commands + wiring, config, docs, go mod tidy)    after T2, T3, T4
Opus review of the full diff                                    after T5
Live test (user, §12.2)                                         after review fixes
```

Recommended tiers: T1 Sonnet, T2 Sonnet, **T3 Opus** (two-goroutine dialog loop, CANCEL/200 race),
**T4 Opus** (actor state machine, detector false-trigger rules, trace shape), T5 Sonnet. Rules for every
task: edit only the files you own (§2); do not change any (API) file's exported surface (stop and
report); do not commit; do not touch `spikes/`; no new dependencies; every exported identifier has a doc
comment; no emojis; never log digits, SDP, raw SIP or secrets.

Acceptance for every task: the three commands of §2 pass for the whole module (if a sibling's package is
unfinished, run them for yours and say so), and `go test -race -count=1 -tags "nocrypto nolibopusfile"
./pkg/<yours>/...` passes offline.

### T1: contracts

Files: `pkg/matrixrtc/{events,membership,membership_test}.go`, `pkg/core/callhost.go`,
`pkg/obs/{attrs,metrics,obs_test}.go`, `internal/obstest/obstest.go`, `pkg/sipua/{outbound,inbound}.go`,
`pkg/sipua/diago.go` (interface method + stub only), `pkg/media/media.go` (+ stubs in `bridge.go`:
`Connect`, `SendDTMF`), `pkg/calls/manager.go` (Config fields, method stubs returning
`ErrOutboundDisabled`/`ErrNoActiveCall`, error vars), minimal stub methods in
`pkg/calls/fakes_test.go` (`fakeBridge.Connect/SendDTMF`, `fakeHost.LookupCallRoom`) and
`pkg/connector/client_lifecycle_test.go` (`fakeUA.Dial`), and `pkg/connector/callhost.go`
(`LookupCallRoom` returning `core.ErrNotCallRoom` as a stub so the interface is satisfied).
Spec: §3. Acceptance: build/vet/test pass; `go doc ./pkg/sipua` and `./pkg/media` show no diago/sipgo/
pion/hraban types; the `org.matrix.msc` grep rule holds.

### T2: media

Files: `pkg/media/*` except `media.go`'s exported surface. Spec: §5. Acceptance: §5.4 tests; `go test
-bench . -benchmem -run ^$ ./pkg/media/` reports 0 allocs/op; `grep -rn "obs\." pkg/media` finds nothing.

### T3: SIP outbound

Files: `pkg/sipua/{outbound_loop,outbound_diago,diago,inbound_diago}.go`,
`pkg/sipua/{outbound_loop,outbound_loopback,offer}_test.go`. Spec: §4. Acceptance: tests including the
loopback cases (a)-(d); `grep -n "SIPDebug" pkg/sipua/*.go` finds nothing; `Body()` is only passed to
`parseOffer`; no log line carries headers, SDP or credentials; the INVITE source-port assertion passes
(or the task stops and reports, §4.1).

### T4: calls

Files: `pkg/calls/*` (T1's exported signatures fixed). Spec: §6, §10, §11, calls rows of §12.1.
Acceptance: all calls tests with `-race -count=3`; no mutex guards call state; no `time.Now()`,
`time.After`, `time.NewTimer` outside tests (everything via `cfg.Clock`); the detector code path does no
I/O (`HandleRTCEvent` calls only `OwnerMXID`, parsing and map operations); P2 inbound tests still pass
unchanged except mechanical renames.

### T5: host wiring, commands, docs

Files: `pkg/connector/{commands,start,client,callhost,config,login}.go`,
`pkg/connector/example-config.yaml`, `pkg/connector/{commands,config,client_lifecycle}_test.go`,
`cmd/mrtc-sip-bridge/main.go`, `README.md`, `CLAUDE.md`, `docs/PLAN.md`; `go mod tidy` (no `go.mod`
change expected). Spec: §7, §9. Acceptance: whole-module build/vet/test (`-race`); `./mrtc-sip-bridge -e`
output contains every §9.1 key with its default; `!sip help` lists `call`, `dtmf`, `hangup`; README
"Tracing and metrics" lists every span, attribute and metric of §10; `./build.sh` succeeds.

### Final (coordinator)

Clean build/vet/test with `-race`; Opus review against this spec (state machine, detector rules, race
handling, trace shape, privacy); live test §12.2; PLAN Current state.

---

## 15. Open questions for the user (defaults chosen)

1. **Trigger default `ring`**: calls start only when your client sends its call notification (Element X
   and Element Call do in DMs). If a client of yours does not, set `outbound_trigger: membership` or use
   `!sip call`. Default: `ring`.
2. **`!sip call` rings your devices first** and dials when you answer (callback). Alternative: just open
   the room and let you press call. Default: callback ring.
3. **Dial policy**: `dial_allowed_prefixes: ["+1"]` (US/Canada only), `dial_number_format: digits`
   (11 digits, as the 2021 Asterisk config for JMP). Emergency/short codes always refused.
4. **Plain digits as DTMF**: typing `123#` in the portal during a call (no `!sip dtmf` prefix) would be
   nicer on mobile. Deferred (needs a careful `HandleMatrixMessage` change). Want it in Phase 3?
5. **DTMF command redaction** on by default. OK?
6. **+17377273232 as the first live-test destination** (testnumber.org, Twilio; voice behaviour
   UNVERIFIED); personal number only after an explicit warning and your confirmation.

## 16. Deviations and known limitations

- **L7 (known UX limitation).** When an outbound call started by the bot command path fails (no
  owner call was ringing, so no decline can be sent), the bridge sends no `m.rtc.decline`. Element X
  may therefore stay in an empty call until the user leaves it manually.
- **Destination validation.** `+1` numbers must be valid NANP numbers (12 chars, area code and
  exchange start with 2-9 and are not N11, area codes 900 and 976 refused); the check runs in
  `PlaceCall`, in `setupOutbound` (notice `noticeOutNotAllowed`, with decline) and for `call` in a
  number's room via `Manager.CheckDestination`. The encrypted-room guard is not pre-checked there.
- **Same-room trigger.** An owner trigger while the active call is in the same room (or its room is
  not yet resolved) is refused silently ("line busy, same room"), with no busy actor.

---

## 17. Fix: the bridge bot leaves 1:1 number portals (ADR-010)

> **Not implemented: rejected (see ADR-010).** Element X's outbound "ringing forever" is an upstream
> Element Call bug (open TODO to respect `io.element.functional_members`). It's documented as a known
> limitation in the README. This section is kept for reference only.

Status: **ready for implementation** (2026-10-17). Read ADR-010 for the why. This section is the
how. It changes D14: in a number portal that the bot has left, the `dtmf` redaction and command replies
are sent by the ghost. No Go code in `pkg/calls`, `pkg/core` or the core packages changes. Everything is
in `pkg/connector` plus `cmd/mrtc-sip-bridge/main.go`.

Pinned references (mautrix v0.32.0, `~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0`):

| What | Where |
|---|---|
| `Portal.CreateMatrixRoom` (synchronous, waits for the portal loop) | `bridgev2/portal.go:5374` |
| Room created by the bot, bot PL 9001, functional members | `bridgev2/portal.go:5420`, `:5464-5475`, `:5503-5507`, `:5582` |
| `syncParticipants` ignores the bot's own membership, invites as the bot | `bridgev2/portal.go:4849`, `:4866`, `:4877` |
| Ghost-DM invite path (bot invited, joined, given power) | `bridgev2/matrixinvite.go:122`, `:200`, `:206`, `:221`, `:251` |
| `start-chat`/provisioning create path | `bridgev2/provisionutil/resolveidentifier.go:113-115` |
| `Bridge.GetAllPortalsWithMXID` | `bridgev2/portal.go:280` |
| `Bridge.BackgroundCtx` | `bridgev2/bridge.go:63` |
| `MatrixConnector.GetPowerLevels`, `GhostIntent`, `BotIntent` | `bridgev2/matrixinterface.go:42-73` (impl `bridgev2/matrix/connector.go:644`) |
| `matrix.Connector.Bot *appservice.IntentAPI`, `.StateStore *sqlstatestore.SQLStateStore` | `bridgev2/matrix/connector.go:62-64` |
| `SQLStateStore.GetMembership` (missing row → `leave`) | `sqlstatestore/statestore.go:117` |
| `IntentAPI.LeaveRoom(ctx, roomID, extra ...any)` (`*mautrix.ReqLeave` for a reason) | `appservice/intent.go:311` (state store updated: `client.go:1721`) |
| `IntentAPI.EnsureJoined` (Forbidden → bot invites, then retries) | `appservice/intent.go:95-145` |
| `PowerLevelsEventContent.Clone`, `GetUserLevel`, `EnsureUserLevelAs`, `GetEventLevel` | `event/powerlevels.go:45`, `:122`, `:174`, `:192` |
| `PowerLevelOverrides.Apply(actor, content) bool` | `bridgev2/portal.go:4367` |
| `commands.Event.Bot` (replies use it: `ReplyAdvanced`) | `bridgev2/commands/event.go:27`, `:59`; set at `commands/processor.go:117` |
| `EventProcessor.On` (append) / bridgev2 runs it `Sync` | `appservice/eventprocessor.go:63`; `bridgev2/matrix/connector.go:135` |

### 17.1 Config (`pkg/connector/config.go`, `example-config.yaml`, README)

- New field: `BotLeavesPortals *bool` with tag `yaml:"bot_leaves_portals"` (nil = true), next to
  `DTMFRedactCommand`. In `ApplyDefaults`, nil becomes `true` (same pattern as `DTMFRedactCommand`). Add
  the accessor `func (c *Config) BotLeavesPortalRooms() bool { return c.BotLeavesPortals == nil || *c.BotLeavesPortals }`.
- Upgrader: `h.Copy(configupgrade.Bool, "bot_leaves_portals")` after `dtmf_redact_command`.
- `example-config.yaml`, after `dtmf_redact_command`:
  ```yaml
  # Make the bridge bot leave 1:1 number rooms (the number's ghost manages them). Needed for calls you start
  # in Element X: with the bot in the room, Element X may keep "ringing" and hang up after 30 s.
  # Setting this to false makes the bot rejoin those rooms at the next start.
  bot_leaves_portals: true
  ```
- README config table: a matching row. In the `!sip dtmf` paragraph, replace "the bot needs permission
  to redact" with "the number's ghost (or the bot in the management room) redacts it". Add a short
  "Bot membership" note: the bot leaves number rooms, so bridge commands other than
  `call`/`hangup`/`dtmf` get no reply there. Use the management room for them.

### 17.2 Shared PL overrides (`pkg/connector/chatinfo.go`)

Pull the `PowerLevelOverrides` literal out of `buildChatInfo` into
`func callMemberPowerOverrides() *bridgev2.PowerLevelOverrides` (same content: `Events{matrixrtc.CallMember: 0,
matrixrtc.CallMemberAlias: 0}`) and use it in `buildChatInfo`. Don't add a `PowerLevel` to the ghost's
`ChatMember`. If you did, bridgev2's `syncParticipants` would try to "fix" the ghost's level as the bot
later (and would *lower* a ghost-DM ghost from 99).

### 17.3 New file `pkg/connector/portalbot.go`

```go
const (
	ghostPortalPowerLevel = 50               // ADR-010: enough to redact, kick, revoke and re-invite the bot
	botLeaveQuiet         = 10 * time.Second // debounce after the last m.room.member in a room
	botLeaveReason        = "Not needed in call rooms"
)

// portalRoomOps is the Matrix side of the bot-eviction logic (a seam for tests).
type portalRoomOps interface {
	BotMXID() id.UserID
	GhostMXID(e164 string) id.UserID
	Membership(ctx context.Context, roomID id.RoomID, userID id.UserID) (event.Membership, error)
	PowerLevels(ctx context.Context, roomID id.RoomID) (*event.PowerLevelsEventContent, error)
	SendPowerLevelsAsBot(ctx context.Context, roomID id.RoomID, pl *event.PowerLevelsEventContent) error
	BotLeave(ctx context.Context, roomID id.RoomID, reason string) error
	BotJoinViaGhost(ctx context.Context, roomID id.RoomID, e164 string) error // rollback: ghost invites, bot joins
	GhostEnsureJoined(ctx context.Context, roomID id.RoomID, e164 string) error
	GhostRevokeMembership(ctx context.Context, roomID id.RoomID, e164 string, target id.UserID, reason string) error
	GhostIntent(ctx context.Context, e164 string) (bridgev2.MatrixAPI, error)
}
```

**`bridgeRoomOps{c *SIPConnector}`** (the production `portalRoomOps`). Every method type-asserts
`c.br.Matrix.(*matrix.Connector)` (as `callhost.roomFor` already does) and returns an error if that
fails.
- `BotMXID`: `c.br.Bot.GetMXID()`. `GhostMXID`: `c.br.Matrix.GhostIntent(MakeUserID(e164)).GetMXID()`.
- `Membership`: `mc.StateStore.GetMembership(ctx, roomID, userID)`.
- `PowerLevels`: `c.br.Matrix.GetPowerLevels(ctx, roomID)` (cache first, falls back to a fetch as the bot).
- `SendPowerLevelsAsBot`: `c.br.Bot.SendState(ctx, roomID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{})`.
- `BotLeave`: `mc.Bot.LeaveRoom(ctx, roomID, &mautrix.ReqLeave{Reason: reason})`.
- `GhostIntent`: `ghost, err := c.br.GetGhostByID(ctx, MakeUserID(e164))`, return `ghost.Intent`.
- `GhostEnsureJoined`: `GhostIntent(...).EnsureJoined(ctx, roomID)` (works while the bot is still joined,
  because the bot invites on Forbidden).
- `GhostRevokeMembership`: ghost intent `SendState(ctx, roomID, event.StateMember, target.String(),
  &event.Content{Parsed: &event.MemberEventContent{Membership: event.MembershipLeave, Reason: reason}}, time.Time{})`.
- `BotJoinViaGhost`: ghost intent `SendState(… StateMember, botMXID, Membership: invite …)`, then
  `mc.Bot.EnsureJoined(ctx, roomID)`.

**`isNumberPortal(p *bridgev2.Portal) bool`** returns true when `p != nil && p.MXID != "" && p.Receiver != "" &&
p.RoomType == database.RoomTypeDM && phone.IsE164(string(p.ID))`.

**`evictBot(ctx, ops, roomID, e164) (botOutcome, error)`** is a pure function: no bridge globals, no
logging. Outcomes: `botAbsent`, `botLeft`, `botInviteRevoked`, `botKept` (could not leave safely).
Steps, in order, and **the bot never leaves unless every earlier step succeeded**:
1. `m, err := ops.Membership(roomID, bot)`. If err, return `botKept, err`. For `leave`, `ban` or `knock`,
   return `botAbsent`. For `invite`, call `GhostRevokeMembership(roomID, e164, bot, botLeaveReason)` and
   return `botInviteRevoked` (or `botKept, err`). For `join`, go on.
2. Ghost joined: if `Membership(roomID, ghost) != join`, call `GhostEnsureJoined`. On error, return
   `botKept, err`. (A room without the ghost has no bridge member left after the bot goes, so the bot
   stays.)
3. `pl, err := ops.PowerLevels(roomID)`. On error or nil, return `botKept`. Then `np := pl.Clone()`,
   `changed := callMemberPowerOverrides().Apply(bot, np)`. If `np.GetUserLevel(ghost) < ghostPortalPowerLevel`,
   then `changed = np.EnsureUserLevelAs(bot, ghost, ghostPortalPowerLevel) || changed`. Never lower the
   ghost: only call `EnsureUserLevelAs` when it's below 50.
4. Check: `np.GetUserLevel(ghost) >= ghostPortalPowerLevel` and
   `np.GetEventLevel(matrixrtc.CallMember) <= np.GetUserLevel(ghost)`. If either is false, return
   `botKept, errors.New("bot cannot grant the ghost enough power")`.
5. If `changed`, call `SendPowerLevelsAsBot(roomID, np)`. On error, return `botKept, err`.
6. `BotLeave(roomID, botLeaveReason)`. On error, return `botKept, err`. Then return `botLeft`.

**`restoreBot(ctx, ops, roomID, e164) (bool, error)`** is the rollback, used only by the sweep when the
config is false. If the bot's membership is `join`, it does nothing. Otherwise it calls `BotJoinViaGhost`.

**`botEvictor`** (one per connector):
```go
type botEvictor struct {
	ops     portalRoomOps
	enabled func() bool // c.Config.BotLeavesPortalRooms
	portal  func(ctx context.Context, roomID id.RoomID) (*bridgev2.Portal, error) // c.br.GetPortalByMXID
	bgCtx   func() context.Context // c.br.BackgroundCtx with the logger attached
	clk     clock.Clock
	quiet   time.Duration
	log     zerolog.Logger // clean, component=portal_bot

	runMu   sync.Mutex // serialises evictBot/restoreBot calls (all paths)
	mu      sync.Mutex
	pending map[id.RoomID]time.Time // deadline per room
}
```
- `ensureOut(ctx, p *bridgev2.Portal)`: if `!enabled() || !isNumberPortal(p)`, return. Then lock
  `runMu`, run `evictBot(ctx, ops, p.MXID, string(p.ID))`, and log through `obs.Logger(ctx, &e.log)`:
  Info "Bridge bot left call portal" (room_id, remote E.164) for `botLeft`/`botInviteRevoked`, Warn with
  the error for `botKept`, nothing for `botAbsent`. It never returns an error, and the caller never fails
  because of it.
- `touch(roomID)`: called on the appservice event path, so **no I/O and no blocking**. If disabled or
  `roomID == ""`, return. Lock `mu` and set `pending[roomID] = clk.Now().Add(quiet)`. If the room was not
  already pending, start `go e.wait(roomID)`.
- `wait(roomID)`: loop. Read the deadline under `mu`. If `d := deadline - clk.Now(); d > 0`, wait on
  `clk.NewTimer(d).C()` (stop the timer afterwards) and loop. Otherwise delete the entry, unlock, and
  run: `p, err := e.portal(bgCtx(), roomID)`. On err, log Warn. Otherwise call `ensureOut(bgCtx(), p)`. Because the
  deadline is re-read after every wake-up, there's no `Reset` race.
- `sweep(ctx)`: `portals, err := c.br.GetAllPortalsWithMXID(ctx)` (inject it as a func field like
  `portal`). For each `isNumberPortal`: when enabled, `ensureOut`; when disabled, `restoreBot` (log Info
  "Bridge bot rejoined call portal" on a rejoin). Run them one after another. At the end, log one Info line
  with counts (`left`, `absent`, `kept`, `restored`).

### 17.4 Wiring

- `SIPConnector` gets `bots *botEvictor`. In `Init` (`connector.go:45`, after `c.br = br`), create it
  with `bridgeRoomOps{c}`, `clock.Real()`, `botLeaveQuiet`, and
  `log: br.Log.With().Str("component", "portal_bot").Logger()`. Every caller is nil-safe:
  `if c.bots != nil`.
- New method `func (c *SIPConnector) RegisterPortalBotHandlers(reg core.EventRegistrar)`, which calls
  `reg.On(event.StateMember, func(_ context.Context, evt *event.Event) { c.bots.touch(evt.RoomID) })`.
  In `cmd/mrtc-sip-bridge/main.go` `PostInit`, call it right after `c.RegisterMatrixRTCHandlers(...)`.
- New method `func (c *SIPConnector) SweepPortalBots(ctx context.Context)` (calls `c.bots.sweep`). In
  `main.go` `PostStart`, add `go c.SweepPortalBots(br.Log.WithContext(context.Background()))`.
- `callhost.ResolveCallRoom` (`callhost.go:36`): after the `if portal.MXID == ""` block (whether the room
  was created or existed already), add `h.c.bots.ensureOut(ctx, portal)` before `h.roomFor(...)`. Inbound
  setup and the `!sip call` path both get here (from `calls/setup.go:228`), so a brand-new portal has no
  bot before its first ring, and the D18 3 s delay comes after this. `LookupCallRoom` stays as it is:
  there it would be too late for the current call, and the watcher has already handled the room.
- Commands (`commands.go` `RegisterCommands`): wrap all three handlers as
  `Func: WithSpan("call", c.inPortalAsGhost(c.cmdCall))` (same for `dtmf` and `hangup`). New:
  ```go
  // inPortalAsGhost makes replies (and the dtmf redaction) come from the number's ghost when the command
  // is sent in a number portal the bridge bot has left (ADR-010). ce.Bot is restored afterwards.
  func (c *SIPConnector) inPortalAsGhost(fn func(*commands.Event)) func(*commands.Event)
  ```
  The swap happens only if `c.bots != nil`, `isNumberPortal(ce.Portal)`, and
  `ops.Membership(ce.Ctx, ce.Portal.MXID, ops.BotMXID())` is not `join`. Then
  `gi, err := ops.GhostIntent(ce.Ctx, string(ce.Portal.ID))`. If there's no error, swap
  `ce.Bot = gi` and `defer` the restore. In every other case (including errors), `ce.Bot` stays as it is.
  `redactCommand` itself doesn't change. Update its doc comment to "as ce.Bot (the bridge bot, or the
  number's ghost in a portal the bot has left)". In the PHASE3 table, add "(see §17)" after D14.

### 17.5 What stays the same (check, don't change)

- The ring, ring redaction, decline, notices and `call.member` are already sent as the ghost
  (`ghostintent.go`, `teardown.go:146`, `callhost.SendCallNotice`). Read receipts go through the double
  puppet (`MarkReadAsOwner`).
- MatrixRTC events still reach the bridge: the ghost is joined, so the appservice keeps getting the room.
  `resolveRTCLogin` (`rtchook.go:20`) and the owner-trigger detector don't change.
- bridgev2 still sends commands sent in a portal to the processor (`bridgev2/queue.go:108-118`), whether or
  not the bot is in the room.

### 17.6 Tests (offline, `-race`, `-tags "nocrypto nolibopusfile"`)

New file `pkg/connector/portalbot_test.go` with a `fakeRoomOps` (memberships map, PL, a call log with
order, injectable errors). Use realistic PLs: bot 9001, `redact`/`invite`/`kick` 50, the `call.member`
overrides present unless the case says otherwise.

1. Bot joined, ghost joined at PL 0: one PL send (ghost = 50, `call.member` levels 0), then `BotLeave`,
   in that order. Outcome `botLeft`.
2. Ghost-DM room (owner 100, bot 100, ghost 99, no `call.member` overrides): the PL send adds the
   overrides, **ghost stays 99**, then the bot leaves.
3. Ghost ≥ 50 and overrides already present: no PL send, then leave.
4. The PL send fails: no `BotLeave`, `botKept`, error returned.
5. Bot `leave`/`ban`: no calls at all, `botAbsent`.
6. Bot `invite`: `GhostRevokeMembership(bot)` called, no leave, `botInviteRevoked`.
7. Ghost not joined and `GhostEnsureJoined` fails: no PL, no leave, `botKept`.
8. The bot can't grant (bot 0, ghost 0, no creator): `botKept`, no send, no leave.
9. Room v12: `CreateEvent` sent by the bot with `room_version: "12"`, bot not in `users`. The grant works
   (creator actor), then the bot leaves.
10. `isNumberPortal` table: nil, empty MXID, empty Receiver, non-E.164 ID, `RoomTypeDefault` → false. A
    DM with an E.164 ID → true.
11. `botEvictor.touch` with `clock.NewFake`: touches at t0 and t0+5 s; advance to t0+14 s → nothing ran;
    advance past t0+15 s → `ensureOut` ran exactly once for that room. Disabled → no goroutine and no run.
    Use a `portal` func returning a number portal, and `WaitForWaiters`.
12. `sweep`: enabled → `ensureOut` for number portals only. Disabled → `restoreBot` calls
    `BotJoinViaGhost` only where the bot isn't joined.
13. `inPortalAsGhost`: bot absent → the handler sees the ghost `MatrixAPI`, and the original `ce.Bot` is
    back afterwards. Bot joined → no swap. `ce.Portal == nil` (management room) → no swap. `GhostIntent`
    error → no swap. Use a stub embedding `bridgev2.MatrixAPI` that implements only `GetMXID`.
14. `config_test.go`: the default is `true`, an explicit `false` is kept, and the upgrader copies
    `bot_leaves_portals`.
15. `chatinfo_test.go`: `buildChatInfo` still carries `callMemberPowerOverrides()` (adjust the existing
    assertion if it compares the literal).

### 17.7 Telemetry and privacy

No new spans or metrics. The startup sweep and the watcher run outside any span, and `ensureOut` uses
`obs.Logger(ctx, &e.log)`, so its lines are correlated when it runs under `call.setup` from
`ResolveCallRoom`. Logs may include room IDs and E.164 numbers, never event content. TELEMETRY-SPEC and
README "Tracing and metrics" don't change.

### 17.8 Migration (existing portals)

The startup sweep (17.3) handles it on the first start of the new build. For each number portal, steps
1-6 of `evictBot` run. In bot-created rooms that's one PL event (ghost 0→50) and one leave. Rooms where
anything fails keep the bot and log a Warn, and the next start tries again. Because of the debounce, the
watcher doesn't race with the sweep (both take `runMu`, and `evictBot` is idempotent). Rollback: set
`bot_leaves_portals: false` and restart. The sweep has each ghost (PL 50 ≥ `invite`) invite the bot back,
and the bot keeps its stale PL 9001 or its creator power. Nothing is written to the bridge DB.

### 17.9 Live re-test checklist (example.org, Element X Android first)

1. Deploy and start. The log shows one "Bridge bot left call portal" per existing number portal and a
   sweep summary with `kept = 0`.
2. In Element X, the existing portal (+17377273232) now shows only you and the ghost as members. The
   timeline shows "sipbot left", and the ghost is a Moderator.
3. **The original bug:** press call in that portal. After the PSTN answer, Element X stops "ringing" and
   shows the connected call within about 1-2 s. The call lasts well past 30 s. Hang up in Element X →
   BYE, notice "Outgoing call · …".
4. Repeat 3 on Element X iOS and Element Web/Desktop if you have them. Sable: the call still works.
5. Create a new portal with `!sip pm <number>` (or `start-chat`) in the management room. About 10 s
   later the bot leaves. Press call → as in 3.
6. Create a new portal by starting a DM with `@sip_<number>:example.org`. About 10 s later the bot
   leaves. (If the client made the DM encrypted, audio is silent: ADR-002, a known issue.)
7. `!sip call <new number>` in the management room. The portal is created with no bot before the ring.
   The callback ring arrives, you answer, it's connected.
8. Inside a portal, during a call: `!sip dtmf 1` → reply "Sent 1 tone." **from the ghost**, and your
   command message gets redacted (by the ghost). `!sip hangup` → reply from the ghost.
9. Inbound regression: call the JMP number from your mobile. It rings in Element X and Sable, you answer,
   two-way audio, hang up both ways. Decline in Element X → 603. Ring-out → 480.
10. Restart the bridge. The sweep summary shows everything `absent`, and no new member events.
11. `!sip help` inside a portal gets no reply (known, ADR-010). In the management room it works.
12. Optional rollback check: `bot_leaves_portals: false`, restart → the bot rejoins every number portal.
    Set it back to `true`, restart → it leaves again.
