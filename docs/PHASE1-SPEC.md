# Phase 1 implementation spec: core library skeleton + standalone host

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Status: **ready for implementation** (written 2026-10-16, branch `phase1-skeleton`).
Audience: implementer agents. Follow it literally. Where this spec and your own taste differ, this spec
wins. If something here does not compile against the pinned versions, stop and report it rather than
redesigning.

Inputs: `docs/PLAN.md` (Phase 1), ADR-002/003/004/006, S1/S2/S3 notes, the
working spikes in `spikes/s1-rtc` and `spikes/s2-sip`, and house conventions from `../mautrix-jabber`.
All bridgev2 references below were checked against `~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0`
(paths relative to that directory). diago/sipgo references were checked against
`~/go/pkg/mod/github.com/emiago/diago@v0.41.0` and `.../sipgo@v1.6.0`.

---

## 0. Scope

In scope:

1. Go module `github.com/williamkray/mrtc-sip-bridge`, mautrix **v0.32.0**, go.mau.fi/util **v0.11.0**,
   zerolog **v1.35.1** (same as mautrix-jabber), `go 1.26.0`.
2. Core contract in `pkg/core` (`CallHost`, `CallRoom`, `GhostIntent`, `LoginRef`, `OpenIDToken`) and the
   MatrixRTC ingest entry point (`core.Ingest.HandleMatrixRTCEvent`) plus a reusable hook registrar.
3. `pkg/matrixrtc`: the single file of MatrixRTC event type names, and parsing of raw events into a
   host-neutral `matrixrtc.Event`.
4. `pkg/phone`: E.164 normalization (reimplemented from mautrix-jabber `cheogram.go`, improved).
5. `pkg/sipua`: registering SIP UA on diago v0.41.0, behind our own interface. AOR `To:` fix, registrar
   IP pinning with re-resolve on failure, unregister only our own Contact, backoff, state callbacks.
   Inbound INVITEs are rejected with `480 Temporarily Unavailable` in Phase 1.
6. Import-boundary test: core packages never import host packages or bridgev2.
7. Standalone host: `cmd/mrtc-sip-bridge` (mxmain) and `pkg/connector` (NetworkConnector, login flow,
   portals keyed by E.164, ghosts per number, NetworkAPI keeping a REGISTER alive and reporting bridge
   state, `ResolveIdentifier` so a portal can be created by hand for testing, S3 hook wired in
   `PostInit` into a log-only sink).
8. Network config section with defaults, validation and upgrader.
9. Dockerfile, `.dockerignore`, `build.sh`, `build-args.sh`, `entrypoint.sh` modeled on mautrix-jabber.
10. README, CREDITS, CLAUDE.md status, new ADR-009.

Out of scope (do **not** implement): call state machine, ringing, LiveKit, media/RTP handling,
transcoding, outbound calls, DTMF, lk-jwt-service discovery (only the config key exists), OpenTelemetry,
persistence of call state, multiple concurrent calls.

---

## 1. Decisions made in this spec (and why)

| # | Decision | Why |
|---|---|---|
| D1 | Core contract lives in package `pkg/core` (not at `pkg/` root). | A Go package at `pkg/` itself is awkward to import. `core` holds the interfaces ADR-006 sketched plus the ingest entry point. |
| D2 | `LoginRef` is `type LoginRef string`, opaque, host-chosen. Standalone uses the bridgev2 `UserLoginID`. | Comparable, map-keyable, zero mautrix dependency. Embedded host can use its own login ID. |
| D3 | `GhostIntent` takes and returns only `id`/`event` types and `map[string]any` content; OpenID token is our own struct. | Keeps the core free of the root `mautrix` and `bridgev2` packages (ADR-006 version-skew constraint). |
| D4 | `CallHost` gets one extra method beyond the ADR sketch: `MarkReadAsOwner`. | S1 result: ring dismissal on push-driven clients needs a read receipt as the owner via double puppet (Phase 2). Freezing it now avoids a contract change later. |
| D5 | The hook registrar (`core.RegisterMatrixRTCHandlers`) is in core, takes an `EventRegistrar` interface satisfied by `*appservice.EventProcessor`, plus a host-supplied `LoginResolver`. | Both hosts reuse it (PLAN J4). Core does not import `appservice`: `appservice.EventHandler` is a type alias (`appservice/eventprocessor.go:30`), so a plain func-typed interface method matches. |
| D6 | Phase 1 sink is `core.LogSink` (Info log of kind/room/sender/state key/event id, never content). | PLAN allows a log-only sink. Info level so the manual hook test is visible with default logging. |
| D7 | REGISTER uses diago `RegisterTransaction` (proven in S2), not a hand-rolled sipgo REGISTER. Refresh is a fixed interval (default 25 s) because diago does not expose the granted `Expires`. | Lowest-risk path; S2 ran exactly this. Granted Expires was 30–60 s for a 60 s request, so 25 s is safe. Recorded as a known limitation. |
| D8 | Unregister own Contact: mutate the transaction's exported `Origin` request (replace `Expires` with `0`, keep the NAT-rewritten `Contact`) and send it with `Qualify` (which handles digest). | diago's `Unregister` sends `Contact: *` (`register_transaction.go:268-277`), which would also drop other devices on the account. `Origin` is exported and `Qualify` sends it verbatim with digest auth, so no fork is needed. |
| D9 | Registrar IP pinning: resolve once (IPv4 only), pin via `Origin.SetDestination(ip:port)` before every attempt. Switch to the next resolved IP (with a fresh DNS lookup) only after **2 consecutive** transient failures. After any failure, the next attempt is a full `Register` (re-derives Contact from `rport`/`received`). | S2: re-resolving per request broke the NAT mapping. Switching on a single lost packet would churn mappings; two failures in a row means the IP is really bad. |
| D10 | Before every full `Register`, remove the `Via` header from `Origin`. | diago's `register()` does not remove `Via` (only `doRequest` does, `register_transaction.go:285`), so a second `Register` would reuse the old branch. Spike never hit this because it only re-registered via `Qualify`. |
| D11 | Auth failures (401/403/404/407 after the digest round) report `BAD_CREDENTIALS` and keep retrying every **5 min**. | Avoids hammering the provider (account lockout risk) but recovers from provider glitches without user action. |
| D12 | Login verifies credentials by actually registering, then hands the running client to `NewLogin` through `NewLoginParams.LoadUserLogin` (`userlogin.go:179`). | Gives the user an immediate "bad password" error like mautrix-jabber, without registering twice. |
| D13 | One SIP UDP/TCP port per active login, from `sip_port_min..sip_port_max` (default 5060–5069), allocated in-process. | Each login needs its own UA (diago derives `From`/`Contact` user from the UA name) and its own socket. |
| D14 | Transports: `udp` (default, proven) and `tcp` (accepted, untested). `tls` is rejected at login with a clear message. | TLS needs SNI/cert handling with IP pinning; not needed for JMP. |
| D15 | Build with `-tags nocrypto` everywhere (local, CI, Docker). No libolm. | Portals must be unencrypted (ADR-002), so end-to-bridge encryption is dead weight, and it removes the libolm build dependency (not installed on the dev machine). `bridgev2/matrix/no-crypto.go` is selected by `nocrypto`. |
| D16 | `PostInit` forces `encryption.default` to `false` at runtime with a warning if the config sets it. | Encrypted portals give silent calls (ADR-002). |
| D17 | Config durations are integer seconds (`*_seconds`), port ranges are min/max ints. | Avoids YAML duration ambiguity in the config upgrader; `configupgrade.Int` copies them cleanly. |
| D18 | `GetConfig` returns `&c.Config` and the framework decodes into it (`mxmain/main.go:375`). Defaults and validation run in `ValidateConfig` (`networkinterface.go:309`, called from `mxmain/main.go:334`). | The canonical bridgev2 pattern. mautrix-jabber's re-decode in `PostInit` is a workaround we don't need to copy. |
| D19 | Ghost network ID = E.164 digits without `+` (e.g. `15551234567`), MXID `@sip_15551234567:server` via the default `username_template` `sip_{{.}}`. Portal ID = full E.164 (`+15551234567`), `Receiver` = login ID. | Matches the S1 wire spec examples; `+` is avoided in localparts for older servers/clients. Receiver keeps each login's rooms separate, as mautrix-jabber does. |
| D20 | `ResolveIdentifier` is implemented in Phase 1 (pulled forward from Phase 3). | Without it nothing creates a portal in Phase 1, so portals, ghosts, power levels and the S3 hook could not be verified. It is ~25 lines. |
| D21 | No OpenTelemetry in Phase 1 (Phase 5). zerolog only. diago's slog output goes through `zerolog.NewSlogHandler` capped at Warn. | Keeps Phase 1 small; Warn cap stops diago from printing request details. |
| D22 | NetworkID `sip`, command prefix `!sip`, default appservice port **29319**, BeeperBridgeType `github.com/williamkray/mrtc-sip-bridge`. | 29318 is mautrix-jabber's default; next free number. |

---

## 2. Package layout

Every file, with owner task (T1–T5, see §11). `_test.go` files are listed with their package.

```
go.mod, go.sum                                   T1 (created), T4 (final go mod tidy)
cmd/mrtc-sip-bridge/main.go                      T4  mxmain entry, PostInit hook wiring, encryption guard
internal/importcheck/doc.go                      T1  package doc only (so the dir has a non-test file)
internal/importcheck/importcheck_test.go         T1  import-boundary test
pkg/core/doc.go                                  T1  package doc: what a host must implement
pkg/core/callhost.go                             T1  LoginRef, OpenIDToken, GhostIntent, CallRoom, CallHost, errors
pkg/core/ingest.go                               T1  RTCEventSink, LogSink, Ingest (HandleMatrixRTCEvent)
pkg/core/hook.go                                 T1  EventRegistrar, LoginResolver, RegisterMatrixRTCHandlers
pkg/core/ingest_test.go                          T1
pkg/core/hook_test.go                            T1
pkg/matrixrtc/doc.go                             T1  package doc
pkg/matrixrtc/events.go                          T1  ALL MatrixRTC event type names (the one place), with citations
pkg/matrixrtc/parse.go                           T1  EventKind, Event, KindOf, FromMatrixEvent
pkg/matrixrtc/events_test.go                     T1
pkg/phone/phone.go                               T1  Normalize, IsE164, Digits, FromDigits, errors
pkg/phone/phone_test.go                          T1
pkg/sipua/doc.go                                 T2  package doc, diago-is-internal note
pkg/sipua/config.go                              T2  Transport, ParseTransport, Config, validate/defaults
pkg/sipua/state.go                               T2  State, StateEvent, ResponseError
pkg/sipua/backoff.go                             T2  backoffDelay, jitter, constants
pkg/sipua/pin.go                                 T2  Resolver, pinner
pkg/sipua/sipmsg.go                              T2  targets, buildTargets, ensureAORTo, prepareUnregister
pkg/sipua/loop.go                                T2  regTxn interface, classify, registerLoop
pkg/sipua/diago.go                               T2  New(), diagoUA (UA impl), diagoTxn (regTxn impl), inbound reject
pkg/sipua/net.go                                 T2  DefaultBindIP, DiscoverPublicIP, SetRTPPortRange
pkg/sipua/mask.go                                T2  MaskUsername
pkg/sipua/{config,backoff,pin,sipmsg,loop,mask}_test.go   T2
pkg/connector/connector.go                       T3  SIPConnector struct + Init/GetName/GetConfig/ValidateConfig/
                                                     GetDBMetaTypes/GetCapabilities/GetBridgeInfoVersion
pkg/connector/config.go                          T3  Config, ApplyDefaults, Validate, upgrader
pkg/connector/example-config.yaml                T3  embedded network example config
pkg/connector/dbmeta.go                          T3  UserLoginMetadata, PortalMetadata, GhostMetadata
pkg/connector/ids.go                             T3  MakePortalKey, MakeUserID, ParseUserID, MakeLoginID
pkg/connector/chatinfo.go                        T3  buildChatInfo, buildUserInfo
pkg/connector/loginparse.go                      T3  parseLoginInput, normalizeRegistrar
pkg/connector/portpool.go                        T3  portPool
pkg/connector/ghostintent.go                     T3  core.GhostIntent adapter over bridgev2/appservice intents
pkg/connector/callhost.go                        T3  core.CallHost implementation (standalone)
pkg/connector/rtchook.go                         T3  RegisterMatrixRTCHandlers + resolveRTCLogin
pkg/connector/{config,ids,chatinfo,loginparse,portpool}_test.go   T3
pkg/connector/start.go                           T4  Start, LoadUserLogin, GetLoginFlows, CreateLogin, interface assertions
pkg/connector/login.go                           T4  sipLoginProcess (LoginProcessUserInput)
pkg/connector/client.go                          T4  SIPClient (NetworkAPI + IdentifierResolvingNetworkAPI)
pkg/connector/client_state.go                    T4  bridgeStateFor (sipua.StateEvent -> status.BridgeState)
pkg/connector/client_state_test.go               T4
pkg/connector/stub_client.go                     T4  stubClient for logins without credentials
Dockerfile, .dockerignore, build.sh, build-args.sh, entrypoint.sh    T5
.gitignore (append)                              T5
README.md, CLAUDE.md (Status section), docs/CREDITS.md, docs/DECISIONS.md (ADR-009)   T5
```

