# Phase J implementation spec: embedding the call core in mautrix-jabber

> **Status note (publication):** this spec describes the original private build. Since publication,
> mautrix-jabber depends on the public module `github.com/williamkray/mrtc-sip-bridge` (no `replace`),
> builds with the `sipcalls` tag in its default images, and is developed on its normal branches. D2,
> D16 and the `local/sip-calls` / never-push notes below are superseded.

Status: **ready for implementation** (written 2026-10-17).
Audience: implementer agents and the coordinator. Follow it literally. Where this spec and your own taste
differ, this spec wins. If something here does not compile against the pinned versions, stop and report
it rather than redesigning.

Inputs: `docs/PLAN.md` (Phase J, J1-J8 and the "sort out before J3" notes), `docs/DECISIONS.md`
(ADR-002, **ADR-006**, ADR-010 rejected), `docs/TELEMETRY-SPEC.md` ("TS §N", §3 injection, §8.2 Phase J),
`docs/PHASE2-SPEC.md` / `docs/PHASE3-SPEC.md` ("P2/P3 §N", host wiring, `CallHost` growth), README
(Calls, Outgoing calls, known limits), the standalone host `pkg/connector/*` and `cmd/mrtc-sip-bridge`
(reference host), and in `../mautrix-jabber` (branch `local/sip-calls`, at `e812e20`): `CLAUDE.md`,
`cmd/mautrix-jabber/main.go`, `pkg/connector/{connector,xmpp_client,xmpp_inbound,xmpp_jingle,cheogram,
metadata,dbmeta,id,telemetry}.go`, `pkg/telemetry/*`, `Dockerfile`, `build.sh`, `build-args.sh`, `go.mod`.

Library facts were checked against `~/go/pkg/mod/maunium.net/go/mautrix@v0.32.0` (`bridgev2/bridge.go:119-230,
379-420` start order; `bridgev2/portal.go:5460-5470` default power levels; `bridgev2/commands/{handler,
processor,login,event}.go` command state and `ce.Redact`; `bridgev2/matrixinterface.go:62,204,220`
`GetPowerLevels`/`SendState`/`EnsureJoined`; `bridgev2/bridgeconfig/config.go:24` `Network yaml.Node`)
and `go.mau.fi/util@v0.11.0/configupgrade` (`Helper.Copy(type, path...)`).

---

## 0. Scope

In scope:

1. **mautrix-jabber embedded host** (build tag `sipcalls`, runtime flag `network.calls.enabled`): a JMP
   number's Cheogram texts and its SIP calls share **one Matrix room** and **one ghost**
   (`+1NXXNXXXXXX@cheogram.com`). Inbound calls ring in the caller's Cheogram portal (created on demand);
   outbound calls start from an existing Cheogram 1:1 portal (press call, or `call` with no number) or
   from `call <number>`.
2. **Per-login SIP credentials** in jabber's login metadata, set with new bot commands `sip-login`,
   `sip-logout`, `sip-status` (validation and "register before saving" exactly like the standalone login).
3. **Shared host code exported from this repo** so both hosts use the same SIP account runtime, config,
   commands and Matrix glue: new packages `pkg/sipaccount` (core-class) and `pkg/hostkit` (host-class);
   the standalone host is refactored onto them with no behaviour change.
4. **Telemetry convergence (J8)**: jabber (sipcalls build) initialises the SDK through this repo's
   `pkg/telemetry` with jabber's service identity (traces **and** metrics), and jabber's span/log helpers
   share `pkg/obs`'s log annotation so trace fields are never written twice.
5. **JMI dedupe (J5)**, **build and packaging (J6)** (private module via a local `replace` and a Docker
   named build context; libopus next to libolm), **docs (J7)**, and a **new dev service** on the dev Docker host
   (`jabbercalls`) for the live test.

Out of scope (do **not** implement): any change to the public `main` branch of mautrix-jabber; pushing
`local/sip-calls`; Jingle/OMEMO; calls in group MMS or MUC portals; multiple SIP accounts per jabber login;
concurrent calls; Phase 4 robustness items; workarounds for client bugs (Element X outbound pickup,
Sable ring dismissal; README only); forcing jabber portals unencrypted; changing the standalone host's
behaviour, config keys or command texts.

---

## 1. Decisions (and why)

