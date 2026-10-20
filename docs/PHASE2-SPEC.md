# Phase 2 implementation spec: inbound calls MVP

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Status: **ready for implementation** (written 2026-10-17, branch `phase2-inbound`).
Audience: implementer agents. Follow it literally. Where this spec and your own taste differ, this spec
wins. If something here does not compile against the pinned versions, stop and report it rather than
redesigning.

Inputs: `docs/PLAN.md` (Phase 2, ring hygiene, risks), ADR-002/003/006/009,
`docs/spikes/S1-matrixrtc-wire-spec.md` (authoritative wire formats; cited as "wire §N"),
`docs/spikes/S2-sip-ua-design.md`, `docs/TELEMETRY-SPEC.md` (cited as "TS §N"), `docs/PHASE1-SPEC.md`
(conventions and existing contracts), the Phase 1 code, and the spikes
`spikes/s1-rtc` and `spikes/s2-sip`.

Library references were checked against the module cache:
`~/go/pkg/mod/github.com/emiago/diago@v0.41.0`, `.../sipgo@v1.6.0`,
`~/go/pkg/mod/github.com/livekit/server-sdk-go/v2@v2.18.1` (see D3 for why not v2.19.0),
`~/go/pkg/mod/gopkg.in/hraban/opus.v2@v2.0.0-20230925203106-0188a62cb302`,
`~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0`.

---

## 0. Scope

In scope:

1. Inbound SIP INVITE on a registered login → caller number → E.164 → portal room via `CallHost`.
2. Ring every owner client: ghost `call.member`, then the MSC4075 ring with `lifetime` = ring timeout.
3. Pre-join LiveKit as the ghost while ringing (S1 decision), publish one Opus mic track.
4. Owner answers (owner `call.member` through the S3 hook / `core.Ingest`) → SIP `200 OK` (PCMU) →
   bridge audio both ways (RTP PCMU 8 kHz ⇄ PCM16 8 kHz ⇄ Opus), jitter buffers, silence fill, early
   RTP toward the carrier immediately after ACK.
5. Teardown on SIP BYE, owner leaving (membership cleared/expired, or owner gone from LiveKit), our
   LiveKit session dropping, RTP timeout, logout or bridge stop.
6. Decline (MSC4310) → `603 Decline`. Caller CANCEL → clear ghost membership **and** redact the ring.
   Ring timeout → `480` plus the same cleanup.
7. On answer, read receipt for the ring as the owner (double puppet) so push-driven clients stop
   ringing.
8. Timeline notices: missed, declined, answered with duration, and failure variants.
9. Encrypted-room guard, one active call per login (`486 Busy Here` for a second INVITE), in-memory
   call state, `.well-known` LiveKit service discovery with config override, RTP port range from config.
10. Telemetry per TS §8.2 Phase 2.

Out of scope (do **not** implement): outbound calls and `call <number>` (Phase 3), DTMF out (Phase 3),
inbound DTMF handling beyond dropping the packets, persistence across restart and stale-membership
cleanup at startup (Phase 4), re-INVITE/hold handling beyond what diago does by itself (Phase 4),
video, concurrent calls per login, PCMA/G.722/Opus over SIP, MSC4143 `rtc/transports` endpoint
discovery, embedded (mautrix-jabber) wiring.

---

## 1. Decisions (and why)