Core packages = every directory under `pkg/` except `pkg/connector` (today: `core`, `matrixrtc`,
`phone`, `sipua`; Phase 2 adds `calls`, `media`, `rtc` and they are covered automatically).

Nested modules under `spikes/` have their own `go.mod`, so `./...` from the repo root never includes
them. Do not touch `spikes/`.

---

## 3. Module bootstrap (T1)

```bash
cd <repo>
go mod init github.com/williamkray/mrtc-sip-bridge
go mod edit -go=1.26.0
go get maunium.net/go/mautrix@v0.32.0 \
       maunium.net/go/mautrix/bridgev2/matrix/mxmain@v0.32.0 \
       go.mau.fi/util@v0.11.0 \
       github.com/rs/zerolog@v1.35.1 \
       gopkg.in/yaml.v3@v3.0.1 \
       github.com/emiago/diago@v0.41.0 \
       github.com/emiago/diago/media@v0.41.0 \
       github.com/emiago/sipgo@v1.6.0 \
       github.com/emiago/sipgo/sip@v1.6.0 \
       github.com/pion/stun/v3@v3.1.7
```

Rules for go.mod during parallel work: **T2, T3 and T5 must not run `go mod tidy`** and must not change
versions. If a build fails with "missing go.sum entry", run `go mod download <module>@<pinned version>`
for exactly that module. T4 runs `go mod tidy` once at the very end and checks the pinned versions above
are unchanged (`go list -m maunium.net/go/mautrix github.com/emiago/diago github.com/emiago/sipgo`).

All builds, vets and tests use **`-tags nocrypto`** (D15). Acceptance commands everywhere:

```bash
go build -tags nocrypto ./...
go vet   -tags nocrypto ./...
go test  -tags nocrypto ./...
```

---

## 4. Core contract (`pkg/core`, `pkg/matrixrtc`) — T1

Allowed imports in core packages: the standard library, `maunium.net/go/mautrix/id`,
`maunium.net/go/mautrix/event`, `github.com/rs/zerolog`, other core packages, and (only in `pkg/sipua`)
diago, sipgo and pion/stun. Enforced by §9.

### 4.1 `pkg/core/callhost.go` (exact)

```go
package core

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// LoginRef identifies one SIP account login inside a host. The host chooses the value
// (the standalone bridge uses its bridgev2 UserLoginID). Core treats it as opaque.
type LoginRef string

// OpenIDToken is the Matrix OpenID token returned by
// POST /_matrix/client/v3/user/{userId}/openid/request_token, minted for a ghost.
type OpenIDToken struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int    `json:"expires_in"`
}

// GhostIntent acts as the ghost user that stands for the remote phone number.
// Content maps are sent as raw JSON event content.
type GhostIntent interface {
	UserID() id.UserID
	SendState(ctx context.Context, roomID id.RoomID, evtType event.Type, stateKey string, content map[string]any) (id.EventID, error)
	SendMessage(ctx context.Context, roomID id.RoomID, evtType event.Type, content map[string]any) (id.EventID, error)
	Redact(ctx context.Context, roomID id.RoomID, eventID id.EventID, reason string) error
	RequestOpenIDToken(ctx context.Context) (*OpenIDToken, error)
}

// CallRoom is the Matrix room in which calls with one remote number ring, plus the ghost for that number.
type CallRoom struct {
	RoomID     id.RoomID
	RemoteE164 string
	Ghost      GhostIntent
}

// CallHost is everything the call core needs from the bridge that embeds it.
type CallHost interface {
	// ResolveCallRoom returns the room and ghost for e164 (E.164, e.g. "+15551234567"),
	// creating the portal room if needed.
	ResolveCallRoom(ctx context.Context, login LoginRef, e164 string) (*CallRoom, error)
	// SendCallNotice posts a short text notice ("Missed call", "Call · 1m20s") in the room.
	SendCallNotice(ctx context.Context, room *CallRoom, text string) error
	// OwnerMXID returns the Matrix user who owns the login, or "" if the login is unknown.
	OwnerMXID(login LoginRef) id.UserID
	// MarkReadAsOwner sends a read receipt for eventID as the owner (double puppet).
	// Returns ErrNoDoublePuppet if the owner has no double puppet.
	MarkReadAsOwner(ctx context.Context, login LoginRef, roomID id.RoomID, eventID id.EventID) error
}

var (
	ErrUnknownLogin   = errors.New("core: unknown login")
	ErrNoDoublePuppet = errors.New("core: owner has no double puppet")
)
```

### 4.2 `pkg/matrixrtc/events.go` (exact names; keep the citation comment)

```go
package matrixrtc

import "maunium.net/go/mautrix/event"

// All MatrixRTC event type names used by this project live in this file (CLAUDE.md rule).
// Checked 2026-10-16 against: MSC3401/MSC4143 legacy per-device membership (SessionMembershipData),
// MSC4075 head 4bdec0f963 (rtc.notification; now also contains decline, superseding MSC4310),
// Element Call v0.28.0 / main 8be229a, matrix-js-sdk d0e8162, ruma 09550bab (Element X).
// Details and rationale: docs/spikes/S1-matrixrtc-wire-spec.md §1.1, §2.1, §3.1.
// Class matters: appservice dispatch matches Type AND Class (appservice/eventprocessor.go:128).
var (
	// Per-device membership state event. The only membership type Element X understands. Send this one.
	CallMember = event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}
	// ruma alias; listened to and given a power level override, never sent.
	CallMemberAlias = event.Type{Type: "m.call.member", Class: event.StateEventType}
	// Ring notification (MSC4075 unstable name). Send this one, not the stable alias.
	RTCNotification = event.Type{Type: "org.matrix.msc4075.rtc.notification", Class: event.MessageEventType}
	// ruma alias accepted by Element X; listened to only.
	RTCNotificationAlias = event.Type{Type: "m.rtc.notification", Class: event.MessageEventType}
	// Decline (MSC4310, folded into MSC4075). Listen for both names.
	RTCDecline      = event.Type{Type: "org.matrix.msc4310.rtc.decline", Class: event.MessageEventType}
	RTCDeclineAlias = event.Type{Type: "m.rtc.decline", Class: event.MessageEventType}
)

// IngestEventTypes are the types hosts must route into core.Ingest. Order is fixed (tests rely on it).
func IngestEventTypes() []event.Type {
	return []event.Type{CallMember, CallMemberAlias, RTCNotification, RTCNotificationAlias, RTCDecline, RTCDeclineAlias}
}

// CallMemberPowerLevelTypes must be set to power level 0 in portal rooms so the owner and the ghost
// can send memberships (wire spec §1.5).
func CallMemberPowerLevelTypes() []event.Type { return []event.Type{CallMember, CallMemberAlias} }
```

### 4.3 `pkg/matrixrtc/parse.go` (exact API)

```go
package matrixrtc

type EventKind int

const (
	KindUnknown EventKind = iota
	KindCallMember
	KindRTCNotification
	KindRTCDecline
)

func (k EventKind) String() string // "unknown", "call_member", "rtc_notification", "rtc_decline"

// Event is a host-neutral MatrixRTC event. Content is the raw JSON content (bridgev2 leaves
// Content.Parsed nil for these types, see S3 §1).
type Event struct {
	Kind      EventKind
	Type      string          // raw type name, e.g. "org.matrix.msc3401.call.member"
	RoomID    id.RoomID
	EventID   id.EventID
	Sender    id.UserID
	StateKey  string          // "" for message events
	IsState   bool
	Timestamp time.Time       // from origin_server_ts (UnixMilli); zero if 0
	Content   json.RawMessage // never nil for a returned event; "{}" if the event had no content
}

// KindOf maps a type (Type AND Class must match one of events.go) to a kind.
// CallMember/CallMemberAlias -> KindCallMember; RTCNotification/Alias -> KindRTCNotification;
// RTCDecline/Alias -> KindRTCDecline; anything else (including a known name with the wrong Class) -> KindUnknown.
func KindOf(t event.Type) EventKind

// FromMatrixEvent converts evt. ok=false if evt is nil or KindOf(evt.Type) is KindUnknown.
// Content: evt.Content.VeryRaw if non-empty; else json.Marshal(evt.Content.Raw) if Raw != nil; else "{}".
// IsState = evt.StateKey != nil; StateKey = *evt.StateKey when set.
func FromMatrixEvent(evt *event.Event) (Event, bool)
```

### 4.4 `pkg/core/ingest.go` (exact API and behavior)

```go
package core

// RTCEventSink receives parsed MatrixRTC events. HandleRTCEvent is called synchronously from the
// appservice transaction path (EventProcessor ExecMode Sync by default, S3 caveats): it MUST NOT block
// or do network I/O; hand work to a goroutine instead.
type RTCEventSink interface {
	HandleRTCEvent(ctx context.Context, login LoginRef, evt matrixrtc.Event)
}

// LogSink is the Phase 1 sink. It logs one Info line via zerolog.Ctx(ctx) with fields
// login, kind, type, room_id, event_id, sender, state_key and message "MatrixRTC event ingested".
// It never logs Content. Log is an optional clean injected logger (obs.Logger adds trace fields);
// nil (LogSink{}) logs through zerolog.Ctx(ctx).
type LogSink struct{ Log *zerolog.Logger }

func (LogSink) HandleRTCEvent(ctx context.Context, login LoginRef, evt matrixrtc.Event)

// Ingest is the MatrixRTC event-ingest entry point that hosts feed.
type Ingest struct {
	mu   sync.RWMutex
	sink RTCEventSink
}

// NewIngest returns an Ingest forwarding to sink; a nil sink means LogSink{}.
func NewIngest(sink RTCEventSink) *Ingest

// SetSink replaces the sink (Phase 2 swaps in the call manager). nil means LogSink{}.
func (in *Ingest) SetSink(sink RTCEventSink)

// HandleMatrixRTCEvent parses evt with matrixrtc.FromMatrixEvent and forwards it to the sink.
// Returns false (and does nothing) if evt is nil, the kind is unknown, or login is "".
func (in *Ingest) HandleMatrixRTCEvent(ctx context.Context, login LoginRef, evt *event.Event) bool
```

### 4.5 `pkg/core/hook.go` (exact API and behavior)

```go
package core

// EventRegistrar is satisfied by *appservice.EventProcessor (appservice/eventprocessor.go:63;
// appservice.EventHandler is an alias for this func type, :30).
type EventRegistrar interface {
	On(evtType event.Type, handler func(ctx context.Context, evt *event.Event))
}

// LoginResolver maps an incoming event to the login whose room it is. Return ok=false to drop the
// event (unknown room, sender is a bridge ghost or the bridge bot, ...). Must not block on network I/O
// (a DB lookup is fine).
type LoginResolver func(ctx context.Context, evt *event.Event) (login LoginRef, ok bool)

// RegisterMatrixRTCHandlers registers one handler per matrixrtc.IngestEventTypes() entry on reg.
// Each handler: login, ok := resolve(ctx, evt); if ok { in.HandleMatrixRTCEvent(ctx, login, evt) }.
// Must be called before the EventProcessor starts (bridgev2/matrix/connector.go:229), i.e. in PostInit.
func RegisterMatrixRTCHandlers(reg EventRegistrar, resolve LoginResolver, in *Ingest)
```

### 4.6 `pkg/core/doc.go`

Package comment (prose, ~15 lines): core is the host-independent call library (ADR-006); a host
implements `CallHost`, feeds MatrixRTC events through `RegisterMatrixRTCHandlers` (or calls
`Ingest.HandleMatrixRTCEvent` directly), and owns login lifecycle. Core must not import host packages or
bridgev2 (enforced by `internal/importcheck`).

---

## 5. Phone normalization (`pkg/phone`) — T1

```go
package phone

var (
	ErrInvalid   = errors.New("phone: not a valid phone number")
	ErrShortCode = errors.New("phone: short code, not an E.164 number")
)

// Normalize converts user or SIP input to E.164 ("+" followed by 7-15 digits, first digit 1-9).
// defaultCC is the default country calling code as digits ("1", "44"); "" means national numbers are rejected.
func Normalize(input, defaultCC string) (string, error)

// IsE164 reports whether s matches ^\+[1-9][0-9]{6,14}$.
func IsE164(s string) bool

// Digits strips the leading "+" ("+15551234567" -> "15551234567"). Input is assumed E.164.
func Digits(e164 string) string

// FromDigits is the inverse of Digits: returns ("+"+d, true) if that is valid E.164, else ("", false).
func FromDigits(d string) (string, bool)
```

`Normalize` algorithm, in this order:

1. `s := strings.TrimSpace(input)`; strip a leading `tel:` (case-insensitive).
2. Remove every `' '`, `'\t'`, `'-'`, `'.'`, `'('`, `')'`.
3. If `s` starts with `+`: the rest must be all ASCII digits, else `ErrInvalid`. Result `"+"+rest`.
4. Else, all characters must be ASCII digits, else `ErrInvalid` (this also rejects a `+` not at index 0).
   Empty → `ErrInvalid`.