| # | Decision | Why |
|---|---|---|
| D1 | **All SIP code in mautrix-jabber lives behind `//go:build sipcalls`** in files named `*sipcalls*.go`. Untagged code gets only small, SIP-free **seams** (an optional `connectorExtension` interface, a generic `Extra` metadata map, one telemetry backend split, one chat-info decoration hook, one JMI suppression hook). The untagged build on `local/sip-calls` must behave exactly like `main`. | The public repo must never contain SIP content, and `local/sip-calls` is never merged or pushed. Keeping the integration in tagged files makes it a small, rebaseable patch series on top of public `main` (rebase when `main` moves) and makes leaks easy to audit (`git diff main -- ':!*sipcalls*'` shows only seams). |
| D2 | **Private module via `replace github.com/williamkray/mrtc-sip-bridge => ../mrtc-sip-bridge`** in jabber's `go.mod` on the local branch (require `v0.0.0-00010101000000-000000000000`). Docker builds use a **BuildKit named context** (`--build-context mrtc=../mrtc-sip-bridge`) and a separate `Dockerfile.sipcalls` that copies only `go.mod`, `go.sum` and `pkg/` of the private repo. No `go mod vendor`, no tokens in builds, no git tag needed (J1's tag becomes optional). | `vendor/` would put private source inside the public repo's working tree (one `git add -A` from a leak). A token-based fetch puts secrets into the build. A local replace plus a named context is reproducible, needs nothing outside the two checkouts, and the copied subset excludes `deploy-dev/` (which holds secrets and is not in `.dockerignore` for named contexts). |
| D3 | **Extract a shared SIP account runtime into `pkg/sipaccount`** (process-wide `Runtime`: bind/external IP, STUN, RTP range, SIP port pool, registrations gauge; per-login `Account`: UA start with port retry, stop with detached unregister, state, first-result wait, `Dial`; `Credentials` + parsing) and **shared bridgev2 glue into `pkg/hostkit`** (config struct and example YAML, ghost intent adapter, encryption check, owner read receipt, call power levels, call-stack construction, call commands, `WithSpan`, field prompts, RTC login resolver). The standalone `pkg/connector` is refactored onto both. | Duplicating `client.go`/`login.go`/`start.go`/`commands.go` into jabber would fork ~1000 lines of live-tested, race-sensitive code. `sipaccount` has no bridgev2 import so it stays a core package (importcheck); `hostkit` needs bridgev2 types and is host-class like `pkg/connector`. |
| D4 | **SIP UA lifecycle is tied to the jabber *login*, not to the XMPP session**: start when the login is loaded (and the runtime is up, whichever comes last) if `calls.enabled` and credentials exist; stop in `xmppClient.Disconnect` (which jabber documents as terminal: bridge stop, client replacement, logout); XMPP reconnects never touch SIP. One `Account` per login ID; a replacement client of the same login **takes over** the running account instead of starting a second UA. | XMPP sessions flap; calls must not. Two UAs for one account would register two Contacts. Jabber's `Disconnect` is already the terminal hook (`xmpp_client.go:370-401`). |
| D5 | **SIP state does not drive jabber's bridge state.** It is reported by `sip-status`, logs, the `mrtcsip.sip.registrations` gauge and **one** management-room notice when the account enters `auth_failed`. | Jabber's `BridgeState` describes the XMPP connection; overloading it would make an XMPP-healthy login look broken (and vice versa). |
| D6 | **`LoginRef` = jabber `UserLoginID`** (lower-case bare JID). The core's ingest span attribute `call.local_e164` must no longer be derived from the LoginRef: `core.Ingest` gains `SetLocalNumber(func(LoginRef) string)`; jabber passes the SIP account's `DisplayNumber`. | `ingest.go:103` cuts the LoginRef at `@`, which in jabber would yield the XMPP localpart under a phone-number attribute. |
| D7 | **Room mapping**: E.164 → `<e164>@cheogram.com` → portal key `{ID: jid, Receiver: loginID}`, i.e. exactly the portal jabber's `pm`/`start-chat`/inbound texts use. Missing room → created with the same `CreateChatResponse` jabber's `ResolveIdentifier` builds (extracted into an untagged helper, §6.3) **minus** the synchronous 10 s MAM backfill. Ghost = the Cheogram ghost (`networkid.UserID(jid)`), joined with `EnsureJoined`. `LookupCallRoom` accepts only portals of that login whose ID is `<E.164>@cheogram.com` (no comma, no `;phone-context`, not `RoomTypeGroupDM`): **group MMS and MUC portals never get calls**. | One room and one ghost per number is the whole point (ADR-006). Blocking the 180 Ringing on a MAM query would delay ringing by up to 10 s. Group MMS has no single remote party. |
| D8 | **Call power levels**: jabber portals use bridgev2's default power levels (`state_default` 50, owner 0), so the owner cannot publish `call.member`. The sipcalls build (a) decorates Cheogram 1:1 `ChatInfo` with the `call.member` PL-0 overrides (new rooms and every resync), (b) runs a one-off **sweep** of the login's Cheogram 1:1 portals when its SIP account starts, and (c) re-checks in `ResolveCallRoom`. | Without (a)/(b) pressing call in an existing text room fails in the client before the bridge sees anything. Same overrides as the standalone `buildChatInfo`. |
| D9 | **JMI dedupe**: while a login has an attached SIP account and calls are enabled, jabber drops XEP-0353 actions whose (non-self) sender is a Cheogram 1:1 E.164 JID; nothing else changes. | JMI sids cannot be correlated with SIP Call-IDs, and Cheogram JMI for the account's number describe the same PSTN call the SIP path rings (ADR-001/006). Narrower than "all JMI" so real XMPP callers still get notices. |
| D10 | **Encryption**: jabber may create encrypted portals (`encryption.default: true`). The embedded host does **not** override the setting (texts may want E2BE). It logs one startup warning when calls are enabled and `encryption.default` is true; `sip-status` reports it; the core already refuses to ring in encrypted rooms with a notice (P2). The dev service runs with encryption off. | Silent calls are worse than refused calls (ADR-002), but forcing the text bridge's encryption policy from the call feature would surprise users. |
| D11 | **Element X outbound pickup bug** applies unchanged (jabber's bot is in every portal). **Document only** (mrtc README embedded section); no bot-leave workaround (ADR-010 rejected). | User preference: no workarounds for upstream client bugs. |
| D12 | **Telemetry (J8)**: in the sipcalls build, jabber's `pkg/telemetry.Init` delegates to this repo's `pkg/telemetry.Init` with a new `Options.ServiceName` (`"mautrix-jabber"`), which installs both providers and calls `obs.Use`. Jabber keeps its own tracer scope (`github.com/mautrix/jabber`) for its spans, and its log-annotation primitives delegate to `pkg/obs` (new export `obs.AnnotateLogger`). The untagged build keeps today's tracing-only backend byte for byte. | Porting T6b's metrics into jabber a second time would duplicate env resolution, exporters and temporality handling. One shared annotation key removes the double-annotation hazard of TS §3.2 without making public jabber code import the private module. |
| D13 | **Commands** (registered with jabber's prefix, e.g. `!jc`): `sip-login` (prompts each field, redacts the password message, registers before saving), `sip-logout`, `sip-status`, and the standalone's `call`, `hangup`, `dtmf` from `hostkit` (same texts, same `WithSpan`, DTMF redaction). | One implementation of the call commands; the login prompt reuses bridgev2's user-input UX (each answer its own message, password redacted). |
| D14 | **Config**: `network.calls` = `enabled` (default false) + exactly the standalone `network:` keys (same names, defaults and validation), validated at `Start` with key prefix `network.calls.`. `calls.enabled: true` **requires** `network.cheogram: true` (startup error otherwise). | Mirrors the standalone so one doc serves both. Without Cheogram mapping there is no room for a number. Fail fast like the standalone's `ValidateConfig`. |
| D15 | **Never two registrations of one SIP account from our bridges**: the live test stops (or logs out) the standalone first and restores it afterwards; docs warn. | ADR-006 constraint; JMP forks calls to all Contacts. |
| D16 | **Images with the sipcalls build are not pushed to the public Docker Hub repo** (`williamkray/mautrix-jabber`). Deployment is `docker save | ssh | docker load`. If the user wants a registry, it must be private. | The binary contains the private code; a public image would publish it. (The user said the image *may* be published: this spec recommends against the public repo and lists it as a user decision, §15.) |

---

## 2. Package layout and file ownership

Tasks are defined in §14. Files marked **(API)** are written as exact contracts first; later tasks must
not change their exported surface (stop and report instead).

### 2.1 mrtc-sip-bridge (private repo, branch `phasej-embedding`)

```
pkg/sipaccount/doc.go                      T1  package doc (core-class: no bridgev2, no host imports)
pkg/sipaccount/credentials.go     (API)    T1  Credentials, field IDs, NormalizeRegistrar, ParseInput
pkg/sipaccount/runtime.go         (API)    T1  NetConfig, Runtime, NewRuntime, Close, gauge
pkg/sipaccount/account.go         (API)    T1  AccountConfig, Account, Start/Stop/State/WaitFirstResult/Dial
pkg/sipaccount/register.go        (API)    T1  Register, Outcome
pkg/sipaccount/portpool.go                 T1  moved from pkg/connector/portpool.go (unexported)
pkg/sipaccount/*_test.go                   T1  moved loginparse/portpool/client_lifecycle tests + new
pkg/hostkit/doc.go                         T1  package doc (host-class: may import bridgev2)
pkg/hostkit/config.go             (API)    T1  Config (moved from connector), ApplyDefaults, Validate(prefix)
pkg/hostkit/example-config.yaml            T1  moved from pkg/connector (unchanged text)
pkg/hostkit/upgrade.go            (API)    T1  ExampleYAML, CopyKeys
pkg/hostkit/ghostintent.go        (API)    T1  NewGhostIntent (moved)
pkg/hostkit/room.go               (API)    T1  RoomEncrypted, MarkReadAsOwner, CallPowerLevelOverrides,
                                               EnsureCallPowerLevels, WithCallOverrides
pkg/hostkit/stack.go              (API)    T1  CallStackDeps, NewCallStack (body of connector.startCalls)
pkg/hostkit/commands.go           (API)    T1  CallAPI, CommandDeps, RegisterCallCommands, reply mappers
pkg/hostkit/span.go               (API)    T1  WithSpan (moved)
pkg/hostkit/prompt.go             (API)    T1  LoginFields, PromptFields
pkg/hostkit/rtchook.go            (API)    T1  ResolveRTCLogin
pkg/hostkit/*_test.go                      T1  moved commands/config/telemetry(WithSpan) tests + new
pkg/core/ingest.go                         T1  Ingest.SetLocalNumber
pkg/core/ingest_obs_test.go                T1  (extend)
pkg/obs/span.go, pkg/obs/obs_test.go       T1  AnnotateLogger export + test
pkg/telemetry/telemetry.go, *_test.go      T1  Options.ServiceName
internal/importcheck/importcheck_test.go   T1  hostkit host-class, sipaccount core-class
pkg/connector/{start,client,login,loginparse,callhost,ghostintent,rtchook,commands,config,
  dbmeta,telemetry,portpool,connector}.go  T1  refactor onto sipaccount/hostkit (no behaviour change)
pkg/connector/*_test.go                    T1  keep passing (tests that moved are deleted here)
cmd/mrtc-sip-bridge/main.go                T1  hostkit.WithSpan / RegisterCallCommands wiring
docs/DECISIONS.md                          T1  append ADR-011 (text in §13.1)
docs/TELEMETRY-SPEC.md                     T1  edits listed in §13.2
docs/EMBEDDING.md                          T5  J1: host contract, lifecycle, event feed, build
README.md, CLAUDE.md, docs/PLAN.md         T5  embedded-mode section, status, J boxes
deploy-dev/jabbercalls/ (gitignored)       T5  config template + compose snippet copies (never committed)
```

### 2.2 mautrix-jabber (public repo, local branch `local/sip-calls`, never pushed)

Untagged files (seams only, no SIP words; T2):

```
pkg/connector/extension.go                 T2  connectorExtension interface, registerExtension, c.extension()
pkg/connector/connector.go                 T2  seam calls in GetConfig/Init/Start/LoadUserLogin
pkg/connector/xmpp_client.go               T2  Disconnect seam; extract dmCreateChatResponse; decorate seam
pkg/connector/xmpp_jingle.go               T2  suppression seam in handleJingleMessageInitiation
pkg/connector/telemetry.go                 T2  Stop seam (before telemetry.Shutdown)
pkg/connector/metadata.go                  T2  JabberLoginMeta.Extra
cmd/mautrix-jabber/main.go                 T2  jc.PostInitExtensions(...), ExtensionStartAttrs, buildVariant
cmd/mautrix-jabber/variant.go              T2  //go:build !sipcalls  const buildVariant = ""
pkg/telemetry/telemetry.go, span.go        T2  move backend parts out (no behaviour change)
pkg/telemetry/backend_otel.go              T2  //go:build !sipcalls  (moved code: Init/Shutdown/annotation)
pkg/telemetry/backend_otel_test.go         T2  //go:build !sipcalls  (moved tests)
go.mod, go.sum                             T2  require + replace (local path)
.git/hooks/pre-push (not tracked)          T2  push guard (§9.4)
```

Tagged files (`//go:build sipcalls`):

```
cmd/mautrix-jabber/variant_sipcalls.go     T2  const buildVariant = "+sipcalls"
pkg/telemetry/backend_sipcalls.go          T2  delegate Init/Shutdown/annotation to mrtc telemetry/obs
pkg/telemetry/backend_sipcalls_test.go     T2  parenting + annotation tests (§11)
pkg/connector/sipcalls.go          (API)   T3  callsExt (implements connectorExtension), registration
pkg/connector/sipcalls_config.go   (API)   T3  CallsConfig, example YAML, upgrader, decode/validate
pkg/connector/sipcalls_accounts.go         T3  per-login accounts, creds in Extra, invite/dial/state
pkg/connector/sipcalls_host.go             T3  jabberCallHost (core.CallHost), cheogramE164, PL sweep
pkg/connector/sipcalls_*_test.go           T3  config/host/accounts/JMI tests
pkg/connector/sipcalls_commands.go         T4  sip-login/sip-logout/sip-status + RegisterCallCommands
pkg/connector/sipcalls_commands_test.go    T4
Dockerfile.sipcalls, build-sipcalls.sh     T2  (untagged files, local branch only; mention SIP)
CLAUDE.md                                  T5  short "local/sip-calls" note (local branch only)
```

Acceptance commands (both repos need cgo and `pkg-config opus`; jabber's default build also needs
libolm, so local runs use `nocrypto`):

```bash
# mrtc-sip-bridge
go build -tags "nocrypto nolibopusfile" ./... && go vet -tags "nocrypto nolibopusfile" ./...
go test -race -count=1 -tags "nocrypto nolibopusfile" ./...
# mautrix-jabber, untagged (public behaviour) and tagged
go build -tags nocrypto ./... && go vet -tags nocrypto ./... && go test -race -count=1 -tags nocrypto ./...
go build -tags "nocrypto sipcalls nolibopusfile" ./... && go vet -tags "nocrypto sipcalls nolibopusfile" ./...
go test -race -count=1 -tags "nocrypto sipcalls nolibopusfile" ./...
```

Worktree note: if an implementer uses an isolation worktree of mautrix-jabber, it starts on stale
`develop`; run `git reset --hard local/sip-calls` first. The `replace` path is relative, so a jabber
worktree under `.claude/worktrees/<name>` needs `../../../../mrtc-sip-bridge`: **do not use worktrees for
jabber tasks**; work in the main checkout on `local/sip-calls`.

---

## 3. Shared code in mrtc-sip-bridge (T1)

### 3.1 `pkg/sipaccount` (core-class; imports: `pkg/sipua`, `pkg/obs`, `pkg/phone`, `pkg/core`, zerolog, stdlib)

```go
package sipaccount

// Field IDs of the SIP login form, shared by both hosts.
const (
	FieldUsername      = "username"
	FieldPassword      = "password"
	FieldRegistrar     = "registrar"
	FieldTransport     = "transport"
	FieldDisplayNumber = "display_number"
)

// Credentials is one SIP account. The JSON tags are the storage format of both hosts (identical to the
// standalone UserLoginMetadata, which becomes an alias of this type). Password is stored in plain text
// in the bridge DB, like the XMPP password; never log it.
type Credentials struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Registrar     string `json:"registrar"`        // "host:port", normalized
	Domain        string `json:"domain,omitempty"` // reserved, not settable yet
	Transport     string `json:"transport"`        // "udp" | "tcp"
	DisplayNumber string `json:"display_number"`   // E.164 of the account's own number
}

// Complete reports whether c has everything needed to register.
func (c *Credentials) Complete() bool

// NormalizeRegistrar is the standalone normalizeRegistrar, moved verbatim.
func NormalizeRegistrar(s string) (string, error)

// ParseInput is the standalone parseLoginInput, moved verbatim (errors never contain the password).
func ParseInput(input map[string]string, defaultCC string) (*Credentials, error)

// NetConfig is the process-wide network part of the host config.
type NetConfig struct {
	SIPBindIP              string
	SIPPortMin, SIPPortMax int
	ExternalIP, STUNServer string
	RTPPortMin, RTPPortMax int
	RegisterExpiry         time.Duration
	RegisterRefresh        time.Duration
}

// Runtime is the process-wide SIP state. Exactly one per process: NewRuntime sets package-level state in
// sipua (RTP port range, library logger).
type Runtime struct{ /* unexported */ }

// NewRuntime resolves the bind IP (DefaultBindIP when empty or 0.0.0.0) and the external IP
// (ExternalIP, else STUN with a 5 s timeout, else the bind IP; a STUN failure is a warning), calls
// sipua.SetLibraryLogger(log) and sipua.SetRTPPortRange, creates the SIP port pool and registers the
// mrtcsip.sip.registrations gauge callback. Must be called after obs.Use (Use rebuilds instruments and
// drops earlier callbacks). log is a plain logger (sipua adds component=sipgo; Runtime logs with
// component=sip). Moves the body of the standalone SIPConnector.Start and registerMetrics.
func NewRuntime(ctx context.Context, cfg NetConfig, log zerolog.Logger) (*Runtime, error)

func (rt *Runtime) BindIP() net.IP
func (rt *Runtime) ExternalIP() net.IP
// Close unregisters the gauge callback. Idempotent. Accounts must be stopped first.
func (rt *Runtime) Close()

// AccountConfig configures one SIP account.
type AccountConfig struct {
	Creds Credentials
	// Log is a clean logger WITHOUT a component key (sipua adds its own). Account logs with component=sip.
	Log zerolog.Logger
	// OnState is called for every state change of the current UA (stale UAs are filtered), on the UA
	// goroutine. Must not block. Optional.
	OnState func(sipua.StateEvent)
	// OnInvite receives inbound calls (diago handler goroutine, must return quickly). nil = 480.
	OnInvite sipua.InviteHandler
	// NewUA is a test seam; nil = sipua.New.
	NewUA func(sipua.Config) (sipua.UA, error)
}

// Account is one SIP registration. All methods are safe for concurrent use.
type Account struct{ /* unexported */ }

func (rt *Runtime) NewAccount(cfg AccountConfig) *Account

// Start creates and starts a UA (no-op if one is running): acquires a SIP port, retries the next port
// when Start fails (bind), returns config errors immediately, releases skipped ports at the end. Resets
// the first-result latch. Moves the standalone SIPClient.start (uaGen filtering included).
func (a *Account) Start(ctx context.Context) error
// Stop stops the UA (no-op if none): bumps the generation, unregisters our Contact under a 5 s timeout
// detached from ctx's cancellation (ctx only carries the caller's span), releases the port, logs
// "SIP unregister failed" on error. Moves the UA half of the standalone SIPClient.stop. The host calls
// calls.Manager.EndLogin BEFORE Stop.
func (a *Account) Stop(ctx context.Context)
func (a *Account) Running() bool
// State returns the latest StateEvent of the current UA (zero value when none).
func (a *Account) State() sipua.StateEvent
// WaitFirstResult waits for the first Registered/AuthFailed/TransientError since the latest Start.
func (a *Account) WaitFirstResult(ctx context.Context, timeout time.Duration) (sipua.StateEvent, error)
// Dial places an outbound call; sipua.ErrNotRegistered when no UA is running.
func (a *Account) Dial(req sipua.DialRequest) (sipua.OutboundCall, error)
func (a *Account) Creds() Credentials

// ErrRegisterTimeout is returned by Register when no result arrived in time.
var ErrRegisterTimeout = errors.New("sipaccount: timed out waiting for SIP registration result")

// Register starts a, waits up to timeout for the first result and stops a unless the result is
// StateRegistered. On ctx cancellation it stops a and returns ctx.Err(). Used by both login flows.
func Register(ctx context.Context, a *Account, timeout time.Duration) (sipua.StateEvent, error)

// Outcome maps a Register result to the closed enum used for mrtcsip.logins and span attributes:
// "ok", "auth_failed", "transient_error", "timeout", "error".
func Outcome(ev sipua.StateEvent, err error) string
```

Notes:
- The standalone login's cancellation handling (`sipLoginProcess.Cancel` racing a pending registration)
  stays in `pkg/connector/login.go`; it uses `Account.Start`/`WaitFirstResult`/`Stop` directly.
- `SIPClient` keeps its bridge-state mapping (`bridgeStateFor`, `sendBridgeState`), the `Connect` single
  retry after 30 s and its spans; it wraps one `*sipaccount.Account`. `SIPConnector.dial` becomes
  `cl.acct.Dial(req)`.
- `newUA` test seam moves to `AccountConfig.NewUA`; `client_lifecycle_test.go` cases that test UA
  lifecycle move to `pkg/sipaccount/account_test.go` (same assertions), bridge-state cases stay.

### 3.2 `pkg/hostkit` (host-class; may import bridgev2, `pkg/calls`, `pkg/rtc`, `pkg/core`, `pkg/obs`,
`pkg/sipaccount`, `pkg/matrixrtc`, `pkg/phone`, `pkg/media`)

```go
package hostkit

// Config is the call/SIP host config: the standalone `network:` section, and jabber's `network.calls`
// minus `enabled`. Moved verbatim from pkg/connector/config.go (fields, yaml tags, defaults, accessors
// RegisterExpiry/RegisterRefresh/RingTimeout/RTPTimeout/DialTimeout/NumberFormat/RedactDTMFCommand).
type Config struct { /* unchanged fields */ }

func (c *Config) ApplyDefaults()
// Validate is the moved Validate with every "network." in messages replaced by prefix
// (standalone: "network.", jabber: "network.calls."). Messages are otherwise unchanged.
func (c *Config) Validate(prefix string) error
// NetConfig returns the sipaccount view of c.
func (c *Config) NetConfig() sipaccount.NetConfig

// ExampleYAML is the embedded example-config.yaml (moved, text unchanged; top-level keys, no indent).
var ExampleYAML string

// CopyKeys copies every Config key from the user's file into the upgrade base under path
// (standalone: no path; jabber: "calls"). Same key list and types as today's upgrader.
func CopyKeys(h configupgrade.Helper, path ...string)

// NewGhostIntent adapts a bridgev2 ghost to core.GhostIntent (moved from pkg/connector/ghostintent.go).
func NewGhostIntent(ghost *bridgev2.Ghost) (core.GhostIntent, error)

// RoomEncrypted reports m.room.encryption from the matrix connector's state store; false (with a
// warning on the ctx logger) when unknown or when br.Matrix is not *matrix.Connector.
func RoomEncrypted(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) bool

// MarkReadAsOwner sends a read receipt as ul's double puppet; core.ErrNoDoublePuppet if none.
func MarkReadAsOwner(ctx context.Context, ul *bridgev2.UserLogin, roomID id.RoomID, eventID id.EventID) error

// CallPowerLevelOverrides returns the PL-0 overrides for matrixrtc.CallMember and CallMemberAlias
// (the standalone buildChatInfo value; buildChatInfo uses this function).
func CallPowerLevelOverrides() *bridgev2.PowerLevelOverrides

// WithCallOverrides sets Events[CallMember]=0 and Events[CallMemberAlias]=0 on pl (allocating the map)
// and reports whether anything changed. Pure function (unit-tested).
func WithCallOverrides(pl *event.PowerLevelsEventContent) (changed bool)

// EnsureCallPowerLevels reads the room's power levels (br.Matrix.GetPowerLevels), applies
// WithCallOverrides and, if changed, sends m.room.power_levels as the bridge bot
// (br.Bot.SendState(ctx, roomID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{})).
func EnsureCallPowerLevels(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) (changed bool, err error)

// CallStackDeps are the host callbacks of the call manager.
type CallStackDeps struct {
	Host        core.CallHost
	Dial        func(login core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error)
	LoginNumber func(login core.LoginRef) string
	Log         zerolog.Logger // plain bridge logger
}

// NewCallStack builds the lk-jwt HTTP client (obs.WrapHTTPTransport with rtc.PathTemplate, 10 s
// timeout), the LiveKit service URL resolver (with the background "LiveKit service URL" log line),
// rtc.SetLibraryLogger and the calls.Manager from cfg. Moved body of SIPConnector.startCalls.
func NewCallStack(br *bridgev2.Bridge, cfg *Config, deps CallStackDeps) (*calls.Manager, *rtc.ServiceURLResolver)

// CallAPI is the part of *calls.Manager the commands use (moved callAPI, exported for host tests).
type CallAPI interface {
	PlaceCall(ctx context.Context, login core.LoginRef, req calls.PlaceCallRequest) (callID, e164 string, err error)
	CheckDestination(login core.LoginRef, e164 string) error
	Hangup(ctx context.Context, login core.LoginRef, roomID id.RoomID) error
	SendDTMF(ctx context.Context, login core.LoginRef, roomID id.RoomID, digits string) (int, error)
}

// CommandDeps adapts the call commands to a host.
type CommandDeps struct {
	// Calls returns the manager, or nil while starting ("The bridge is still starting...").
	Calls func() CallAPI
	// Login picks the login a command acts on; on refusal returns nil and the reply to send.
	Login func(ce *commands.Event) (*bridgev2.UserLogin, string)
	// RoomNumber returns the E.164 of a 1:1 number portal, ok=false for any other room.
	RoomNumber func(p *bridgev2.Portal) (e164 string, ok bool)
	// RedactDTMF redacts dtmf command messages (Config.RedactDTMFCommand()).
	RedactDTMF bool
}

// RegisterCallCommands adds call, dtmf and hangup (each wrapped in WithSpan) with the standalone
// help texts, using br.Config.CommandPrefix in the help. The handler bodies are the moved
// cmdCall/cmdDTMF/cmdHangup with commandLogin -> d.Login and phone.IsE164(portal.ID) -> d.RoomNumber.
// The "not a number's room" usage reply is unchanged.
func RegisterCallCommands(proc *commands.Processor, d CommandDeps)

// WithSpan is the moved connector.WithSpan (root CONSUMER span "cmd."+name, args never recorded).
func WithSpan(name string, fn func(*commands.Event)) func(*commands.Event)

// LoginFields returns the five SIP login fields (moved from sipLoginProcess.Start, same names,
// descriptions, validators and order; FieldTransport optional with default "udp").
func LoginFields(defaultCC string) []bridgev2.LoginInputDataField

// PromptFields runs bridgev2's one-field-per-message prompt flow for fields (same texts as
// bridgev2/commands/login.go userInputLoginCommandState: "Please enter your <Name>", description,
// options, "Enter `skip` to skip this field"), storing commands.CommandState{Action: action, Cancel:
// onCancel}. Password and token fields are redacted with ce.Redact() before validation. Invalid input
// re-prompts the same field. When all fields are filled it clears the state and calls done(ce, input).
func PromptFields(ce *commands.Event, action string, fields []bridgev2.LoginInputDataField,
	done func(ce *commands.Event, input map[string]string), onCancel func())

// ResolveRTCLogin returns the core.LoginResolver of the S3 hook: drop ghost and bot senders, look up the
// portal by room ID, return its Receiver if accept(receiver) (accept nil = all). Moved from rtchook.go.
func ResolveRTCLogin(br *bridgev2.Bridge, accept func(networkid.UserLoginID) bool) core.LoginResolver
```

The standalone `sipLoginProcess.Start` returns `hostkit.LoginFields(cfg.DefaultCountryCode)` in its
`LoginUserInputParams`; `parseLoginInput` becomes `sipaccount.ParseInput`.

### 3.3 Small core additions

```go
// pkg/core/ingest.go
// SetLocalNumber sets how the matrixrtc.ingest span gets call.local_e164 for a login. nil (default)
// keeps the standalone rule (LoginRef up to the first '@'). f must not block (no I/O).
func (in *Ingest) SetLocalNumber(f func(LoginRef) string)

// pkg/obs/span.go
// AnnotateLogger installs in ctx a logger carrying sc's four trace fields, with exactly the semantics of
// the root-span annotation done by Start (rebuild from the clean logger over an annotated ctx; no-op for
// invalid/unsampled sc, disabled loggers, zerolog.DefaultContextLogger and loggers derived from an
// annotated one). It is the existing unexported withLogFields, exported for hosts with their own
// tracer (TS §3.3 amendment).
func AnnotateLogger(ctx context.Context, sc trace.SpanContext) context.Context

// pkg/telemetry/telemetry.go
type Options struct {
	ServiceName    string // default service.name; "" = "mrtc-sip-bridge" (DD_SERVICE / OTEL_* still override)
	ServiceVersion string
	CommitSHA      string
	RepositoryURL  string
	Log            zerolog.Logger
}
```

---

## 4. mautrix-jabber seams (untagged, T2)

All seams are no-ops when no extension is registered (public behaviour). No SIP words in untagged files
except `Dockerfile.sipcalls`, `build-sipcalls.sh`, `go.mod` and the tagged-file list in `CLAUDE.md`.

### 4.1 `pkg/connector/extension.go`

```go
package connector

// connectorExtension is optional functionality compiled in with a build tag. Every method is called only
// when an extension is registered. Implementations must be safe for concurrent use.
type connectorExtension interface {
	exampleConfig() string                                    // appended to GetConfig's example
	upgradeConfig(h configupgrade.Helper)                     // called from GetConfig's upgrader
	init(br *bridgev2.Bridge)                                 // from JabberConnector.Init
	postInit(proc *commands.Processor, network *yaml.Node)    // from PostInitExtensions (main PostInit)
	start(ctx context.Context) error                          // from JabberConnector.Start; error fails startup
	stop()                                                    // from JabberConnector.Stop, before telemetry.Shutdown
	loginLoaded(ul *bridgev2.UserLogin, x *xmppClient)        // from LoadUserLogin, real clients only
	loginStopped(x *xmppClient)                               // from xmppClient.Disconnect, after the XMPP teardown
	decorateChatInfo(portalID string, info *bridgev2.ChatInfo) // DM chat info (getChatInfo DM paths, dmCreateChatResponse)
	suppressCallNotice(loginID networkid.UserLoginID, peerBareJID string) bool
	startAttrs() []attribute.KeyValue                         // extra bridge.start attributes
}

// registerExtension is set by an init() in a build-tagged file; nil in the default build.
var registerExtension func(c *JabberConnector) connectorExtension

// extension returns the connector's extension (created once; nil when none is compiled in).
// GetConfig runs before Init (mxmain), so this must not depend on c.br.
func (c *JabberConnector) extension() connectorExtension

// PostInitExtensions is called from main's PostInit after SetNetworkConfig. No-op without an extension.
func (c *JabberConnector) PostInitExtensions(proc *commands.Processor, network *yaml.Node)

// ExtensionStartAttrs returns extra bridge.start attributes (nil without an extension).
func (c *JabberConnector) ExtensionStartAttrs() []attribute.KeyValue
```

`JabberConnector` gets `extnOnce sync.Once; extn connectorExtension`. `gopkg.in/yaml.v3` is already in
jabber's module graph (indirect) and becomes direct.

### 4.2 Seam call sites (exact)

| File / function | Change |
|---|---|
| `connector.go` `GetConfig` | `example += e.exampleConfig()` and, inside the upgrader func after the `cheogram` copy, `e.upgradeConfig(helper)`, when `e := c.extension(); e != nil`. |
| `connector.go` `Init` | after `c.br = br`: `if e := c.extension(); e != nil { e.init(br) }`. |
| `connector.go` `Start` | `if e := c.extension(); e != nil { return e.start(ctx) }`. |
| `connector.go` `LoadUserLogin` | after `login.Client = client` (real client path only): `if e := c.extension(); e != nil { e.loginLoaded(login, client) }`. |
| `xmpp_client.go` `Disconnect` | after `x.mu.Unlock()` and before the bridge-state send: `if e := x.conn.extension(); e != nil { e.loginStopped(x) }`. (`x.conn` may be nil in tests: guard.) |
| `xmpp_client.go` `resolveIdentifier` | extract the `if createChat {...}` struct building into `func (x *xmppClient) dmCreateChatResponse(jidStr string) *bridgev2.CreateChatResponse` (memberMap, group-MMS members, `PortalInfo`, `ExtraUpdates`; **no** backfill). `resolveIdentifier` calls it, then keeps its synchronous backfill block unchanged. At the end of `dmCreateChatResponse`: `x.conn.decorateChatInfo(jidStr, resp.PortalInfo)`. |
| `xmpp_client.go` `getChatInfo` | the three DM return paths (`RoomTypeDM`, `IsCheogramDMJID`, default) go through `info, err := x.getDMChatInfo(...); if err == nil { x.conn.decorateChatInfo(portalID, info) }`. |
| `connector.go` | `func (c *JabberConnector) decorateChatInfo(portalID string, info *bridgev2.ChatInfo)`: nil-safe on `c`, `info`; calls the extension if any. |
| `xmpp_jingle.go` `handleJingleMessageInitiation` | after the `x.login == nil` guard: `if !fromSelf && x.conn != nil { if e := x.conn.extension(); e != nil && e.suppressCallNotice(x.loginID, fromBare) { return } }`. A suppressed `propose` stores no session, so later `accept`/`reject` for it already return early. |
| `telemetry.go` `Stop` | `if e := c.extension(); e != nil { e.stop() }` before `telemetry.Shutdown`. |
| `metadata.go` | `Extra map[string]json.RawMessage \`json:"extra,omitempty"\`` on `JabberLoginMeta`, doc: "opaque data owned by optional build-tagged features; preserved across builds". |
| `cmd/mautrix-jabber/main.go` | `Version: "0.1.10" + buildVariant`; in PostInit after `jc.SetAppServiceID(...)`: `jc.PostInitExtensions(br.Bridge.Commands.(*commands.Processor), &br.Config.Network)`; in PostStart append `jc.ExtensionStartAttrs()...` to the `bridge.start` attributes (get `jc` by the same type assertion as PostInit). |

### 4.3 `pkg/telemetry` backend split

Move from `telemetry.go`/`span.go` into `backend_otel.go` (`//go:build !sipcalls`), unchanged:
`Init`, `initWith`, `resolveConfig`, `safeEndpoint`, `buildResource`, `Shutdown`, `newErrorHandler`,
`otelLogSink`, `withLogFields`, `Logger`, `LoggerSC`, `logAnnotKey`/`logAnnot`, `addLogFields`,
`ddTraceID`/`ddSpanID`, and the `tp`, `initOnce`, `shutdownOnce` vars. `telemetry.go` keeps `TracerName`,
`Options`, `enabled`, `tracer`, `noopSpan`, `Enabled`. `span.go` keeps `Start` (calls `withLogFields`),
`StartChild`, `StartLinkedRoot`, `CarrySpan`, `WithParent`, `StartRemoteEvent`/`EndRemoteEvent`,
`stripURLs`, `Fail`. Tests of moved functions go to `backend_otel_test.go` (`!sipcalls`).

`backend_sipcalls.go` (`//go:build sipcalls`):

```go
// mrtcInit is a test seam.
var mrtcInit = mrtctelemetry.Init

// Init configures tracing and metrics through mrtc-sip-bridge's host telemetry with jabber's identity.
// It never fails the bridge. Only the first call has an effect.
func Init(ctx context.Context, o Options) // once: mrtcInit(ctx, mrtctelemetry.Options{ServiceName:
	// "mautrix-jabber", ServiceVersion: o.ServiceVersion, CommitSHA: o.CommitSHA, RepositoryURL:
	// o.RepositoryURL, Log: o.Log}); if obs.Enabled() { tracer = otel.GetTracerProvider().Tracer(TracerName);
	// enabled.Store(true) }
func Shutdown(ctx context.Context)                                   // mrtctelemetry.Shutdown(ctx)
func withLogFields(ctx context.Context, sc trace.SpanContext) context.Context { return obs.AnnotateLogger(ctx, sc) }
func Logger(ctx context.Context, base *zerolog.Logger) *zerolog.Logger { return obs.Logger(ctx, base) }
func LoggerSC(sc trace.SpanContext, base *zerolog.Logger) *zerolog.Logger { return obs.LoggerSC(sc, base) }
```

Behaviour change in the sipcalls build only (documented in `docs/EMBEDDING.md`): setting
`OTEL_EXPORTER_OTLP_ENDPOINT` also enables metrics (`OTEL_METRICS_EXPORTER=none` turns them off), Go
runtime metrics are exported, and the env resolution is mrtc's (identical rules for traces).

---

## 5. Configuration (jabber `network.calls`, T3)

```go
// sipcalls_config.go
type CallsConfig struct {
	Enabled        bool `yaml:"enabled"`
	hostkit.Config `yaml:",inline"`
}

type networkCallsSection struct {
	Calls CallsConfig `yaml:"calls"`
}
```

- `exampleConfig()` returns `"# SIP calls for Cheogram numbers (private build). Requires cheogram: true.\ncalls:\n    enabled: false\n"`
  followed by `hostkit.ExampleYAML` with every line indented by 4 spaces (comments included).
- `upgradeConfig(h)`: `h.Copy(configupgrade.Bool, "calls", "enabled")` and `hostkit.CopyKeys(h, "calls")`.
- `postInit` decodes `network.Decode(&networkCallsSection{})`. A decode error is stored in `cfgErr` and
  returned by `start` as `network.calls: <err>` (fail fast; nothing is registered in `postInit` then).
- `start`: if `!Enabled` → log Info "SIP calls disabled" and return nil (no runtime, no handlers were
  registered). Else `ApplyDefaults()`, `Validate("network.calls.")`, require `CheogramEnabled(br)`
  (error `network.calls.enabled requires network.cheogram: true`), warn once if
  `br.Config.Encryption.Default` ("encryption.default is true: calls only ring in unencrypted portals"),
  then create the runtime and the call stack (§6.1).

Dev service values are in §9.3. Defaults equal the standalone's (SIP 5060-5069, RTP 40000-40100 etc.).

---

## 6. Embedded host (T3)

### 6.1 `callsExt` (sipcalls.go)

```go
func init() { registerExtension = func(c *JabberConnector) connectorExtension { return &callsExt{c: c} } }

type callsExt struct {
	c   *JabberConnector
	br  *bridgev2.Bridge
	log zerolog.Logger // br.Log with component=calls-host (clean)

	cfg     CallsConfig
	cfgErr  error
	enabled bool          // cfg.Enabled after postInit
	rtc     *core.Ingest  // created in postInit when enabled
	rt      *sipaccount.Runtime // set in start
	calls   *calls.Manager      // set in start

	mu       sync.Mutex
	accounts map[networkid.UserLoginID]*loginCalls
}

type loginCalls struct {
	ul        *bridgev2.UserLogin
	client    *xmppClient // owner token: only this client may stop the account
	acct      *sipaccount.Account // nil when no credentials
	authNoted bool        // auth_failed notice already sent for the current account
}
```

- `init(br)`: store `br`, `log`.
- `postInit(proc, network)`: decode config (§5). If enabled: `x.rtc = core.NewIngest(core.LogSink{Log: &lg})`
  (component=matrixrtc), `x.rtc.SetLocalNumber(x.loginNumber)`,
  `core.RegisterMatrixRTCHandlers(br.Matrix.(*matrix.Connector).EventProcessor, hostkit.ResolveRTCLogin(br, x.hasAccount), x.rtc)`,
  and register commands (§7). When disabled, register only `sip-status` (it explains that calls are
  disabled) so users get a clear answer.
- `start(ctx)`: §5 checks, then `x.rt, err = sipaccount.NewRuntime(ctx, x.cfg.NetConfig(), br.Log)`,
  `x.calls, _ = hostkit.NewCallStack(br, &x.cfg.Config, hostkit.CallStackDeps{Host: &jabberCallHost{x: x},
  Dial: x.dial, LoginNumber: x.loginNumber, Log: br.Log})`, `x.rtc.SetSink(x.calls)`, then start every
  attached account with credentials (accounts loaded before the runtime existed, §6.2).
- `stop()`: `calls.Stop(5 s)` then `rt.Close()`. (Logins were already disconnected by the framework.)
- `startAttrs()`: `calls.enabled` (bool), and when enabled `rtp.port_range_size`, `sip.port_range_size`,
  `ring_timeout_s`, `livekit.service_url.source`.
- `hasAccount(loginID) bool`: true if the login has a non-nil `acct` (map read under `mu`; no I/O).
- `loginNumber(ref) string`: the account's `Creds().DisplayNumber`, "" if none.
- `dial(ref, req)`: `acct.Dial(req)` or `sipua.ErrNotRegistered`.

### 6.2 Per-login accounts (sipcalls_accounts.go)

Credentials live in `JabberLoginMeta.Extra["sip"]` as the JSON of `sipaccount.Credentials`:

```go
func sipCreds(meta *JabberLoginMeta) (*sipaccount.Credentials, bool)  // ok=false if absent/invalid/incomplete
func setSIPCreds(meta *JabberLoginMeta, c *sipaccount.Credentials)    // nil deletes the key (and Extra if empty)
```

Lifecycle (all under `x.mu` for map changes; UA start/stop outside the lock):

| Event | Action |
|---|---|
| `loginLoaded(ul, client)` | If disabled: return. Get or create `loginCalls` for `ul.ID`; set `ul`, `client`. If an `acct` already exists and runs (client replacement / re-login): keep it (takeover, D4). Else, if creds exist: create `acct = rt.NewAccount(...)` when `rt != nil` and start it async (`startAsync`); when `rt == nil` the account is created and started by `start`. |
| `startAsync(lc)` | goroutine: root span `bridge.sip_connect` (attrs: masked user, registrar host, transport, `xmpp.login.jid`), `acct.Start(ctx)`; on error `obs.Fail`, log, retry once after 30 s as a linked root (same as standalone `Connect`), unless the account was replaced/stopped meanwhile (generation check). On success, spawn the PL sweep (§6.4). |
| `loginStopped(client)` | If `lc.client != client` (an old client of a replaced login): return. Else root span `bridge.sip_disconnect`: `calls.EndLogin(8 s)`, `acct.Stop(ctx)`, delete the map entry. |
| `OnState(ev)` | UA goroutine, must not block: on transition into `StateAuthFailed` with `!authNoted`, set it and hand off to a goroutine that sends the bot notice "SIP registration for <DisplayNumber> was rejected (SIP <code>). Fix the password with `<prefix> sip-login`." to `ul.User.ManagementRoom` (skip if empty). Reset `authNoted` on `StateRegistered`. |
| `OnInvite(call)` | `calls.HandleInvite(core.LoginRef(ul.ID), creds.DisplayNumber, call)`; `Reject(480, ...)` if `calls == nil`. |
| `sip-login` success (§7) | stop the previous account of that login (EndLogin + Stop) **before** registering the new one (no double Contact); after `Register` returns Registered, save creds, set `lc.acct` to the new account, spawn the PL sweep. |
| `sip-logout` | EndLogin + Stop, clear creds, save, `lc.acct = nil`. |

Account log: `br.Log.With().Stringer("user_mxid", ul.UserMXID).Str("login", string(ul.ID)).Str("sip_user", sipua.MaskUsername(u)).Logger()` (no component key).

### 6.3 `jabberCallHost` (sipcalls_host.go)

```go
type jabberCallHost struct{ x *callsExt }
var _ core.CallHost = (*jabberCallHost)(nil)

// cheogramE164 returns the E.164 of a Cheogram 1:1 JID ("+15551234567@cheogram.com", domain compared
// case-insensitively, resource ignored); ok=false for group MMS (comma), short codes (;phone-context),
// other domains and anything phone.IsE164 rejects.
func cheogramE164(jidStr string) (string, bool)
func cheogramJID(e164 string) string // e164 + "@" + cheogramDomain
```

| Method | Behaviour |
|---|---|
| `ResolveCallRoom(ctx, ref, e164)` | `phone.IsE164` else error. `ul := br.GetCachedUserLoginByID(ref)` (nil → `core.ErrUnknownLogin`); `xc, ok := ul.Client.(*xmppClient)` (else error "login is not connected"). `jid := cheogramJID(e164)`, `key := PortalKey{ID: jid, Receiver: ul.ID}`, `portal := br.GetPortalByKey`. If `portal.MXID == ""`: `resp := xc.dmCreateChatResponse(jid)` (decorated, D7/D8), `portal.CreateMatrixRoom(ctx, ul, resp.PortalInfo)`, `created = true`. Else `hostkit.EnsureCallPowerLevels` (warn on error, continue). Then `roomFor`. Never does XMPP I/O. |
| `roomFor(ctx, portal, e164)` | `ghost := br.GetGhostByID(ctx, networkid.UserID(cheogramJID(e164)))`; `ghost.Intent.EnsureJoined(ctx, portal.MXID)` (warn on error); `gi := hostkit.NewGhostIntent(ghost)`; `Encrypted: hostkit.RoomEncrypted(ctx, br, portal.MXID)`. |
| `LookupCallRoom(ctx, ref, roomID)` | `portal := br.GetPortalByMXID`; nil, `Receiver != ref`, `RoomType == RoomTypeGroupDM` or `!cheogramE164(portal.ID)` → `core.ErrNotCallRoom`. Else `roomFor` (`Created` false). Creates nothing, sends no PL change. |
| `SendCallNotice` | ghost `m.notice` (as standalone). |
| `OwnerMXID(ref)` | `ul.UserMXID`, "" if unknown. |
| `MarkReadAsOwner` | `hostkit.MarkReadAsOwner(ctx, ul, roomID, eventID)`. |

`decorateChatInfo(portalID, info)`: if enabled and `cheogramE164(portalID)` ok and `info.Members != nil`:
merge `CallPowerLevelOverrides()` into `info.Members.PowerLevels` (create if nil; keep other entries).

`suppressCallNotice(loginID, peer)`: `enabled && hasAccount(loginID) && cheogramE164(peer) ok`.

### 6.4 Power-level sweep

`sweepCallPowerLevels(loginID)`: goroutine, linked root `bridge.calls.pl_sweep`; `br.GetAllPortalsWithMXID`,
keep `Receiver == loginID && cheogramE164(ID) ok && RoomType != GroupDM`, sequential
`hostkit.EnsureCallPowerLevels` (stop on ctx done, 2 min overall timeout). Attributes
`portals.checked`, `portals.updated`, `portals.failed`; one Info log line with the counts.

### 6.5 Event hook and routing

Exactly the S3 pattern (`hostkit.ResolveRTCLogin`), registered in PostInit before the event processor
starts. `accept = hasAccount` so rooms of logins without SIP produce no spans (counted `routed=false`).
The calls manager rejects non-call rooms through `LookupCallRoom` (MUC, group MMS, non-Cheogram DMs).

---

## 7. Commands (T4, sipcalls_commands.go)

Registered in `postInit` with `hostkit.WithSpan` (spans `cmd.sip_login`, `cmd.sip_logout`,
`cmd.sip_status`, `cmd.call`, `cmd.hangup`, `cmd.dtmf`), `RequiresLogin: true`,
`Section: commands.HelpSectionAuth` for `sip-*`, `HelpSectionChats` for the call commands.

Target login for every command (`commandLogin`): the portal's Receiver when sent in a portal (must belong
to the sender), else `ce.User.GetDefaultLogin()`. Refusal replies: no login → "You are not logged in.
Use `<prefix> login` first."; stub client → "Log in to Jabber first.".