| # | Decision | Why |
|---|---|---|
| D1 | Three new core packages: `pkg/calls` (state machine, one actor goroutine per call), `pkg/rtc` (LiveKit participant as the ghost, lk-jwt-service client, service-URL discovery), `pkg/media` (codecs, jitter buffers, the audio bridge). Plus a tiny `pkg/clock` (real and fake clock). All are core packages under ADR-006 and the import check. | Matches the PLAN architecture diagram. Each external library is confined to one package (D2), so each can be faked at its seam. |
| D2 | Library confinement, enforced by `internal/importcheck`: `github.com/emiago/*` and `github.com/pion/stun/*` only in `pkg/sipua`; `github.com/livekit/*` and every other `github.com/pion/*` only in `pkg/rtc`; `gopkg.in/hraban/opus.v2` only in `pkg/media`. No diago, LiveKit or pion type appears in any exported identifier. | ADR-009 keeps diago swappable; the same applies to LiveKit. `pkg/calls` then depends only on our interfaces and its tests never touch a network stack. |
| D3 | **LiveKit `server-sdk-go/v2` v2.18.1** (with `livekit/protocol` v1.49.0, `pion/webrtc/v4` v4.2.15), not the v2.19.0 used by spike S1. | Verified by a throwaway build: v2.19.0 requires `pion/ice/v4` v4.4.x, which takes `stun/v4` URIs and breaks `diago@v0.41.0/media/media_session_webrtc.go:220` (`ice.WithUrls`). v2.18.1 resolves to `pion/ice/v4` v4.3.0 (diago's version), builds with diago, keeps `go 1.26.0`, and has every API this spec uses (`ConnectToRoomWithToken`, `RoomCallback`, `WithAutoSubscribe`, `WithConnectTimeout`, `NewLocalSampleTrack`, `PublishTrack`, `WriteSample`, `RemoteTrackPublication.SetSubscribed`, `SetLogger`). |
| D4 | Codec: **hraban/opus directly, with Opus running at 8 kHz** (`opus.NewEncoder(8000, 1, opus.AppVoIP)`, `opus.NewDecoder(8000, 1)`). No resampler, no `livekit/media-sdk`. G.711 µ-law is our own 30-line table codec in `pkg/media`. | Opus encodes from and decodes to 8 kHz natively (libopus resamples internally, at high quality); the bitstream is rate-independent, so browsers decode it normally and the RTP clock stays 48 kHz (set by `WriteSample` from the 20 ms duration). PSTN audio is narrowband anyway. media-sdk's root package needs cgo **libsoxr** (`resample_soxr.go`, `//go:build cgo`), which the image does not ship, plus a large dependency tree. hraban/opus is already proven in S1 and is MIT. |
| D5 | Build tags everywhere: **`-tags "nocrypto nolibopusfile"`**. | hraban/opus links `opusfile` unless `nolibopusfile` is set (`stream.go: // +build !nolibopusfile`); we never use the stream decoder. The dev machine has libopus 1.6.1 but no opusfile. One tag set for local, CI and Docker. |
| D6 | Concurrency: one **actor goroutine per call** owns all call state (no mutex on call state). Inputs arrive on channels: the actor's own event channel (Matrix events, async results, shutdown), the SIP call's event channel and the RTC session's event channel, plus clock timers. The actor does short Matrix requests inline (bounded timeouts); the two long operations (LiveKit join, SIP answer until ACK) run asynchronously and report back as events. | No locks to get wrong, deterministic tests with a fake clock, and nothing ever blocks a diago handler goroutine, a LiveKit callback or the appservice event path (CLAUDE.md telemetry/concurrency rules, TS §1 L3). |
| D7 | `pkg/sipua` gets an inbound-call API (`InboundCall` interface, `CallEvent` channel). Each INVITE is served by a **per-call command loop on the diago handler goroutine**: our commands (`Ringing`, `Answer`, `Reject`, `Hangup`) are non-blocking sends to that loop; diago calls that block (`Answer` waits up to 64×T1 = 32 s for ACK, a non-2xx `Respond` waits for ACK) run there. Results come back as `CallEvent`s. | diago's model is "the handler is the call scope; returning ends the dialog" (`diago.go:306-323`). Keeping every diago call on that goroutine respects its threading, and the actor never blocks on SIP. |
| D8 | SIP audio is read **packet by packet** (`DialogMedia.RTPPacketReader.Read` + `PacketHeader`, same goroutine) and written with `RTPPacketWriter.WriteSamples` (no pacing). `pkg/media` owns the 20 ms media clock for both directions and its own jitter buffers. diago's `AudioWriter().Write` (self-pacing, `rtp_packet_writer.go:154`) and its jitter buffer are not used. | Sequence numbers are needed for reordering and loss accounting; a single media clock in our code is testable with a fake clock; and the writer/reader objects survive diago's re-INVITE media updates (`UpdateReader`/`UpdateRTPSession`). |
| D9 | Early RTP: the RTC→SIP leg starts sending a continuous 50 pps PCMU stream (µ-law silence `0xFF` until owner audio arrives) **immediately after ACK**. No 183 early media. | S2 proved this opens the NAT pinhole (the spike wrote 5 silence packets right after `Answer` returned). 183 with SDP would replace the carrier's ringback with our silence. A continuous stream makes "early RTP" just the first frames. |
| D10 | `call.inbound` is started by the call actor **backdated** to INVITE receipt (`trace.WithTimestamp(info.ReceivedAt)`), and the `100 Trying` event is added with its real timestamp. No live span crosses goroutines. | Produces exactly the TS §5.6 trace shape without the one allowed live-span transfer. TS §5.6 is updated to allow this (flagged edit, §13). |
| D11 | Pre-join failure policy: LiveKit join is attempted at ring start; on failure it is retried **once**. If both attempts fail (or no service URL is known), the call is **rejected with `480`** (caller goes to voicemail), ghost membership cleared, ring redacted, notice "Missed call (could not connect audio)". | Answering into dead air is worse than voicemail. Two attempts at 10 s each fit inside the 30 s default ring. |
| D12 | Final SIP responses: decline `603 Decline`; ring timeout, encrypted room, withheld caller, no audio path, shutdown while ringing `480 Temporarily Unavailable`; second call `486 Busy Here`; no PCMU in the offer or no SDP `488 Not Acceptable Here`. CANCEL's `487` is sent by sipgo itself (`sip/transaction_layer.go:154-183`). | 480 reached JMP voicemail in the Phase 1 live test. PLAN asks for 603 on decline (live test checks where it goes, §12). |
| D13 | LiveKit service URL: `network.livekit_service_url` if set, else discovered from `https://<homeserver.domain>/.well-known/matrix/client`, key `org.matrix.msc4143.rtc_foci`, first entry with `type: livekit` and a non-empty `livekit_service_url`. Discovered at connector start (non-fatal), cached, re-tried lazily per call (negative cache 60 s). The MSC4143 `rtc/transports` endpoint is not used. | The dev server has `msc4143_enabled: false` (no transports endpoint) but serves `.well-known` (spike results). The endpoint needs an authenticated client and is deferred. |
| D14 | Owner answer = a `call.member` event **from the owner** in the call's room whose content is live (wire §1.3/§1.4 rules, ported from S1 `memberIsLive`), received while ringing. Owner leave = a non-live content (`{}` or expired) for a device we saw join. The call ends when **no** owner device remains (handles answering on two devices and device hand-off). Separately, if no owner participant is present in LiveKit for 10 s after answer, the call ends (`rtc_lost`). | Membership is what clients use; LiveKit presence covers crashed clients whose membership lingers until expiry (4 h). |
| D15 | Ghost membership refresh for long calls: at `created_ts + expires − 10 min`, re-send the membership with the **same** `created_ts` and `expires += 4 h` (Element Call's pattern, wire §1.3/§6.6). Span `call.membership.refresh` (pulled forward from Phase 4). | Without it a call longer than 4 h silently "ends" for every client. The code is ~20 lines. |
| D16 | Read receipt on answer via `CallHost.MarkReadAsOwner` (exists since Phase 1, D4 there). The ring is **not** redacted on answer or on decline (decline references it; Element Web already stops on owner join). It **is** redacted on CANCEL, ring timeout and setup failures after it was sent. | Wire §3.2 table and §7 steps 6-8. |
| D17 | Withheld or unparseable caller IDs are rejected with `480` (no room, no notice). Caller number = `P-Asserted-Identity` user if present, else `From` user, normalised with `phone.Normalize(_, default_country_code)`. | `CallHost.ResolveCallRoom` requires E.164; a shared "unknown caller" portal is a later decision (§14 Q2). |
| D18 | Encrypted-room guard: `CallRoom` gains `Encrypted bool`, filled by the host. The standalone host reads `(*matrix.Connector).StateStore.IsEncrypted` (local, no round trip). An encrypted room gets `480`, an Error log line and a notice. | ADR-002. Silent audio is the failure mode to avoid. |
| D19 | One active call per login, enforced in `calls.Manager` under a mutex. A busy INVITE still creates a short-lived actor (for its own `call.inbound` trace, metrics and a "Missed call (line busy)" notice) but never becomes the active call. | Busy calls are worth seeing in history. |
| D20 | Call IDs are ULIDs generated by our own 30-line generator (`pkg/calls/callid.go`, Crockford base32, `crypto/rand`). Ghost membership device id = `"SIP"` + the last 8 characters of the ULID (only `[0-9A-Z]`). | TS §5.1 wants a ULID; no new dependency. Wire §1.2 wants a stable, unique, `[A-Z0-9]` device id. |
| D21 | Owner LiveKit audio: `WithAutoSubscribe(false)`; on `OnTrackPublished` subscribe only to audio tracks of participants whose identity starts with `<owner MXID>:`. Received Opus RTP goes straight into the media layer's receiver (copy into a jitter-buffer slot, zero allocation). | Never decode audio we do not need; one owner stream at a time (the most recently subscribed owner track wins; §6.6). |
| D22 | LiveKit and pion logs go through a logr sink in `pkg/rtc` that drops every key/value, scrubs the message with `obs.Scrub`, and logs at Warn and above only (`rtc.SetLibraryLogger`, called by the host like `sipua.SetLibraryLogger`). | The SDK can log URLs with `access_token=` query strings; S1 saw pion IPv6 floods at Info. Same approach as TS §1 L4. |
| D23 | Calls are kept in memory only. Graceful stop/logout tears every call down (BYE or 480, membership clear). A crash leaves a phantom ghost membership until expiry (cleanup is Phase 4). | Phase 2 scope. |
| D24 | New config key `rtp_timeout_seconds` (default 60). Everything else uses existing keys (`ring_timeout_seconds`, `livekit_service_url`, `rtp_port_min/max`). | Keeps config small. 60 s tolerates short carrier silences/hold without leaving dead calls up forever. |

---

## 2. Package layout

Every file, with owner task (T1–T6, see §11). Files marked **(API)** are written by T1 as exact
contracts (types, interfaces, constants, doc comments, function signatures with a `panic("T<n>")` or
minimal body where noted). Implementers add files but must not change an API file's exported
surface; if they need to, they stop and report.

```
go.mod, go.sum                                   T1 (deps added), T6 (final go mod tidy)
internal/importcheck/importcheck_test.go         T1  library confinement rules (D2) + detection tests
internal/obstest/obstest.go                      T1  DefaultMetricAllowlist gains the Phase 2 instruments
pkg/clock/clock.go                               T1  Clock, Timer, Ticker, Real()
pkg/clock/fake.go                                T1  Fake clock (non-test file; used by other packages' tests)
pkg/clock/clock_test.go                          T1
pkg/core/callhost.go                             T1  CallRoom.Encrypted
pkg/core/ingest.go                               T1  routed_to default set before the sink runs
pkg/core/ingest_obs_test.go                      T1  (extend) sink can overwrite matrixrtc.routed_to
pkg/matrixrtc/events.go                          T1  + content field names, constants, WellKnownRTCFociKey
pkg/matrixrtc/membership.go                      T1  state keys, JoinContent, LeaveContent, RingContent, parsers
pkg/matrixrtc/membership_test.go                 T1
pkg/obs/metrics.go                               T1  Phase 2 instruments
pkg/obs/attrs.go                                 T1  new attribute keys
pkg/obs/obs_test.go                              T1  (extend) instruments exist and are no-op when off
pkg/media/doc.go                                 T1
pkg/media/media.go                     (API)     T1  RTPPacket, SIPLeg, RTCLeg, PacketReceiver, Config, Stats, Bridge, Start (stub)
pkg/media/ulaw.go                                T2  ULawEncode, ULawDecode
pkg/media/opus.go                                T2  opus encoder/decoder wrappers (8 kHz mono)
pkg/media/jitter.go                              T2  jitterBuffer (seq-ordered, fixed delay, concealment)
pkg/media/bridge.go                              T2  Start implementation, the two legs, stats, RTP timeout
pkg/media/stats.go                               T2  atomic counters, RFC 3550 jitter estimator
pkg/media/{ulaw,opus,jitter,bridge,stats}_test.go T2
pkg/media/bench_test.go                          T2  hot-path zero-allocation test + benchmarks
pkg/sipua/inbound.go                   (API)     T1  InviteHandler, InviteInfo, Offer, InboundCall, CallEvent, AnswerInfo, enums
pkg/sipua/config.go                              T1  Config.OnInvite field only (T3 owns the behaviour)
pkg/sipua/inbound_loop.go                        T3  per-call command loop (diago-free, uses dialogServer seam)
pkg/sipua/inbound_diago.go                       T3  diago adapter: dialogServer impl, media leg impl, offer parsing
pkg/sipua/diago.go                               T3  ServeBackground handler dispatches to OnInvite (rejectInbound kept for nil)
pkg/sipua/doc.go                                 T3  package doc updated (no longer "rejects all INVITEs")
pkg/sipua/{inbound_loop,offer}_test.go           T3
pkg/sipua/inbound_loopback_test.go               T3  localhost INVITE through a real diago UAC
pkg/rtc/doc.go                                   T1
pkg/rtc/rtc.go                         (API)     T1  JoinParams, Joiner, Session, Event, JoinError, Config, NewJoiner (stub), SetLibraryLogger (stub)
pkg/rtc/token.go                                 T4  lk-jwt-service /sfu/get client
pkg/rtc/discover.go                              T4  ServiceURLResolver (.well-known), LiveKitRoomName
pkg/rtc/livekit.go                               T4  Join, session, callbacks, owner track subscription, RTCLeg impl
pkg/rtc/logger.go                                T4  logr sink for LiveKit/pion (D22)
pkg/rtc/httptemplate.go                          T4  path templater for obs.WrapHTTPTransport
pkg/rtc/{token,discover,logger,identity}_test.go T4
pkg/calls/doc.go                                 T5
pkg/calls/manager.go                             T5  Manager, Config, HandleInvite, HandleRTCEvent, EndLogin, Stop
pkg/calls/call.go                                T5  call actor: run loop, event dispatch
pkg/calls/state.go                               T5  state and event types, outcome/end-reason enums
pkg/calls/setup.go                               T5  guards, resolve, membership, ring, join, answer
pkg/calls/teardown.go                            T5  failure branch and answered-call teardown
pkg/calls/owner.go                               T5  owner device and LiveKit presence tracking
pkg/calls/notices.go                             T5  notice texts, duration formatting
pkg/calls/callid.go                              T5  ULID generator, device id
pkg/calls/telemetry.go                           T5  span helpers, metric recording, stats flush
pkg/calls/fakes_test.go                          T5  fake SIP call, joiner/session, host, ghost, media bridge
pkg/calls/{call,manager,notices,callid,telemetry}_test.go  T5
pkg/calls/integration_test.go                    T5  fake SIP + fake RTC legs + real media.Bridge + real Ingest
pkg/connector/connector.go                       T6  SIPConnector.Calls, LiveKit resolver fields
pkg/connector/start.go                           T6  rtc.SetLibraryLogger, resolver, manager, RTC.SetSink
pkg/connector/client.go                          T6  sipua.Config.OnInvite, EndLogin before UA stop
pkg/connector/callhost.go                        T6  CallRoom.Encrypted
pkg/connector/config.go, example-config.yaml     T6  rtp_timeout_seconds, livekit comment
pkg/connector/telemetry.go                       T6  StartAttrs: livekit.service_url.source; Stop calls Calls.Stop
pkg/connector/{config,client_lifecycle}_test.go  T6  (extend)
Dockerfile                                       T6  build tags
README.md, CLAUDE.md, docs/CREDITS.md, docs/PLAN.md   T6
docs/TELEMETRY-SPEC.md                           (this spec's author; flagged edits, §13)
```

The `Config.OnInvite` field is added to `pkg/sipua/config.go` by T1 (one field plus doc comment, no
behaviour) because T5 and T6 code against it.

Nested modules under `spikes/` are untouched.

---

## 3. Dependencies and build (T1)

```bash
cd <repo>
go get github.com/livekit/server-sdk-go/v2@v2.18.1 \
       github.com/livekit/protocol@v1.49.0 \
       github.com/pion/webrtc/v4@v4.2.15 \
       gopkg.in/hraban/opus.v2@v2.0.0-20230925203106-0188a62cb302
```

Then check (must print exactly these):

```bash
go list -m github.com/emiago/diago github.com/emiago/sipgo maunium.net/go/mautrix \
  github.com/livekit/server-sdk-go/v2 github.com/pion/ice/v4 go.opentelemetry.io/otel
# github.com/emiago/diago v0.41.0
# github.com/emiago/sipgo v1.6.0
# maunium.net/go/mautrix v0.32.0
# github.com/livekit/server-sdk-go/v2 v2.18.1
# github.com/pion/ice/v4 v4.3.0
# go.opentelemetry.io/otel v1.47.0
grep '^go ' go.mod   # go 1.26.0
```

If `pion/ice/v4` resolves above v4.3.x, diago's media package will not compile (D3): stop and report.

Rules for parallel work: only T1 runs `go get`; T2–T5 must not run `go mod tidy` or change versions
(`go mod download <module>@<pinned>` for a missing go.sum entry is fine); T6 runs `go mod tidy` once at
the end and repeats the version check. `pion/rtp` becomes a direct dependency of `pkg/rtc` (T4 imports
`github.com/pion/rtp` at whatever version `go get` resolved; do not change it).

Acceptance commands everywhere (cgo and `pkg-config opus` required):

```bash
go build -tags "nocrypto nolibopusfile" ./...
go vet   -tags "nocrypto nolibopusfile" ./...
go test  -race -count=1 -tags "nocrypto nolibopusfile" ./...
```

---

## 4. Shared contracts (T1)

### 4.1 `pkg/clock`

```go
package clock

// Clock abstracts time for the call actor and the media bridge so tests can run on a fake clock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real returns the wall clock (time.NewTimer / time.NewTicker).
func Real() Clock

// Fake is a deterministic clock. Timer channels have capacity 1; ticker channels have capacity 1 and
// drop ticks the receiver has not consumed (like time.Ticker). Safe for concurrent use.
type Fake struct{ /* unexported */ }

func NewFake(start time.Time) *Fake
func (f *Fake) Now() time.Time
func (f *Fake) NewTimer(d time.Duration) Timer
func (f *Fake) NewTicker(d time.Duration) Ticker
// Advance moves time forward by d, firing due timers and ticks in time order.
func (f *Fake) Advance(d time.Duration)
// WaitForWaiters blocks until at least n timers/tickers are pending (test synchronisation), or 5 s.
// Returns false on timeout.
func (f *Fake) WaitForWaiters(n int) bool
```

Tests: timer fires once at the deadline; `Stop` before deadline prevents it; `Reset`; ticker drops
unread ticks; `Advance` across several deadlines fires in order; `WaitForWaiters`.

### 4.2 `pkg/core` changes

`callhost.go`:

```go
// CallRoom is the Matrix room in which calls with one remote number ring, plus the ghost for that number.
type CallRoom struct {
	RoomID     id.RoomID
	RemoteE164 string
	Ghost      GhostIntent
	// Encrypted is true if the room has m.room.encryption. Calls refuse to ring in encrypted rooms
	// (Element Call would use SFrame E2EE and the bridged audio would decode to silence, ADR-002).
	Encrypted bool
}
```

No new `CallHost` methods: `ResolveCallRoom`, `SendCallNotice`, `OwnerMXID` and `MarkReadAsOwner`
(Phase 1) are sufficient; redaction and state/message sends are on `GhostIntent`.

`ingest.go`: in `Ingest.ingest`, move `span.SetAttributes(attribute.String("matrixrtc.routed_to", "none"))`
to **before** `sink.HandleRTCEvent(sctx, login, parsed)` and delete the line after it. Update the
`RTCEventSink` doc comment: "The sink may set `matrixrtc.routed_to` and `call.id` on
`trace.SpanFromContext(ctx)`; the default is `none`." Remove the "Phase 1: no sink routes events" comment.
Extend `ingest_obs_test.go`: a sink that sets `routed_to=call` wins.

### 4.3 `pkg/matrixrtc` additions

`events.go` gains (keep the citation comment; this stays the one file with MatrixRTC names):

```go
// Well-known key listing MatrixRTC foci (wire §4.1, §8.2). ruma alias "m.rtc_foci" is not read.
const WellKnownRTCFociKey = "org.matrix.msc4143.rtc_foci"

// Content values (wire §1.3, §2.2).
const (
	ApplicationCall        = "m.call"
	ScopeRoom              = "m.room"
	CallIDRoom             = ""            // "" = the room call (slot m.call#ROOM)
	SlotIDRoom             = "m.call#ROOM"
	IntentAudio            = "audio"
	FocusTypeLiveKit       = "livekit"
	FocusSelectionMultiSFU = "multi_sfu"
	NotificationTypeRing   = "ring"
)

// DefaultMembershipExpiry is `expires` for a fresh membership: a duration relative to created_ts
// (Element Call's DEFAULT_EXPIRE_DURATION, wire §1.3).
const DefaultMembershipExpiry = 4 * time.Hour

// Content field names read or written by this package (wire §1.3, §2.2, §3.1, §4.1).
const (
	FieldApplication       = "application"
	FieldCallID            = "call_id"
	FieldScope             = "scope"
	FieldDeviceID          = "device_id"
	FieldMembershipID      = "membershipID"
	FieldCreatedTS         = "created_ts"
	FieldExpires           = "expires"
	FieldCallIntent        = "m.call.intent"
	FieldFocusActive       = "focus_active"
	FieldFociPreferred     = "foci_preferred"
	FieldType              = "type"
	FieldFocusSelection    = "focus_selection"
	FieldLiveKitServiceURL = "livekit_service_url"
	FieldLiveKitAlias      = "livekit_alias"
	FieldNotificationType  = "notification_type"
	FieldSenderTS          = "sender_ts"
	FieldLifetime          = "lifetime"
	FieldMentions          = "m.mentions"
	FieldUserIDs           = "user_ids"
	FieldRelatesTo         = "m.relates_to"
	FieldRelType           = "rel_type"
	FieldEventID           = "event_id"
	FieldSlotID            = "slot_id"
	RelTypeReference       = "m.reference"
)
```

`membership.go` (exact API):

```go
package matrixrtc

// MemberStateKey builds the per-device membership state key (wire §1.2):
// "_" + userID + "_" + deviceID + "_" + ApplicationCall + CallIDRoom.
func MemberStateKey(userID id.UserID, deviceID string) string

// ParseMemberStateKey reverses MemberStateKey the way ruma does (wire §1.2): strip one leading "_"
// (accept keys without it too), the user ID runs up to the first "_" after the first ":", the rest is
// the member id ("<device>_m.call"). ok=false if there is no ":" or no "_" after it.
func ParseMemberStateKey(stateKey string) (userID id.UserID, memberID string, ok bool)

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
func JoinContent(p JoinParams) map[string]any

// LeaveContent is the empty membership content (wire §1.4).
func LeaveContent() map[string]any

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
func RingContent(p RingParams) map[string]any

// Membership is a parsed call.member event.
type Membership struct {
	UserID   id.UserID // from the state key
	DeviceID string    // content device_id, else the member id without the "_m.call" suffix
	Live     bool      // non-empty, application m.call, scope m.room (or absent), base+expires > now
}

// ParseMembership parses a KindCallMember event. Live uses S1's memberIsLive rule: base = created_ts if
// present else evt.Timestamp; expires defaults to DefaultMembershipExpiry. ok=false if evt is not a
// call member event, the state key does not parse, or the state key user differs from evt.Sender
// (wire §1.5: the sender is the member).
func ParseMembership(evt Event, now time.Time) (Membership, bool)

// DeclineTarget returns the m.reference event_id of a KindRTCDecline event. ok=false otherwise.
func DeclineTarget(evt Event) (id.EventID, bool)

// ParseRTCFoci extracts the first livekit_service_url from a .well-known/matrix/client body
// (WellKnownRTCFociKey array, first entry with type "livekit" and a non-empty livekit_service_url,
// trailing "/" trimmed). ok=false if absent or malformed.
func ParseRTCFoci(wellKnown []byte) (serviceURL string, ok bool)
```

Tests (`membership_test.go`): `MemberStateKey("@sip_15551234567:example.org", "SIPAB12CD34")` ==
`_@sip_15551234567:example.org_SIPAB12CD34_m.call`; `ParseMemberStateKey` round trip, a localpart with
underscores, a key without the leading `_`, garbage; `JoinContent` golden JSON equal to wire §1.3
(with fixed time) and has no `url` key; `RingContent` golden JSON equal to wire §2.2 with
`lifetime: 30000`; `ParseMembership` table: live (created_ts + 4 h in future), `{}` → not live,
expired, wrong application, `scope: m.user` → not live, missing scope → live, missing created_ts uses
event timestamp, sender ≠ state-key user → ok=false, alias type `m.call.member` accepted (Kind is set by
`KindOf`), device from content and from state key; `DeclineTarget` for both decline type names, missing
relates_to; `ParseRTCFoci` with the wire §8.2 example, a `type: other` entry first, empty list, missing
key, invalid JSON. `grep -rn "org.matrix.msc" pkg/ --include=*.go | grep -v _test.go` still matches only
`events.go`.

### 4.4 `pkg/obs` additions

`attrs.go` (new keys; reuse existing ones everywhere else):

```go
AttrMediaDirection     = attribute.Key("media.direction")       // metric: rx, tx
AttrMediaLeg           = attribute.Key("media.leg")             // metric: sip_to_rtc, rtc_to_sip
AttrMatrixRTCRoutedTo  = attribute.Key("matrixrtc.routed_to")   // call, none
AttrRingLifetimeMS     = attribute.Key("matrixrtc.ring.lifetime_ms")
AttrRTCJoinAttempt     = attribute.Key("rtc.join.attempt")
AttrLiveKitDisconnect  = attribute.Key("livekit.disconnect_reason")
```

`metrics.go`: add these fields to `Instruments`, created in `newInstruments` exactly per TS §4.3
(name, unit, description, buckets):

```go
CallStarted       metric.Int64Counter         // mrtcsip.call.started        {call}
CallEnded         metric.Int64Counter         // mrtcsip.call.ended          {call}
CallActive        metric.Int64UpDownCounter   // mrtcsip.call.active         {call}
CallSetupDuration metric.Float64Histogram     // mrtcsip.call.setup.duration s  0.25,0.5,1,2,3,5,10,20,30,60
CallRingDuration  metric.Float64Histogram     // mrtcsip.call.ring.duration  s  same buckets
CallDuration      metric.Float64Histogram     // mrtcsip.call.duration       s  10,30,60,120,300,600,1800,3600,7200
RTCJoinDuration   metric.Float64Histogram     // mrtcsip.rtc.join.duration   s  0.1,0.25,0.5,1,2,3,5,10
RTPPackets        metric.Int64Counter         // mrtcsip.media.rtp.packets   {packet}
RTPLost           metric.Int64Counter         // mrtcsip.media.rtp.lost      {packet}
RTPLate           metric.Int64Counter         // mrtcsip.media.rtp.late      {packet}
RTPJitter         metric.Float64Histogram     // mrtcsip.media.rtp.jitter    s  0.001,0.0025,0.005,0.01,0.02,0.04,0.08,0.16
MediaConcealed    metric.Int64Counter         // mrtcsip.media.concealed     {frame}
```

`internal/obstest.DefaultMetricAllowlist` gains (closed enums from TS §4.3):

```go
"mrtcsip.call.started":        {"call.direction": {"inbound", "outbound"}},
"mrtcsip.call.ended":          {"call.direction": {"inbound", "outbound"},
	"call.outcome": {"answered", "missed", "declined", "cancelled", "timeout", "busy", "failed", "rejected"},
	"sip.response.status_class": {"1xx", "2xx", "3xx", "4xx", "5xx", "6xx", "none"}},
"mrtcsip.call.active":         {"call.direction": {"inbound", "outbound"}},
"mrtcsip.call.setup.duration": {"call.direction": {...}, "call.outcome": {...same 8...}},
"mrtcsip.call.ring.duration":  {"call.direction": {...}, "call.outcome": {...same 8...}},
"mrtcsip.call.duration":       {"call.direction": {...}, "media.codec": {"PCMU", "PCMA", "other"}},
"mrtcsip.rtc.join.duration":   {"outcome": {"ok", "openid_error", "token_error", "connect_error", "publish_error", "timeout"}},
"mrtcsip.media.rtp.packets":   {"media.direction": {"rx", "tx"}},
"mrtcsip.media.rtp.lost":      {},
"mrtcsip.media.rtp.late":      {},
"mrtcsip.media.rtp.jitter":    {},
"mrtcsip.media.concealed":     {"media.leg": {"sip_to_rtc", "rtc_to_sip"}},
```

### 4.5 Import check (`internal/importcheck/importcheck_test.go`)

Replace `sipOnlyPrefixes`/`isSIPOnly`/`inSIPUA` with a confinement table (longest prefix wins):

```go
// confined maps a library import prefix to the only core package directory (directly under pkg/)
// allowed to import it (ADR-009, PHASE2-SPEC D2).
var confined = []struct{ Prefix, Dir string }{
	{"github.com/emiago/", "sipua"},
	{"github.com/pion/stun/", "sipua"},
	{"github.com/pion/", "rtc"},
	{"github.com/livekit/", "rtc"},
	{"gopkg.in/hraban/opus", "media"},
}
```

A violation is an import matching a prefix in a file outside `pkg/<Dir>/`. Keep every existing rule.
Raise the "scanned at least N core dirs" guard to 9 (`calls, clock, core, matrixrtc, media, obs, phone,
rtc, sipua`). Detection test adds: `media/x.go` importing `github.com/livekit/server-sdk-go/v2` (bad),
`rtc/x.go` importing `github.com/pion/webrtc/v4` (ok), `calls/x.go` importing `github.com/pion/rtp`
(bad), `media/x.go` importing `gopkg.in/hraban/opus.v2` (ok), `sipua/x.go` importing
`github.com/pion/stun/v3` (ok), `rtc/x.go` importing `github.com/emiago/diago` (bad).

---

## 5. SIP inbound API (`pkg/sipua`) — API by T1, implementation T3

### 5.1 Exported API (exact)

`config.go` gains one field (T1):

```go
	// OnInvite receives every new inbound call (not re-INVITEs). It runs on the diago handler
	// goroutine and MUST return quickly (hand the call to another goroutine). nil means every INVITE is
	// rejected with 480 Temporarily Unavailable (Phase 1 behaviour).
	OnInvite InviteHandler
```

`inbound.go` (T1, exact):

```go
package sipua

// InviteHandler receives a new inbound call. See Config.OnInvite.
type InviteHandler func(call InboundCall)

// InviteInfo holds facts about an INVITE. Nothing in it is secret; SDP and raw headers are not kept.
type InviteInfo struct {
	CallID       string    // SIP Call-ID
	FromUser     string    // From URI user part (usually the caller number)
	FromDisplay  string    // From display name, "" if none
	AssertedUser string    // P-Asserted-Identity URI user part, "" if the header is absent
	ToUser       string    // To URI user part (the dialled number or the SIP username)
	RemoteAddr   string    // signalling source "ip:port"
	ReceivedAt   time.Time // when the handler started
	TryingSentAt time.Time // when 100 Trying was sent (zero if sending failed)
	Offer        Offer
}

// Offer is what the INVITE's SDP offers, reduced to the facts the core needs.
type Offer struct {
	HasSDP         bool
	PCMU           bool     // payload type 0 offered on the audio m-line
	TelephoneEvent bool     // telephone-event/8000 offered
	MediaAddr      string   // "ip:port" from c= and m=audio, "" if unknown
	Codecs         []string // offered audio formats as "NAME/PT" (e.g. "PCMU/0", "G729/18"), for logs
}

// InboundCall is one inbound INVITE dialog. Every method is non-blocking and safe for concurrent use;
// commands are executed in order on the call's own goroutine. Commands after the call ended are ignored.
type InboundCall interface {
	Info() InviteInfo
	// Ringing sends 180 Ringing (ignored once a final response was sent).
	Ringing()
	// Answer sends 200 OK with a PCMU (+telephone-event) SDP answer and symmetric RTP, waits for ACK,
	// then emits EventAnswered (or EventAnswerFailed). Ignored after a final response.
	Answer()
	// Reject sends a final non-2xx response (code 300..699) and ends the call (EventEnded, EndRejected).
	// Ignored once answered or ended.
	Reject(code int, reason string)
	// Hangup sends BYE if answered (EventEnded, EndLocalHangup). Before answer it behaves like
	// Reject(480, "Temporarily Unavailable").
	Hangup()
	// Events delivers CallEvents. Exactly one EventEnded is delivered last, then the channel is closed.
	// At most one of EventAnswered/EventAnswerFailed precedes it. The buffer is large enough that the
	// call goroutine never blocks (capacity 4).
	Events() <-chan CallEvent
}

// CallEventKind identifies a CallEvent.
type CallEventKind int

const (
	EventAnswered CallEventKind = iota + 1
	EventAnswerFailed
	EventEnded
)

// EndCause says why a call ended.
type EndCause int

const (
	EndCancelled   EndCause = iota + 1 // caller sent CANCEL before a final response (sipgo answered 487)
	EndRemoteBye                       // caller sent BYE after answer
	EndLocalHangup                     // we sent BYE (Hangup)
	EndRejected                        // we sent a final non-2xx (Reject, or Hangup before answer)
	EndFailed                          // transaction timeout, no ACK, transport error, safety timeout
)

// String: "cancelled", "remote_bye", "local_hangup", "rejected", "failed".
func (c EndCause) String() string

// AnswerInfo describes the negotiated media after ACK.
type AnswerInfo struct {
	Codec          string // "PCMU"
	PtimeMS        int    // 20
	TelephoneEvent bool
	LocalRTP       string // "ip:port" we bind (SDP advertises the external IP)
	RemoteRTP      string // "ip:port" from the offer (symmetric RTP may re-learn it)
	OKSentAt       time.Time
	AckAt          time.Time
}

// CallEvent is delivered on InboundCall.Events.
type CallEvent struct {
	Kind       CallEventKind
	At         time.Time
	Media      media.SIPLeg // EventAnswered only; valid until EventEnded
	Answer     AnswerInfo   // EventAnswered only
	Cause      EndCause     // EventEnded only
	StatusCode int          // EventEnded: final status WE sent (200 if answered, 0 if none, 487 for CANCEL)
	Err        error        // EventAnswerFailed / EndFailed: underlying error (scrubbed before logging)
}
```

`pkg/sipua` imports `pkg/media` for `media.SIPLeg` and `media.RTPPacket` only.

### 5.2 Behaviour (T3)

`diago.go`: `dg.ServeBackground(serveCtx, u.serveInvite)`; `serveInvite(d)`: if `cfg.OnInvite == nil`
call the existing `rejectInbound(d)`; else `c := newInboundCall(newDiagoDialog(d), u.cfg.Log, clock.Real())`
and `c.serve(u.cfg.OnInvite)` (blocks until the call ended; this is the diago handler goroutine).

`inbound_loop.go` (no diago imports; unit-tested through the `dialogServer` seam):

```go
// dialogServer is the subset of *diago.DialogServerSession the loop uses. inbound_diago.go adapts it.
type dialogServer interface {
	Info() InviteInfo                      // built once from the INVITE (offer parsed)
	Trying() error
	Ringing() error
	Answer() (media.SIPLeg, AnswerInfo, error) // blocks until ACK (or error)
	Respond(code int, reason string) error // final non-2xx; blocks until ACK/tx end
	Bye(ctx context.Context) error
	Done() <-chan struct{}                 // dialog context done (CANCEL, BYE, ended)
	Cancelled() bool                       // context.Cause(Done) is sip.ErrTransactionCanceled
	Confirmed() bool                       // dialog state >= Confirmed
}
```

`serve(onInvite)` exact sequence:

1. `info.ReceivedAt = now`; `Trying()`; `info.TryingSentAt = now` on success (log Warn on error).
2. Log Info "Inbound INVITE" with `sip_call_id`, `from` (FromUser), `asserted` (AssertedUser),
   `offered_codecs` (`Offer.Codecs`), `remote_media` (`Offer.MediaAddr`). Never the SDP or headers.
3. `onInvite(c)` (must return quickly).
4. Command loop: `select` over the command channel (capacity 8), `dialog.Done()`, and a safety timer
   (`inboundSafetyTimeout = 6 * time.Minute`, longer than the 300 s maximum ring):
   - `ringing`: if no final response yet → `Ringing()`.
   - `answer`: if no final response yet → `Answer()`; success → emit `EventAnswered` (Media, Answer,
     At=AckAt), mark answered; error → emit `EventAnswerFailed{Err}`; then if `Done()` is already
     closed or the error means the transaction ended, finish with `EndCancelled` if `Cancelled()`, else
     `EndFailed`.
   - `reject(code, reason)`: if not answered and not ended → `Respond(code, reason)`; finish with
     `EndRejected`, `StatusCode = code`.
   - `hangup`: answered → `Bye(ctx 5 s)`, finish with `EndLocalHangup` (`StatusCode = 200`); not answered
     → as `reject(480, "Temporarily Unavailable")`.
   - `Done()` closed: answered → finish `EndRemoteBye` (`StatusCode = 200`); not answered → `EndCancelled`
     (`StatusCode = 487`) if `Cancelled()`, else `EndFailed`.
   - safety timer: not answered → `Respond(480, ...)`, `EndFailed`; answered → timer disabled after
     answer.
5. "Finish" = emit exactly one `EventEnded{Cause, StatusCode, At}`, close the event channel, mark the
   command channel closed (later commands are dropped silently), return. diago's post-handler `Hangup`
   then runs and is a no-op (`diago.go:308-321` returns nil when the dialog context is cancelled).

Emission never blocks: the event channel has capacity 4 and at most 2 events are ever sent.

`inbound_diago.go`:

- `newDiagoDialog(d *diago.DialogServerSession)` builds `InviteInfo` from `d.InviteRequest`:
  `CallID().Value()`, `From().Address.User`, `From().DisplayName`, the `P-Asserted-Identity` header
  parsed as a name-addr (`sip.ParseAddressValue` or equivalent; user part only; ignore parse errors),
  `To().Address.User`, `InviteRequest.Source()`; and `Offer` from `parseOffer(d.InviteRequest.Body())`.
- `parseOffer(body []byte) Offer` (pure, tested): `HasSDP = len(body) > 0`; parse with
  `github.com/emiago/diago/media/sdp.Unmarshal` → `MediaDescription("audio")`; `PCMU` = format `"0"`
  present; `TelephoneEvent` = an `a=rtpmap:<pt> telephone-event/8000` line; `MediaAddr` from
  `ConnectionInformation()` + m-line port; `Codecs` from rtpmap lines plus static payload types without
  rtpmap (`0 → PCMU`, `8 → PCMA`, `18 → G729`, `9 → G722`, other → `pt<N>`). Malformed SDP → `HasSDP:
  true, PCMU: false`.
- `Answer()`: `med, err := d.Answer(diago.AnswerOptions{Codecs: []media.Codec{media.CodecAudioUlaw,
  media.CodecTelephoneEvent8000}, RTPNAT: media.RTPNATSymetric})` (`dialog_server_session.go:183`; it
  returns after ACK, `sipgo/dialog_server.go:261-351`). `OKSentAt` is taken just before the call
  (accurate to the 200's first transmission), `AckAt` after it returns. Returns `&diagoLeg{med: med}`.
- `Respond`: `d.Respond(code, reason, nil)`. `Bye`: `d.Hangup(ctx)`. `Done`: `d.Context().Done()`.
  `Cancelled`: `errors.Is(context.Cause(d.Context()), sip.ErrTransactionCanceled)`
  (`sipgo/dialog_ua.go` ReadInvite `OnCancel` → `endWithCause`). `Confirmed`: `d.LoadState() >=
  sip.DialogStateConfirmed`.
- `diagoLeg` implements `media.SIPLeg`:
  - `ReadRTP(p)`: `n, err := med.RTPPacketReader.Read(l.buf)` (`l.buf` is `make([]byte, 1500)`, allocated
    once); `io.EOF` passes through; `n == 0` → loop again (keepalive/zero payload); then copy the header
    fields from `med.RTPPacketReader.PacketHeader` (same goroutine, `rtp_packet_reader.go:45-47`) into
    `p` and set `p.Payload = l.buf[:n]`. Zero allocations.
  - `WriteRTP(payload, samples, marker)`: `med.RTPPacketWriter.WriteSamples(payload, samples, marker,
    0)` (`rtp_packet_writer.go:164`, no pacing). Only one goroutine calls it.
  - Accessing `med.RTPPacketReader`/`RTPPacketWriter` on every call (not cached) keeps working across
    diago's media updates on re-INVITE.

### 5.3 What sipua deliberately does not do

No ringback generation, no 183, no PRACK/100rel, no session timers, no SDP in logs, no DTMF decoding
(PT 101 packets are delivered to the media layer, which drops them).

---

## 6. LiveKit participant (`pkg/rtc`) — API by T1, implementation T4

### 6.1 Exported API (exact, `rtc.go`)

```go
package rtc

// JoinParams describes one ghost join.
type JoinParams struct {
	Ghost      core.GhostIntent // OpenID token is requested as this ghost
	RoomID     id.RoomID
	DeviceID   string    // same device id as the ghost membership (identity = "<ghost>:<device>")
	ServiceURL string    // lk-jwt-service base URL (no trailing "/")
	Owner      id.UserID // only this user's audio tracks are subscribed
	CallID     string    // logs and spans only
}

// Joiner joins LiveKit rooms. Join blocks until the track is published or fails (bounded by
// Config.ConnectTimeout plus the HTTP timeouts); ctx cancellation aborts the OpenID and token steps.
type Joiner interface {
	Join(ctx context.Context, p JoinParams) (Session, error)
}

// Session is a joined LiveKit participant publishing one Opus microphone track.
type Session interface {
	Identity() string      // LiveKit identity from the JWT, "<ghost>:<device>"
	LiveKitRoom() string   // LiveKit room name (hashed by lk-jwt-service)
	ServerAddress() string // LiveKit server host
	Audio() media.RTCLeg   // publish Opus; receive owner Opus
	// Events delivers owner presence and disconnects. Capacity 32; sends never block (an event that does
	// not fit is dropped and logged). Closed after Leave returns.
	Events() <-chan Event
	// Leave disconnects (idempotent). Bounded by ctx.
	Leave(ctx context.Context) error
}

// EventKind identifies an Event.
type EventKind int

const (
	EventOwnerJoined  EventKind = iota + 1 // an owner participant (identity "<owner>:<device>") connected
	EventOwnerLeft                         // an owner participant disconnected
	EventDisconnected                      // our own session ended (not by Leave); Reason set
)

// Event is delivered on Session.Events.
type Event struct {
	Kind     EventKind
	Identity string // owner participant identity for OwnerJoined/OwnerLeft
	Reason   string // EventDisconnected: lksdk DisconnectionReason
	At       time.Time
}

// JoinError says which stage failed; Stage values are the rtc.join.duration outcome enum.
type JoinError struct {
	Stage string // "openid_error", "token_error", "connect_error", "publish_error", "timeout"
	Err   error
}

func (e *JoinError) Error() string // "rtc join: <stage>: <scrubbed err>"
func (e *JoinError) Unwrap() error

// Config configures NewJoiner.
type Config struct {
	// HTTPClient for lk-jwt-service and .well-known. nil = &http.Client{Timeout: 10 s} whose transport is
	// obs.WrapHTTPTransport(http.DefaultTransport, PathTemplate).
	HTTPClient     *http.Client
	ConnectTimeout time.Duration // LiveKit signalling+ICE connect; 0 = 10 s
	Log            zerolog.Logger
	Clock          clock.Clock // nil = clock.Real()
}

// NewJoiner returns the LiveKit-backed Joiner.
func NewJoiner(cfg Config) Joiner

// SetLibraryLogger routes LiveKit SDK and pion logs through log at Warn+, dropping key/values and
// scrubbing messages (D22). Process-global; the host calls it once at startup.
func SetLibraryLogger(log zerolog.Logger)

// PathTemplate maps lk-jwt-service and well-known paths to bounded url.template values:
// suffix "/sfu/get" -> "/sfu/get", "/.well-known/matrix/client" -> itself, anything else -> "*".
func PathTemplate(path string) string

// ServiceURLResolver returns the lk-jwt-service base URL (D13).
type ServiceURLResolver struct{ /* unexported */ }

// NewServiceURLResolver: override wins when non-empty; otherwise discover from
// https://<serverName>/.well-known/matrix/client. hc nil = the default client above.
func NewServiceURLResolver(override, serverName string, hc *http.Client, clk clock.Clock) *ServiceURLResolver

// Get returns the URL. Discovery results are cached forever on success; failures are cached for 60 s
// (Get returns the cached error without I/O meanwhile). Concurrent callers share one in-flight fetch.
// Each fetch is bounded by 5 s.
func (r *ServiceURLResolver) Get(ctx context.Context) (string, error)

// Source reports "config", "well_known" or "" (unknown yet).
func (r *ServiceURLResolver) Source() string

// LiveKitRoomName mirrors lk-jwt-service's room alias (wire §4.5), for diagnostics only.
func LiveKitRoomName(roomID id.RoomID) string
```

### 6.2 Join sequence (T4, `livekit.go`)

`Join(ctx, p)` emits `rtc.join` (INTERNAL, **child-only**) with attributes `call.id`, `matrix.room_id`,
`matrix.ghost_id`, `matrixrtc.membership.device_id`, and records `mrtcsip.rtc.join.duration{outcome}`
from step 1 to the end of step 4. Steps (each a child-only span):

1. `rtc.openid_token` (CLIENT): `tok, err := p.Ghost.RequestOpenIDToken(ctx)` → `JoinError{"openid_error"}`.
   The token is never logged or recorded.
2. `rtc.jwt_fetch` (CLIENT): `POST {ServiceURL}/sfu/get`, body
   `{"room": RoomID, "openid_token": {access_token, token_type, matrix_server_name, expires_in},
   "device_id": DeviceID}` (wire §4.4, S1 `getSFUToken`), response `{"url","jwt"}`; non-200 →
   `JoinError{"token_error"}` with status and at most 300 bytes of the (non-secret) error body, passed
   through `obs.Scrub`. The JWT is never logged or recorded. Attributes: `server.address` (service host),
   `http.response.status_code`. The wrapped transport adds its own `POST /sfu/get` CLIENT child.
3. `rtc.livekit.connect` (CLIENT): create the Opus track
   `lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000,
   Channels: 2})`; `room, err := lksdk.ConnectToRoomWithToken(url, jwt, cb, lksdk.WithAutoSubscribe(false),
   lksdk.WithConnectTimeout(cfg.ConnectTimeout))` → `JoinError{"connect_error"}` (or `"timeout"` when the
   error wraps `context.DeadlineExceeded` or the SDK's timeout). Attributes: `livekit.room` =
   `room.Name()`, `livekit.server.address` = host of `url`, `livekit.participant.identity` =
   `room.LocalParticipant.Identity()`. If the identity is not `"<ghost>:<device>"`, log Warn (owner
   clients would not render the participant, wire §5) but continue.
4. `rtc.track.publish` (INTERNAL): `room.LocalParticipant.PublishTrack(track,
   &lksdk.TrackPublicationOptions{Name: "audio", Source: livekit.TrackSource_MICROPHONE, DisableDTX:
   true})` → on error `room.Disconnect()` and `JoinError{"publish_error"}`. No video track ever.
5. For participants already present (`room.GetRemoteParticipants()`), emit `EventOwnerJoined` for owner
   identities and subscribe their audio publications.

`ctx` is checked between steps; cancellation returns `JoinError{"timeout", ctx.Err()}` and disconnects if
connected. `ConnectToRoomWithToken` itself is not ctx-aware; its own timeout bounds it.

Callbacks (`lksdk.RoomCallback`, all must return immediately, never do I/O or telemetry calls other than
atomics):

- `OnParticipantConnected(rp)` / `OnParticipantDisconnected(rp)`: if `isOwner(rp.Identity())` emit
  `EventOwnerJoined`/`EventOwnerLeft`.
- `ParticipantCallback.OnTrackPublished(pub, rp)`: owner + audio kind → `go pub.SetSubscribed(true)`
  (the SDK call may do signalling; never inline in a callback).
- `ParticipantCallback.OnTrackSubscribed(track, pub, rp)`: owner + audio → start one reader goroutine
  for this track (below); it becomes the current owner track (stop forwarding from any previous one).
- `ParticipantCallback.OnTrackUnsubscribed(...)`: reader goroutine ends on read error by itself.
- `OnDisconnectedWithReason(reason)`: if not leaving, emit `EventDisconnected{Reason: string(reason)}`.

`isOwner(identity) = strings.HasPrefix(identity, string(p.Owner)+":")`.

Track reader goroutine (zero allocation per packet): `buf := make([]byte, 1500)`, `var pkt rtp.Packet`,
loop `n, _, err := track.Read(buf)`; on error return; `pkt.Unmarshal(buf[:n])`; skip if not the current
owner track or no receiver; fill a reused `media.RTPPacket` (SSRC, Seq, Timestamp, PayloadType, Marker,
Payload = pkt.Payload) and call `receiver.ReceiveRTP(&mp)`. The receiver pointer is an
`atomic.Pointer` set by `SetReceiver`.

`RTCLeg` implementation: `WriteOpus(frame, d)` = `track.WriteSample(pionmedia.Sample{Data: frame,
Duration: d}, nil)` (`localtrack.go:404`; it packetizes synchronously, so the caller may reuse `frame`
after return). `SetReceiver(r)` stores r (nil detaches).

`Leave(ctx)`: idempotent; marks leaving, `room.Disconnect()` in a goroutine, waits for it or ctx; closes
the event channel after the callbacks can no longer fire. Span `rtc.leave` (CLIENT, child-only) is
started by the **caller** (`pkg/calls`), not here.

### 6.3 Token client (`token.go`)

`fetchToken(ctx, hc, serviceURL, roomID, deviceID string, tok *core.OpenIDToken) (url, jwt string, err
error)`: JSON body as above, `Content-Type: application/json`, response limited to 1 MiB, accept only
200 with non-empty `url` and `jwt`. Unit-tested with `httptest.Server` (asserts the request body shape and
that error strings never contain the OpenID access token or the JWT).

### 6.4 Discovery (`discover.go`)

`GET https://<serverName>/.well-known/matrix/client` with the resolver's client (traced only if the ctx
has a span; it normally has none at startup), parse with `matrixrtc.ParseRTCFoci`. Errors: "no
org.matrix.msc4143.rtc_foci livekit entry in .well-known; set network.livekit_service_url". Tests use an
`httptest.NewTLSServer` with the resolver pointed at its host through an unexported test hook
(`r.base = srv.URL`), plus override precedence, negative caching with the fake clock, concurrent `Get`.

### 6.5 Logger sink (`logger.go`)

A `logr.LogSink` (`github.com/go-logr/logr`, already a dependency): `Enabled(level)` true only for
errors and V(0) warnings mapped as: LiveKit's `Warnw`/`Errorw` → zerolog Warn/Error; Info/Debug dropped.
`Info`/`Error` write `obs.Scrub(msg)` (and `obs.Scrub(err.Error())` as `error`), **never** the
key/values. `SetLibraryLogger(log)` calls `lksdk.SetLogger(protoLogger.LogRLogger(logr.New(sink)))`
with `log.With().Str("component", "livekit").Logger()`. Test: a message carrying
`wss://x/rtc?access_token=eyJhbGciOi.eyJzdWIi.sig` and key/values with a secret: the output has neither.

### 6.6 Owner track selection

Only one owner audio track is forwarded at a time: the most recently subscribed one. If its participant
leaves and another owner participant still publishes, that track is subscribed/forwarded (the
`OnTrackPublished`/`OnTrackSubscribed` path handles it naturally when the remaining track is already
subscribed: on `EventOwnerLeft` of the current track's participant, switch to any other subscribed owner
track). Mixing two owner devices is out of scope.

---

## 7. Media bridge (`pkg/media`) — API by T1, implementation T2

### 7.1 Exported API (exact, `media.go`)

```go
package media

const (
	SampleRate    = 8000                  // PCM and PCMU sample rate
	FrameDuration = 20 * time.Millisecond // media clock tick, SIP ptime
	FrameSamples  = 160                   // samples per 20 ms frame at 8 kHz
	PayloadPCMU   = 0
	ULawSilence   = 0xFF
)

// RTPPacket is one received RTP packet. Payload aliases the producer's buffer and is valid only during
// the call that delivered it (ReceiveRTP) or until the next ReadRTP.
type RTPPacket struct {
	SSRC        uint32
	Seq         uint16
	Timestamp   uint32
	PayloadType uint8
	Marker      bool
	Payload     []byte
}

// SIPLeg is the SIP side of a call (implemented by pkg/sipua).
type SIPLeg interface {
	// ReadRTP blocks for the next packet. io.EOF once the SIP media is closed.
	ReadRTP(p *RTPPacket) error
	// WriteRTP sends one PCMU packet now (no pacing); samples is the RTP timestamp increment.
	WriteRTP(payload []byte, samples uint32, marker bool) error
}

// PacketReceiver receives the owner's Opus packets. ReceiveRTP is called from a LiveKit track goroutine;
// it must not block and must copy what it keeps.
type PacketReceiver interface {
	ReceiveRTP(p *RTPPacket)
}

// RTCLeg is the LiveKit side of a call (implemented by pkg/rtc).
type RTCLeg interface {
	// WriteOpus publishes one Opus frame of duration d now. The caller may reuse frame after return.
	WriteOpus(frame []byte, d time.Duration) error
	// SetReceiver installs the receiver for the owner's audio (nil detaches).
	SetReceiver(r PacketReceiver)
}

// Config configures Start. Zero values get the defaults in brackets.
type Config struct {
	Clock       clock.Clock    // [clock.Real()]
	Log         zerolog.Logger // clean logger with call_id
	JitterDelay time.Duration  // initial playout delay per direction [60 ms]
	JitterMax   time.Duration  // max buffered audio per direction [400 ms]
	RTPTimeout  time.Duration  // no SIP RTP for this long ends the bridge with ErrRTPTimeout [60 s]
}

// Stats is a snapshot of the bridge counters (monotonic since Start).
type Stats struct {
	SIPRx             uint64        // SIP RTP packets received (all payload types)
	SIPTx             uint64        // SIP RTP packets sent
	SIPLost           uint64        // SIP sequence gaps not filled before playout
	SIPLate           uint64        // SIP packets discarded (late, duplicate or overflow)
	SIPJitter         time.Duration // RFC 3550 interarrival jitter estimate, current value
	ConcealedSIPToRTC uint64        // frames sent to LiveKit as PLC/silence because SIP audio was missing
	ConcealedRTCToSIP uint64        // frames sent to SIP as PLC/silence because owner audio was missing
	RTCRx             uint64        // owner Opus packets received
	LastSIPRx         time.Time     // zero if none yet
}

// Bridge moves audio between one SIPLeg and one RTCLeg.
type Bridge interface {
	Stats() Stats          // lock-free (atomics)
	Done() <-chan struct{} // closed when the bridge stops by itself (ErrRTPTimeout, SIP EOF) or after Stop
	Err() error            // after Done: ErrRTPTimeout, io.EOF (SIP closed) or nil (Stop)
	Stop()                 // idempotent; waits for the clocked goroutines; detaches the RTC receiver
}

var ErrRTPTimeout = errors.New("media: no RTP received from the SIP side")

// Start creates the codecs and starts the bridge. Errors only on codec creation.
func Start(cfg Config, sip SIPLeg, rtc RTCLeg) (Bridge, error)

// ULawEncode encodes len(src) PCM16 samples into dst (len(dst) >= len(src)). G.711 µ-law, bias 0x84.
func ULawEncode(dst []byte, src []int16)
// ULawDecode decodes len(src) µ-law bytes into dst.
func ULawDecode(dst []int16, src []byte)
```

### 7.2 Pipeline (T2)

```
SIP RTP (PCMU, 50 pps) ──sipReader goroutine──► jitterSIP ──tick 20 ms──► µ-law decode ─► PCM16 8k
                                                  (seq order, loss/late)             │ (missing: silence,
                                                                                     │  count concealed)
                                                                                     ▼
                                                     Opus encode @8 kHz mono ─► RTCLeg.WriteOpus(20 ms)
Owner Opus RTP ──ReceiveRTP (LiveKit goroutine)──► jitterRTC ──tick 20 ms──► Opus decode @8 kHz mono
                                                                (missing: DecodePLC for up to 3 frames,
                                                                 then silence; count concealed)
                                                     PCM FIFO (re-chunk to 160) ─► µ-law ─► SIPLeg.WriteRTP(160)
```

Goroutines started by `Start` (exactly three):

1. **sipReader**: loop `sip.ReadRTP(&pkt)`; on `io.EOF` (or any error) end the bridge with that error;
   update `SIPRx`, `LastSIPRx` (clock), RFC 3550 jitter (8 kHz clock: `D = (arrival - prevArrival) -
   (ts - prevTs)/8000`, `J += (|D| - J)/16`, stored as atomic nanoseconds) for PT 0 only; drop PT ≠ 0
   (telephone-event and anything else, counted in `SIPRx` only); push PT 0 into `jitterSIP`.
2. **sipToRTC** ticker (20 ms): `jitterSIP.pop()` → payload of 160 bytes (shorter/longer payloads are
   decoded to their sample count then padded/truncated to 160) or concealment (160 samples of 0,
   `ConcealedSIPToRTC++`); µ-law decode into a reused `[]int16`; Opus encode into a reused `[]byte` of
   1275 bytes; `rtc.WriteOpus(out[:n], 20 ms)`; errors are logged at most once per 10 s (rate-limited
   logger) and counted nowhere else. Before the first SIP packet arrives the RTC side still gets
   concealment frames (keeps the published track continuous).
   Also checks `clock.Now() - LastSIPRx` (or since Start if none) `> RTPTimeout` → end with `ErrRTPTimeout`.
3. **rtcToSIP** ticker (20 ms): produce exactly one 160-sample frame: if the PCM FIFO has ≥ 160 samples
   use them; else `jitterRTC.pop()`: packet → `dec.Decode(payload, pcmBuf)` (pcmBuf sized for 120 ms
   = 960 samples) appended to the FIFO; loss/underrun → if fewer than 3 consecutive concealed frames
   and a frame was decoded before, `dec.DecodePLC(pcmBuf[:lastFrameSamples])`, else 160 zeros;
   `ConcealedRTCToSIP++` for each concealed frame. µ-law encode into a reused `[160]byte`;
   `sip.WriteRTP(frame, 160, first)` (marker on the first packet only); `SIPTx++`. This stream starts at
   the first tick after `Start` (D9: early RTP / NAT opener) and sends `0xFF` silence until owner audio
   arrives.

`Bridge` also implements `PacketReceiver` for `jitterRTC` and `Start` calls `rtc.SetReceiver(b)`; `Stop`
calls `rtc.SetReceiver(nil)`. `ReceiveRTP` increments `RTCRx` and pushes into `jitterRTC`.

Stop semantics: `Stop` closes a stop channel, waits for the two ticker goroutines, detaches the
receiver and closes `Done` (if not closed). The sipReader goroutine exits when the SIP media closes
(`io.EOF` after the dialog ends); after `Stop` it discards packets. Tests assert it exits after the fake
SIP leg returns `io.EOF`.

Opus settings (`opus.go`): `opus.NewEncoder(8000, 1, opus.AppVoIP)`, `SetBitrate(24000)`,
`SetInBandFEC(true)`, `SetPacketLossPerc(5)`, `SetDTX(false)`; decoder `opus.NewDecoder(8000, 1)`
(libopus downmixes stereo streams). Encoder and decoder are used from one goroutine each (not shared).

### 7.3 Jitter buffer (`jitter.go`)

One unexported type used for both directions; packets keyed by RTP sequence number only (timestamps are
ignored for playout; the media clock is the 20 ms ticker).

Note: on an empty-buffer underrun `pop` does not advance `next` (it still counts underruns and re-buffers after 10), so a merely-late or post-DTX packet plays instead of being dropped as late; depth above delay+3 for 50 consecutive pops drops one frame to bound latency.

```go
type jitterBuffer struct { /* mutex; ring of slots with preallocated 1500-byte payload buffers */ }

func newJitterBuffer(delayFrames, maxFrames int) *jitterBuffer // delay = JitterDelay/20ms (min 1); max = JitterMax/20ms
// push copies p.Payload into a free slot. Called from the producer goroutine.
func (j *jitterBuffer) push(p *RTPPacket)
// pop returns the payload for the next playout tick (aliasing the slot until the next pop) or ok=false
// for a concealed frame. Called from the ticker goroutine.
func (j *jitterBuffer) pop() (payload []byte, ok bool)
func (j *jitterBuffer) stats() (lost, late uint64)
```

Rules (sequence arithmetic is modulo 2^16, `int16(a-b)` comparisons):

1. Not started: buffer packets; playout starts when `delayFrames` packets are buffered; `next` = lowest
   buffered seq. `pop` before start returns ok=false (counted as concealment by the caller).
2. Started: `pop` returns the slot for `next` if present, then `next++`. If absent but a later packet is
   buffered: `lost++`, ok=false, `next++`. If the buffer is empty: ok=false, `next++` (underrun, not
   loss), and after 10 consecutive underruns (200 ms) go back to "not started" (re-buffer on resume).
3. `push`: seq before `next` (already played) or duplicate → `late++`, dropped. Seq more than
   `maxFrames` ahead of `next`, or an SSRC change, or a jump of more than 1000 → reset (flush, not
   started, start from this packet). Buffer full → drop the oldest buffered packet (`late++`).
4. No allocation on push/pop after construction.

### 7.4 Tests (T2)

- `ulaw_test.go`: `ULawDecode(0xFF) == 0`; `ULawEncode(0) == 0xFF`; known pairs (`0x00 → -32124`,
  `0x80 → 32124`, `0x7F → 0`... use the ITU-T G.711 table values); encode→decode round trip error ≤ 2 %
  of magnitude for |x| > 1000 across a sweep; 256-entry decode table symmetry.
- `jitter_test.go`: in-order; reorder within delay; single loss counted once; late packet counted;
  duplicate; underrun then re-buffer; seq wrap 65534→1; SSRC reset; overflow drops oldest;
  `testing.AllocsPerRun(1000, push+pop) == 0`.
- `opus_test.go`: encode 1 s of a 440 Hz sine at 8 kHz in 20 ms frames, decode, verify RMS within 3 dB
  of the input after the first 3 frames and dominant frequency 440 ± 20 Hz (zero-crossing count);
  `DecodePLC` returns 160 samples.
- `bridge_test.go` (fake clock, fake legs): (a) SIP→RTC: fake SIP leg yields a PCMU 440 Hz sine at 50 pps
  (pushed as the clock advances); the fake RTC leg collects Opus frames; decoding them yields the tone,
  `ConcealedSIPToRTC` only for the initial delay frames. (b) RTC→SIP: fake RTC delivers Opus-encoded
  tone via `ReceiveRTP`; the fake SIP leg receives 50 pps of 160-byte PCMU whose decode has the tone.
  (c) Silence fill: with no owner audio the SIP leg receives a packet every tick from the first tick,
  all `0xFF`, first with marker. (d) RTP timeout: no SIP packets for `RTPTimeout` → `Done` closes, `Err
  == ErrRTPTimeout`. (e) SIP EOF → `Done`, `Err == io.EOF`. (f) `Stop` is idempotent and detaches the
  receiver (`SetReceiver(nil)` observed). (g) Telephone-event (PT 101) packets counted in `SIPRx` and
  never reach the decoder. (h) `Stats` counters match injected loss/late patterns.
- `bench_test.go`: `TestHotPathZeroAllocs`: with fake legs that do not allocate, one SIP packet push +
  one sipToRTC tick + one ReceiveRTP + one rtcToSIP tick (driven through unexported step functions) has
  `testing.AllocsPerRun == 0`. `BenchmarkSIPToRTCFrame`, `BenchmarkRTCToSIPFrame` with
  `b.ReportAllocs()`. This is the TS §8.2 "zero allocations per RTP packet from telemetry" proof (the
  hot path touches only atomics; instruments are called by `pkg/calls` on its 5 s ticker).

---

## 8. Call state machine (`pkg/calls`) — T5

### 8.1 Exported API (exact)

```go
package calls

// Config configures a Manager.
type Config struct {
	Host               core.CallHost
	Joiner             rtc.Joiner
	ServiceURL         func(ctx context.Context) (string, error) // e.g. (*rtc.ServiceURLResolver).Get
	RingTimeout        time.Duration                             // ring lifetime and timer [30 s]
	RTPTimeout         time.Duration                             // passed to media.Config [60 s]
	DefaultCountryCode string                                    // for phone.Normalize ["1"]
	Clock              clock.Clock                               // [clock.Real()]
	Log                zerolog.Logger                            // clean logger
	// StartMedia starts the audio bridge; nil = media.Start. Tests inject fakes.
	StartMedia func(cfg media.Config, sip media.SIPLeg, rtc media.RTCLeg) (media.Bridge, error)
}

// Manager owns all calls of all logins. Safe for concurrent use.
type Manager struct{ /* unexported */ }

var _ core.RTCEventSink = (*Manager)(nil)

func NewManager(cfg Config) *Manager

// HandleInvite takes ownership of a new inbound call for login. localE164 is the login's own number
// (span attribute only). Non-blocking: starts the call actor and returns (it runs on the diago handler
// goroutine).
func (m *Manager) HandleInvite(login core.LoginRef, localE164 string, call sipua.InboundCall)

// HandleRTCEvent routes a MatrixRTC event to the login's active call. Non-blocking (appservice path):
// a full call channel drops the event with a Warn log. Sets matrixrtc.routed_to (call|none) and call.id
// on the ingest span in ctx.
func (m *Manager) HandleRTCEvent(ctx context.Context, login core.LoginRef, evt matrixrtc.Event)

// EndLogin tears down the login's calls (hangup.by=bridge, end_reason=bridge_stop) and waits for them
// until ctx is done. Call before stopping the login's SIP UA so BYE/480 can still be sent.
func (m *Manager) EndLogin(ctx context.Context, login core.LoginRef)

// Stop ends every call like EndLogin and makes HandleInvite reject new calls with 480. Idempotent.
func (m *Manager) Stop(ctx context.Context)
```

Internal constants (`state.go`):

```go
const (
	matrixOpTimeout       = 5 * time.Second  // each membership/ring/redact/notice/receipt request
	resolveRoomTimeout    = 15 * time.Second // ResolveCallRoom (may create the portal)
	joinTimeout           = 10 * time.Second // one rtc.Join attempt (ctx deadline)
	maxJoinAttempts       = 2
	answerTimeout         = 40 * time.Second // owner joined → EventAnswered (covers rtc join + 32 s ACK wait)
	ownerRTCGrace         = 10 * time.Second // no owner participant in LiveKit after answer → rtc_lost
	ringKeepalive         = 60 * time.Second // re-send 180 while ringing
	statsInterval         = 5 * time.Second
	membershipRefreshLead = 10 * time.Minute
	teardownStepTimeout   = 5 * time.Second
	callEventBuffer       = 32
)
```

### 8.2 Actor model and concurrency rules

1. `HandleInvite` (diago handler goroutine) does only: mutex, busy check, construct `call`, `go
   c.run()`. Busy = the login already has an active call; the new call gets `busy=true` and is not
   registered as active.
2. Each call has one goroutine (`run`) that owns every field of the call. No other goroutine reads or
   writes call state. The manager map is the only shared structure (mutex; holds `*call` pointers whose
   only concurrency-safe members are `events chan event`, `id`, `roomID` (set once before registration
   is visible to `HandleRTCEvent`; see 3) and `done chan struct{}`).
3. A call becomes routable for Matrix events only after its room is resolved: `run` sets `roomID` and
   then calls `m.setRoutable(c)` (under the manager mutex). `HandleRTCEvent` delivers only if
   `evt.RoomID == c.roomID`.
4. Inputs to `run`'s single `select`: `c.events` (Matrix events, join results, shutdown), the SIP call's
   `Events()`, the RTC session's `Events()` (nil channel until joined), the bridge's `Done()` (nil until
   active), and clock timers/tickers (`ringTimer`, `keepaliveTicker`, `answerTimer`, `graceTimer`,
   `statsTicker`, `refreshTimer`; nil channels when unset).
5. Senders never block: `HandleRTCEvent` and the join goroutine use `select { case c.events <- e: default:
   drop + Warn }` (the join goroutine's result is never dropped because at most one join is in flight and
   the buffer is 32; still non-blocking). sipua and rtc channels are buffered by their packages.
6. Inline I/O in `run` (allowed, bounded): `ResolveCallRoom` (15 s), every GhostIntent call and
   `SendCallNotice`/`MarkReadAsOwner` (5 s each), `bridge.Stop()`, `session.Leave` (5 s). Async: `rtc.Join`
   (a goroutine per attempt, ctx with `joinTimeout`), SIP `Answer`/`Reject`/`Hangup` (sipua commands).
7. The join goroutine receives `obs.CarrySpan(context.Background(), setupCtx)` and starts `rtc.join` as a
   child (via `Joiner.Join`). The actor guarantees the parent waits: before ending `call.inbound` it
   cancels any in-flight join and waits for its result event (bounded by `joinTimeout` + 1 s), and a
   late successful join is immediately left.
8. Telemetry never adds I/O on any of these goroutines; metrics are recorded by the actor only (stats
   flush every 5 s, call metrics at the end).
9. Logs: the call's clean logger is `cfg.Log.With().Str("component", "calls").Str("call_id", id).
   Str("sip_call_id", info.CallID).Logger()`; every line under a span uses `c.logFor(ctx)` =
   `obs.Logger(ctx, &c.log)`.

### 8.3 States, events, outcomes

```go
type state int

const (
	stSetup     state = iota // guards, room, membership, ring (inline, one step)
	stRinging                // ring sent; join in flight or done; waiting for owner/decline/cancel/timeout
	stAnswering              // owner joined; waiting for rtc ready and/or SIP ACK
	stActive                 // ACK received; media bridged
	stEnding                 // teardown running (all inputs ignored)
	stEnded
)

type eventKind int

const (
	evOwnerMembership eventKind = iota + 1 // Matrix: owner call.member (parsed in the actor)
	evDecline                              // Matrix: rtc.decline
	evJoinResult                           // async: rtc.Join returned (session or error, attempt number)
	evShutdown                             // EndLogin / Stop
)

type event struct {
	kind    eventKind
	rtc     matrixrtc.Event   // evOwnerMembership, evDecline
	from    trace.SpanContext // span context of the matrixrtc.ingest span (for links)
	session rtc.Session       // evJoinResult
	err     error             // evJoinResult
	attempt int               // evJoinResult
}
```

Outcomes (`call.outcome`, closed enum of TS §5.2) and end reasons (`call.end_reason`, TS §5.2 as edited
in §13):

| Situation | outcome | end_reason | SIP final we send | Notice |
|---|---|---|---|---|
| Answered, then any hangup | `answered` | `bye` (SIP BYE either way), `rtc_lost`, `rtp_timeout`, `bridge_stop`, `error` | 200 | `Call · <duration>` |
| Caller CANCEL while ringing/answering | `cancelled` | `cancel` | (487 by sipgo) | `Missed call` |
| Ring timeout | `timeout` | `ring_timeout` | 480 | `Missed call` |
| Owner declined | `declined` | `decline` | 603 | `Declined call` |
| Owner joined then left before audio connected | `missed` | `bye` | 480 | `Missed call` |
| Second INVITE while a call is active | `busy` | `rejected` | 486 | `Missed call (line busy)` |
| Withheld/unparseable caller | `rejected` | `rejected` | 480 | none (no room) |
| No PCMU / no SDP | `rejected` | `rejected` | 488 | `Missed call (unsupported audio codec)` |
| Encrypted room | `rejected` | `rejected` | 480 | `Missed call: calls need an unencrypted room. This room is encrypted, so the call was sent to voicemail.` |
| Room resolve failed, owner unknown | `failed` | `error` | 480 | none |
| No service URL, membership/ring send failed, LiveKit join failed twice, answer timeout, SIP answer failed (not cancel) | `failed` | `error` | 480 (if no final yet) | `Missed call (could not connect audio)` |
| Shutdown/logout while ringing | `failed` | `bridge_stop` | 480 | `Missed call` |

`hangup.by` (answered calls): `remote` (SIP BYE received), `owner` (owner left: membership or LiveKit
presence), `bridge` (RTP timeout, our LiveKit session lost, shutdown, error).

### 8.4 Setup step (`stSetup`, inline in `run`, before the first `select`)

Order (each failure jumps to the failure branch, §8.6, with the outcome from the table):

1. Start `call.inbound` (§10.2), `mrtcsip.call.started{inbound}` +1, `mrtcsip.call.active{inbound}` +1.
2. Caller: `raw = info.AssertedUser` if non-empty else `info.FromUser`; `e164, err =
   phone.Normalize(raw, cfg.DefaultCountryCode)`; error (incl. "anonymous", short codes) → withheld:
   `Reject(480)`, outcome `rejected`, `call.remote.withheld=true`.
3. If `busy`: `Reject(486, "Busy Here")`, resolve the room (as in step 5, but never `setRoutable`: a
   busy call never receives Matrix events) best effort, notice busy, outcome `busy`.
4. Offer: `!info.Offer.HasSDP || !info.Offer.PCMU` → `Reject(488, "Not Acceptable Here")`, resolve room
   best effort, notice codec, outcome `rejected`.
5. `owner := Host.OwnerMXID(login)`; "" → failed. `room, err := Host.ResolveCallRoom(ctx 15 s, login,
   e164)` inside `call.resolve_room`; error → failed. Set `c.roomID`, `m.setRoutable(c)`.
6. `room.Encrypted` → `Reject(480)`, notice encrypted, Error log "Refusing to ring in an encrypted room",
   outcome `rejected`.
7. `serviceURL, err := cfg.ServiceURL(ctx 5 s)`; error → failed (notice "could not connect audio").
8. `sip.Ringing()`; span event `sip.response.sent{sip.response.status_code=180}`.
9. Device id = `"SIP" + id[len(id)-8:]`. Ghost membership: `memberEventID, err :=
   room.Ghost.SendState(ctx 5 s, room.RoomID, matrixrtc.CallMember,
   matrixrtc.MemberStateKey(ghost, device), matrixrtc.JoinContent({...CreatedTS: now, Expires:
   DefaultMembershipExpiry}))` inside `matrixrtc.membership.publish`; error → failed.
10. Ring: `ringEventID, err := room.Ghost.SendMessage(ctx 5 s, room.RoomID, matrixrtc.RTCNotification,
    matrixrtc.RingContent({Owner, MembershipEventID, Lifetime: RingTimeout, SenderTS: now}))` inside
    `matrixrtc.ring.send`; error → failed (membership is cleared by the failure branch).
11. Start `call.ring` span; `ringTimer = RingTimeout`; `keepaliveTicker = ringKeepalive` (only if
    `RingTimeout > ringKeepalive`); start join attempt 1 (goroutine); state `stRinging`.

### 8.5 Transitions

`own(e)` = `matrixrtc.ParseMembership(e.rtc, now)` ok and `UserID == owner` and `e.rtc.Sender == owner`.
`devices` = set of owner device ids seen live since the ring. `lkOwners` = count of owner LiveKit
participants (from rtc events). "Fail(x)" = failure branch with outcome x (§8.6). "Teardown(by, reason)"
= answered-call teardown (§8.7).

| State | Input | Guard | Actions | Next |
|---|---|---|---|---|
| Ringing | SIP `EventEnded{EndCancelled}` | | event `sip.cancel.received`; Fail(cancelled) | Ending |
| Ringing | SIP `EventEnded{other}` | | Fail(failed) | Ending |
| Ringing | `evDecline` | `DeclineTarget == ringEventID` and sender == owner | end `call.ring` (link ingest); `Reject(603, "Decline")`; Fail(declined) | Ending |
| Ringing | `evDecline` | otherwise | ignore (Debug log) | Ringing |
| Ringing | `ringTimer` | | end `call.ring`; `Reject(480)`; Fail(timeout) | Ending |
| Ringing | `keepaliveTicker` | | `sip.Ringing()` | Ringing |
| Ringing | `evOwnerMembership` | `own(e)` and `Live` | end `call.ring` (link ingest); add device; stop ring timer and keepalive; `answerTimer = answerTimeout`; if session ready → **answer** (below); then `MarkReadAsOwner(ringEventID)` (5 s; `ErrNoDoublePuppet` → Debug, other errors → Warn) | Answering |
| Ringing | `evOwnerMembership` | not own or not Live | ignore | Ringing |
| Ringing | `evJoinResult` ok | | store session; subscribe to its events | Ringing |
| Ringing | `evJoinResult` err | `attempt < maxJoinAttempts` | start attempt+1 | Ringing |
| Ringing | `evJoinResult` err | last attempt | `Reject(480)`; Fail(failed) | Ending |
| Ringing | rtc `EventDisconnected` | | drop session; start a new attempt if attempts remain, else `Reject(480)`, Fail(failed) | Ringing / Ending |
| Ringing | rtc `EventOwnerJoined/Left` | | update `lkOwners` | Ringing |
| Ringing | `evShutdown` | | `Reject(480)`; Fail(failed, bridge_stop) | Ending |
| Answering | `evJoinResult` ok | | store session; if not yet answered → **answer** | Answering |
| Answering | `evJoinResult` err | attempts remain | next attempt | Answering |
| Answering | `evJoinResult` err | last | `Reject(480)`; Fail(failed) | Ending |
| Answering | SIP `EventAnswered` | `len(devices) > 0` | **activate** (below) | Active |
| Answering | SIP `EventAnswered` | `len(devices) == 0` (owner left meanwhile) | **activate**, then Teardown(owner, bye) | Ending |
| Answering | SIP `EventAnswerFailed` / `EventEnded{EndCancelled}` | | event `sip.cancel.received` if cancelled; Fail(cancelled or failed) | Ending |
| Answering | `evOwnerMembership` | own, Live | add device | Answering |
| Answering | `evOwnerMembership` | own, not Live | remove device; if empty and answer not yet issued → `Reject(480)`, Fail(missed) | Answering / Ending |
| Answering | `answerTimer` | | `Hangup()` (sipua: 480 if not answered, BYE if it raced); Fail(failed) | Ending |
| Answering | `evDecline` | | ignore (answered elsewhere) | Answering |
| Answering | `evShutdown` | | `Hangup()`; Fail(failed, bridge_stop) | Ending |
| Active | SIP `EventEnded{EndRemoteBye}` | | event `sip.bye.received`; Teardown(remote, bye), no BYE sent | Ending |
| Active | SIP `EventEnded{other}` | | Teardown(bridge, error) | Ending |
| Active | `evOwnerMembership` | own, Live | add device | Active |
| Active | `evOwnerMembership` | own, not Live | remove device; if empty → Teardown(owner, bye) else emit `call.owner.left` linked root | Active / Ending |
| Active | rtc `EventOwnerLeft` | `lkOwners` becomes 0 | `graceTimer = ownerRTCGrace` | Active |
| Active | rtc `EventOwnerJoined` | | `lkOwners++`; stop grace timer | Active |
| Active | `graceTimer` | | Teardown(owner, rtc_lost) | Ending |
| Active | rtc `EventDisconnected` | | Teardown(bridge, rtc_lost) | Ending |
| Active | bridge `Done()` | | Teardown(bridge, `rtp_timeout` if `Err()==ErrRTPTimeout` else `error`) | Ending |
| Active | `statsTicker` | | flush media stats (§10.3); degraded check | Active |
| Active | `refreshTimer` | | membership refresh (D15), span `call.membership.refresh` (linked root to setup); reschedule | Active |
| Active | `evShutdown` | | Teardown(bridge, bridge_stop) | Ending |
| Ending | anything | | ignored (SIP/RTC channels still drained until closed) | Ending |

**answer** (issued once): start `call.answer` span (child of `call.inbound`) with `media.codec=PCMU`,
`media.ptime_ms=20`; `sip.Answer()`.

**activate** (on `EventAnswered`): add events to `call.answer`: `sip.response.sent{200}` at `OKSentAt`
and `sip.ack.received` at `AckAt`; `media.start` (INTERNAL, child of `call.answer`): `bridge, err :=
StartMedia(media.Config{Clock, Log: c.log, RTPTimeout}, ev.Media, session.Audio())` (error → end spans,
Teardown(bridge, error)); end `media.start`, end `call.answer`; end `call.inbound` with `call.outcome=
answered`, `sip.response.status_code=200` (end time = `AckAt`, so the setup span ends at ACK);
`answeredAt = AckAt`; start `statsTicker`; `refreshTimer` at `memberCreated + DefaultMembershipExpiry -
membershipRefreshLead`; `graceTimer` if `lkOwners == 0`; record setup duration (`OKSentAt -
ReceivedAt`).

The rtc session must be ready before `answer` is issued (the ghost track is published before the owner
can hear anything, S1 pre-join decision).

### 8.6 Failure branch (unanswered calls, inside `call.inbound`)

Runs inline, in this order, each step best effort with `matrixOpTimeout`, each a child span of
`call.inbound` (names per TS §5.6):

1. SIP final response: already issued by the transition (`Reject`/`Hangup`) or implicit (CANCEL → 487 by
   sipgo). Add span event `sip.response.sent{status}` when the sipua `EventEnded` arrives (wait up to
   5 s for it; its `StatusCode` is the authoritative final status).
2. Cancel an in-flight join and wait for its result (§8.2 rule 7).
3. If the ring was sent and the outcome is `cancelled`, `timeout`, `failed`, `missed` or `bridge_stop`:
   `matrixrtc.ring.redact` → `Ghost.Redact(room, ringEventID, "call ended")`. Not on `declined`.
4. If the membership was sent: `matrixrtc.membership.clear` → `Ghost.SendState(..., MemberStateKey,
   LeaveContent())`.
5. If a session exists: `rtc.leave` (CLIENT) → `session.Leave(ctx 5 s)`.
6. Notice per the table (`call.notice.send` → `Host.SendCallNotice`), if a room exists.
7. End `call.ring` if still open (with the outcome), end `call.inbound` with `call.outcome`, final
   `sip.response.status_code`, Error status only for `failed`.
8. Metrics and `call.session` (§10), `call.active` −1, remove from manager, close `done`.

### 8.7 Answered-call teardown (`call.teardown`, linked root)

`call.teardown` (INTERNAL) is a linked root to the setup span context, attributes `call.id`,
`call.hangup.by`, `call.end_reason`. Steps (inline, each bounded by `teardownStepTimeout`):

1. `media.stop`: `bridge.Stop()`; read final `Stats`.
2. SIP: if the call did not end remotely: `sip.bye` (CLIENT) → `sip.Hangup()` and wait for
   `EventEnded` (5 s); status from the event. If it ended remotely: event `sip.bye.received` (already
   added in the transition).
3. `rtc.leave` → `session.Leave`.
4. `matrixrtc.membership.clear`.
5. `call.notice.send` → `Call · <formatDuration(endedAt - answeredAt)>`.
6. End `call.teardown`; then `call.session` (§10.2), metrics, `call.active` −1, remove, close `done`.

Racing hangup triggers (BYE and owner leave arriving together) produce one teardown: the first input
moves the actor to `stEnding`; later inputs are ignored. sipua ignores `Hangup` after the dialog ended,
so at most one BYE is sent.

### 8.8 Notices (`notices.go`, exact strings)

```go
const (
	noticeMissed      = "Missed call"
	noticeDeclined    = "Declined call"
	noticeBusy        = "Missed call (line busy)"
	noticeCodec       = "Missed call (unsupported audio codec)"
	noticeNoAudio     = "Missed call (could not connect audio)"
	noticeEncrypted   = "Missed call: calls need an unencrypted room. This room is encrypted, so the call was sent to voicemail."
	noticeAnsweredFmt = "Call · %s" // formatDuration
)

// formatDuration: < 1 min "45s"; < 1 h "1m 20s" (seconds always shown, "2m 0s"); else "1h 02m".
// Rounds down to whole seconds; negative → "0s".
func formatDuration(d time.Duration) string
```

### 8.9 Edge cases (normative)

1. **Owner answers on two devices**: both device ids join `devices`; the first triggers the answer; the
   call continues while any remains; `call.owner.left` marks the first device leaving.
2. **Owner membership before LiveKit is ready**: `stAnswering` waits for the join (attempts continue)
   within `answerTimeout`; SIP is answered only once the session exists.
3. **Caller hangs up during answer**: before 200 is sent, sipgo's `WriteResponse` sees the CANCEL
   (`tx.Done()`), `Answer` fails, sipua emits `EventAnswerFailed` then `EventEnded{EndCancelled}` →
   Fail(cancelled). After 200 but before ACK, a BYE ends the dialog; sipua emits `EventAnswerFailed`
   (no ACK) or `EventAnswered` followed by `EventEnded{EndRemoteBye}`; both lead to a single teardown.
4. **BYE vs owner leave**: §8.7 last paragraph.
5. **Owner's stale membership**: only new events count; an existing live owner membership from an earlier
   call produces no event and is ignored (as S1 did with its `ignore` set). A refresh of such a stale
   membership during ringing would be taken as an answer: accepted limitation.
6. **Ghost membership expiry**: D15 refresh keeps it alive for any call length.
7. **LiveKit token expiry** (1 h): the SDK refreshes over signalling; a final disconnect is
   `EventDisconnected` → teardown `rtc_lost`.
8. **Decline from a non-owner or for another ring**: ignored (wire §3.1).
9. **Events for another room** of the same login: not routed (`roomID` check).
10. **Login logged out / bridge stopping mid-call**: `EndLogin` → `evShutdown`; the SIP UA is stopped
    only after `EndLogin` returns (BYE goes out first).
11. **INVITE before the login is attached** (during `!sip login`): the host rejects with 480 (§9.3).
12. **Ring timeout > 120 s**: allowed by config; Element Web caps the ring at 120 s (wire §2.2), Element X
    does not. Documented in the README.

---

## 9. Standalone host changes (`pkg/connector`, Dockerfile, docs) — T6

### 9.1 Config

```go
	RTPTimeoutSeconds int `yaml:"rtp_timeout_seconds"`
```

Default `60` (`defaultRTPTimeoutSeconds`); `ApplyDefaults` sets it when 0; `Validate`:
`network.rtp_timeout_seconds: 10..3600`; helper `RTPTimeout() time.Duration`; upgrader
`h.Copy(configupgrade.Int, "rtp_timeout_seconds")`. `RingTimeout()` doc comment drops "unused until
Phase 2". `example-config.yaml` changes:

```yaml
# How long an incoming call rings in Matrix before it is treated as missed (5-300). Element Web stops
# ringing after 120 s regardless.
ring_timeout_seconds: 30
# Hang up an answered call when no audio (RTP) arrives from the SIP provider for this long.
rtp_timeout_seconds: 60
# lk-jwt-service (MatrixRTC auth service) URL, e.g. https://matrix-rtc.example.org/livekit/jwt.
# Empty = discover from https://<homeserver.domain>/.well-known/matrix/client (org.matrix.msc4143.rtc_foci).
livekit_service_url: ""
```

`config_test.go`: example round trip includes the new key; validation rows for 9 and 3601; upgrade keeps
a user value.

### 9.2 Connector wiring

`connector.go`: add fields `Calls *calls.Manager` and `lkURL *rtc.ServiceURLResolver`.

`start.go` `Start(ctx)`, after the existing steps:

1. `rtc.SetLibraryLogger(log)`.
2. `hc := &http.Client{Timeout: 10 * time.Second, Transport: obs.WrapHTTPTransport(http.DefaultTransport,
   rtc.PathTemplate)}`.
3. `c.lkURL = rtc.NewServiceURLResolver(c.Config.LiveKitServiceURL, c.br.Matrix.ServerName(), hc,
   clock.Real())` (`bridgev2/matrixinterface.go:71` `ServerName() string`, implemented at
   `bridgev2/matrix/connector.go:764` as `Config.Homeserver.Domain`).
4. Warm-up: `go func() { u, err := c.lkURL.Get(context.Background()); log Info "LiveKit service URL"
   (url host only, source) or Warn with err }()`.
5. `c.Calls = calls.NewManager(calls.Config{Host: c.CallHost(), Joiner: rtc.NewJoiner(rtc.Config{HTTPClient:
   hc, Log: log}), ServiceURL: c.lkURL.Get, RingTimeout: c.Config.RingTimeout(), RTPTimeout:
   c.Config.RTPTimeout(), DefaultCountryCode: c.Config.DefaultCountryCode, Log:
   c.br.Log.With().Str("component", "calls").Logger()})`.
6. `c.RTC.SetSink(c.Calls)`.

`telemetry.go`: `Stop()` calls `c.Calls.Stop(ctx 5 s)` (nil-safe) **before** `telemetry.Shutdown`;
`StartAttrs` adds `attribute.String("livekit.service_url.source", c.lkURL.Source())` ("config" or ""
at that moment; discovery may still be running).

### 9.3 Client wiring (`client.go`)

In `start()`'s `sipua.Config`, add:

```go
OnInvite: func(call sipua.InboundCall) {
	cl.mu.Lock()
	login := cl.login
	cl.mu.Unlock()
	if login == nil || cl.c.Calls == nil {
		call.Reject(480, "Temporarily Unavailable")
		return
	}
	cl.c.Calls.HandleInvite(core.LoginRef(login.ID), cl.meta.DisplayNumber, call)
},
```

In `stop(ctx)`, before `ua.Stop`: if `cl.login != nil && cl.c.Calls != nil`:
`ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second); cl.c.Calls.EndLogin(ectx,
core.LoginRef(login.ID)); cancel()`.

Login completion text (`login.go`) changes to: `"Registered %s via %s. Incoming calls now ring your
Matrix clients."`.

### 9.4 CallHost (`callhost.go`)

In `ResolveCallRoom`, after the portal exists:

```go
encrypted := false
if mc, ok := h.c.br.Matrix.(*matrix.Connector); ok {
	if enc, err := mc.StateStore.IsEncrypted(ctx, portal.MXID); err == nil {
		encrypted = enc
	} else {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to read room encryption state")
	}
}
return &core.CallRoom{RoomID: portal.MXID, RemoteE164: e164, Ghost: gi, Encrypted: encrypted}, nil
```

(`bridgev2/matrix/connector.go:64` `StateStore *sqlstatestore.SQLStateStore`; `statestore.go:52`
`IsEncrypted(ctx, roomID) (bool, error)`.)

### 9.5 Dockerfile

Build line: `go build -tags "nocrypto nolibopusfile" ...`. Keep `opus-dev` (builder) and `opus` (runtime);
`opusfile`/`opusfile-dev` may stay (harmless). Comment: "nolibopusfile: hraban/opus without opusfile
(stream decoding unused)".

### 9.6 Docs

- README: status (inbound calls work); requirements (MatrixRTC transport via `.well-known` or
  `livekit_service_url`; lk-jwt-service with the homeserver in `LIVEKIT_FULL_ACCESS_HOMESERVERS`, wire
  §8.4); build tags; config table (`rtp_timeout_seconds`, livekit discovery); networking (RTP range must
  be reachable; 2 ports per call); "Calls" section (what rings, decline → 603, missed → voicemail, busy,
  encrypted rooms refused, withheld callers rejected, one call at a time, Element Web 120 s cap, Sable
  Android needs double puppeting to stop ringing); "Tracing and metrics" updated with every Phase 2 span
  and metric (TS §8.2 definition of done) and the `call_id` log field.
- CLAUDE.md: Status section (Phase 2 on `phase2-inbound`, see this spec); Conventions: build/vet/test with
  `-tags "nocrypto nolibopusfile"`.
- CREDITS.md: add `livekit/server-sdk-go` v2.18.1 (Apache-2.0), `livekit/protocol` v1.49.0
  (Apache-2.0), `pion/webrtc` v4.2.15 and `pion/rtp` (MIT), mark hraban/opus as used directly by
  `pkg/media` (MIT); note no media-sdk; G.711 table is our own implementation of the public ITU-T
  algorithm.
- PLAN.md: tick Phase 2 boxes when done; Current state entry.

---

## 10. Telemetry (TS §5.6, §5.10, §4.3, §8.2)

### 10.1 Span catalogue for Phase 2

| Span | Kind | Root? | Emitted by | Attributes |
|---|---|---|---|---|
| `call.inbound` | SERVER | **root** (backdated to `ReceivedAt`) | calls | `call.id`, `call.direction=inbound`, `call.trigger=invite`, `sip.call_id`, `call.remote.e164` or `call.remote.withheld=true`, `call.local.e164`, `matrix.room_id`, `matrix.ghost_id`, `matrix.owner_id`, `network.peer.address`/`network.peer.port` (signalling source), final `sip.response.status_code`, `call.outcome`. Events: `sip.response.sent {sip.response.status_code}` (100 at `TryingSentAt`, 180, final), `sip.cancel.received`. |
| `call.resolve_room` | INTERNAL | child-only | calls | `call.remote.e164` |
| `matrixrtc.membership.publish` | CLIENT | child-only | calls | `matrix.room_id`, `matrixrtc.membership.device_id`, `matrixrtc.membership.expires_ms`, `matrix.event_id` (result) |
| `matrixrtc.ring.send` | CLIENT | child-only | calls | `matrix.room_id`, `matrixrtc.ring.lifetime_ms`, `matrix.event_id` |
| `rtc.join` | INTERNAL | child-only | rtc | `call.id`, `rtc.join.attempt`, `matrix.room_id`, `matrix.ghost_id`, `matrixrtc.membership.device_id`, `outcome` |
| `rtc.openid_token` | CLIENT | child-only | rtc | none (token never recorded) |
| `rtc.jwt_fetch` | CLIENT | child-only | rtc | `server.address`, `http.response.status_code` |
| `rtc.livekit.connect` | CLIENT | child-only | rtc | `livekit.room`, `livekit.server.address`, `livekit.participant.identity` |
| `rtc.track.publish` | INTERNAL | child-only | rtc | |
| `call.ring` | INTERNAL | child-only | calls | `matrix.event_id` (ring), `call.outcome` (what ended it); **link** to the `matrixrtc.ingest` span that ended it (owner join/decline) |
| `call.answer` | INTERNAL | child-only | calls | `media.codec`, `media.ptime_ms`, `media.telephone_event`; events `sip.response.sent{200}`, `sip.ack.received` |
| `media.start` | INTERNAL | child-only | calls | `network.peer.address` (remote RTP host) |
| `matrixrtc.membership.clear` | CLIENT | child-only | calls | |
| `matrixrtc.ring.redact` | CLIENT | child-only | calls | `matrix.event_id` |
| `rtc.leave` | CLIENT | child-only | calls | |
| `call.notice.send` | INTERNAL | child-only | calls | `call.outcome` (notice kind) — never the text |
| `call.teardown` | INTERNAL | linked root → setup | calls | `call.id`, `call.hangup.by`, `call.end_reason`; event `sip.bye.received` |
| `sip.bye` | CLIENT | child-only (of teardown) | calls | `sip.response.status_code` |
| `media.stop` | INTERNAL | child-only | calls | `rtp.*` summary |
| `call.owner.left` | INTERNAL | linked root → setup | calls | `call.id`, `matrixrtc.membership.device_id` (zero-length) |
| `call.media.degraded` | INTERNAL | linked root → setup | calls | `call.id`, `rtp.lost`, `rtp.packets.rx` (once per call; loss > 5 % in a 5 s window or no SIP RTP for 5 s) |
| `call.membership.refresh` | CLIENT | linked root → setup | calls | `call.id`, `matrixrtc.membership.expires_ms` |
| `call.session` | INTERNAL | linked root, links → setup and teardown (if any) | calls | post hoc: start = `ReceivedAt`, ended immediately; `call.id`, `call.direction`, `call.outcome`, `call.duration_s` (0 if unanswered), `media.codec`, `rtp.packets.rx`, `rtp.packets.tx`, `rtp.lost`, `rtp.late`, `rtp.jitter.max_ms`, `rtp.jitter.mean_ms`, `call.end_reason`, `call.hangup.by` (answered); up to 32 buffered past events (owner left, degraded, refresh) with original timestamps |

Every span carries `call.id`. Matrix HTTP spans (host transport) and `POST /sfu/get` (wrapped
lk-jwt client) appear automatically as children because the ctx is passed through.

`matrixrtc.ingest` (Phase 1) gains `matrixrtc.routed_to` = `call` and `call.id` when the manager routes
the event (set by `HandleRTCEvent` on `trace.SpanFromContext(ctx)`), else stays `none`.

### 10.2 Lifetimes

- `call.inbound` ends at ACK (`AckAt`) for answered calls, or after the failure branch. Its children all
  end before it (join goroutine waited for, §8.2.7). Bounded by ring timeout + answer timeout + teardown
  steps (≈ 2 minutes worst case).
- `call.ring` ends at owner join, decline, CANCEL, timeout or failure.
- No span with children stays open past answer (TS §5.10). `call.teardown` lasts seconds;
  `call.session` is zero-length post hoc.
- On bridge stop, `Manager.Stop` drives every active call through teardown (`bridge_stop`), emitting
  `call.session` before `telemetry.Shutdown` flushes.

### 10.3 Metrics (recorded only by the actor)

| Instrument | When | Attributes |
|---|---|---|
| `call.started` +1, `call.active` +1 | setup step 1 | `call.direction=inbound` |
| `call.active` −1, `call.ended` +1 | end of failure branch / teardown | `call.direction`, `call.outcome`, `sip.response.status_class` (class of the final status we sent; `2xx` answered, `4xx` for 480/486/487/488, `6xx` for 603, `none` if 0) |
| `call.setup.duration` | at the end | `ReceivedAt` → `OKSentAt` (answered) or → final failure time; `call.direction`, `call.outcome` |
| `call.ring.duration` | at the end, only if the ring was sent | ring sent → `call.ring` end; `call.direction`, `call.outcome` |
| `call.duration` | answered calls | `AckAt` → teardown start; `call.direction`, `media.codec=PCMU` |
| `rtc.join.duration` | per attempt, by `pkg/rtc` | `outcome` |
| `media.rtp.packets` | stats tick + final | delta `SIPRx` (`rx`), `SIPTx` (`tx`) |
| `media.rtp.lost`, `media.rtp.late` | stats tick + final | deltas |
| `media.rtp.jitter` | stats tick | `SIPJitter.Seconds()` if at least one packet arrived in the window |
| `media.concealed` | stats tick + final | deltas, `media.leg=sip_to_rtc` / `rtc_to_sip` |

Pre-built option values (package-level vars in `pkg/calls/telemetry.go`, built once with
`metric.WithAttributeSet(attribute.NewSet(...))`) for every fixed attribute combination used on the 5 s
path (rx, tx, sip_to_rtc, rtc_to_sip, none), so the stats flush allocates nothing per call. Call-end
metrics may build their attribute sets (once per call).

### 10.4 Logs

One Info line per transition worth reading: "Inbound call" (from, to, room), "Ringing owner" (ring
event id, lifetime), "Owner answered" (device), "Call connected" (codec, remote RTP), "Call ended"
(outcome, end_reason, hangup_by, duration, rtp summary). Warn for best-effort failures (redact, clear,
receipt, notice). Error for encrypted rooms and unexpected failures. All via `c.logFor(ctx)`, all with
`call_id`. Never: SDP, raw SIP, tokens, JWTs, event content, audio, DTMF.

---

## 11. Privacy (additions to TS §6 for Phase 2)

- OpenID tokens and LiveKit JWTs: never logged, never in spans, never in error strings (token client
  tests assert it); LiveKit library logs are scrubbed and key/values dropped (D22).
- SDP: only `Offer` facts (codec names, media `ip:port`) are logged; never the body.
- Matrix event content (call.member, decline, ring) is parsed in memory, never logged.
- Phone numbers and MXIDs only on spans/logs, never on metrics (allowlist test).
- DTMF: telephone-event packets are counted in `SIPRx` and dropped; never decoded or logged.
- Notices contain no number (the room already identifies it).

Tests (`pkg/calls/telemetry_test.go`): over a full answered call and over a call whose fake joiner
fails with the error text `token_error: access_token=SECRETTOKEN jwt eyJhbGciOi.eyJzdWIi.c2ln`, assert
`obstest.AssertNoSecrets(t, env.Rec, logBuf, "SECRETTOKEN", "eyJhbGciOi.eyJzdWIi.c2ln", "v=0",
"a=rtpmap")` (the scrubbing of `obs.Fail`/`obs.Scrub` covers both forms; the fake offer carries an SDP-like
`Codecs` list but the real body never reaches calls). And
`obstest.AssertMetricAttrsAllowed(t, env.Reader, obstest.DefaultMetricAllowlist)`.

---

## 12. Tests and live test plan

### 12.1 Unit and component tests

All offline, standard `testing` only, `-race`.

| Package | Tests |
|---|---|
| `pkg/clock` | §4.1 |
| `pkg/matrixrtc` | §4.3 |
| `pkg/core` | `ingest_obs_test.go`: sink-set `routed_to` wins; default `none` |
| `pkg/obs` | Phase 2 instruments non-nil when off; created with the right names when a manual reader is installed |
| `internal/importcheck` | §4.5 |
| `pkg/media` | §7.4 |
| `pkg/sipua` | `offer_test.go`: JMP-like offer (`m=audio 5004 RTP/AVP 0 18 101`, rtpmaps `PCMU/8000`, `G729/8000`, `telephone-event/8000`) → PCMU, TelephoneEvent, MediaAddr, Codecs `["PCMU/0","G729/18","telephone-event/101"]`; PCMA-only → PCMU false; empty body → HasSDP false; garbage. `inbound_loop_test.go` with a fake `dialogServer`: Trying sent first; `Ringing` before final only; `Answer` → `EventAnswered` with media and times, then remote BYE (`Done`) → `EventEnded{EndRemoteBye, 200}`; `Reject(603)` → `Respond(603)` and `EventEnded{EndRejected, 603}`; commands after end ignored and never block; CANCEL (`Done` with `Cancelled`) → `EventEnded{EndCancelled, 487}`; `Answer` error with `Cancelled` → `EventAnswerFailed` then `EventEnded{EndCancelled}`; `Hangup` before answer → 480; `Hangup` after answer → `Bye` once even if called twice; safety timeout with a fake clock → 480 + `EndFailed`; channel closed after `EventEnded`; never more than 2 events. `inbound_loopback_test.go`: start a `sipua.UA` bound to 127.0.0.1 with a registrar pointing at a closed local UDP port (registration fails transiently; the socket serves), `OnInvite` that answers; a diago UAC on another 127.0.0.1 port INVITEs `sip:x@127.0.0.1:<port>` with PCMU; assert `OnInvite` called with parsed `InviteInfo`, `EventAnswered` arrives, RTP flows both ways for 200 ms (UAC writes PCMU, test reads it from `media.SIPLeg.ReadRTP`; leg writes, UAC reads), UAC BYE → `EventEnded{EndRemoteBye}`. Skip with `t.Skip` if binding 127.0.0.1 fails. |
| `pkg/rtc` | `token_test.go` (§6.3); `discover_test.go` (§6.4); `logger_test.go` (§6.5); `identity_test.go`: `isOwner` prefix rules (`@o:s:DEV` yes, `@o:s2:DEV` no, `@oo:s:DEV` no), `LiveKitRoomName` matches the MSC4195 test vector of wire §4.5 when called with the vector's inputs (expose an unexported `hashJSON` for the test), `PathTemplate`. LiveKit connect is covered by the live test only. |
| `pkg/calls` | below |
| `pkg/connector` | config (§9.1); `client_lifecycle_test.go`: `OnInvite` with no login rejects 480 (fake InboundCall); `stop` calls `EndLogin` before `UA.Stop` (ordering recorded by fakes). |

`pkg/calls` fakes (`fakes_test.go`):

- `fakeSIPCall` implements `sipua.InboundCall`: records commands in order; the test emits events with
  `answer(t)`, `cancel()`, `bye()`, `answerFail(err)`; `Answer` auto-answers if configured; `Hangup`
  after answered emits `EventEnded{EndLocalHangup}`; `Reject` emits `EventEnded{EndRejected, code}`.
- `fakeJoiner`/`fakeSession`: scripted results per attempt (ok, error, block-until-ctx); session
  `Audio()` returns a fake `media.RTCLeg`; test pushes rtc events.
- `fakeHost` + `fakeGhost`: record `ResolveCallRoom`, `SendState` (type, state key, content),
  `SendMessage`, `Redact`, `SendCallNotice` texts, `MarkReadAsOwner`; configurable errors, `Encrypted`,
  owner MXID.
- `fakeBridge` via `Config.StartMedia`: records start/stop, settable `Stats`, closable `Done` with `Err`.
- `clock.Fake`.

`call_test.go` cases (each also asserts the final metric outcome):

1. Happy path: INVITE → `Ringing` command; membership content equals `JoinContent` with the configured
   service URL and device `SIP…`; ring content has `lifetime: 30000` and references the membership event;
   join attempt 1 started; owner live membership → `MarkReadAsOwner(ringEventID)`; `Answer` command
   issued; `EventAnswered` → bridge started with the SIP leg and session audio; remote BYE → no `Hangup`
   command, `bridge.Stop`, `session.Leave`, membership cleared with `{}`, notice `Call · 1m 20s` (fake
   clock advanced 80 s), ring **not** redacted, outcome `answered`.
2. Owner joins before the join finishes → no `Answer` until `evJoinResult` ok; then `Answer`.
3. Join fails once then succeeds → 2 attempts, call proceeds.
4. Join fails twice → `Reject(480)`, redact, clear, notice `Missed call (could not connect audio)`, `failed`.
5. Decline from owner referencing the ring → `Reject(603)`, clear, **no** redact, notice `Declined call`.
6. Decline from another user or for another event → ignored; then ring timeout.
7. Ring timeout → `Reject(480)`, redact then clear (order asserted), notice `Missed call`, `timeout`.
8. CANCEL while ringing → no Reject, redact, clear, leave, `Missed call`, `cancelled`.
9. CANCEL while answering (`EventAnswerFailed` + `EndCancelled`) → `cancelled`, no BYE.
10. Busy: a second `HandleInvite` while call 1 is active → `Reject(486)`, notice `Missed call (line busy)`,
    call 1 unaffected; after call 1 ends, a new INVITE rings normally.
11. Encrypted room → `Reject(480)`, notice encrypted, no membership, no ring, `rejected`.
12. Offer without PCMU → `Reject(488)`, notice codec, `rejected`.
13. Withheld caller (`FromUser "anonymous"`, no PAI) → `Reject(480)`, no `ResolveCallRoom`, no notice.
14. PAI preferred over From.
15. No service URL → `Reject(480)` before any membership, notice no-audio, `failed`.
16. Owner membership leave while active (only device) → `Hangup` (BYE) once, teardown `owner/bye`.
17. Two owner devices; first leaves → call continues, `call.owner.left` span; second leaves → teardown.
18. Owner leaves LiveKit (rtc `EventOwnerLeft`, `lkOwners` 0) → after 10 s grace teardown `owner/rtc_lost`;
    rejoin within grace cancels it.
19. Our session `EventDisconnected` while active → teardown `bridge/rtc_lost` with BYE.
20. Bridge `Done` with `ErrRTPTimeout` → teardown `bridge/rtp_timeout` with BYE.
21. BYE and owner leave queued together → exactly one teardown, zero `Hangup` commands (BYE first) or
    exactly one (owner first).
22. `EndLogin` while ringing → `Reject(480)`, redact, clear, `failed`/`bridge_stop`; while active →
    BYE, teardown `bridge/bridge_stop`; `EndLogin` returns after the actor finished.
23. Ring keepalive: `RingTimeout = 150 s` → `Ringing` commands at 0, 60, 120 s.
24. Membership refresh: active call, clock advanced to `created + 4 h − 10 min` → second `SendState` with
    the same `created_ts` and `expires = 8 h` in ms.
25. Matrix event for another room of the same login → not routed (`routed_to=none`).
26. Owner answers after `answerTimeout` without join success → `Hangup`, `failed`.
27. Owner joins then leaves before the join completes → `Reject(480)`, outcome `missed`.

`telemetry_test.go` (with `obstest.Setup`): for branches answer, cancel, decline, timeout, busy:
`call.inbound` is the only root besides linked roots; it is SERVER, has the §10.1 children in order
(names asserted), its end time equals `AckAt` in the answer branch; `call.ring` has a link to the
ingest span (route a real `core.Ingest` event); `call.teardown` and `call.session` are roots with links to
the setup span; every span's duration is under 10 minutes (TS §5.10 hard cap), including in the
membership-refresh case where the call lasts 4 hours of fake time;
`matrixrtc.ingest` has `matrixrtc.routed_to=call` and `call.id`; metrics: `call.started`, `call.ended`
with the expected outcome and status class, `call.active` back to 0, `setup.duration` and
`ring.duration` recorded once; allowlist and no-secrets assertions (§11).

`manager_test.go`: `HandleInvite` returns without blocking even if the actor blocks in a fake host call;
`HandleRTCEvent` never blocks (fill the channel; the 33rd event is dropped and logged); `Stop` rejects
later INVITEs with 480.

`integration_test.go`: real `media.Start` with fake SIP leg (yields a PCMU tone, collects written
packets) and fake RTC leg (collects Opus frames, delivers Opus-encoded tone via the receiver), real
`core.Ingest` + `core.RegisterMatrixRTCHandlers` with a fake registrar and resolver feeding
`*event.Event` values built from JSON fixtures (an Element X style owner membership with
`livekit_alias`, a `{}` leave, a decline with `m.relates_to`), fake clock. Flow: INVITE → ring →
owner membership event via the registrar handler → answer → 2 s of fake time → assert both legs received
tone audio (decode and check frequency) → owner leave fixture → BYE issued, teardown complete, membership
cleared. A second scenario drives decline through the registrar.

### 12.2 Live test plan (user)

Environment: the bridge in the example.org compose stack (as in Phase 1), double puppeting on, JMP
account logged in, ATA unplugged/logged out. Owner clients: Element X Android, Sable web, Sable
Android. The user calls the **JMP number +15550004321** from their mobile phone. The bridge places no
outbound calls in Phase 2; do not dial the user's personal number +15550001234 (never call it without
warning the user first).

| # | Action | Expect |
|---|---|---|
| 0 | Start the bridge without `livekit_service_url` | Log "LiveKit service URL" with host `matrixrtc.example.org`, source `well_known`. |
| 1 | Call the JMP number; answer in Element X within ~10 s; talk 2+ minutes both ways; hang up on the phone | All three clients ring. Ghost appears in the call, audio both ways from the first second (no clipping), no echo/garble. Sable Android stops ringing on answer (read receipt). Notice `Call · 2m …`. Trace: `call.inbound` → … → `call.answer`, ending at ACK; `call.teardown` (`remote/bye`) and `call.session`. |
| 2 | Same, answer in Sable web, hang up in Sable | BYE to the phone immediately; notice; `owner/bye`. |
| 3 | Call, decline in Element X | Phone gets 603 (record what the caller hears: voicemail or busy). Other devices stop. Notice `Declined call`. |
| 4 | Call, let it ring out (30 s) | All devices stop at 30 s; caller goes to voicemail (480); ring event redacted; ghost membership cleared; `Missed call`. |
| 5 | Call, hang up the phone after 5 s of ringing | All devices stop within ~1 s; redacted; `Missed call`; `cancelled`. |
| 6 | Answer on Element X, then also join from Sable web, leave Element X | Call continues on Sable; ends when Sable leaves. |
| 7 | During a call, kill Element X (force stop) | Call ends within ~10–30 s (`rtc_lost`) with BYE to the phone. |
| 8 | (Optional, needs a second phone) Call while a call is active | Second caller gets busy (486); `Missed call (line busy)` in that caller's room. |
| 9 | (Optional) Withheld caller (*67 on many US carriers) | Caller goes to voicemail; log "rejected" with `withheld=true`. |
| 10 | Stop the bridge during an active call | BYE to the phone, membership cleared, `call.session` with `bridge_stop`; shutdown completes. |
| 11 | Check metrics in the backend | `mrtcsip.call.*`, `mrtcsip.rtc.join.duration`, `mrtcsip.media.*` present; loss/jitter plausible; no phone numbers in metric attributes. |

Record the outcome in PLAN and tick
PLAN Phase 2 boxes.

---

## 13. Edits to `docs/TELEMETRY-SPEC.md` (flagged)

Made by the author of this spec in the same change; minimal:

1. §5.2 `call.end_reason` value list adds `rejected` (setup refused by the bridge: busy, unsupported
   codec, encrypted room, withheld caller) and `bridge_stop` (already used in §5.10 but missing from the
   list).
2. §5.6 last paragraph before "Mid-call": allows the equivalent shape where the call actor starts
   `call.inbound` backdated to INVITE receipt and adds the `100 Trying` event with its timestamp (D10),
   instead of transferring the live span from the handler goroutine.
3. §4.3 `mrtcsip.media.rtp.*` rows: clarify they count the **SIP** RTP leg.
4. §8.2 Phase 2 bullet: `call.membership.refresh` is implemented in Phase 2 (D15); Phase 4 keeps the
   bullet for re-INVITE and network change.

No span or metric names change.

---

## 14. Work split

Dependency order:

```
T1 (deps, contracts, clock, matrixrtc, obs, core, importcheck, API files)
 ├── T2 (pkg/media)            ┐
 ├── T3 (pkg/sipua inbound)    │
 ├── T4 (pkg/rtc)              ├─ parallel, all code against T1's API files
 └── T5 (pkg/calls)            ┘  (uses fakes only; does not need T2–T4 to compile its tests,
                                   except the integration test, which needs T2)
T6 (connector wiring, Dockerfile, docs, go mod tidy)      after T2, T3, T4, T5
Opus review of the full diff                                after T6
Live test (user, §12.2)                                     after review fixes
```

Recommended tiers: T1 Sonnet, T2 Sonnet, T3 Sonnet, T4 Sonnet, **T5 Opus** (concurrency and trace-shape
correctness), T6 Sonnet. Rules for every task: edit only the files you own (§2); do not change any (API)
file's exported surface (stop and report instead); do not commit; do not touch `spikes/`; no new
dependencies beyond §3; every exported identifier has a doc comment; no emojis.

Acceptance for every task (plus the task-specific items): the three commands of §3 pass for the whole
module (if a sibling task's package does not compile yet, run them for your packages and the packages
that do not depend on unfinished ones, and say so), and `go test -race -count=1 -tags "nocrypto
nolibopusfile" ./pkg/<yours>/...` passes offline.

### T1: dependencies, shared contracts, API files

Files: `go.mod`, `go.sum`, `internal/importcheck/importcheck_test.go`, `internal/obstest/obstest.go`,
`pkg/clock/*`, `pkg/core/callhost.go`, `pkg/core/ingest.go`, `pkg/core/ingest_obs_test.go`,
`pkg/matrixrtc/events.go`, `pkg/matrixrtc/membership.go`, `pkg/matrixrtc/membership_test.go`,
`pkg/obs/metrics.go`, `pkg/obs/attrs.go`, `pkg/obs/obs_test.go`, `pkg/sipua/config.go` (the `OnInvite`
field only), `pkg/sipua/inbound.go`, `pkg/media/doc.go`, `pkg/media/media.go`, `pkg/rtc/doc.go`,
`pkg/rtc/rtc.go`. (`docs/TELEMETRY-SPEC.md` was already edited with this spec, §13.)
API stubs: `media.Start`, `rtc.NewJoiner`, `rtc.SetLibraryLogger`, `rtc.PathTemplate`,
`rtc.NewServiceURLResolver` and its methods, `rtc.LiveKitRoomName`, `(*JoinError)` methods get real
trivial bodies where obvious (`JoinError` methods, `PathTemplate`) and `panic("not implemented: T2")`
/ `panic("not implemented: T4")` otherwise, so the module builds. `pkg/rtc/rtc.go` imports only what its
signatures need (no LiveKit yet, but keep the go.mod requirement via T1's `go get`; add a blank import
`_ "github.com/livekit/server-sdk-go/v2"` in `pkg/rtc/doc.go` so `go mod tidy` cannot drop it before T4,
and likewise `_ "gopkg.in/hraban/opus.v2"` in `pkg/media/doc.go`; T2/T4 remove the blank imports).
Spec: §3, §4, §5.1, §6.1, §7.1.
Acceptance: §3 version check output exact; build/vet/test pass; import check scans ≥ 9 core dirs and the
new detection cases pass; `grep -rn "org.matrix.msc" pkg/ --include=*.go | grep -v _test.go` matches only
`pkg/matrixrtc/events.go`; `go doc ./pkg/sipua`, `./pkg/rtc`, `./pkg/media` show no diago, sipgo,
LiveKit, pion or hraban types.

### T2: media

Files: `pkg/media/*` except `media.go` (may fill in `Start`'s body by moving it to `bridge.go`: keep the
signature and doc in `media.go` as a one-line wrapper calling `start`), remove the blank import in
`doc.go`.
Spec: §7.
Acceptance: §7.4 tests pass with `-race`; `TestHotPathZeroAllocs` passes; `go test -bench . -benchmem
-run ^$ ./pkg/media/` reports `0 allocs/op` for both benchmarks; `grep -rn "obs.M()" pkg/media` finds
nothing (metrics are recorded by calls).

### T3: SIP inbound

Files: `pkg/sipua/inbound_loop.go`, `pkg/sipua/inbound_diago.go`, `pkg/sipua/diago.go`,
`pkg/sipua/doc.go`, `pkg/sipua/{inbound_loop,offer,inbound_loopback}_test.go`.
Spec: §5.2, §5.3, sipua rows of §12.1. Reference: `spikes/s2-sip/main.go` (`handleInbound`) and
`media.go` (`sdpSummary`); diago sources (§5.2 line references).
Acceptance: tests pass (loopback test runs on localhost); `go doc ./pkg/sipua` shows no diago/sipgo types;
`grep -n "SIPDebug" pkg/sipua/*.go` finds nothing; no SDP body is ever passed to a logger
(`grep -n "Body()" pkg/sipua/*.go` shows only `parseOffer` call sites).

### T4: rtc

Files: `pkg/rtc/*` except `rtc.go` (stub bodies move to the implementation files; signatures and docs in
`rtc.go` stay), remove the blank import in `doc.go`.
Spec: §6, rtc rows of §12.1. Reference: `spikes/s1-rtc/rtc.go` (`getSFUToken`, `connectRTC`,
`inspectJWT`), `events.go`.
Acceptance: tests pass; `go doc ./pkg/rtc` shows no LiveKit/pion types; no test or code path logs or
records the OpenID access token or the JWT (`grep -n "jwt\|access_token" pkg/rtc/*.go` reviewed: only
JSON field names and redaction).

### T5: calls

Files: `pkg/calls/*`.
Spec: §8, §10, §11, calls rows of §12.1.
Acceptance: all §12.1 calls tests pass with `-race -count=3` (flake check); the integration test passes
once T2 has landed (until then it may be skipped with a clear `t.Skip` that T6 removes); no mutex guards
call state (only the manager map); `grep -rn "time.Now()\|time.After\|time.NewTimer" pkg/calls/*.go |
grep -v _test.go` finds nothing (everything goes through `cfg.Clock`).

### T6: host wiring, packaging, docs

Files: `pkg/connector/{connector,start,client,callhost,config,telemetry,login}.go`,
`pkg/connector/example-config.yaml`, `pkg/connector/{config,client_lifecycle}_test.go`, `Dockerfile`,
`README.md`, `CLAUDE.md`, `docs/CREDITS.md`, `docs/PLAN.md`, final `go.mod`/`go.sum` tidy; remove T5's
integration-test skip if present.
Spec: §9.
Acceptance: `go mod tidy` run and the §3 version check repeated (exact); build/vet/test (`-race`) pass for
the whole module; `go build -tags "nocrypto nolibopusfile" -o /tmp/mrtc-sip-bridge ./cmd/mrtc-sip-bridge
&& /tmp/mrtc-sip-bridge -e -c /tmp/mrtc-example.yaml` succeeds and the file contains
`rtp_timeout_seconds: 60`; `./build.sh` (Docker) succeeds; README "Tracing and metrics" lists every span
of §10.1 and every metric of §10.3.

### Final (coordinator)

1. Clean checkout build/vet/test with `-race`. 2. Opus review of the full diff against this spec
(state machine, concurrency rules, trace shape, privacy, import boundaries). 3. Live test §12.2.
4. PLAN Current state + ticks; live-test outcome.

---

## 15. Open questions for the user (defaults chosen)

1. **Decline response** `603 Decline` (PLAN). If JMP plays a "busy" message instead of voicemail and you
   prefer voicemail, switch to `480`. Default: 603, revisit after live test #3.
2. **Withheld / anonymous callers** are rejected (to voicemail) in Phase 2 rather than ringing in a shared
   "Unknown caller" room. Default: reject.
3. **LiveKit SDK v2.18.1** instead of the spike's v2.19.0 (forced by diago's pion version, D3).
   Informational; revisit when diago moves to `pion/ice` v4.4+.