5. If `defaultCC == "1"` and `s` starts with `011`: result `"+"+s[3:]`.
6. Else if `s` starts with `00`: result `"+"+s[2:]`.
7. Else if `len(s) <= 6`: return `ErrShortCode`.
8. Else if `defaultCC == ""`: `ErrInvalid`.
9. Else if `defaultCC == "1"`: 10 digits → `"+1"+s`; 11 digits starting with `1` → `"+"+s`; otherwise `ErrInvalid`.
10. Else (other CC): strip **one** leading `0` (trunk prefix) if present, result `"+"+defaultCC+s`.
11. Final check: if `!IsE164(result)` → `ErrInvalid`. Return errors wrapped with the input:
    `fmt.Errorf("%w: %q", ErrInvalid, input)` so `errors.Is` works.

Differences from mautrix-jabber `ResolveCheogramIdentifier` (intentional): it prepended `+1` to *any*
national number (so `15551234567` became `+115551234567`); here NANP 11-digit numbers starting with 1
are recognized, `00`/`011` international prefixes are handled, other countries' trunk `0` is stripped,
and the result is validated as E.164. Short-code length threshold (≤ 6) is kept.

---

## 6. SIP UA (`pkg/sipua`) — T2

diago types must not appear in any exported identifier of `pkg/sipua`. Only `diago.go` and `net.go`
import diago/sipgo/stun.

### 6.1 Public API (exact; T4 codes against this)

```go
package sipua

type Transport string

const (
	TransportUDP Transport = "udp"
	TransportTCP Transport = "tcp"
)

// ParseTransport is case-insensitive and trims spaces. "" -> TransportUDP.
// "tls" -> error "TLS transport is not supported yet". Anything else -> error.
func ParseTransport(s string) (Transport, error)

type Config struct {
	Username  string    // SIP auth + AOR user. Required.
	Password  string    // Required. Never logged.
	Registrar string    // "host:port", port required (connector normalizes). Required.
	Domain    string    // Optional SIP domain for AOR/Request-URI; "" = registrar host.
	Transport Transport // TransportUDP or TransportTCP; "" = UDP.

	BindIP       string // Concrete local IPv4 to bind (not 0.0.0.0). Required.
	BindPort     int    // Local SIP port, shared by REGISTER and inbound requests. Required.
	ExternalIP   string // Public IPv4 for Contact and SDP c=; "" = BindIP.
	ExternalPort int    // 0 = BindPort.

	Expiry          time.Duration // REGISTER Expires requested; 0 = 60s.
	RefreshInterval time.Duration // Re-REGISTER interval after success; 0 = 25s. Must be >= 5s and < Expiry.

	Log           zerolog.Logger   // Zero value is fine (disabled).
	OnStateChange func(StateEvent) // Called on every State change, from the UA goroutine. Must not block.
	Resolver      Resolver         // nil = net.DefaultResolver.
}

type State int

const (
	StateIdle State = iota
	StateRegistering
	StateRegistered
	StateAuthFailed
	StateTransientError
	StateStopped
)

// String: "idle", "registering", "registered", "auth_failed", "transient_error", "stopped".
func (s State) String() string

type StateEvent struct {
	State         State
	StatusCode    int           // SIP status of the failing final response; 0 for timeouts/network/DNS errors
	Reason        string        // SIP reason phrase or short error text; never contains credentials
	Err           error         // underlying error for logs; may be nil
	RegistrarAddr string        // pinned "ip:port" used for the attempt ("" if DNS failed)
	Contact       string        // our Contact "host:port" after success (NAT-rewritten by rport/received)
	NextRetry     time.Duration // for AuthFailed/TransientError: delay before the next attempt
}

// ResponseError is returned by the transaction layer for non-200 final responses.
type ResponseError struct {
	StatusCode int
	Reason     string
}

func (e *ResponseError) Error() string // fmt.Sprintf("SIP %d %s", e.StatusCode, e.Reason)

type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error) // *net.Resolver satisfies this
}

// UA is one registered SIP account.
type UA interface {
	// Start binds the SIP socket synchronously (bind errors are returned), then registers and keeps the
	// registration alive in a background goroutine until Stop. Calling Start twice returns an error.
	Start(ctx context.Context) error
	// Stop is idempotent. It stops the register loop, then, if the last state was StateRegistered,
	// unregisters OUR Contact only (bounded by ctx), closes the socket and emits StateStopped.
	// Returns the unregister error, if any (the UA is stopped regardless).
	Stop(ctx context.Context) error
	// State returns the latest StateEvent (StateIdle before Start).
	State() StateEvent
}

// New validates cfg (no I/O) and returns an unstarted UA. Validation errors name the field.
func New(cfg Config) (UA, error)

// DefaultBindIP returns the first non-loopback IPv4 (sip.ResolveInterfacesIP("ip4", nil)).
func DefaultBindIP() (net.IP, error)

// DiscoverPublicIP asks a STUN server ("host:port") for our public IPv4. Honors ctx cancellation.
func DiscoverPublicIP(ctx context.Context, stunServer string) (net.IP, error)

// SetRTPPortRange sets diago's process-wide RTP port range (media.RTPPortStart/End). Call once at startup.
func SetRTPPortRange(min, max int)

// MaskUsername returns "ab***yz" (first two and last two runes); "***" if 4 runes or fewer.
func MaskUsername(u string) string
```

### 6.2 Config validation (`config.go`)

`New` applies defaults (`Transport`→udp, `Expiry`→60s, `RefreshInterval`→25s, `ExternalPort`→`BindPort`,
`ExternalIP`→`BindIP`, `Resolver`→`net.DefaultResolver`) then returns an error if: `Username` empty;
`Password` empty; `Registrar` not `host:port` (`net.SplitHostPort`) or port not 1–65535 or host empty;
`Transport` not udp/tcp; `BindIP` not a valid non-unspecified IPv4; `BindPort` or `ExternalPort` not
1–65535; `ExternalIP` not a valid IPv4; `RefreshInterval < 5s` or `RefreshInterval >= Expiry`.

### 6.3 Targets and message helpers (`sipmsg.go`) — pure, unit-tested

```go
type targets struct {
	RequestURI sip.Uri // REGISTER Request-URI
	AORUser    string  // = cfg.Username
	AORHost    string  // = cfg.Domain if set, else registrar host (no port)
	DestPort   int     // registrar port, used with the pinned IP
}

func buildTargets(cfg Config) (targets, error)
```

- `Domain == ""`: Request-URI string `"sip:" + cfg.Registrar` (host:port, exactly what S2 used).
- `Domain != ""`: Request-URI string `"sip:" + cfg.Domain`.
- TCP: append `";transport=tcp"` to the Request-URI string. Parse with `sip.ParseUri`.

```go
// ensureAORTo sets To to the address-of-record sip:<user>@<host> (no port) if req has no To header.
// diago builds To from the Request-URI, which has no user part; Bandwidth/JMP answers 500 to that (S2).
// sipgo's ClientRequestRegisterBuild keeps an existing To (client.go:367-376, 443-449).
func ensureAORTo(req *sip.Request, user, host string)
// implementation: if req.To() == nil { req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: host}, Params: sip.NewParams()}) }

// prepareUnregister turns the REGISTER Origin into an unregister for OUR Contact only:
// remove every Expires header (loop RemoveHeader("Expires") until it returns false), append
// sip.ExpiresHeader(0). The Contact header is left untouched (it holds the rport/received-rewritten
// address of our binding). Never Contact: * (that would drop every device on the account).
func prepareUnregister(req *sip.Request)
```

### 6.4 Registrar pinning (`pin.go`) — pure, unit-tested with a fake Resolver

```go
type pinner struct {
	host     string
	port     int
	resolver Resolver
	ips      []net.IP
	idx      int
}

func newPinner(registrar string, r Resolver) (*pinner, error) // parses "host:port"

// resolve: if host is an IP literal -> ips=[that IP], idx=0, no DNS. Otherwise
// LookupIP(ctx, "ip4", host). Empty result -> error. If the currently pinned IP is in the new list,
// keep it (idx = its new index); else idx=0. On error, keep the previous list unchanged and return the error.
func (p *pinner) resolve(ctx context.Context) error

// advance: remember cur := current IP; call resolve (ignore its error for rotation purposes);
// then if cur is in p.ips: idx = (index(cur)+1) % len(ips), else idx = 0. Returns resolve's error.
func (p *pinner) advance(ctx context.Context) error

// current returns net.JoinHostPort(ips[idx].String(), port), or "" if no IPs.
func (p *pinner) current() string
```

DNS order is kept as returned (no sorting). IPv4 only (`udp4`/`tcp4` transports, S2 §3.10).

### 6.5 Backoff (`backoff.go`) — pure, unit-tested

```go
const (
	backoffBase       = 2 * time.Second
	backoffMax        = 60 * time.Second
	authRetryInterval = 5 * time.Minute
	attemptTimeout    = 10 * time.Second
	ipSwitchAfter     = 2 // consecutive transient failures on one IP before switching
)

// backoffDelay(attempt) for attempt >= 1: min(backoffBase * 2^(attempt-1), backoffMax).
// 1->2s, 2->4s, 3->8s, 4->16s, 5->32s, 6+->60s. attempt < 1 is treated as 1.
func backoffDelay(attempt int) time.Duration

// jitter(d, r) with r in [0,1): d * (0.8 + 0.4*r), i.e. ±20%. jitter(d, 0.5) == d.
func jitter(d time.Duration, r float64) time.Duration
```

### 6.6 Register loop (`loop.go`) — unit-tested with fakes

```go
// regTxn is the minimal REGISTER transaction surface. diagoTxn implements it; tests use a fake.
// Implementations must return *ResponseError for non-200 final responses.
type regTxn interface {
	Register(ctx context.Context) error   // full REGISTER: fresh Via, digest, Contact re-derived from rport/received
	Refresh(ctx context.Context) error    // re-send the current Origin (diago Qualify)
	Unregister(ctx context.Context) error // prepareUnregister + send (diago Qualify)
	SetDestination(hostport string)
	Contact() string
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeAuth
	outcomeTransient
)

// classify: nil -> outcomeOK. errors.As(err, *ResponseError) with StatusCode 401, 403, 404 or 407 ->
// outcomeAuth (status returned). Any other ResponseError -> outcomeTransient (status returned).
// Every other error (timeouts, network, DNS, context deadline) -> outcomeTransient, status 0.
func classify(err error) (outcome, int)

type registerLoop struct {
	cfg     Config
	txn     regTxn
	pin     *pinner
	log     zerolog.Logger
	sleep   func(ctx context.Context, d time.Duration) error // default: timer + ctx.Done; returns ctx.Err()
	rand    func() float64                                    // default math/rand/v2 Float64
	emit    func(StateEvent)                                  // stores current state; calls cfg.OnStateChange on State change only
}

func (l *registerLoop) run(ctx context.Context)          // returns when ctx is done
func (l *registerLoop) shutdown(ctx context.Context, last State) error // Unregister iff last == StateRegistered
```

`run` behavior (exact):

```
emit(Registering)
needFull := true; attempt := 0; failsOnIP := 0
if err := pin.resolve(ctx); err != nil  -> handle as a transient failure (below) with Reason "resolve registrar: <err>"
loop:
    if ctx done: return
    if pin.current() == "": err := pin.resolve(ctx); if err: transient failure (as above), sleep, continue
    txn.SetDestination(pin.current())
    actx := WithTimeout(ctx, attemptTimeout)
    err := needFull ? txn.Register(actx) : txn.Refresh(actx)
    if ctx done: return                                   // Stop() during an attempt
    switch classify(err):
      OK:        attempt=0; failsOnIP=0; needFull=false
                 emit(Registered{RegistrarAddr, Contact: txn.Contact()}); wait = cfg.RefreshInterval
      Auth:      needFull=true; attempt=0; failsOnIP=0
                 wait = authRetryInterval; emit(AuthFailed{StatusCode, Reason, NextRetry: wait})
      Transient: needFull=true; attempt++; failsOnIP++
                 if failsOnIP >= ipSwitchAfter { pin.advance(ctx) (log error, continue anyway); failsOnIP=0 }
                 wait = jitter(backoffDelay(attempt), rand()); emit(TransientError{StatusCode, Reason, Err, NextRetry: wait})
    log one line per attempt result (Info on success transitions, Warn on failures) with
    registrar_addr, status, reason, next_retry, username=MaskUsername(user). Never the password.
    if sleep(ctx, wait) != nil: return
```

`emit` stores the event as the current `State()` every time, but calls `OnStateChange` only when `State`
differs from the previously emitted `State` (so `CONNECTED` is not re-sent every 25 s, and repeated
transient failures produce one callback).

Reason text for `ResponseError` is its `Reason`; for other errors use `err.Error()` truncated to 200
bytes. Neither ever contains credentials (sipgo/diago errors carry only the request line).

### 6.7 diago adapter (`diago.go`)

`New` returns `*diagoUA` (unexported). `Start(ctx)`:

1. `t, _ := buildTargets(cfg)`.
2. `ua, err := sipgo.NewUA(sipgo.WithUserAgent(cfg.Username), sipgo.WithUserAgentHostname(t.AORHost))`.
   (sipgo derives the `From`/`Contact` user from the UA name and the `From` host from the hostname,
   `client.go:348-365`; S2 relied on this.)