**`sip-login`** (no arguments; arguments are rejected with "Run `<prefix> sip-login` without arguments;
the bridge asks for each value."): if calls are disabled → "SIP calls are disabled on this bridge
(network.calls.enabled)." Else reply the instructions (standalone login text, plus "This registers the
account from this bridge. Make sure no other bridge is registered with the same SIP account.") and
`hostkit.PromptFields(ce, "SIP login", hostkit.LoginFields(cfg.DefaultCountryCode), done, nil)`.
`done`: span `cmd.sip_login.submit` (root CONSUMER, attrs masked user, registrar host, transport,
`call.local_e164`), `creds := sipaccount.ParseInput`, stop the login's current account (§6.2),
`acct := rt.NewAccount(...)`, `ev, err := sipaccount.Register(ctx, acct, 30*time.Second)`, record
`obs.M().Logins{outcome}`, reply:

| Outcome | Reply |
|---|---|
| ok | "Registered <number> via <registrar>. Incoming calls to that number ring in its Cheogram room; start a call in a number's room or use `<prefix> call <number>`." (then save creds: `setSIPCreds`, `ul.Save`; save failure → stop account, reply "Registered, but saving failed: <err>") |
| auth_failed | "The registrar rejected the credentials (SIP <code> <reason>)." |
| timeout | "Timed out registering with <registrar>." |
| transient_error | "Could not register with <registrar>: <reason>." |
| error | "Could not start the SIP client: <err>." (err never contains the password; `obs.Scrub` for spans) |

After a failed attempt the previous credentials stay saved but their account stays stopped until the
next bridge restart or a successful `sip-login`; the reply adds "Your previous SIP registration was
stopped." when one existed.

**`sip-logout`**: no creds → "No SIP account is set up for this login." Else EndLogin + Stop, clear,
save, reply "SIP account removed. Calls to <number> no longer ring here."

**`sip-status`**: lines (no secrets): calls enabled yes/no; Cheogram on/off; `encryption.default`
warning if true; SIP account none / `<masked user>` via `<registrar>` (`<transport>`), number;
registration state (`registered`, `registering`, `auth_failed (SIP 403)`, `transient_error (retry in 25s)`,
`stopped`); active call yes/no (room) via a new read-only `Manager` query **only if it already exists**;
otherwise omit the line (no core change for it).

**`call`, `hangup`, `dtmf`**: `hostkit.RegisterCallCommands(proc, hostkit.CommandDeps{Calls: func() hostkit.CallAPI
{ if x.calls == nil { return nil }; return x.calls }, Login: x.commandLoginWithAccount, RoomNumber:
func(p) (string, bool) { if p.RoomType == database.RoomTypeGroupDM { return "", false }; return cheogramE164(string(p.ID)) },
RedactDTMF: x.cfg.RedactDTMFCommand()})`. `commandLoginWithAccount` additionally refuses logins without
a SIP account: "No SIP account is set up for this login. Use `<prefix> sip-login`." Guard against a nil
interface: return a typed nil only via the explicit `nil` branch above.

---

## 8. Telemetry (TS §8.2 Phase J)

- **Init**: unchanged call site in jabber's PostInit (`telemetry.Init`); the sipcalls backend delegates
  (§4.3). `obs.Use` therefore runs before `JabberConnector.Start` creates the runtime and registers the
  registrations gauge (PLAN J8 ordering rule).
- **Matrix HTTP**: jabber's existing `WrapMatrixTransport` (tracing only) stays; core HTTP spans to
  Matrix nest under core spans via the ctx. No Matrix HTTP metrics in embedded mode (documented).
- **New host spans** (all jabber-side, created with `obs`/`hostkit` helpers): `bridge.sip_connect`
  (root, INTERNAL; retry is a linked root with `retry=true`), `bridge.sip_disconnect` (root),
  `bridge.calls.pl_sweep` (linked root), `cmd.sip_login`, `cmd.sip_login.submit` (root CONSUMER: the
  submit arrives as a separate message), `cmd.sip_logout`, `cmd.sip_status`, `cmd.call`, `cmd.hangup`,
  `cmd.dtmf`. Core spans (`sip.*`, `call.*`, `rtc.*`, `matrixrtc.*`, `media.*`) are unchanged.
- **Metrics**: all `mrtcsip.*` instruments work in embedded mode (resource = jabber's). `mrtcsip.logins`
  is recorded by `sip-login` with the same outcome enum.
- **Log correlation**: one annotation key (obs) for both packages; our own lines under spans use
  `obs.Logger(ctx, &clean)`; account/runtime logs carry `login` (JID, allowed) and `sip_user` (masked).
- **Resource**: `service.name=mautrix-jabber` by default; the dev service sets `DD_SERVICE=mautrix-jabbercalls`
  so its data is distinguishable from the old dev jabber service.
- Docs: `docs/EMBEDDING.md` "Telemetry" lists the new host spans and the metrics behaviour change; mrtc
  README "What is traced" gets an "Embedded host" row group. Jabber's public README is **not** changed.

---

## 9. Build, packaging and deployment

### 9.1 `go.mod` (jabber, local branch)

```
require github.com/williamkray/mrtc-sip-bridge v0.0.0-00010101000000-000000000000
replace github.com/williamkray/mrtc-sip-bridge => ../mrtc-sip-bridge
```

Then `go mod tidy` (tidy considers all build tags, so the sipcalls dependencies land in `go.mod`/`go.sum`).
Both repos stay on `maunium.net/go/mautrix v0.32.0`, `go.mau.fi/util v0.11.0`, OTel v1.47.0, zerolog
v1.35.1; T2 checks `go list -m all` shows no downgrade/upgrade of those and reports any other MVS bumps.
Consequence (accepted): on `local/sip-calls` even the untagged build needs `../mrtc-sip-bridge` checked out.

### 9.2 `Dockerfile.sipcalls` and `build-sipcalls.sh` (jabber, local branch)

```dockerfile
# syntax=docker/dockerfile:1
# Private build of mautrix-jabber with SIP calls (build tag sipcalls). Needs the named build context
# "mrtc" (../mrtc-sip-bridge); see build-sipcalls.sh. Never push images built from this file publicly.
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache ca-certificates gcc g++ musl-dev sqlite-dev olm-dev opus-dev pkgconf
WORKDIR /src/mautrix-jabber
COPY --from=mrtc go.mod go.sum /src/mrtc-sip-bridge/
COPY go.mod go.sum ./
RUN go mod download
COPY --from=mrtc pkg /src/mrtc-sip-bridge/pkg
COPY . .
ARG GIT_COMMIT=unknown
ARG GIT_TAG=unknown
ARG GIT_REPOSITORY_URL=unknown
ARG BUILD_TIME=unknown
# Default crypto build (libolm) plus sipcalls; nolibopusfile: hraban/opus without opusfile.
RUN CGO_ENABLED=1 go build -tags "sipcalls nolibopusfile" -ldflags "-s -w \
	-X main.Commit=${GIT_COMMIT} -X main.Tag=${GIT_TAG} \
	-X main.RepositoryURL=${GIT_REPOSITORY_URL} -X main.BuildTime=${BUILD_TIME}" \
	-o mautrix-jabber ./cmd/mautrix-jabber

FROM alpine:3.19
ARG GIT_COMMIT=unknown
ARG MRTC_COMMIT=unknown
LABEL org.opencontainers.image.revision=${GIT_COMMIT} \
	com.github.williamkray.mautrix-jabber.variant=sipcalls \
	com.github.williamkray.mautrix-jabber.mrtc-revision=${MRTC_COMMIT}
RUN apk add --no-cache ca-certificates tzdata sqlite-libs olm opus
RUN adduser -D -h /app -s /sbin/nologin bridge
WORKDIR /app
COPY --from=builder /src/mautrix-jabber/mautrix-jabber .
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
USER bridge
EXPOSE 29318
# SIP (network.calls.sip_port_min..max) and RTP (rtp_port_min..max) defaults; publish what you configure.
EXPOSE 5060-5069/udp 5060-5069/tcp 40000-40100/udp
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["-c", "/data/config.yaml", "-r", "/data/registration.yaml"]
```

(The `GIT_REPOSITORY_URL` label is deliberately not set: the public URL would mislabel a private build.)

```bash
#!/usr/bin/env bash
# Private sipcalls image. Usage: ./build-sipcalls.sh [extra docker build args]. Never push the result
# to a public registry.
set -e
MRTC_DIR=${MRTC_DIR:-../mrtc-sip-bridge}
[ -z "$(git -C "$MRTC_DIR" status --porcelain -- pkg go.mod go.sum)" ] || echo "warning: $MRTC_DIR has uncommitted changes" >&2
mrtc_commit=$(git -C "$MRTC_DIR" rev-parse HEAD)
short=$(git rev-parse --short HEAD)
docker build -f Dockerfile.sipcalls --build-context mrtc="$MRTC_DIR" \
	$(./build-args.sh) --build-arg MRTC_COMMIT="$mrtc_commit" \
	-t mautrix-jabber:sipcalls-dev -t "mautrix-jabber:sipcalls-$short" "$@" .
```

The public `Dockerfile` and `build.sh` are unchanged (they would fail on this branch because of the
replace; that is expected and documented in `CLAUDE.md`).

### 9.3 New dev service `jabbercalls` (dev Docker host, confined to `/opt/services/<stack>`)

Deploy (from the laptop; `<docker-host>` = the example.org Synapse host's ssh alias used in earlier phases):

```bash
cd ~/Code/mautrix-jabber && ./build-sipcalls.sh
docker save mautrix-jabber:sipcalls-dev | gzip | ssh <docker-host> 'gunzip | docker load'
```

On the Docker host, in `/opt/services/<stack>`:

```bash
mkdir -p mautrix-jabbercalls && chown <same uid:gid as the mrtc-sip-bridge data dir> mautrix-jabbercalls
docker run --rm --entrypoint /app/mautrix-jabber mautrix-jabber:sipcalls-dev -e -c - > mautrix-jabbercalls/config.yaml
# edit config.yaml (values below), then generate the registration (also writes tokens into config.yaml):
docker run --rm -v "$PWD/mautrix-jabbercalls:/data" mautrix-jabber:sipcalls-dev -g -c /data/config.yaml -r /data/registration.yaml
# register it with Synapse exactly as the mrtc-sip-bridge registration was (Phase 1): copy
# registration.yaml next to the existing appservice files, add it to app_service_config_files, restart synapse.
docker compose up -d mautrix-jabbercalls
```

Config values (everything else stays at the generated defaults):

```yaml
homeserver:
    address: <same as mrtc-sip-bridge's config>
    domain: example.org
appservice:
    address: http://mautrix-jabbercalls:29318
    hostname: 0.0.0.0
    port: 29318
    id: jabbercalls
    bot:
        username: jabbercallsbot
        displayname: Jabber calls bot
    username_template: jabbercalls_{{.}}
database:
    type: sqlite3-fk-wal
    uri: file:/data/mautrix-jabbercalls.db?_txlock=immediate
bridge:
    command_prefix: '!jc'
    permissions:
        "@admin:example.org": admin
double_puppet:
    secrets:
        example.org: <same as mrtc-sip-bridge's config>
encryption:
    allow: false
    default: false
backfill:
    enabled: false        # user choice; avoids mass MAM backfill into the new rooms
logging:
    writers: [{type: stdout, format: json}]
network:
    cheogram: true
    calls:
        enabled: true
        sip_port_min: 5070
        sip_port_max: 5079
        rtp_port_min: 40200
        rtp_port_max: 40300
        external_ip: <same as mrtc-sip-bridge's network.external_ip>
        stun_server: <same as mrtc-sip-bridge>
        livekit_service_url: <same as mrtc-sip-bridge, byte-identical to the well-known>
```

Compose service (append to the stack's compose file; networks as used by `mrtc-sip-bridge`, which reaches
Synapse over `traefik6`; SQLite means no `postgres` network). The SIP and RTP ranges differ from the
standalone's published 5060-5069 / 40000-40100 so both services can be defined and started side by side
(only one may hold the SIP registration at a time, D15):

```yaml
  mautrix-jabbercalls:
    image: mautrix-jabber:sipcalls-dev
    container_name: mautrix-jabbercalls
    restart: unless-stopped
    stop_grace_period: 30s
    volumes:
      - ./mautrix-jabbercalls:/data
    networks:
      - traefik6
    ports:
      - "5070-5079:5070-5079/udp"
      - "5070-5079:5070-5079/tcp"
      - "40200-40300:40200-40300/udp"
    environment:
      OTEL_EXPORTER_OTLP_ENDPOINT: <same as mrtc-sip-bridge>
      OTEL_EXPORTER_OTLP_PROTOCOL: <same as mrtc-sip-bridge>
      OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE: delta
      DD_SERVICE: mautrix-jabbercalls
      DD_ENV: dev
    labels:
      com.datadoghq.tags.service: mautrix-jabbercalls
```

Name checks (verified in code at `e812e20`): mautrix-jabber hardcodes no appservice ID, bot username,
ghost template or prefix. `!jabber` appears only as a fallback when `bridge.command_prefix` is empty;
the network ID `jabber` is internal (bridge info), not a Matrix namespace. Ghost display names get the
suffix " (jabbercalls)" from `appservice.id` (`withAppServiceIDSuffix`), which is expected. The generated
namespaces `@jabbercalls_.*` / `@jabbercallsbot` do not overlap `@jabber_.*`, `@jabberbot` or `@sip_.*`.

### 9.4 Leak guards (jabber, T2)

- `.git/hooks/pre-push` (untracked, executable): reject pushing any ref whose name starts with `local/`,
  and reject any pushed commit range that touches a path matching `*sipcalls*` or adds the string
  `mrtc-sip-bridge` to any file (`git log -p <range> | grep -q mrtc-sip-bridge`). Print why and exit 1.
- `git config branch.local/sip-calls.pushRemote no-push-local-branch` (a remote name that does not exist).
- Images: never tag the sipcalls image as `williamkray/mautrix-jabber:*`.

---

## 10. Privacy

Everything in TS §6 and P2/P3 §11 applies. Additionally:

- SIP credentials are stored in `JabberLoginMeta.Extra["sip"]` in plain text (same trust level as the
  XMPP password). Never logged, traced, put in errors or replies. The `sip-login` password message is
  redacted by the bot (bridgev2 prompt semantics; the management room bot is admin).
- The SIP username appears only masked (`sipua.MaskUsername`) in logs, spans and `sip-status`.
- Command arguments are never recorded (`WithSpan`); DTMF digits never reach logs/spans; dtmf messages are
  redacted in portals too (jabber's bot has PL 9001 in its portals).
- JIDs and phone numbers are allowed on spans and logs, never on metrics (unchanged rules).
- Public-repo privacy: §9.4 guards; the private module path appears only in `go.mod`/`go.sum`, tagged
  files, `Dockerfile.sipcalls` and `build-sipcalls.sh` on the never-pushed branch.

---

## 11. Tests

### 11.1 mrtc-sip-bridge (T1, offline, `-race`)

- All existing tests pass; moved tests keep their assertions (only package/identifier renames).
- `./mrtc-sip-bridge -e` output is byte-identical before and after the refactor (golden comparison done
  by the task and reported; add `pkg/hostkit` test asserting `ExampleYAML` equals the previous file).
- `pkg/sipaccount`: port retry on bind failure and release of skipped ports; config error returns
  immediately and releases the port; `Stop` idempotent, unregister runs under a detached 5 s ctx even when
  the caller ctx is cancelled; stale-UA state events ignored after Stop/Start; `WaitFirstResult` latch
  reset per `Start`; `Dial` after `Stop` → `ErrNotRegistered`; `Register` for registered / auth_failed /
  transient / timeout / ctx cancel (account stopped in every non-ok case); `Outcome` table; gauge counts
  per state with two accounts; `ParseInput`/`NormalizeRegistrar` moved table tests.
- `pkg/hostkit`: `Validate("network.calls.")` messages carry the prefix (table over every rule);
  `CopyKeys(h, "calls")` round trip preserves non-default values under the nested path; `WithCallOverrides`
  (nil map, already set, other events kept); commands tests moved (fake `CallAPI`, `RoomNumber` refusal,
  DTMF redaction even on error, digits absent from replies/spans); `PromptFields` (password field
  redacted before validation, invalid value re-prompts the same field, `skip` for optional fields, `done`
  receives all values, Cancel callback stored); `ResolveRTCLogin` drops bot/ghost senders and respects
  `accept`.
- core: `SetLocalNumber` sets `call.local_e164` on `matrixrtc.ingest`; nil keeps the standalone rule.
  obs: `AnnotateLogger` equals the root-annotation behaviour (fields once; no-op for unsampled/disabled).
  telemetry: `ServiceName` default and override precedence (`DD_SERVICE`, `OTEL_SERVICE_NAME` win).
- importcheck: `pkg/sipaccount` is checked as core (a test fixture importing bridgev2 fails);
  `pkg/hostkit` is skipped like `pkg/connector`.

### 11.2 mautrix-jabber untagged (T2)

`go test -race -count=1 -tags nocrypto ./...` passes with the same test count as before T2 (report both
numbers); `dmCreateChatResponse` returns the same struct `resolveIdentifier` built before (table: 1:1
Cheogram, group MMS, plain JID); with no extension, `decorateChatInfo`/seams are no-ops (a test asserts
`registerExtension == nil` in the default build).

### 11.3 mautrix-jabber sipcalls (T2-T4)

- telemetry (T2): `TestEmbeddedParenting`: with a `tracetest.SpanRecorder` provider installed through a
  test hook (sets jabber's tracer and calls `obs.Use`), a jabber `telemetry.Start` span → an `obs.Start`
  child → both share the trace, parent IDs match, scopes are `github.com/mautrix/jabber` and
  `github.com/williamkray/mrtc-sip-bridge`. `TestNoDoubleAnnotation`: jabber root over an obs-annotated
  ctx and obs linked root over a jabber-annotated ctx each produce exactly one `trace_id` key in a JSON
  log line. `TestInitServiceName`: `mrtcInit` seam receives `ServiceName: "mautrix-jabber"`.
- config (T3): example YAML parses and contains every key with the standalone default under `calls:`;
  defaults/validation with prefix; upgrader preserves user values; `enabled` with `cheogram: false` → start
  error; decode error → start error; disabled → `start` returns nil and creates nothing.
- host (T3): `cheogramE164` table (1:1, upper-case domain, resource, group MMS, short code, other domain,
  invalid E.164); `LookupCallRoom` refusals via a small portal-lookup seam (`func(ctx, roomID) (*bridgev2.Portal, error)`
  field on `jabberCallHost`, default `br.GetPortalByMXID`): other receiver, group DM, MUC JID, non-Cheogram
  DM; `decorateChatInfo` merges overrides only for Cheogram 1:1 and keeps existing entries;
  `suppressCallNotice` table (disabled, no account, Cheogram 1:1, group MMS, plain JID) and an
  integration test through `handleJingleMessageInitiation` (existing `xmpp_jingle_test.go` harness):
  suppressed propose/retract queue nothing, unsuppressed still queue.
- accounts (T3, fake UA via `AccountConfig.NewUA`): attach before runtime → started by `start`; client
  replacement keeps the running account (one UA created); `loginStopped` by the old client is a no-op,
  by the owner calls `EndLogin` then stops; re-login stops the old UA before the new one starts; creds
  round trip through `Extra` (other `Extra` keys preserved); auth_failed notice sent once until registered.
- commands (T4): `sip-login` prompt sequence end to end with a fake `Register` result per outcome (reply
  texts of §7; creds saved only on ok; previous account stopped first); arguments rejected; disabled
  bridge reply; `sip-logout` with/without creds; `sip-status` lines; call commands refuse logins without
  an account and non-Cheogram rooms. **Privacy test**: run `sip-login` with password `hunter2-secret-pw`
  and `dtmf 4242#`, capture all log output and recorded spans: neither string appears, the SIP username
  appears only masked.

---

## 12. Live test plan (user + coordinator)

Environment: example.org stack, new service `mautrix-jabbercalls` (§9.3), double puppeting on, OTLP on.
Phones: the **JMP test number is +15550004321** (the account's own number). Outbound tests call **only the
public test line +17377273232**. **Never call +15550001234** (the user's real number) without warning the
user first: in particular never press call and never run `!jc call` without a number in that number's
room (it is the room the user's own inbound test calls land in).

Pre-flight (needs the user's go-ahead, §15): agree on the window; confirm which XMPP account the new
service uses.

| # | Step | Expect |
|---|---|---|
| 0 | Baseline: standalone logs show "SIP registered"; note the ATA state. | Standalone registered. |
| 1 | **Stop the standalone SIP registration** (preferred: `docker compose stop mrtc-sip-bridge` in the stack dir: unregisters only its own Contact, keeps the login for restore; alternative: `!sip logout`, which deletes the stored credentials). | Log "SIP unregistered"; no Contact of the standalone left. |
| 2 | Deploy and start `mautrix-jabbercalls`; DM `@jabbercallsbot`; `!jc login password` (user enters the XMPP JID/password). | XMPP connected; bot replies; `!jc sip-status` says "no SIP account". |
| 3 | User texts the JMP number from the phone. | A Cheogram portal for the user's number appears (text bridged). Note this is the **714 room: no outbound calls here**. |
| 4 | `!jc sip-login` with a wrong password. | Prompt per field, password message redacted, "registrar rejected the credentials (SIP 401/403 ...)" within ~1 s, no retry storm. |
| 5 | `!jc sip-login` with the real JMP SIP credentials (user types them). | "Registered +15550004321 via ...". `sip-status` registered; PL sweep log line; the 714 room's power levels now include `call.member: 0`. |
| 6 | User calls +15550004321 from the phone; answer in Element X. | Rings in the **same room as step 3's text**, from the same ghost; 2+ min two-way audio; hang up from the phone; "Call · Nm Ns" notice. No "Incoming call" JMI notice. |
| 7 | Call again; decline in Matrix. Call again; let it ring out. | "Declined call" (caller to voicemail); "Missed call" after `ring_timeout_seconds`. |
| 8 | `!jc call +17377273232` in the management room; answer the ring. | A Cheogram portal for the test line is created (unencrypted), devices ring from its ghost; answer → dial → the line reads back the JMP number as caller ID. |
| 9 | During the call: `!jc dtmf 123#`, then `!jc hangup`. | Line recognises 123; command message redacted; digits not in logs; "Outgoing call · ..." notice. |
| 10 | In the **test line's** portal: press call in Sable (and Element X); then `!jc call` with no number in that portal. | Sable connects; Element X shows the known pickup bug (documented, not a failure); `!jc call` rings and connects. |
| 11 | `!jc call` inside a group MMS portal or a non-Cheogram DM (if one exists). | "Usage..." refusal; nothing dialled. |
| 12 | `docker compose restart mautrix-jabbercalls`, then one short inbound call. | Unregister on stop, re-register on start; the call rings in the same room. |
| 13 | Telemetry check (Datadog, service `mautrix-jabbercalls`). | `call.inbound`/`call.outbound` traces under the service; `cmd.sip_login*`, `bridge.sip_connect`; `mrtcsip.*` metrics present; log lines carry `trace_id` once; no SIP username unmasked, no digits, no password. |
| 14 | `!jc sip-logout`. | "SIP account removed"; a call to the JMP number no longer rings in Matrix (ATA/voicemail). |
| 15 | **Restore**: `docker compose stop mautrix-jabbercalls` (or keep it running without SIP), `docker compose start mrtc-sip-bridge` (if T1's refactor was deployed to the standalone, use the new image and place one inbound call as a regression check). | Standalone "SIP registered"; one inbound call rings in the standalone portal. |

Record the outcome in PLAN and tick the J boxes.

---

## 13. Doc edits

### 13.1 ADR-011 (T1 appends to `docs/DECISIONS.md`)

> **ADR-011: Phase J embedding: build-tagged integration on a private branch, shared host kit.**
> Status: accepted (2026-10-17). Context: ADR-006 option B; the user requires that no SIP content reaches
> the public mautrix-jabber repo. Decision: mautrix-jabber gains SIP calls only on the never-pushed local
> branch `local/sip-calls`, in `//go:build sipcalls` files behind SIP-free seams, built with a local
> `replace` and a Docker named build context; images are not pushed publicly. Host logic shared by both
> hosts moves to `pkg/sipaccount` (core-class) and `pkg/hostkit` (host-class). Answers to the PLAN
> "before J3" items: group MMS and MUC portals never get calls (`LookupCallRoom` accepts only
> `<E.164>@cheogram.com` 1:1 portals; inbound always maps to the 1:1 JID); jabber portals are not forced
> unencrypted (startup warning, core refusal notice, docs). SIP lifecycle follows the jabber login, not
> the XMPP session. JMI notices are suppressed for Cheogram 1:1 numbers while the login has a SIP account.
> Telemetry: jabber's sipcalls build initialises through this repo's `pkg/telemetry` with
> `ServiceName: mautrix-jabber`; `obs.AnnotateLogger` unifies log annotation. Would revisit if:
> mautrix-jabber's public repo ever accepts an optional SIP module, or the replace-based build becomes
> painful (then: private module proxy).

### 13.2 TELEMETRY-SPEC edits (T1)

- §3.2 "Embedded host": replace the `obs.Use(otel.Get...)` snippet with "jabber's sipcalls telemetry
  backend calls `telemetry.Init` of this repo with `Options.ServiceName = "mautrix-jabber"`, which calls
  `obs.Use`; jabber's annotation helpers delegate to `obs.AnnotateLogger`/`Logger`/`LoggerSC`."
  Replace the "Log-annotation convergence" paragraph accordingly (resolved by PHASEJ-SPEC §4.3).
- §3.3: add `AnnotateLogger` to the export list (amendment, Phase J).
- §5: add an "Embedded host (Phase J)" table with the host spans of §8 above.
- §8.2 Phase J: "jabber README documents" → "`docs/EMBEDDING.md` documents" (public README unchanged).
- §10 item 3: resolved (metrics come from this repo's `pkg/telemetry`).

---

## 14. Work split

```
T1 (mrtc: sipaccount + hostkit extraction, standalone refactor, core/obs/telemetry additions, ADR, TS edits)
 └── T2 (jabber: go.mod replace, seams, telemetry backend split + sipcalls backend, Docker, push guard)
      └── T3 (jabber: callsExt, config, accounts, CallHost, PL sweep, JMI suppression, RTC hook)
           └── T4 (jabber: sip-login/logout/status + call commands, privacy test)
T5 (docs: EMBEDDING.md, mrtc README embedded section, PLAN/CLAUDE status, jabber CLAUDE.md note,
    deploy-dev/jabbercalls templates)                                   parallel with T3/T4 after T2
Opus review of both diffs                                              after T4 + T5
Deploy + live test §12 (user go-aheads first)                          after review fixes
```

Recommended tiers: **T1 Opus** (concurrency-sensitive extraction of live-tested code, contract owner),
T2 Sonnet, **T3 Opus** (lifecycle races, takeover, CallHost semantics), T4 Sonnet, T5 Haiku/Sonnet.
Rules for every task: edit only the files you own (§2); do not change an (API) file's exported surface
(stop and report); **do not commit** unless the coordinator says so, and **never push** mautrix-jabber;
no new dependencies beyond those implied by the replace; every exported identifier has a doc comment;
no emojis; never log secrets, digits, SDP or raw SIP; no SIP words in untagged jabber files except those
listed in §4.

### T1: mrtc shared code (branch `phasej-embedding` in mrtc-sip-bridge)

Files: §2.1 rows marked T1. Spec: §3, §11.1, §13. Acceptance: mrtc acceptance commands pass; moved
tests pass; `-e` output byte-identical (report the diff command used); `go doc ./pkg/sipaccount` and
`./pkg/hostkit` match §3 signatures; importcheck passes with the new classification; `./build.sh`
succeeds; `git diff --stat` shows the standalone `pkg/connector` shrinking (report line counts).

### T2: jabber plumbing

Files: §2.2 rows marked T2. Spec: §4, §9.1, §9.2, §9.4, §11.2, telemetry part of §11.3. Acceptance:
untagged commands pass with an unchanged test count; tagged build compiles with `sipcalls.go` absent
(T3 adds it; until then the tagged build has no extension and must still compile and pass);
`git diff main --stat -- . ':!*sipcalls*' ':!go.mod' ':!go.sum' ':!Dockerfile.sipcalls' ':!build-sipcalls.sh'`
lists only the seam files of §2.2 and `grep -niE 'sip|mrtc' ` over that diff finds nothing;
`./build-sipcalls.sh` produces an image whose binary reports version `0.1.10+sipcalls`; the pre-push
hook rejects a dry-run push of `local/sip-calls` (`git push --dry-run origin local/sip-calls` fails).

### T3: embedded host

Files: `pkg/connector/sipcalls{,_config,_accounts,_host}.go` and their tests. Spec: §5, §6, §8, §10,
§11.3 (config/host/accounts). Acceptance: tagged commands pass with `-race -count=3` for
`./pkg/connector/`; no XMPP I/O on the `ResolveCallRoom`/`LookupCallRoom` paths (review); the resolver and
`suppressCallNotice` do no network I/O; untagged commands still pass.

### T4: commands

Files: `pkg/connector/sipcalls_commands{,_test}.go`. Spec: §7, §10, §11.3 (commands, privacy).
Acceptance: tagged commands pass; `!jc help` (manual run with a local config is optional) lists
`sip-login`, `sip-logout`, `sip-status`, `call`, `hangup`, `dtmf`; privacy test passes.

### T5: docs and deploy templates

Files: mrtc `docs/EMBEDDING.md` (host contract: implement `core.CallHost`, feed events with
`core.RegisterMatrixRTCHandlers` in PostInit, `obs.Use`/telemetry before Start, one `sipaccount.Runtime`
per process, account lifecycle, encryption, build tags; then a user section for the jabber embedded mode:
config, commands, ports, the "never register one SIP account from two bridges" warning, Element X pickup
bug, encrypted portals, telemetry behaviour change), `README.md` ("Embedded mode (mautrix-jabber)"
section pointing to EMBEDDING.md, Element X note repeated), `CLAUDE.md` + `docs/PLAN.md` (Current state,
J1-J8 boxes; J1 tag marked optional per D2), `deploy-dev/jabbercalls/{config-values.yaml,compose-snippet.yaml}`
(gitignored copies of §9.3); jabber `CLAUDE.md` (local branch): a short section "Local branch
`local/sip-calls` (never push): build tag `sipcalls`, files `*sipcalls*`, build with `./build-sipcalls.sh`,
spec in ../mrtc-sip-bridge/docs/PHASEJ-SPEC.md". Acceptance: links resolve; no secrets in tracked files.

### Final (coordinator)

Clean build/vet/test (both repos, both jabber tag sets, `-race`); Opus review against this spec
(lifecycle and takeover races, seams leak nothing, telemetry parenting/annotation, privacy, PL handling);
user go-aheads (§15); deploy (§9.3); live test (§12); PLAN Current state.
Commits only when the user asks; mautrix-jabber `local/sip-calls` commits are local only.

---

## 15. Items needing the user

1. **Go-ahead to stop (preferred) or log out the standalone `mrtc-sip-bridge` SIP registration** for the
   test window, and to restore it afterwards (§12 steps 1 and 15).
2. **XMPP login for the new service**: which JID `jabbercalls` should use (the same account as the old dev
   jabber service works: separate resource, but both would create rooms for the same contacts if the old
   service still runs; consider stopping the old, known-broken dev service). The user types the
   credentials (`!jc login password`).
3. **SIP credentials**: the user enters them with `!jc sip-login` (or explicitly allows copying them from
   the standalone's database on the Docker host).
4. **Synapse registration**: adding `registration.yaml` to `app_service_config_files` and restarting
   Synapse (inside the stack dir, but it interrupts the dev homeserver briefly).
5. **Docker Hub**: confirm sipcalls images stay off the public `williamkray/mautrix-jabber` repository
   (recommended, D16), or name a private repository.
6. **Host firewall**, only if inbound audio/INVITEs fail: opening UDP 5070-5079 and 40200-40300 on the
   Docker host is outside the stack dir (Phase 1/2 needed no host-level change for the standalone).
7. The phone calls themselves (user's phone → +15550004321; the user answers and judges audio), and
   reading back the test line results.
8. Choice for `backfill.enabled` on the new service (default here: off).

---

## 16. Open questions (defaults chosen)

1. `sip-login` takes no inline arguments (passwords only via the redacted prompt). Default: prompt only.
2. Auth-failure notice to the management room (D5): one per account until it registers again. Default: on.
3. Call-created portals get no MAM backfill (D7); history appears as new texts arrive (or via
   `sync-roster` full backfill when enabled). Default: no backfill on call creation.
4. If the user later wants calls in the old dev service or production, they switch the image and set
   `network.calls.enabled`; nothing else in their config changes.