---

## 16. Review-fix deviations (phase2-inbound)

- **8.5 ordering**: on the owner's first live membership the SIP answer is queued *before*
  `MarkReadAsOwner` (the receipt is two inline HTTP calls and used to delay the 200 OK). `devices` is keyed
  by the membership state key (not content `device_id`); while Ringing, owner memberships whose event
  timestamp is older than `ringSentAt - 5 s` are ignored (appservice backlog after a restart).
- **Join results** travel on their own channel `call.joinResults` (capacity `maxJoinAttempts+1`), not on
  `call.events`, so a full event queue can never drop them (`evJoinResult` no longer exists).
- **Media EOF vs remote BYE**: when the bridge stops with `io.EOF`, the actor waits up to 1 s for the SIP
  `EventEnded`; `EndRemoteBye` gives `hangup.by=remote end_reason=bye` (with `sip.bye.received`), otherwise
  `bridge/error`.
- **Active slot release**: `fail`/`teardown` release the login's active/routable slots at their start, so a
  redial during the (slow) cleanup is accepted instead of getting 486.
- **Shutdown during setup**: `quit` is polled before steps 8-11; the failure branch runs with
  `end_reason=bridge_stop`.
- **rtc**: `OnParticipantDisconnected` skips `EventOwnerLeft` if the room still has a participant with that
  identity (lksdk runs connect/disconnect callbacks on separate goroutines); `roomConn.HasParticipant`.
- **Service URL**: the LiveKit service URL is kept byte-for-byte in `foci_preferred[0].livekit_service_url`
  (Element Call keys connections by that string); a trailing `/` is trimmed only when building `/sfu/get`.
  `ParseMembership` treats a non-empty `call_id` as not live (other slot).
- **Ring read receipt on unanswered ends**: `MarkReadAsOwner` is also sent (once, `call.markRingRead`) in
  the `fail` branch whenever a ring event was sent (decline, ring timeout, caller CANCEL, other pre-answer
  teardown), so push-driven clients (Sable Android) get the unread-count push that dismisses the ring
  notification. It runs after the SIP final response and `cancelJoinAndWait`, before the ring redaction,
  bounded by `matrixOpTimeout`; errors are logged (debug for no double puppet, warn otherwise) and never
  fail teardown. The answer path keeps its receipt; `ringRead` prevents a duplicate if an answered call
  later fails.
- **Call ended log**: `jitter_max_ms` is a float (0.01 ms resolution) and `jitter_avg_ms` is added.