3. diago logger: `slog.New(zerolog.NewSlogHandler(cfg.Log.With().Str("component", "diago").Logger().Level(zerolog.WarnLevel)))`.
4. ```go
   dg := diago.NewDiago(ua,
       diago.WithLogger(slogger),
       diago.WithTransport(diago.Transport{
           Transport:       string(cfg.Transport) + "4", // "udp4" / "tcp4"
           BindHost:        cfg.BindIP,
           BindPort:        cfg.BindPort,
           ExternalHost:    cfg.ExternalIP,
           ExternalPort:    cfg.ExternalPort,
           MediaExternalIP: net.ParseIP(cfg.ExternalIP),
           RewriteContact:  true,
       }),
       diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000}}),
   )
   ```
5. `serveCtx, serveCancel := context.WithCancel(context.Background())`;
   `if err := dg.ServeBackground(serveCtx, u.rejectInbound); err != nil { serveCancel(); ua.Close(); return fmt.Errorf("sip listen %s:%d/%s: %w", ...) }`
   (`ServeBackground` waits for the listener, `diago.go:523`; the socket must be up to receive REGISTER
   responses.)
6. `rt, err := dg.RegisterTransaction(serveCtx, t.RequestURI, diago.RegisterOptions{Username: cfg.Username, Password: cfg.Password, Expiry: cfg.Expiry, RetryInterval: cfg.RefreshInterval})`
   (`diago.go:828`). Leave `ProxyHost` empty: the loop sets the destination before every attempt.
7. `ensureAORTo(rt.Origin, t.AORUser, t.AORHost)`.
8. Build `pinner` (`newPinner(cfg.Registrar, cfg.Resolver)`), `registerLoop{txn: &diagoTxn{rt: rt}, ...}`,
   `loopCtx, loopCancel := context.WithCancel(context.Background())`, start `go loop.run(loopCtx)` and
   track completion with a `done` channel.

`rejectInbound(d *diago.DialogServerSession)`: log Info `"Rejecting inbound call: call handling is not
implemented yet"` with `from` = `d.FromUser()` (a phone number, allowed) and `call_id`
= `d.InviteRequest.CallID().Value()`, then `_ = d.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)`
(`sip/message.go:46`; `Respond` is `sipgo/dialog_server.go:198`). Phase 2 replaces this with a callback.

`Stop(ctx)` (idempotent via `sync.Once`; returns nil if never started): `loopCancel()`, wait for `done`,
`err := loop.shutdown(ctx, last.State)`, `serveCancel()`, `ua.Close()` (`sipgo/ua.go:105`),
emit `StateStopped`, return `err`. `Unregister` runs only after the loop goroutine has exited because
`Origin` is not safe for concurrent use.

`diagoTxn` (wraps `*diago.RegisterTransaction`, `register_transaction.go`):

| Method | Implementation |
|---|---|
| `Register(ctx)` | `rt.Origin.RemoveHeader("Via")` (D10), `err := rt.Register(ctx)`, `return mapErr(err)` |
| `Refresh(ctx)` | `return mapErr(rt.Qualify(ctx))` |
| `Unregister(ctx)` | `prepareUnregister(rt.Origin)`; `return mapErr(rt.Qualify(ctx))` (D8) |
| `SetDestination(hp)` | `rt.Origin.SetDestination(hp)` (`sip/message.go:193`) |
| `Contact()` | `c := rt.Origin.Contact(); if c == nil return ""; return net.JoinHostPort(c.Address.Host, strconv.Itoa(c.Address.Port))` |

`mapErr`: `var rr *diago.RegisterResponseError; if errors.As(err, &rr) { return &ResponseError{StatusCode: rr.RegisterRes.StatusCode, Reason: rr.RegisterRes.Reason} }; return err`.

Never set `sip.SIPDebug` (it prints raw SIP including digest responses).

### 6.8 `net.go`

- `DefaultBindIP`: `ip, _, err := sip.ResolveInterfacesIP("ip4", nil)` (`sip/utils.go:290`).
- `DiscoverPublicIP`: port `spikes/s2-sip/stun.go` (`stun.Dial("udp4", server)`, binding request,
  `XORMappedAddress`). Run it in a goroutine; `select` on the result and `ctx.Done()`; close the client.
  Return an error if the IP is nil or not IPv4.
- `SetRTPPortRange`: `media.RTPPortStart, media.RTPPortEnd = min, max` (diago `media` package globals).

### 6.9 What sipua deliberately does not do (Phase 1)

No OPTIONS/CRLF keepalive (the 25 s re-register is the keepalive, as in S2). No re-reading of the
server-granted `Expires` (D7). No STUN re-discovery when the public IP changes (Phase 4). No call
handling (Phase 2).

---

## 7. Standalone host (`pkg/connector`, `cmd/mrtc-sip-bridge`)

### 7.1 Verified bridgev2 integration points (mautrix v0.32.0)

| What | Where | Signature / note |
|---|---|---|
| `NetworkConnector` | `bridgev2/networkinterface.go:217` | `Init(*Bridge)`, `Start(context.Context) error`, `GetName() BridgeName`, `GetDBMetaTypes() database.MetaTypes`, `GetCapabilities() *NetworkGeneralCapabilities`, `GetConfig() (example string, data any, upgrader configupgrade.Upgrader)`, `LoadUserLogin(ctx, *UserLogin) error`, `GetLoginFlows() []LoginFlow`, `CreateLogin(ctx, *User, flowID string) (LoginProcess, error)`, `GetBridgeInfoVersion() (info, capabilities int)` |
| `ConfigValidatingNetwork` | `networkinterface.go:309` | `ValidateConfig() error`; config already decoded into GetConfig's `data` (called at `mxmain/main.go:334`) |
| Network config decode | `mxmain/main.go:345-353, 374-379` | framework decodes `network:` into the pointer GetConfig returns; upgrader proxied under `network` |
| `BridgeName` | `networkinterface.go:188` | `DisplayName, NetworkURL, NetworkIcon, NetworkID, BeeperBridgeType, DefaultPort uint16, DefaultCommandPrefix` |
| `NetworkGeneralCapabilities` | `networkinterface.go:373` | return `&bridgev2.NetworkGeneralCapabilities{}` |
| `NetworkAPI` | `networkinterface.go:399` | `Connect(ctx)`, `Disconnect()`, `IsLoggedIn() bool`, `LogoutRemote(ctx)`, `IsThisUser(ctx, networkid.UserID) bool`, `GetChatInfo(ctx, *Portal) (*ChatInfo, error)`, `GetUserInfo(ctx, *Ghost) (*UserInfo, error)`, `GetCapabilities(ctx, *Portal) *event.RoomFeatures`, `HandleMatrixMessage(ctx, *MatrixMessage) (*MatrixMessageResponse, error)` |
| `IdentifierResolvingNetworkAPI` | `networkinterface.go:827` | `ResolveIdentifier(ctx, identifier string, createChat bool) (*ResolveIdentifierResponse, error)`; response structs at `:784` (`Ghost, UserID, UserInfo, Chat`) and `:805` (`PortalKey, Portal, PortalInfo`) |
| Login interfaces | `bridgev2/login.go:33` `LoginProcess` (`Start`, `Cancel`), `:84` `LoginProcessUserInput` (`SubmitUserInput(ctx, map[string]string) (*LoginStep, error)`), `:107` `LoginFlow`, `:133` `LoginStep`, `:322-334` field types, `:336` `LoginInputDataField` (`Type, ID, Name, Description, DefaultValue, Optional, Pattern, Options, Validate func(string) (string, error)`), `:436` `LoginUserInputParams`, `:462` `LoginCompleteParams` |
| Login command behavior | `bridgev2/commands/login.go:169-177, 222-260` | one-line form needs exactly one arg per field (passwords with spaces must use the prompt form); prompts list `Options`, `skip` for optional fields; our `Validate` is used if set |
| `User.NewLogin` | `bridgev2/userlogin.go:195`, params `:179` | `NewLogin(ctx, *database.UserLogin, *NewLoginParams{LoadUserLogin, DeleteOnConflict, DontReuseExisting})`; disconnects an existing client with the same ID first (`:238-240`); **does not call Connect** |
| Connect at startup | `bridgev2/bridge.go:404-412` | `StartLoginsForUsers` calls `login.Client.Connect(...)` after `StartConnectors` (`:206` Matrix.Start, `:211` Network.Start) |
| Init order | `bridgev2/bridge.go:99-101` | `Matrix.Init`, `Bot = Matrix.BotIntent()`, then `Network.Init(br)` (inside `NewBridge`, before `PostInit`) |
| Bridge state | `bridgev2/userlogin.go:35` `UserLogin.BridgeState *BridgeStateQueue`; `bridgev2/bridgestate.go:352` `Send(status.BridgeState)`; states `bridgev2/status/bridgestate.go:46-52`; struct `:116`; `UserActionRelogin` `:79`; `RemoteProfile` `:83` |
| `database.UserLogin` | `bridgev2/database/userlogin.go:26` | `ID, RemoteName, RemoteProfile status.RemoteProfile, Metadata any` |
| `database.MetaTypes` | `bridgev2/database/database.go:40` | `Portal, Ghost, Message, Reaction, UserLogin MetaTypeCreator` (nil allowed) |
| Portals | `bridgev2/portal.go:330` `GetPortalByKey(ctx, networkid.PortalKey)`, `:269` `GetPortalByMXID(ctx, id.RoomID)`, `:5374` `(*Portal).CreateMatrixRoom(ctx, source *UserLogin, info *ChatInfo) error` (nil info → calls `GetChatInfo`) |
| Chat info | `portal.go:4418` `ChatInfo` (`Name, Topic, Avatar, Members *ChatMemberList, JoinRule, Type *database.RoomType, ..., CanBackfill`), `:4307` `ChatMemberList` (`IsFull, OtherUserID, MemberMap ChatMemberMap, PowerLevels *PowerLevelOverrides`), `:4265` `ChatMember` (`EventSender, Membership, PowerLevel, UserInfo`), `:4283` `ChatMemberMap` + `Set`, `:4345` `PowerLevelOverrides{Events map[event.Type]int, ...}` |
| `EventSender` | `networkinterface.go:60` | `IsFromMe bool, SenderLogin networkid.UserLoginID, Sender networkid.UserID` |
| Ghosts | `bridgev2/ghost.go:95` `GetGhostByID(ctx, networkid.UserID)`, `:82` `IsGhostMXID(id.UserID) bool`, `:32-36` `Ghost{Intent MatrixAPI}`, `:146` `UserInfo{Identifiers, Name *string, Avatar, IsBot *bool, ...}` |
| Double puppet | `bridgev2/user.go:137` | `(*User).DoublePuppet(ctx) MatrixAPI` (nil if none) |
| Login lookup | `bridgev2/userlogin.go:150` | `(*Bridge).GetCachedUserLoginByID(networkid.UserLoginID) *UserLogin` |
| `MatrixAPI` | `bridgev2/matrixinterface.go:199-205` | `GetMXID() id.UserID`; `SendMessage(ctx, roomID, event.Type, *event.Content, *MatrixSendExtra) (*mautrix.RespSendEvent, error)`; `SendState(ctx, roomID, event.Type, stateKey string, *event.Content, ts time.Time) (*mautrix.RespSendEvent, error)`; `MarkRead(ctx, roomID, eventID, ts time.Time) error`. Zero `ts` is fine (only `> 0` is sent, `client.go:1496`) |
| Raw ghost intent | `bridgev2/matrix/intent.go:37` | `type ASIntent struct { Matrix *appservice.IntentAPI; ... }`; `ghost.Intent.(*matrix.ASIntent).Matrix` |
| OpenID as ghost | `client.go:1995` | `(*Client).RequestOpenIDToken(ctx) (*RespOpenIDToken, error)`; fields `responses.go:807-812`; IntentAPI masquerades with `?user_id=` (proven in S1) |
| Redact as ghost | `appservice/intent.go:442` | `RedactEvent(ctx, roomID, eventID, ...mautrix.ReqRedact) (*mautrix.RespSendEvent, error)`; `ReqRedact{Reason}` at `requests.go:161` |
| Event hook | `appservice/eventprocessor.go:30` `EventHandler = func(ctx, *event.Event)` (alias), `:63` `On(event.Type, EventHandler)`, `:128` `Dispatch` (exact Type+Class match); `bridgev2/matrix/connector.go:89` `Connector.EventProcessor`, `:229` processor started in `Connector.Start` |
| PostInit | `mxmain/main.go:71` field, `:270-272` called at end of `Init`, before `Start`; `BridgeMain.Matrix *matrix.Connector` (`:87`), `.Config *bridgeconfig.Config` (`:86`) |
| Encryption flag | `bridgev2/bridgeconfig/config.go:36` `Encryption EncryptionConfig`; `encryption.go:15` `Default bool` |
| Unsupported message error | `bridgev2/errors.go:48-89` | `WrapErrorInStatus(err).WithIsCertain(true).WithErrorAsMessage().WithSendNotice(bool).WithErrorReason(event.MessageStatusUnsupported)` |
| Version stamping | `mxmain/main.go:469` | `InitVersion(tag, commit, rawBuildTime string)` |

### 7.2 Network config (`config.go`, `example-config.yaml`) — T3

```go
package connector

// Config is the `network:` section. Field names are YAML keys.
type Config struct {
	SIPBindIP              string `yaml:"sip_bind_ip"`
	SIPPortMin             int    `yaml:"sip_port_min"`
	SIPPortMax             int    `yaml:"sip_port_max"`
	ExternalIP             string `yaml:"external_ip"`
	STUNServer             string `yaml:"stun_server"`
	RTPPortMin             int    `yaml:"rtp_port_min"`
	RTPPortMax             int    `yaml:"rtp_port_max"`
	RegisterExpirySeconds  int    `yaml:"register_expiry_seconds"`
	RegisterRefreshSeconds int    `yaml:"register_refresh_seconds"`
	RingTimeoutSeconds     int    `yaml:"ring_timeout_seconds"`
	LiveKitServiceURL      string `yaml:"livekit_service_url"`
	DefaultCountryCode     string `yaml:"default_country_code"`
}

const (
	defaultSIPPortMin             = 5060
	defaultSIPPortMax             = 5069
	defaultRTPPortMin             = 40000
	defaultRTPPortMax             = 40100
	defaultRegisterExpirySeconds  = 60
	defaultRegisterRefreshSeconds = 25
	defaultRingTimeoutSeconds     = 30
	defaultCountryCode            = "1"
	defaultSTUNServer             = "stun.l.google.com:19302" // only used in the example YAML
)

// ApplyDefaults fills zero-valued ints and an empty DefaultCountryCode with the defaults above.
// Strings that may legitimately be empty (SIPBindIP, ExternalIP, STUNServer, LiveKitServiceURL) are left alone.
func (c *Config) ApplyDefaults()

// Validate returns the first error found, naming the YAML key ("network.<key>: ..."):
//   - sip_bind_ip: "" or a valid IPv4 (0.0.0.0 allowed, means auto-detect)
//   - sip_port_min/max and rtp_port_min/max: 1..65535, min <= max; rtp range must have >= 2 ports
//   - SIP and RTP ranges must not overlap
//   - external_ip: "" or a valid non-unspecified IPv4
//   - stun_server: "" or host:port (net.SplitHostPort succeeds, port numeric)
//   - register_refresh_seconds >= 5 and < register_expiry_seconds; register_expiry_seconds <= 3600
//   - ring_timeout_seconds: 5..300
//   - livekit_service_url: "" or an absolute http/https URL (url.Parse, Scheme http|https, Host non-empty)
//   - default_country_code: 1-3 ASCII digits, not starting with 0
func (c *Config) Validate() error

// Helpers used by T4:
func (c *Config) RegisterExpiry() time.Duration  // seconds -> Duration
func (c *Config) RegisterRefresh() time.Duration
func (c *Config) RingTimeout() time.Duration     // unused until Phase 2

//go:embed example-config.yaml
var ExampleConfig string

// upgrader copies every key from the user's file into the new base (mautrix-jabber lesson: keys that
// are not copied get reset to the example value on every start).
var upgrader = configupgrade.SimpleUpgrader(func(h configupgrade.Helper) {
	h.Copy(configupgrade.Str, "sip_bind_ip")
	h.Copy(configupgrade.Int, "sip_port_min")
	h.Copy(configupgrade.Int, "sip_port_max")
	h.Copy(configupgrade.Str, "external_ip")
	h.Copy(configupgrade.Str, "stun_server")
	h.Copy(configupgrade.Int, "rtp_port_min")
	h.Copy(configupgrade.Int, "rtp_port_max")
	h.Copy(configupgrade.Int, "register_expiry_seconds")
	h.Copy(configupgrade.Int, "register_refresh_seconds")
	h.Copy(configupgrade.Int, "ring_timeout_seconds")
	h.Copy(configupgrade.Str, "livekit_service_url")
	h.Copy(configupgrade.Str, "default_country_code")
})
```

`example-config.yaml` (exact content; keys at top level, the framework nests it under `network:`):

```yaml
# Local IPv4 to bind SIP sockets on. Empty or 0.0.0.0 = auto-detect the first non-loopback IPv4.
# (diago needs a concrete IP so REGISTER and incoming INVITEs share one socket.)
sip_bind_ip: ""
# SIP ports, one per active login (UDP and TCP). Publish/forward this range if behind NAT.
sip_port_min: 5060
sip_port_max: 5069
# Public IPv4 put in the SIP Contact and SDP. Empty = discover with STUN (below), else the bind IP.
external_ip: ""
# STUN server (host:port) used when external_ip is empty. Empty disables STUN.
stun_server: stun.l.google.com:19302
# RTP (call audio) UDP port range. Must be reachable from your SIP provider.
rtp_port_min: 40000
rtp_port_max: 40100
# REGISTER Expires to request, and how often to re-register. Re-registering often keeps NAT
# bindings open; refresh must be shorter than expiry.
register_expiry_seconds: 60
register_refresh_seconds: 25
# How long an incoming call rings in Matrix before it is treated as missed.
ring_timeout_seconds: 30
# lk-jwt-service (MatrixRTC auth service) URL. Empty = discover from the homeserver (not implemented yet).
livekit_service_url: ""
# Country calling code assumed for numbers without a leading + (1 = North America).
default_country_code: "1"
```

### 7.3 `connector.go` — T3 (struct and framework methods)

```go
package connector

type SIPConnector struct {
	br     *bridgev2.Bridge
	Config Config
	RTC    *core.Ingest // created in Init with core.LogSink{}; Phase 2 calls RTC.SetSink

	// Set in Start (T4):
	bindIP     net.IP
	externalIP net.IP
	ports      *portPool
}
```

| Method | Behavior |
|---|---|
| `Init(br)` | `c.br = br; c.RTC = core.NewIngest(core.LogSink{})` |
| `GetName()` | `bridgev2.BridgeName{DisplayName: "SIP", NetworkURL: "https://github.com/williamkray/mrtc-sip-bridge", NetworkID: "sip", BeeperBridgeType: "github.com/williamkray/mrtc-sip-bridge", DefaultPort: 29319, DefaultCommandPrefix: "!sip"}` |
| `GetConfig()` | `return ExampleConfig, &c.Config, upgrader` |
| `ValidateConfig()` | `c.Config.ApplyDefaults(); return c.Config.Validate()` |
| `GetDBMetaTypes()` | `database.MetaTypes{Portal: func() any { return &PortalMetadata{} }, Ghost: func() any { return &GhostMetadata{} }, UserLogin: func() any { return &UserLoginMetadata{} }}` (Message, Reaction nil) |
| `GetCapabilities()` | `&bridgev2.NetworkGeneralCapabilities{}` |
| `GetBridgeInfoVersion()` | `return 1, 1` |

T3 must **not** add the `var _ bridgev2.NetworkConnector = ...` assertion (T4 adds it once `Start`,
`LoadUserLogin`, `GetLoginFlows`, `CreateLogin` exist).

### 7.4 Metadata (`dbmeta.go`) — T3

```go
// UserLoginMetadata is stored as JSON in user_login.metadata. Password is stored in plain text in the
// bridge DB, like mautrix-jabber's JabberLoginMeta. Never log it.
type UserLoginMetadata struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Registrar     string `json:"registrar"`        // "host:port", normalized
	Domain        string `json:"domain,omitempty"` // reserved: SIP domain if different from registrar host; not settable yet
	Transport     string `json:"transport"`        // "udp" | "tcp"
	DisplayNumber string `json:"display_number"`   // E.164 of the account's phone number
}

// PortalMetadata is reserved for Phase 2+ (e.g. last call info). Empty for now.
type PortalMetadata struct{}

// GhostMetadata is reserved. Empty for now.
type GhostMetadata struct{}
```

### 7.5 IDs (`ids.go`) — T3

```go
// MakePortalKey: one DM portal per remote number per login (D19).
func MakePortalKey(e164 string, login networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(e164), Receiver: login}
}
// MakeUserID: ghost ID = E.164 digits without "+".
func MakeUserID(e164 string) networkid.UserID { return networkid.UserID(phone.Digits(e164)) }
// ParseUserID: inverse of MakeUserID; ok=false if not valid E.164 digits.
func ParseUserID(uid networkid.UserID) (e164 string, ok bool) { return phone.FromDigits(string(uid)) }
// MakeLoginID: lower(displayE164 + "@" + registrarHost), registrarHost without port. The SIP username is
// never part of the ID (bridgev2 logs login IDs unmasked); it lives only in UserLoginMetadata.
func MakeLoginID(displayE164, registrarHost string) networkid.UserLoginID
```

### 7.6 Chat/user info (`chatinfo.go`) — T3

```go
// buildUserInfo: Name = e164, Identifiers = ["tel:"+e164], IsBot = false.
func buildUserInfo(e164 string) *bridgev2.UserInfo

// buildChatInfo returns the info for the DM portal between the login owner and remoteE164.
func buildChatInfo(loginID networkid.UserLoginID, ownerE164, remoteE164 string) *bridgev2.ChatInfo
```

`buildChatInfo` returns:

- `Type: ptr.Ptr(database.RoomTypeDM)` (`go.mau.fi/util/ptr`), `Name: nil` (DM naming via the ghost),
  `CanBackfill: false`.
- `Members: &bridgev2.ChatMemberList{IsFull: true, OtherUserID: MakeUserID(remoteE164), MemberMap: m, PowerLevels: &bridgev2.PowerLevelOverrides{Events: map[event.Type]int{matrixrtc.CallMember: 0, matrixrtc.CallMemberAlias: 0}}}`
  where `m := bridgev2.ChatMemberMap{}` with
  `m.Set(bridgev2.ChatMember{EventSender: bridgev2.EventSender{IsFromMe: true, SenderLogin: loginID}, Membership: event.MembershipJoin})` and
  `m.Set(bridgev2.ChatMember{EventSender: bridgev2.EventSender{Sender: MakeUserID(remoteE164)}, Membership: event.MembershipJoin, UserInfo: buildUserInfo(remoteE164)})`.
  The owner member deliberately has no `Sender`: with one, bridgev2 invites a ghost of the owner's own
  number into every portal when the owner is not double-puppeted. `ChatMemberMap.Set` accepts a
  login-only member (it is keyed under the empty user ID), and `OtherUserID` is set explicitly so the DM
  still resolves.
  The power-level overrides are what let Element show the call button and let the ghost send
  `call.member` (wire spec §1.5).

### 7.7 Login input parsing (`loginparse.go`) — T3, pure

```go
const (
	fieldUsername      = "username"
	fieldPassword      = "password"
	fieldRegistrar     = "registrar"
	fieldTransport     = "transport"
	fieldDisplayNumber = "display_number"
	defaultSIPPort     = "5060"
)

// normalizeRegistrar: trim spaces; strip a leading "sip:" (case-insensitive); reject anything with "@",
// "/" or "[" (IPv6 not supported); if there is no port (net.SplitHostPort fails with "missing port"),
// append ":5060"; port must be 1..65535; host must be non-empty. Returns lower-cased "host:port".
func normalizeRegistrar(s string) (string, error)

// parseLoginInput validates the submitted map and returns metadata ready to store.
// username: TrimSpace, required. password: required, NOT trimmed or modified.
// registrar: normalizeRegistrar. transport: sipua.ParseTransport (""->udp; "tls" -> error).
// display_number: phone.Normalize(v, defaultCC), required.
func parseLoginInput(input map[string]string, defaultCC string) (*UserLoginMetadata, error)
```

### 7.8 Port pool (`portpool.go`) — T3

```go
var ErrNoFreePort = errors.New("no free SIP port in sip_port_min..sip_port_max")

type portPool struct { mu sync.Mutex; min, max int; used map[int]bool }

func newPortPool(min, max int) *portPool
func (p *portPool) Acquire() (int, error) // lowest free port, marks it used
func (p *portPool) Release(port int)       // no-op for unknown/unused ports
```

### 7.9 Ghost intent adapter (`ghostintent.go`) — T3

```go
type ghostIntent struct {
	api bridgev2.MatrixAPI     // ghost.Intent
	raw *appservice.IntentAPI  // ghost.Intent.(*matrix.ASIntent).Matrix
}

var _ core.GhostIntent = (*ghostIntent)(nil)

// newGhostIntent type-asserts ghost.Intent to *matrix.ASIntent; error if that fails.
func newGhostIntent(ghost *bridgev2.Ghost) (*ghostIntent, error)
```

| Method | Implementation |
|---|---|
| `UserID()` | `g.api.GetMXID()` |
| `SendState(...)` | `resp, err := g.api.SendState(ctx, roomID, evtType, stateKey, &event.Content{Raw: content}, time.Time{})`; return `resp.EventID` |
| `SendMessage(...)` | `resp, err := g.api.SendMessage(ctx, roomID, evtType, &event.Content{Raw: content}, nil)`; return `resp.EventID` |
| `Redact(...)` | `_, err := g.raw.RedactEvent(ctx, roomID, eventID, mautrix.ReqRedact{Reason: reason})` |
| `RequestOpenIDToken(ctx)` | `r, err := g.raw.RequestOpenIDToken(ctx)`; map the four fields to `core.OpenIDToken` |

### 7.10 Standalone CallHost (`callhost.go`) — T3

```go
type callHost struct{ c *SIPConnector }

var _ core.CallHost = (*callHost)(nil)

// CallHost returns the standalone core.CallHost (used by Phase 2).
func (c *SIPConnector) CallHost() core.CallHost { return &callHost{c: c} }
```

- `login(ref)`: `c.br.GetCachedUserLoginByID(networkid.UserLoginID(ref))`; nil → `core.ErrUnknownLogin`.
- `ResolveCallRoom(ctx, ref, e164)`: `phone.IsE164(e164)` else error; `ul := login(ref)`;
  `meta := ul.Metadata.(*UserLoginMetadata)`; `portal, err := c.br.GetPortalByKey(ctx, MakePortalKey(e164, ul.ID))`;
  if `portal.MXID == ""`: `err = portal.CreateMatrixRoom(ctx, ul, buildChatInfo(ul.ID, meta.DisplayNumber, e164))`;
  `ghost, err := c.br.GetGhostByID(ctx, MakeUserID(e164))`; `gi, err := newGhostIntent(ghost)`;
  return `&core.CallRoom{RoomID: portal.MXID, RemoteE164: e164, Ghost: gi}`.
- `SendCallNotice(ctx, room, text)`: `room.Ghost.SendMessage(ctx, room.RoomID, event.EventMessage, map[string]any{"msgtype": "m.notice", "body": text})`.
- `OwnerMXID(ref)`: `ul.UserMXID` or `""`.
- `MarkReadAsOwner(ctx, ref, roomID, eventID)`: `dp := ul.User.DoublePuppet(ctx)`; nil → `core.ErrNoDoublePuppet`;
  `return dp.MarkRead(ctx, roomID, eventID, time.Now())`.

### 7.11 S3 hook (`rtchook.go`) — T3

```go
// RegisterMatrixRTCHandlers wires call.member / rtc.notification / rtc.decline into c.RTC.
// Call from PostInit (before the EventProcessor starts). reg is br.Matrix.EventProcessor.
func (c *SIPConnector) RegisterMatrixRTCHandlers(reg core.EventRegistrar) {
	core.RegisterMatrixRTCHandlers(reg, c.resolveRTCLogin, c.RTC)
}

// resolveRTCLogin drops events from bridge ghosts and the bridge bot (bridgev2's own filtering
// does not apply to raw handlers, S3 §1), then maps the room to its portal's Receiver login.
func (c *SIPConnector) resolveRTCLogin(ctx context.Context, evt *event.Event) (core.LoginRef, bool) {
	// if c.br.IsGhostMXID(evt.Sender) || evt.Sender == c.br.Bot.GetMXID() -> false
	// portal, err := c.br.GetPortalByMXID(ctx, evt.RoomID); err != nil or portal == nil -> false
	//   (log err at Warn via zerolog.Ctx(ctx))
	// portal.Receiver == "" -> false
	// return core.LoginRef(portal.Receiver), true
}
```

### 7.12 Login flow (`login.go`, `start.go`) — T4

Flow list (`GetLoginFlows`): `[]bridgev2.LoginFlow{{ID: "password", Name: "SIP account", Description: "Log in with a SIP username, password and registrar"}}`.
`CreateLogin(ctx, user, flowID)`: `flowID != "password"` → error `unknown login flow %q`; else
`&sipLoginProcess{c: c, user: user}`.

Step IDs: `com.github.williamkray.mrtcsip.credentials` and `com.github.williamkray.mrtcsip.complete`.

`Start(ctx)` returns one `LoginStepTypeUserInput` step with instructions:

> Enter your SIP account details. Answer each prompt as its own message (passwords may contain spaces).
> Registering will take over incoming calls for this account; if a desk phone or ATA uses the same
> account, calls may stop reaching it.

and fields in this order (IDs are the constants from §7.7):

| ID | Type | Name | Description | Extra |
|---|---|---|---|---|
| `username` | `LoginInputFieldTypeUsername` | SIP username | The SIP account username. For JMP this is the numeric SIP username, not your phone number. | |
| `password` | `LoginInputFieldTypePassword` | SIP password | Your SIP account password. | |
| `registrar` | `LoginInputFieldTypeDomain` | Registrar | Registrar host:port from your provider or ATA config, e.g. `example.auth.bandwidth.com:5008`. Port defaults to 5060. | `Validate: normalizeRegistrar` |
| `transport` | `LoginInputFieldTypeSelect` | Transport | SIP transport. Use udp unless your provider says otherwise. | `Options: ["udp","tcp"]`, `DefaultValue: "udp"`, `Optional: true`, `Validate`: `t, err := sipua.ParseTransport(s); return string(t), err` |
| `display_number` | `LoginInputFieldTypePhoneNumber` | Phone number | The account's phone number, e.g. +15551234567 (numbers without + use the default country code). | `Validate: func(s string) (string, error) { return phone.Normalize(s, c.Config.DefaultCountryCode) }` (must be set, otherwise bridgev2's default validator demands a leading +) |

`SubmitUserInput(ctx, input)`:

1. `meta, err := parseLoginInput(input, c.Config.DefaultCountryCode)`; error → return it (no wrapping
   that could include the password; `parseLoginInput` errors never contain the password).
2. `client := c.newClient(meta)` (no login attached yet); `err := client.start()`; error → return
   `fmt.Errorf("could not start SIP client: %w", err)`.
3. `ev, err := client.waitFirstResult(ctx, 30*time.Second)`:
   - `StateRegistered` → continue.
   - `StateAuthFailed` → `client.stop()`; return a `bridgev2.RespError` (ErrCode `COM.GITHUB.WILLIAMKRAY.MRTCSIP.BAD_CREDENTIALS`, HTTP 403) whose message is `the registrar rejected the credentials (SIP %d %s)`.
   - `StateTransientError` → `client.stop()`; return a `RespError` (`...REGISTER_FAILED`, HTTP 502) with message `could not register with %s: %s`.
   - timeout/ctx error → `client.stop()`; return a `RespError` (`...REGISTER_TIMEOUT`, HTTP 504) with message `timed out registering with %s`.
4. ```go
   host, _, _ := net.SplitHostPort(meta.Registrar)
   loginID := MakeLoginID(meta.DisplayNumber, host)
   ul, err := p.user.NewLogin(ctx, &database.UserLogin{
       ID:            loginID,
       RemoteName:    meta.DisplayNumber,
       RemoteProfile: status.RemoteProfile{Phone: meta.DisplayNumber},
       Metadata:      meta,
   }, &bridgev2.NewLoginParams{LoadUserLogin: func(_ context.Context, ul *bridgev2.UserLogin) error {
       client.attach(ul)
       ul.Client = client
       return nil
   }})
   ```
   error → `client.stop()`; return `fmt.Errorf("failed to save login: %w", err)`.
5. `client.sendBridgeState()` (sends the bridge state for the current sipua state, i.e. CONNECTED).
6. Clear `p.pending`; return `LoginStepTypeComplete` with `StepID` complete,
   `Instructions: fmt.Sprintf("Registered %s via %s. Incoming calls are not bridged yet in this version; they are rejected so your provider can send them to voicemail.", meta.DisplayNumber, meta.Registrar)`,
   `CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: loginID, UserLogin: ul}`.

`sipLoginProcess` keeps `pending *SIPClient` between steps 2 and 6; `Cancel()` stops `pending` if set.

`LoadUserLogin(ctx, login)` (`start.go`): `meta, _ := login.Metadata.(*UserLoginMetadata)`; if
`meta == nil || meta.Username == "" || meta.Password == "" || meta.Registrar == ""` →
`login.Client = &stubClient{login: login}`; else `cl := c.newClient(meta); cl.attach(login); login.Client = cl`.
Do **not** start it here; the framework calls `Connect` (`bridge.go:410`).

`Start(ctx)` (`start.go`):

1. Bind IP: if `Config.SIPBindIP` is `""` or `"0.0.0.0"` → `sipua.DefaultBindIP()`, else parse.
   Error → return `fmt.Errorf("network.sip_bind_ip: %w", err)` (bridge refuses to start).
2. External IP: `Config.ExternalIP` if set; else if `STUNServer != ""` → `sipua.DiscoverPublicIP` with a
   5 s timeout, on error log Warn `"STUN failed; using bind IP as external IP (set network.external_ip)"`
   and use the bind IP; else the bind IP (log Info).
3. `sipua.SetRTPPortRange(Config.RTPPortMin, Config.RTPPortMax)`.
4. `c.ports = newPortPool(Config.SIPPortMin, Config.SIPPortMax)`.
5. Log Info `bind_ip`, `external_ip`, SIP and RTP ranges. Return nil.

Add at the top of `start.go`:

```go
var (
	_ bridgev2.NetworkConnector        = (*SIPConnector)(nil)
	_ bridgev2.ConfigValidatingNetwork = (*SIPConnector)(nil)
)
```

### 7.13 NetworkAPI client (`client.go`, `client_state.go`, `stub_client.go`) — T4

```go
type SIPClient struct {
	c     *SIPConnector
	meta  *UserLoginMetadata
	login *bridgev2.UserLogin // nil until attach
	log   zerolog.Logger

	mu        sync.Mutex
	ua        sipua.UA
	port      int
	last      sipua.StateEvent
	lastSent  sipua.State   // last state for which a bridge state was sent (StateIdle = none)
	firstCh   chan sipua.StateEvent // buffered 1
	firstDone bool
}

var (
	_ bridgev2.NetworkAPI                    = (*SIPClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*SIPClient)(nil)
)
```

Internal methods:

- `newClient(meta)`: `log` = `c.br.Log.With().Str("component", "sip").Str("sip_user", sipua.MaskUsername(meta.Username)).Logger()`; `firstCh = make(chan sipua.StateEvent, 1)`.
- `attach(ul)`: set `login = ul`, `log = ul.Log.With().Str("component", "sip").Logger()`.
- `start()`: under `mu`; if `ua != nil` return nil. `port, err := c.ports.Acquire()`. Build
  `sipua.Config{Username, Password, Registrar, Domain, Transport: sipua.Transport(meta.Transport), BindIP: c.bindIP.String(), BindPort: port, ExternalIP: c.externalIP.String(), Expiry: c.Config.RegisterExpiry(), RefreshInterval: c.Config.RegisterRefresh(), Log: log, OnStateChange: cl.onState}`.
  `ua, err := sipua.New(cfg)`; `err = ua.Start(context.Background())`; on any error release the port and
  return it. Store `ua`, `port`.
- `stop()`: under `mu` take `ua`/`port` and nil them; outside the lock
  `ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)`; `ua.Stop(ctx)` (log error at
  Warn); `c.ports.Release(port)`. Safe to call repeatedly / when never started.
- `onState(ev)`: under `mu`: `last = ev`; if `!firstDone` and `ev.State` is Registered, AuthFailed or
  TransientError → non-blocking send on `firstCh`, `firstDone = true`. Then (outside the lock)
  `sendBridgeState()`.
- `sendBridgeState()`: if `login == nil` return. `bs, ok := bridgeStateFor(last)`; if `!ok` or
  `last.State == lastSent` return; `lastSent = last.State`; `login.BridgeState.Send(bs)`.
- `waitFirstResult(ctx, timeout) (sipua.StateEvent, error)`: select on `firstCh`, `time.After(timeout)`, `ctx.Done()`.

`bridgeStateFor(ev sipua.StateEvent) (status.BridgeState, bool)` (`client_state.go`, pure, tested):

| sipua state | BridgeState | ok |
|---|---|---|
| `StateRegistering` | `{StateEvent: status.StateConnecting}` | true |
| `StateRegistered` | `{StateEvent: status.StateConnected}` | true |
| `StateAuthFailed` | `{StateEvent: status.StateBadCredentials, Error: "sip-auth-failed", Message: fmt.Sprintf("SIP registrar rejected the credentials (%d %s)", ev.StatusCode, ev.Reason), UserAction: status.UserActionRelogin}` | true |
| `StateTransientError` | `{StateEvent: status.StateTransientDisconnect, Error: "sip-register-failed", Message: fmt.Sprintf("SIP registration failed (%s); retrying in %s", reasonOrStatus, ev.NextRetry.Round(time.Second))}` where `reasonOrStatus` is `"<code> <reason>"` if `StatusCode != 0`, else `Reason` | true |
| `StateIdle`, `StateStopped` | zero value | false |

NetworkAPI methods:

| Method | Behavior |
|---|---|
| `Connect(ctx)` | `if err := cl.start(); err != nil { login.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: "sip-start-failed", Message: err.Error()}) }`. If already running (post-login), just `sendBridgeState()`. |
| `Disconnect()` | `cl.stop()` |
| `IsLoggedIn()` | `meta != nil && last.State != sipua.StateAuthFailed` (under `mu`) |
| `LogoutRemote(ctx)` | `cl.stop()` (SIP has no remote session to invalidate; stop unregisters our Contact) |
| `IsThisUser(ctx, uid)` | `uid == MakeUserID(meta.DisplayNumber)` |
| `GetChatInfo(ctx, portal)` | `remote := string(portal.ID)`; `!phone.IsE164(remote)` → error; `return buildChatInfo(login.ID, meta.DisplayNumber, remote), nil` |
| `GetUserInfo(ctx, ghost)` | `e164, ok := ParseUserID(ghost.ID)`; `!ok` → error; `return buildUserInfo(e164), nil` |
| `GetCapabilities(ctx, portal)` | `return &event.RoomFeatures{ID: "com.github.williamkray.mrtcsip.capabilities.v1"}` (package-level var, same pointer every call) |
| `HandleMatrixMessage(ctx, msg)` | `return nil, bridgev2.WrapErrorInStatus(errors.New("this bridge only handles calls; text messages are not sent")).WithIsCertain(true).WithErrorAsMessage().WithSendNotice(false).WithErrorReason(event.MessageStatusUnsupported)` |
| `ResolveIdentifier(ctx, identifier, createChat)` | `e164, err := phone.Normalize(identifier, c.Config.DefaultCountryCode)`; error → `fmt.Errorf("invalid phone number: %w", err)`. `ghost, err := c.br.GetGhostByID(ctx, MakeUserID(e164))`. Response `{Ghost: ghost, UserID: MakeUserID(e164), UserInfo: buildUserInfo(e164)}`; if `createChat` add `Chat: &bridgev2.CreateChatResponse{PortalKey: MakePortalKey(e164, login.ID), PortalInfo: buildChatInfo(login.ID, meta.DisplayNumber, e164)}`. |

`stubClient` (`stub_client.go`): `Connect` sends `{StateEvent: status.StateBadCredentials, Error: "sip-no-credentials", Message: "This login has no SIP credentials; log in again", UserAction: status.UserActionRelogin}`;
`IsLoggedIn` false; `Disconnect`/`LogoutRemote` no-op; `IsThisUser` false; `GetChatInfo`/`GetUserInfo`
return `bridgev2.ErrNotLoggedIn`; `GetCapabilities` returns the same features var; `HandleMatrixMessage`
returns `bridgev2.ErrNotLoggedIn`.

Logging rules (all of `pkg/connector` and `pkg/sipua`): never log `Password`, the raw login `input`
map, SIP message bodies or headers; the SIP username only via `MaskUsername`; phone numbers are fine.

### 7.14 `cmd/mrtc-sip-bridge/main.go` — T4

```go
package main

import (
	"github.com/williamkray/mrtc-sip-bridge/pkg/connector"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

// Filled at build time with -X (see Dockerfile / build-args.sh).
var (
	Tag           = "unknown"
	Commit        = "unknown"
	BuildTime     = "unknown"
	RepositoryURL = "unknown" // unused until telemetry (Phase 5); kept for ldflags parity with mautrix-jabber
)

func main() {
	c := &connector.SIPConnector{}
	br := &mxmain.BridgeMain{
		Name:        "mrtc-sip-bridge",
		Description: "A Matrix-SIP calling bridge using MatrixRTC (Element Call / LiveKit).",
		URL:         "https://github.com/williamkray/mrtc-sip-bridge",
		Version:     "0.1.0",
		Connector:   c,
	}
	br.PostInit = func() {
		if br.Config.Encryption.Default {
			br.Log.Warn().Msg("encryption.default is true, but calls need unencrypted rooms; overriding to false")
			br.Config.Encryption.Default = false
		}
		c.RegisterMatrixRTCHandlers(br.Matrix.EventProcessor)
	}
	br.InitVersion(Tag, Commit, BuildTime)
	br.Run()
}
```

`*appservice.EventProcessor` satisfies `core.EventRegistrar` (D5); if the compiler disagrees, report it
rather than adding an adapter.

---

## 8. Build and packaging — T5

Model: `../mautrix-jabber/{Dockerfile,build.sh,build-args.sh,entrypoint.sh,.dockerignore}`.

`Dockerfile` (exact):

```dockerfile
# Build stage (CGO required: sqlite3; libopus headers installed now for Phase 2 media)
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates gcc g++ musl-dev sqlite-dev opus-dev opusfile-dev pkgconf

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Build metadata (.git is excluded from the build context, so pass it in; see build.sh).
ARG GIT_COMMIT=unknown
ARG GIT_TAG=unknown
ARG GIT_REPOSITORY_URL=unknown
ARG BUILD_TIME=unknown
# nocrypto: portals must be unencrypted (ADR-002), so no libolm / end-to-bridge encryption.
RUN CGO_ENABLED=1 go build -tags nocrypto -ldflags "-s -w \
	-X main.Commit=${GIT_COMMIT} -X main.Tag=${GIT_TAG} \
	-X main.RepositoryURL=${GIT_REPOSITORY_URL} -X main.BuildTime=${BUILD_TIME}" \
	-o mrtc-sip-bridge ./cmd/mrtc-sip-bridge

# Runtime stage
FROM alpine:3.22

ARG GIT_COMMIT=unknown
ARG GIT_REPOSITORY_URL=unknown
LABEL org.opencontainers.image.revision=${GIT_COMMIT} \
	org.opencontainers.image.source=${GIT_REPOSITORY_URL}

RUN apk add --no-cache ca-certificates tzdata sqlite-libs opus opusfile

# Run as non-root
RUN adduser -D -h /app -s /sbin/nologin bridge
WORKDIR /app

COPY --from=builder /build/mrtc-sip-bridge .
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

USER bridge

# Appservice port
EXPOSE 29319
# SIP signalling, one port per active login (network.sip_port_min..sip_port_max)
EXPOSE 5060-5069/udp
EXPOSE 5060-5069/tcp
# RTP media (network.rtp_port_min..rtp_port_max)
EXPOSE 40000-40100/udp

ENTRYPOINT ["/app/entrypoint.sh"]
```

`build.sh`: same as mautrix-jabber with `mautrix-jabber` → `mrtc-sip-bridge` (image tag
`mrtc-sip-bridge`, binary extracted from `/app/mrtc-sip-bridge`, example tag comment
`williamkray/mrtc-sip-bridge:v0.1.0`). `build-args.sh`: copy verbatim, only change the example tag in
the comment. `entrypoint.sh`: copy verbatim with the binary path `/app/mrtc-sip-bridge`. All three
`chmod +x`.

`.dockerignore`:

```
.git
.gitignore
*.md
docs
spikes
config.yaml
registration.yaml
*.db
*.db-shm
*.db-wal
/mrtc-sip-bridge
vendor
logs
```

`.gitignore`: append `/mrtc-sip-bridge`, `config.yaml`, `registration.yaml`, `*.db`, `*.db-shm`,
`*.db-wal` (keep existing lines).

---

## 9. Import-boundary test (`internal/importcheck`) — T1

`doc.go`: `// Package importcheck holds the test that enforces ADR-006's import boundary.` + `package importcheck`.

`importcheck_test.go`, standard library only (`go/parser`, `go/token`, `io/fs`, `path/filepath`,
`strings`, `testing`, `strconv`):

```go
const module = "github.com/williamkray/mrtc-sip-bridge"

var forbiddenPrefixes = []string{
	module + "/pkg/connector",
	module + "/cmd",
	"maunium.net/go/mautrix/bridgev2",
	"maunium.net/go/mautrix/appservice",
	"go.mau.fi/util/configupgrade",
}

// Any import starting with "maunium.net/go/mautrix" must be exactly one of these.
var allowedMautrix = map[string]bool{
	"maunium.net/go/mautrix/id":    true,
	"maunium.net/go/mautrix/event": true,
}

type violation struct{ File, Import string }

// checkDir walks root recursively, skipping any directory named "connector" directly under root and
// testdata dirs; parses every non-_test .go file with parser.ImportsOnly; returns violations.
func checkDir(root string) ([]violation, error)
```

Tests:

1. `TestCorePackagesImportBoundary`: `checkDir("../../pkg")`; fail listing every violation. Also fail
   if fewer than 4 core package directories were scanned (guards against a wrong path silently
   passing).
2. `TestCheckDirDetectsViolations`: write into `t.TempDir()` a fake core file importing
   `maunium.net/go/mautrix/bridgev2`, one importing `maunium.net/go/mautrix` (root, not allowed), one
   importing `github.com/williamkray/mrtc-sip-bridge/pkg/connector`, one clean file importing
   `maunium.net/go/mautrix/event`, a `connector/x.go` that imports bridgev2 (must be skipped), and an
   `x_test.go` that imports bridgev2 (must be skipped). Assert exactly the 3 expected violations.

---

## 10. Test list

All tests are table-driven where a table is natural, use only the standard library `testing` package
(no testify), and run without network access.

| File | Tests |
|---|---|
| `pkg/phone/phone_test.go` | `TestNormalize` table below; `TestIsE164`; `TestDigitsFromDigitsRoundTrip` |
| `pkg/matrixrtc/events_test.go` | `TestKindOf` (each of the 6 types → expected kind; `CallMember` name with MessageEventType class → Unknown; `m.room.message` → Unknown); `TestFromMatrixEvent` (state event with VeryRaw: all fields copied, Timestamp from ms; message event with only Raw: Content is marshalled Raw; nil content → `{}`; unknown type → ok=false; nil → ok=false); `TestIngestEventTypesOrder` |
| `pkg/core/ingest_test.go` | fake sink records calls. Known event forwarded with right login and kind; unknown type not forwarded (returns false); empty login not forwarded; nil event; `SetSink(nil)` falls back to LogSink without panic; `LogSink` with a zerolog ctx logger writing to a buffer: output contains kind and room, does **not** contain a secret string placed in the content |
| `pkg/core/hook_test.go` | fake registrar records `(type, handler)`. Registered types equal `IngestEventTypes()`; handler with resolve→false does not reach sink; resolve→true reaches sink with the resolved login |
| `internal/importcheck/importcheck_test.go` | §9 |
| `pkg/sipua/config_test.go` | `New` validation table: each required field missing; bad registrar (`"host"`, `"host:0"`, `":5060"`); bad transport; bind IP `0.0.0.0` rejected; external IP `"nope"`; refresh ≥ expiry; refresh < 5s; valid config passes with defaults applied (Expiry 60s, Refresh 25s, ExternalIP = BindIP). `TestParseTransport`: `""`→udp, `"UDP"`→udp, `" tcp "`→tcp, `"tls"`→error mentioning TLS, `"sctp"`→error |
| `pkg/sipua/backoff_test.go` | `backoffDelay` for attempts 0..8 = 2,2,4,8,16,32,60,60,60 s; `jitter(10s, 0)` = 8s, `jitter(10s, 0.5)` = 10s, `jitter(10s, 0.999)` < 12s |
| `pkg/sipua/pin_test.go` | fake resolver with scripted responses and call counter. IP literal registrar: no DNS call, current `"1.2.3.4:5008"`. Two IPs: current is first; `advance` → second; `advance` → first (wrap). Re-resolve returns `[C, A]` while pinned to A: resolve keeps A (idx 1). Re-resolve returns `[C]` while pinned to A: idx 0 → C. Resolver error in `advance`: rotation still happens within the old list, error returned. Empty DNS result → error. IPv4 only (`network` argument is `"ip4"`) |
| `pkg/sipua/sipmsg_test.go` | `buildTargets`: no domain → Request-URI `sip:reg.example.com:5008`, AOR host `reg.example.com`; with domain `example.org` → Request-URI `sip:example.org`, AOR host `example.org`, DestPort 5008; tcp → Request-URI has `transport=tcp` param. `ensureAORTo` on `sip.NewRequest(sip.REGISTER, uri)` (uri without user): To user = username, host = AOR host, port 0; second call does not add a second To; existing To is left untouched. `prepareUnregister` on a request with `Contact: <sip:user@203.0.113.5:40123>` and `Expires: 60`: Contact string unchanged and not `*`, exactly one Expires header with value `0`; request with two Expires headers ends with one |
| `pkg/sipua/loop_test.go` | fake `regTxn` (scripted results per call, records method names and destinations), fake resolver, `sleep` recording durations and cancelling ctx after N sleeps, `rand` returning 0.5. Cases: (a) success → callbacks `[Registering, Registered]`, second attempt uses `Refresh`, sleep = RefreshInterval; (b) 401 → `[Registering, AuthFailed]`, sleep 5m, next attempt `Register`; (c) three transient errors on a 2-IP registrar → sleeps 2s,4s,8s; destinations `ip1, ip1, ip2`; only one `TransientError` callback; (d) transient then success → next attempt after the failure is `Register`; callbacks `[Registering, TransientError, Registered]`; a following failure sleeps 2s (attempt reset); (e) success then refresh failure → following attempt is `Register`; (f) DNS failure at start → `TransientError` with Reason containing `resolve`; (g) `shutdown` calls `Unregister` once when last state is Registered and never otherwise. `TestClassify` table: nil, 401, 403, 404, 407 (auth), 408, 480, 500, 503 (transient), `context.DeadlineExceeded`, `errors.New("x")` (transient, status 0), `fmt.Errorf("wrap: %w", &ResponseError{StatusCode: 401})` (auth) |
| `pkg/sipua/mask_test.go` | `"12345678"`→`"12***78"`, `"abcd"`→`"***"`, `""`→`"***"` |
| `pkg/connector/config_test.go` | (1) `yaml.Unmarshal(ExampleConfig)` into `Config`, `Validate()` passes, every field equals the documented default. (2) Zero `Config` + `ApplyDefaults` equals the defaults (strings except country code stay empty) and validates. (3) `Validate` error table: one row per rule in §7.2, asserting the error mentions the key. (4) Upgrade preserves user values: `configupgrade.DoBytes([]byte(userYAML), &configupgrade.StructUpgrader{SimpleUpgrader: configupgrade.SimpleUpgrader(upgrader.DoUpgrade), Base: ExampleConfig})` where `userYAML` sets `external_ip: 203.0.113.9`, `rtp_port_min: 41000`, `default_country_code: "44"` and omits the rest; unmarshal the output: the three user values survive, omitted keys have example defaults |
| `pkg/connector/ids_test.go` | `MakeUserID("+15551234567") == "15551234567"`; `ParseUserID` round trip; `ParseUserID("abc")` not ok; `MakePortalKey` fields; `MakeLoginID("+15551234567", "Reg.Example.com") == "+15551234567@reg.example.com"` |
| `pkg/connector/chatinfo_test.go` | DM type; two members (owner IsFromMe with SenderLogin, ghost with UserInfo name = E.164); `OtherUserID` = ghost ID; power-level overrides contain both call.member types at 0; `buildUserInfo` identifiers `tel:+…` |
| `pkg/connector/loginparse_test.go` | `normalizeRegistrar`: `"Reg.Example.com"`→`"reg.example.com:5060"`, `"sip:reg.example.com:5008"`→`"reg.example.com:5008"`, `"1.2.3.4:5008"` ok, `"[::1]:5060"` error, `"user@host"` error, `"host:99999"` error, `""` error. `parseLoginInput`: full valid input → metadata (transport defaulted to udp when missing, number normalized with cc "1"); password with leading/trailing spaces preserved exactly; empty username/password errors; `transport: "tls"` error; bad number error; error strings never contain the password value |
| `pkg/connector/portpool_test.go` | acquire returns min, then min+1; release then acquire reuses the lowest; exhausting returns `ErrNoFreePort`; releasing an unknown port is a no-op |
| `pkg/connector/client_state_test.go` | `bridgeStateFor` table per §7.13 (each state; transient with and without status code; Idle/Stopped → ok=false) |

`TestNormalize` table (`pkg/phone`), `cc` = default country code:

| input | cc | want |
|---|---|---|
| `+15551234567` | 1 | `+15551234567` |
| ` +1 (555) 123-4567 ` | 1 | `+15551234567` |
| `5551234567` | 1 | `+15551234567` |
| `15551234567` | 1 | `+15551234567` |
| `555.123.4567` | 1 | `+15551234567` |
| `1-555-123-4567` | 1 | `+15551234567` |
| `tel:+15551234567` | 1 | `+15551234567` |
| `011 44 20 7946 0958` | 1 | `+442079460958` |
| `0044 20 7946 0958` | 1 | `+442079460958` |
| `+44 20 7946 0958` | 1 | `+442079460958` |
| `020 7946 0958` | 44 | `+442079460958` |
| `2079460958` | 44 | `+442079460958` |
| `+15551234567` | 44 | `+15551234567` |
| `611` | 1 | ErrShortCode |
| `123456` | 1 | ErrShortCode |
| `1234567` | 1 | ErrInvalid (NANP needs 10/11 digits) |
| `555123456` | 1 | ErrInvalid |
| `25551234567` | 1 | ErrInvalid (11 digits not starting with 1) |
| `5551234567` | (empty) | ErrInvalid |
| `` (empty) | 1 | ErrInvalid |
| `+` | 1 | ErrInvalid |
| `+0123456789` | 1 | ErrInvalid |
| `+1234567890123456` | 1 | ErrInvalid (16 digits) |
| `555-CALL-NOW` | 1 | ErrInvalid |
| `1+5551234567` | 1 | ErrInvalid |
| `anonymous` | 1 | ErrInvalid |

---

## 11. Work split

Dependency order:

```
T1 (module + core + matrixrtc + phone + importcheck)
 ├── T2 (pkg/sipua)            ┐
 ├── T3 (connector, no sipua)  ├─ parallel
 └── T5 (build files + docs)   ┘
T4 (connector client/login/start + cmd + go mod tidy)   after T2 and T3
Final: coordinator integration check + Opus review       after T4 and T5
```

Recommended tiers: T1 Sonnet, T2 Sonnet, T3 Sonnet, T4 Sonnet, T5 Haiku (files are near-copies) or
Sonnet for the README/ADR prose.

Rules for every task: edit **only** the files you own (§2). Do not run `go mod tidy` (except T4). Do not
commit. Do not touch `spikes/`. Build/vet/test with `-tags nocrypto`. No new dependencies beyond §3.
Every exported identifier gets a doc comment. No emojis in code or docs.

### T1: module, core contract, MatrixRTC types, phone, import check

Files: `go.mod`, `go.sum`, `internal/importcheck/*`, `pkg/core/*`, `pkg/matrixrtc/*`, `pkg/phone/*`.
Spec: §3, §4, §5, §9, tests in §10 for these packages.
Acceptance:

- `go build -tags nocrypto ./...`, `go vet -tags nocrypto ./...`, `go test -tags nocrypto ./...` pass.
- `go list -m maunium.net/go/mautrix github.com/emiago/diago github.com/emiago/sipgo go.mau.fi/util github.com/rs/zerolog`
  prints exactly v0.32.0, v0.41.0, v1.6.0, v0.11.0, v1.35.1.
- `TestCorePackagesImportBoundary` scans at least `core`, `matrixrtc`, `phone` (it requires ≥ 4 dirs, so
  create `pkg/sipua/doc.go` containing only `// Package sipua ...` + `package sipua` as a placeholder;
  T2 owns and may overwrite it).
- `grep -rn "org.matrix.msc" pkg/ --include=*.go | grep -v _test.go` matches only `pkg/matrixrtc/events.go`.

### T2: SIP UA

Files: `pkg/sipua/*` (including overwriting `doc.go`).
Spec: §6, sipua rows of §10. Reference code: `spikes/s2-sip/main.go` (`registerLoop`, `run`) and
`spikes/s2-sip/stun.go`; diago sources at `~/go/pkg/mod/github.com/emiago/diago@v0.41.0`.
Acceptance:

- build/vet/test pass (`-tags nocrypto`); sipua tests need no network (`go test -tags nocrypto ./pkg/sipua/ -count=1` works offline).
- `go doc ./pkg/sipua` shows no diago or sipgo types in exported API.
- `grep -n "SIPDebug" pkg/sipua/*.go` finds nothing. `grep -n "Contact\", \"\\*\"" pkg/sipua/*.go` finds nothing.
- The exported API matches §6.1 exactly (T4 depends on it).

### T3: connector building blocks

Files: `pkg/connector/{connector.go,config.go,example-config.yaml,dbmeta.go,ids.go,chatinfo.go,loginparse.go,portpool.go,ghostintent.go,callhost.go,rtchook.go}` and their tests.
Spec: §7.1–§7.11, connector rows of §10. `loginparse.go` calls `sipua.ParseTransport`: code against
the §6.1 signature; if T2 has not landed yet, the package will not compile until it does. In that case
finish everything else, and run the acceptance commands once `pkg/sipua` exists (coordinate through the
caller; do not stub sipua yourself).
Acceptance:

- build/vet/test pass (`-tags nocrypto`).
- No `var _ bridgev2.NetworkConnector` assertion in T3 files.
- `go vet` clean; `ExampleConfig` round-trip test passes; upgrade-preservation test passes.

### T4: connector client, login, start, entry point

Files: `pkg/connector/{start.go,login.go,client.go,client_state.go,client_state_test.go,stub_client.go}`,
`cmd/mrtc-sip-bridge/main.go`, final `go.mod`/`go.sum` tidy.
Spec: §7.12–§7.14.
Acceptance:

- `go mod tidy` run; pinned versions unchanged (same `go list -m` check as T1).
- build/vet/test pass (`-tags nocrypto`).
- `rm -f /tmp/mrtc-example.yaml && go build -tags nocrypto -o /tmp/mrtc-sip-bridge ./cmd/mrtc-sip-bridge && /tmp/mrtc-sip-bridge -e -c /tmp/mrtc-example.yaml`
  succeeds, and the generated file has a `network:` section containing every key of §7.2 and
  `command_prefix: '!sip'`, `username_template: sip_{{.}}`, `port: 29319`.
- `grep -rn "Password" pkg/connector/*.go | grep -i "log\.\|Str(\|Msg"` finds nothing.

### T5: packaging and docs

Files: `Dockerfile`, `.dockerignore`, `build.sh`, `build-args.sh`, `entrypoint.sh`, `.gitignore`,
`README.md`, `CLAUDE.md` (only the **Status** section), `docs/CREDITS.md`, `docs/DECISIONS.md`.
Spec: §8 plus:

- **README**: replace "Status: planning" with Phase 1 status (registers and keeps a SIP account
  registered, reports bridge state, creates per-number rooms via `!sip start-chat <number>`; incoming
  calls are rejected with 480 until Phase 2). Add sections: Requirements (unchanged list plus "rooms
  must be unencrypted; the bridge forces `encryption.default: false`"); Build (`./build.sh`, or local
  `go build -tags nocrypto ./cmd/mrtc-sip-bridge` with Go 1.26+, cgo and sqlite headers); Configure
  (`-e` to generate config, `-g` for registration, the `network:` keys table from §7.2 with defaults);
  Networking (one SIP port per login, RTP range, recommend Docker `network_mode: host`; otherwise
  publish `5060-5069/udp`, `5060-5069/tcp`, `40000-40100/udp` with identical host and container ports,
  and set `external_ip`); Login (`!sip login`, answer each prompt as its own message; the warning that
  registering competes with an existing ATA/desk phone, JMP does not officially support two
  registrations); Bridge states (CONNECTED / BAD_CREDENTIALS / TRANSIENT_DISCONNECT meanings).
- **CLAUDE.md** Status section: "Phase 1 (skeleton) in progress/landed on `phase1-skeleton`; see
  `docs/PHASE1-SPEC.md` and PLAN.md Current state." Also add one bullet under Conventions:
  "Build, vet and test with `-tags nocrypto` (no libolm; rooms are unencrypted)."
- **CREDITS.md**: in Planned dependencies mark diago as adopted (v0.41.0, linked, no files copied, used
  by `pkg/sipua`); add rows for `go.mau.fi/util` (MPL-2.0, v0.11.0), `github.com/rs/zerolog` (MIT,
  v1.35.1), `gopkg.in/yaml.v3` (MIT and Apache-2.0, v3.0.1, test/config only). Mention that
  `pkg/sipua/net.go`'s STUN helper is ported from our own spike.
- **DECISIONS.md**: append **ADR-009: Adopt diago behind `pkg/sipua`; build without crypto**
  (Status accepted, 2026-10-16). Content: S2 passed with diago; production uses diago v0.41.0 for SIP
  only behind the `sipua.UA` interface so it can be swapped for raw sipgo + media-sdk; three
  workarounds applied on top of diago (AOR `To`, per-attempt `Via` reset, own-Contact unregister via
  `Origin` + `Qualify`) and registrar IP pinning; known limitation: refresh interval is fixed because
  diago does not expose the granted Expires; the bridge builds with `-tags nocrypto` because portals
  must be unencrypted (ADR-002). Would revisit if diago's API churn or media threading becomes a
  problem (fallback per S2 §2).

Acceptance: `bash -n build.sh build-args.sh entrypoint.sh` passes; scripts are executable; no Go files
touched. (The Docker build itself is verified in the final check, after T4.)

### Final integration check (coordinator, after T4 and T5)

1. `go build/vet/test -tags nocrypto ./...` from a clean checkout of the branch.
2. `./build.sh` succeeds; `docker run --rm mrtc-sip-bridge | head` prints an example config.
3. Opus review of the full diff against this spec (contract, import boundary, credential logging,
   sipua state machine).
4. Manual test (user, needs the live environment from Phase 0): run the bridge, `!sip login` with the
   JMP credentials → CONNECTED; bridge state stays CONNECTED for 10 min; `!sip start-chat <mobile>`
   creates an unencrypted DM with ghost `@sip_<digits>`; starting an Element call in that room logs
   `MatrixRTC event ingested kind=call_member`; calling the JMP number gets rejected (caller hears the
   provider's unavailable/voicemail handling) and the log shows `Rejecting inbound call`; stopping the
   bridge sends an `Expires: 0` REGISTER for our Contact only (check the ATA, if any, stays registered).
5. Update `docs/PLAN.md`: tick Phase 1 boxes (note lk-jwt-service discovery deferred to Phase 2; only
   the override key exists) and add a Current state entry.

---

## 12. Open questions for the user (defaults chosen; change only if you disagree)

1. **No end-to-bridge encryption** (`-tags nocrypto`, D15/D16). Calls need unencrypted rooms anyway.
   Default: accepted.
2. **Defaults**: appservice port 29319, command prefix `!sip`, ghost localparts `sip_<digits>`,
   SIP ports 5060–5069, RTP 40000–40100. Default: accepted.
3. **STUN default** `stun.l.google.com:19302` (what S2 used). Set `stun_server: ""` and `external_ip`
   for deployments that should not contact Google. Default: keep.
