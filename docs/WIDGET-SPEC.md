# Dialpad widget spec (ADR-008 option 1)

Status: **design, not implemented** (2026-10-17, branch `widget-dialpad`). Implements ADR-008 option 1
("Matrix widget") for both hosts: the standalone bridge and the embedded mautrix-jabber host (Phase J,
local branch `local/sip-calls`, never pushed).

Goal: a "Dialpad" room widget in every 1:1 number portal and in the bridge bot's management room. In
widget-capable clients it gives a small UI to:

1. call the room's contact with one click (same as `call` with no number in a portal),
2. dial any number (same as `call <number>`: creates or opens the portal and starts the callback ring),
3. send DTMF during a call (same as `dtmf`),
4. hang up (same as `hangup`).

The widget only **triggers** calls. Audio still flows through the client's own Element Call (MatrixRTC)
exactly as in Phase 3: a command-placed call rings the owner's devices first (callback ring, PHASE3-SPEC
D6) and dials when the owner answers. Bot commands stay; the widget is an extra.

---

## 1. Client support (research, 2026-10-17)

| Client | Renders room widgets (`im.vector.modular.widgets`) | Widget API, `get_openid` | Notes |
|---|---|---|---|
| **Sable, sable-next** (Svelte + Rust core, web/desktop/Android via Tauri) | **Yes** | **Yes, granted without a prompt** | Target client |
| **Sable, old TS client** (github.com/SableClient/Sable) | **Yes** | **Yes, granted without a prompt** | |
| **gomuks web** | **Yes** | **Yes, granted without a prompt** (token cached) | Target client |
| **Element X Android / iOS** | **No** | Element Call only | Users keep the bot commands |
| Element Web / Desktop | Yes (also honours `io.element.widgets.layout`) | Yes, with a user prompt ("remember" option) | Deprioritized by the user; it should work but nothing is designed for it |

Sources (shallow clones, commit and file:line):

- **sable-next** `87a2f331de9ea4f708c2fd89a1228f1fb43c3f7a` (git.sable.moe/SableClient/sable-next, 2026-10-17):
  - Reads widgets from `im.vector.modular.widgets` state only: `src/lib/features/room/room-session.svelte.ts:97-103`
    (`refreshWidgets`, `event_type: 'im.vector.modular.widgets'`). The Rust watcher reloads on
    `im.vector.modular.widgets` or `m.widget` (`crates/sable-core/src/widgets.rs:920`), but the list itself
    is read from `im.vector.modular.widgets` only. Parsing needs a non-empty state key, a string `type` and an
    http(s) `url` (`src/lib/features/widgets/widget-content.ts:9-34`).
  - Widgets show in a side panel (desktop) or a modal opened from the room header menu (phone; button
    shown when the room has widgets, `src/lib/features/room/RoomView.svelte:983-985`). The panel lists
    widgets by name, and the user taps one to open it (`WidgetsPanel.svelte`). Nothing auto-opens, and
    `io.element.widgets.layout` is ignored (no reference in the tree).
  - Iframe host: `ClientWidgetApi` from `matrix-widget-api` ^1.19.0 with `waitForIframeLoad: true` forced
    (`WidgetFrame.svelte:26-31`). Sandbox `allow-forms allow-pointer-lock allow-popups
    allow-popups-to-escape-sandbox allow-presentation allow-same-origin allow-scripts allow-downloads`
    (`WidgetFrame.svelte:70`).
  - URL templating via `runTemplate` (`$matrix_room_id`, `$matrix_user_id`, `$matrix_widget_id`,
    `$org.matrix.msc2873.client_theme`, ...). `widgetId` is added to the query if missing. No `parentUrl`
    (`src/lib/features/widgets/widget-url.ts:19-47`).
  - **`get_openid`**: `SableWidgetDriver.askOpenID` requests a token from the homeserver and answers
    `Allowed` without asking the user (`src/lib/features/widgets/widget-driver.ts:315-332`). In
    matrix-widget-api, `get_openid` is not gated by a capability (`src/ClientWidgetApi.ts:449-511`,
    matrix-org/matrix-widget-api `19e340c`).
  - **Capability limits**: `validateCapabilities` opens an approval dialog **every time the widget loads**
    when it requests any capability (`widget-driver.ts:101-103` → `WidgetsPanel.svelte:103-113`), and
    there is no "remember". An **empty request is approved silently** (`WidgetsPanel.svelte:105`,
    `if (requested.size === 0) return new Set();`). `m.allow_openid`, `m.always_on_screen` and to-device
    sends are flagged as sensitive in the dialog (`capabilities.ts:1-7`).
    **Consequence: the dialpad requests no capabilities**, so it opens with no prompt in sable-next.
- **old Sable** `3adfe1e6cddf9cc724574a6bf58fb7eac041d294`: `GenericWidgetDriver.askOpenID` returns
  `Allowed` with `mxClient.getOpenIdToken()`, no prompt (`src/app/features/widgets/GenericWidgetDriver.ts:248-253`).
  Widgets come from `im.vector.modular.widgets` (`src/types/matrix/room.ts:17`,
  `src/app/hooks/useRoomWidgets.ts:96-110`). There is a widgets drawer on desktop and a full-screen modal on
  mobile (`src/app/features/room/Room.tsx:151-170`). URL templating adds `widgetId` and `parentUrl`
  (`useRoomWidgets.ts:15-56`).
- **gomuks** `3bf3026f2e5816366f6c55d9b8793119da041145` (github.com/tulir/gomuks, web frontend): widget list
  from `im.vector.modular.widgets` in the right panel (`web/src/ui/rightpanel/WidgetList.tsx:24-40`).
  `askOpenID` fetches through the backend RPC `request_openid_token` with no prompt, and caches the token
  for half its lifetime (`web/src/ui/widget/widgetDriver.ts:258-271`). `validateCapabilities` always opens
  `PermissionPrompt` for non-Element-Call widgets (`widget.tsx:77-92,156`). **UNVERIFIED**: whether it
  shows an empty dialog for an empty request (one extra tap at worst). `parentUrl` is set (`widget.tsx:44`).
- **Element X Android** `ea8d647daab24eca15f813ea03c83f9c82ed0e3c`: the only widget code is the Element Call
  driver (`libraries/matrix/impl/.../widget/RustWidgetDriver.kt`, `DefaultCallWidgetSettingsProvider.kt`,
  used only by `features/call`). There is no reference to `im.vector.modular.widgets`. **Element X iOS**
  `734c0678fa2affe485746126458fca888639e608`: same, only `ElementCallWidgetDriver.swift`.
- **Element Web** `fc4d45cce130f3dce0d824ecef87bd948bcef842`: `ElementWidgetDriver.askOpenID`
  (`apps/web/src/stores/widgets/ElementWidgetDriver.ts:654`) prompts. Layout handling is in
  `apps/web/src/stores/widgets/types.ts` / `WidgetLayoutStore`.

**Element X alternative (short note, not designed):** the widget page needs a Widget API parent for
OpenID, so it does not work as a standalone link. Element X users keep `call`/`dtmf`/`hangup`. A
standalone PWA with its own Matrix login, or quick-reply bot messages, could come later. Out of scope here.

---

## 2. Architecture overview

```
 Sable / gomuks iframe ──postMessage──> client (get_openid → token)
        │  same-origin fetch, Authorization: Bearer <session>
        ▼
 Traefik (Host example.org && PathPrefix /sip-dialpad/)  ──>  bridge AS listener :29319
        │                                                        │ http.ServeMux (AS.Router)
        │                                                        ├── /_matrix/app/...  (not routed publicly)
        │                                                        └── /sip-dialpad/     pkg/widget.Server
        ▼                                                                   │
                                     hostkit actions (shared with bot commands) → calls.Manager
```

- New package **`pkg/widget`** (host-class, like `pkg/hostkit`: may import bridgev2; add it to
  `hostOnlyDirs` in `internal/importcheck`). It holds the HTTP server, auth, rate limits, the installer
  and the embedded static assets (`go:embed static/*`).
- **`pkg/hostkit`** gains typed call actions (refactored out of `commands.go`) and the shared
  `WidgetConfig` (part of `hostkit.Config`, so both hosts get identical keys, defaults and validation).
- **`pkg/calls`** gains a read-only status snapshot (`Manager.Snapshot`).
- Hosts supply a small `widget.Deps` (room → login and number resolution, the call API, the bot intent),
  and mount the handler on their appservice router.

---

## 3. Hosting

### 3.1 Listener: the existing appservice HTTP server, under a path prefix

Mount `widget.Server` on the bridgev2 appservice router: `br.Matrix.(*matrix.Connector).AS.Router` (an
`*http.ServeMux`; `ServeMux.Handle` is safe after the server started). Do **not** use
`MatrixConnectorWithServer.GetRouter()`: it returns nil unless `appservice.public_address` is set
(mautrix v0.32.0 `bridgev2/matrix/connector.go:295-300`), and setting that changes other framework
behaviour (public media). One process, one port, no new listener. The prefix comes from the path of
`widget.public_url` (Traefik does not strip it), so the bridge serves exactly the public path.

Rules:
- Pattern `<prefix>` (trailing slash), with sub-routes `<prefix>` (index.html), `<prefix>static/…`,
  `<prefix>api/v1/…`. Everything else under the prefix returns 404.
- Validation: the prefix must start with `/`, end with `/`, and must not be `/` or start with `/_matrix/`.
  Otherwise mounting would shadow appservice routes.
- Only the prefix is exposed publicly (Traefik rule below). `/_matrix/app/...` stays internal.
- Handlers are bounded: `http.MaxBytesReader` 4 KiB on API bodies, a 10 s context timeout per request,
  and no long-lived connections (polling, not SSE, see §7.4). The AS transaction handler shares this
  server, so nothing here may block for long.

### 3.2 Dev stack (<synapse-host>:/opt/services/<stack>)

`*.example.org` has **no wildcard DNS** (checked: `randomxyz.example.org` does not resolve). A new
hostname would need a DNS record, which the user would have to add. Instead, use **path prefixes on the
existing `example.org` host** (DNS and an LE cert already exist; router `dev-mm-site` there is a
catch-all redirect for `/`, and nginx serves `/.well-known` at priority 1). An explicit priority of 300
puts our routers above both. Synapse is on `synapse.example.org` and is not affected.

Compose changes (inside `/opt/services/<stack>`, no change outside it is needed). Replace
`traefik.enable=false` on the two bridge services with:

```yaml
  mrtc-sip-bridge:
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=traefik6"
      - "traefik.http.routers.dev-mm-sipwidget.entrypoints=websecure"
      - "traefik.http.routers.dev-mm-sipwidget.tls.certresolver=le"
      - "traefik.http.routers.dev-mm-sipwidget.rule=Host(`example.org`) && PathPrefix(`/sip-dialpad/`)"
      - "traefik.http.routers.dev-mm-sipwidget.priority=300"
      - "traefik.http.routers.dev-mm-sipwidget.service=dev-mm-sipwidget"
      - "traefik.http.services.dev-mm-sipwidget.loadbalancer.server.port=29319"

  mautrix-jabbercalls:
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=traefik6"
      - "traefik.http.routers.dev-mm-jcwidget.entrypoints=websecure"
      - "traefik.http.routers.dev-mm-jcwidget.tls.certresolver=le"
      - "traefik.http.routers.dev-mm-jcwidget.rule=Host(`example.org`) && PathPrefix(`/jabber-dialpad/`)"
      - "traefik.http.routers.dev-mm-jcwidget.priority=300"
      - "traefik.http.routers.dev-mm-jcwidget.service=dev-mm-jcwidget"
      - "traefik.http.services.dev-mm-jcwidget.loadbalancer.server.port=29318"
```

Both services are already on `traefik6`. Bridge configs: `widget.public_url:
https://example.org/sip-dialpad/` (standalone `network.widget`) and `https://example.org/jabber-dialpad/`
(jabber `network.calls.widget`).

**Needs the user only if** they prefer dedicated hostnames (e.g. `dialpad.example.org`): that needs a DNS
CNAME to `example.org` (outside the directory), then swap the `Host(...)` rule and drop the `PathPrefix`.

Production deployments: any HTTPS reverse proxy that forwards `<prefix>` to the AS port works. HTTPS is
required (widgets are framed by HTTPS clients; mixed content is blocked).

---

## 4. Configuration (shared, `hostkit.WidgetConfig`)

New `Widget WidgetConfig yaml:"widget"` field on `hostkit.Config`. That puts it at `network.widget` in the
standalone bridge and at `network.calls.widget` in jabber; the jabber config already embeds
`hostkit.Config`. Example YAML goes into the shared example block, and `hostkit.CopyKeys` copies the keys
in both upgraders.

```yaml
widget:
    # Add a Dialpad widget to number rooms and the management room (Sable, gomuks; not Element X).
    enabled: false
    # Public HTTPS URL of the widget, ending in "/". Its path is served on the appservice listener,
    # so forward it there from your reverse proxy (only this path, never /_matrix/app).
    public_url: ""
    # Secret for the per-room widget keys. "generate" is replaced with a random value on start.
    # Changing it changes every widget URL (the next sweep updates the rooms).
    # A generated secret is written back to this file, so the file must be writable; otherwise set
    # the secret explicitly (a new one on every start would change every widget URL).
    secret: generate
    # Origins allowed to frame the widget (CSP frame-ancestors). "*" allows any client.
    frame_ancestors: ["*"]
    # Users of other homeservers may use the widget (their OpenID token is checked over federation).
    allow_remote_users: false
    # When enabled is false: remove existing Dialpad widgets from rooms on the next start.
    remove_when_disabled: false
    # Reverse proxies (CIDRs or IPs) whose last X-Forwarded-For hop is used as the client address for
    # rate limits. Empty: only private and loopback peers are believed.
    trusted_proxies: []
```

Validation (`Config.Validate(prefix)`), only when `enabled`:
- `public_url` is an absolute `https://` URL (`http://` only for `localhost`/`127.0.0.1`, for tests),
  with no query or fragment, and its path is a valid prefix (§3.1).
- `secret` is at least 32 characters after generation.
- each `trusted_proxies` entry is a CIDR or an IP address; `public_url` does not spell out the default
  port (`:443` for https, `:80` for http), because browsers omit it from `Origin` (the server also
  normalizes it away).
- each `frame_ancestors` entry is `*`, `'self'`, or a scheme-plus-host source without spaces or `;`.

`secret: generate` is handled in the upgrader: if the value is `generate`, write `random.String(64)`
(go.mau.fi/util/random) to the upgraded config, the same pattern bridgev2 uses for `as_token`. Jabber:
same helper, called from `callsExt.upgradeConfig`.

---

## 5. Authentication and authorization

### 5.1 Threat model

- The API places PSTN calls, so it must act only for the bridge user who owns the room's login, and
  only in rooms that user may act in (the same rules as the bot commands).
- **sable-next, old Sable and gomuks hand an OpenID token to *any* widget without asking** (§1). OpenID
  alone would therefore let any widget the user opens, in any room, act on our API as that user. So we add
  a **room-bound widget key** that only our widget's URL carries.
- Tokens must never appear in logs, traces, URLs sent to servers, or Traefik access logs.

### 5.2 Widget URL and the room key

The installer writes this URL (§6.2):

```
<public_url>?widgetId=$matrix_widget_id&theme=$org.matrix.msc2873.client_theme#room=<urlenc room id>&k=<key>
```

- `k = base64url(HMAC-SHA256(secret, "mrtcsip-dialpad-v1\x00" + roomID))[:22]` (128 bits).
- `room` and `k` are in the **fragment**, so the browser never sends them to the server and they never
  appear in access logs. The template variables are in the query, because clients template the whole
  string and the server does not need them. `widgetId` is needed for Widget API messages.
- The room state that carries `k` is readable only by the room's members: the owner, the bot and the
  ghost in a portal, and the owner and the bot in the management room. A widget in another room cannot
  read it without a cross-room state capability, which clients prompt for.
- **Residual risk.** Anyone who can read the room's state holds `k`: invited users, members of other
  homeservers through federation, and past members. `k` alone is not enough (a valid OpenID identity that
  passes `ResolveRoom` is still required), and it can only be revoked by rotating `widget.secret`. The
  management-room key is the most powerful one. Mitigation: `widget.PrivateRoom(ctx, br, room,
  allowed...)` (bot state store only, no network I/O) is false when a joined or invited member is
  neither the bot, a ghost of this bridge nor in `allowed`. The installer uses it (default rule: at most
  one bridge user, optionally `Deps.RoomOwners`; `Deps.RoomMembers` overrides the member source) and skips
  rooms that fail it (debug log; `Ensure` returns `ErrNotPrivateRoom`, the command replies
  `MsgNotPrivateRoom`). Hosts' `ResolveRoom` should return `Refuse(MsgNotPrivateRoom)` for such rooms
  (`"This room has other members; the Dialpad works only in private bridge rooms."`).

### 5.3 Session exchange

1. The page loads, finishes the Widget API handshake (it answers `capabilities` with `[]`), then sends
   `fromWidget` `get_openid`. Possible answers: `allowed` with credentials; `request`, followed later by
   `toWidget` `openid_credentials`; or `blocked`, which shows "This client did not allow sign-in".
2. `POST <prefix>api/v1/session` with the JSON body `{"openid": {access_token, matrix_server_name,
   expires_in}, "room_id": "...", "key": "...", "widget_id": "..."}`.
3. Server checks, in this order (each failure returns 401/403 with a generic error and increments the
   failure rate limit):
   1. `key` equals `k(room_id)` (constant-time compare).
   2. `matrix_server_name` is a valid server name. If it equals `br.Matrix.ServerName()`, verify with
      `GET <homeserver.address>/_matrix/federation/v1/openid/userinfo?access_token=…`, which avoids
      federation discovery. Otherwise refuse unless `allow_remote_users`, and then use mautrix
      `federation.Client.GetOpenIDUserInfo` (it already rejects a `sub` from a different server,
      `federation/client.go:281-296`). For the local path, apply the same check: `sub.Homeserver()` must
      equal the server name. Use a 5 s timeout and an `obs.WrapHTTPTransport` client with a fixed
      `url.template` (`/_matrix/federation/v1/openid/userinfo`), so the token never reaches a span.
      The federation client's `.well-known` fetch uses an HTTP client that dials through the client's
      IP-filtering dialer (`AllowIP` = `DefaultAllowIP` minus 100.64.0.0/10), with timeouts and no
      redirects; at most 256 distinct remote server names are resolved per hour (more: transient
      `openid_error`). Resolution, IP-filter and transport errors are transient (not negative-cached);
      only a 4xx answer or a `sub` on another server rejects the token.
      Dev check: Synapse's 8008 listener serves `client` + `federation` (`homeserver.yaml` listeners), and
      Traefik routes all of `synapse.example.org` to it.
   3. `br.GetExistingUserByMXID(sub)` exists (never create users) and has `Permissions.Commands`.
   4. Room scoping: `Deps.ResolveRoom(ctx, user, roomID)` (§5.4) returns a room context. If it refuses,
      the response is 403 with the refusal text.
4. Response: `{"session": "<43-char base64url random>", "expires_in": <s>, "room": {kind, number}}`.
   Sessions live in memory: a map of `sha256(session)` to `{mxid, roomID, expiry}`. Expiry is
   `min(openid expires_in, 3600 s)`. Cap 8 sessions per user (oldest evicted) and 1000 overall. A restart
   drops all sessions, and the page silently gets a new OpenID token and exchanges again (no prompt in the
   target clients).
5. Every later request sends `Authorization: Bearer <session>`. The session is bound to its room: the
   room is taken from the session, never from the request.

### 5.4 Room scoping (`Deps.ResolveRoom`)

Same rules as the commands' `Login` and `RoomNumber` deps:

| Room | Login | Number |
|---|---|---|
| A portal of the bridge with `Receiver` = login L | L, if `L.UserMXID == user` (else refuse) | `RoomNumber(portal)`; only 1:1 number portals (standalone: E.164 portal ID; jabber: `isCallPortal`, never group MMS or MUC) |
| `user.ManagementRoom` | `user.GetDefaultLogin()` | none (dial by number only) |
| anything else | refuse ("This room is not a bridge room you own") | |

Jabber additionally applies `commandLoginWithAccount`: the login must be a real `*xmppClient` login and
have a SIP account. The refusal texts are the same as the commands' (e.g. "No SIP account is set up for
this login. Use `!jc sip-login`."). For the API, a refusal in step 3.4 is not fatal for the session
when the room is valid but the login has no SIP account. The session is created and `GET state` reports
`login.ready=false` with the refusal text, so the widget can show the hint.

Resolution runs on every action (not only at session creation). A logout or a sip-logout during a
session takes effect immediately.

### 5.5 Browser-side protections

- No cookies, and therefore no CSRF surface. State-changing requests additionally require
  `Content-Type: application/json` and, when an `Origin` header is present, `Origin` must equal the
  origin of `public_url` (requests from the widget page are same-origin). No CORS headers are ever sent.
- Headers on every response: `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`. API:
  `Cache-Control: no-store`. HTML: `Content-Security-Policy: default-src 'none'; script-src 'self';
  style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none';
  frame-ancestors <widget.frame_ancestors>`. No `X-Frame-Options`, because the page must be framable.
  No inline scripts or styles, and no external CDNs or fonts.
- postMessage: the page accepts messages only where `ev.source === window.parent`. It posts to the
  `parentUrl` origin when the query has one (old Sable, gomuks), otherwise to `"*"`. Requests we send
  carry no secrets; the token comes *to* us.

### 5.6 Rate limits (in memory, token buckets)

| Key | Limit | On exceed |
|---|---|---|
| `session` per client IP (last `X-Forwarded-For` hop only when the peer is in `widget.trusted_proxies`, or private/loopback when that list is empty; IPv6 keyed by /64) | 10/min | 429 + `Retry-After` |
| failed `session` per IP | 5/min, then 15 min lockout | 429 |
| `session` per MXID | 20/min | 429 |
| `call` per MXID | 5/min | 429 |
| `normalize` per MXID | 60/min | 429 |
| `dtmf` per MXID | 10/s, burst 32 (the media queue still applies `ErrDTMFBusy`) | 429 |
| `hangup` per MXID | 30/min | 429 |
| `state` per session | 4/s | 429 |

Buckets are evicted after 30 min idle.

### 5.7 Privacy

Never log or trace: OpenID tokens, session tokens, `k`, request bodies, or DTMF digits (only the digit
count, as `dtmf.digit_count`). Numbers and MXIDs are allowed on spans and logs, never on metrics. The
mautrix `requestlog` access logger is **not** used on the widget routes. Our own log line per action has
`action`, `user`, `room_id`, `outcome`, plus `to` for `call`. Static asset requests are not logged. SIP
credentials, registrar details and the login's own number are never returned by the API; the
login's number appears only implicitly, through the self-dial error.

---

## 6. Installation in rooms

### 6.1 Who sends it, power levels

The **bridge bot** sends the state event. It is in every portal: ADR-010 was rejected, bridgev2 creates
portals as the bot (PL 9001, or creator in room v12), and the jabber bot is required in its rooms. The bot
also creates management rooms (`User.GetManagementRoom` → `Bot.CreateRoom`). `im.vector.modular.widgets`
falls under `state_default` (50), and the bot is above that. If a management room was created by the
user, the bot may have PL 0. The send then fails with `M_FORBIDDEN`: count it as `failed`, log at Debug,
and mark the room as done for this process (no retry loop). The `widget` command (§6.5) explains the fix.
No ghost is involved.

### 6.2 Content

Event type `im.vector.modular.widgets` (the only type all three target clients read), state key
`<appservice id>_dialpad`, so the two bridges never collide:

```json
{
  "type": "m.custom",
  "url": "<public_url>?widgetId=$matrix_widget_id&theme=$org.matrix.msc2873.client_theme#room=%21abc%3Aexample.org&k=…",
  "name": "Dialpad",
  "id": "<state key>",
  "creatorUserId": "<bot mxid>",
  "waitForIframeLoad": true,
  "data": {"title": "Call +15551234567"}
}
```

`data.title` is set only in portals. `waitForIframeLoad: true` matches sable-next (which forces it), so
the page must **not** send `content_loaded`. `io.element.widgets.layout` is not sent: only Element Web
reads it, and it is deprioritized.

### 6.3 Which rooms, when (idempotent)

Eligible rooms: every 1:1 number portal with an MXID whose receiver login is eligible (standalone: any
login; jabber: logins with a SIP account), plus `user.ManagementRoom` (only if already set, never create
one) of every user with an eligible login.

`widget.Installer` (one per host) has:
- `Ensure(ctx, roomID, title)`: read the current state with `br.Matrix.GetStateEvent(ctx, room,
  type, key)`. If the `url`, `name` and `data` already match, do nothing. Otherwise send as the bot. The
  room ID and a hash of the content go into an in-memory `done` set, so each room costs at most one
  state read per process (and per content change).
- A **worker goroutine** with a bounded queue (256). `Enqueue(roomID, title)` never blocks, and drops the
  room when the queue is full (the next sweep catches it).
- **Triggers:**
  1. Startup sweep after logins load (standalone `Start`; jabber after each login's PL sweep, which runs
     when the SIP account attaches): all eligible rooms.
  2. Periodic sweep every 10 min: only rooms not yet in `done` (covers portals created by texting or
     `start-chat`).
  3. `ResolveCallRoom` with `Created=true` in both hosts: enqueue the new portal.
  4. Any `call`/`hangup`/`dtmf`/`widget` command in an eligible room: enqueue that room.
  All of these enqueue only; Matrix I/O happens on the worker. Nothing runs on the appservice event path.
- **Removal:** `enabled: false` mounts nothing and runs no sweep. With `remove_when_disabled: true`, one
  startup sweep sends empty content (`{}`) for our state key wherever it is non-empty.
- `public_url` or `secret` changes alter the content hash, so the next sweep updates every room.

### 6.4 Migration

Existing rooms get the widget from the first startup sweep (trigger 1): one state event per room, shown
as "sipbot added widget Dialpad". No DB migration (the `done` set is in memory). bridgev2 metadata is not
used, because jabber's portal metadata types live in public files.

### 6.5 `widget` bot command (shared, hostkit)

`$cmdprefix widget` (`RequiresLogin`, wrapped in `WithSpan`): in an eligible room, runs `Ensure`
synchronously (5 s timeout) and replies "The Dialpad widget is in this room's widget list (Sable,
gomuks). Element X does not show widgets: use `call`, `dtmf` and `hangup`." On `M_FORBIDDEN`, the reply
asks the user to give the bot power level 50 in this room. When the widget is disabled, the reply says so.

---

## 7. HTTP API (`<prefix>api/v1/`)

JSON in and out. Errors: `{"error": "<code>", "message": "<user-facing text>"}`. The codes are stable,
and the messages are the same strings the commands reply with.

| Method, path | Body | Response | Notes |
|---|---|---|---|
| `POST session` | §5.3 | §5.3 | no auth header |
| `GET state` | | `{room:{kind:"portal"\|"management", number}, login:{ready, message}, call: null \| CallView}` | polled |
| `POST normalize` | `{number}` | `{ok, e164, error?, message?}` | `phone.Normalize` + `CheckDestination` (prefix, self, NANP); no call |
| `POST call` | `{}` or `{number}` | `{ok:true, number, message}` or error | `{}` only in portals (the room's number, as `call` without args) |
| `POST dtmf` | `{digits}` | `{ok:true, count}` or error | 1-32 of `0-9*#A-D` |
| `POST hangup` | `{}` | `{ok:true}` or error | |

`CallView` = `{id, direction:"inbound"|"outbound", phase, number, room_id, in_this_room, started_at,
answered_at, can_dtmf}`. `phase` is a UI enum mapped from the actor state:
- outbound `setup`/`ringing` → `ringing_you` ("Answer on your device to connect")
- `joining` → `connecting`
- `dialing` → `calling`
- `early` → `ringing_them`
- `active` → `connected`
- inbound `ringing`/`answering` → `incoming`
- `ending` → `ending`

`can_dtmf` = connected outbound or active inbound.

Error codes (from the shared actions, §8.1): `invalid_number`, `not_allowed`, `self_dial`, `line_busy`,
`outbound_disabled`, `no_active_call`, `not_connected`, `dtmf_unsupported`, `invalid_digits`,
`dtmf_busy`, `not_ready` (manager nil), `login_required` (refusal text), `unauthorized`, `forbidden`,
`rate_limited`, `bad_request`, `internal`.

Scoping, as for the commands: in a portal, `hangup`/`dtmf` target the call in *that* room
(`Hangup(login, roomID)`); in the management room they target the login's active call in any room
(`roomID=""`). `call {number}` from any room is the `call <number>` path (it may create a portal; the
callback ring happens in that portal). The response and `state.call.room_id` give the portal, so the
widget can offer "Open room" (§9).

### 7.4 Polling, not SSE

`GET state` every 1 s while `call != null` or within 10 s after an action, and every 5 s otherwise.
Polling stops while `document.hidden`. It is simple, survives Traefik and proxies, and needs no
long-lived handler on the AS server. SSE can come later if it is ever needed.

---

## 8. Shared code changes

### 8.1 `pkg/hostkit/actions.go` (refactor; command replies unchanged)

```go
type Result struct { OK bool; Code, Message string; Number string; Count int }
func PlaceCallAction(ctx, api CallAPI, login core.LoginRef, req calls.PlaceCallRequest, roomNumber string) Result
func HangupAction(ctx, api CallAPI, login core.LoginRef, room id.RoomID) Result
func DTMFAction(ctx, api CallAPI, login core.LoginRef, room id.RoomID, digits string) Result
func CheckNumberAction(api CallAPI, login core.LoginRef, number, defaultCC string) Result // normalize + CheckDestination
```

`callReply`/`hangupReply`/`dtmfReply`/`placeErrReply` become thin wrappers that return `Result.Message`.
The existing command tests must pass unchanged. In the portal room form, `PlaceCallAction` includes the
`CheckDestination(roomNumber)` pre-check that `cmdCall` does today. `CallAPI` gains
`Snapshot(login core.LoginRef) (calls.Snapshot, bool)`.

### 8.2 `pkg/calls` status snapshot

```go
type Snapshot struct {
    ID string; Direction string; State string // state.String()
    Outbound bool; RoomID id.RoomID; Remote string // E.164, "" until known
    StartedAt, AnsweredAt time.Time
}
func (m *Manager) Snapshot(login core.LoginRef) (Snapshot, bool) // the login's active call, under m.mu
```

The actor owns `c.state`. Add `pub atomic.Pointer[Snapshot]` on `call`, written only by the actor through
a new `c.setState(s)` helper. All 13 `c.state = …` sites go through it, and the remote number and
answer time are set at the points where they become known. `Snapshot` loads the pointer, so it takes no
actor locks and does no I/O. Busy (second) calls are not reported; only `m.active[login]` is.

### 8.3 `pkg/widget`

- `server.go`: `New(cfg hostkit.WidgetConfig, deps Deps) (*Server, error)`; `Handler() http.Handler`;
  routing, headers, body limits, JSON helpers.
- `auth.go`: key derivation, OpenID verification (local HS / federation), session store.
- `ratelimit.go`.
- `api.go`: handlers → `Deps.ResolveRoom` → hostkit actions; `CallView` mapping.
- `installer.go`: `Installer` (content builder, `Ensure`, queue worker, sweeps, removal).
- `telemetry.go`: spans and metrics (§10).
- `static/index.html`, `static/app.js`, `static/app.css` (embedded).

```go
type RoomContext struct {
    Kind     string                 // "portal" | "management"
    Login    *bridgev2.UserLogin    // nil when not ready
    Number   string                 // portal E.164
    Refusal  string                 // set when Login == nil
}
type Deps struct {
    Bridge      *bridgev2.Bridge
    Calls       func() hostkit.CallAPI           // nil while starting
    ResolveRoom func(ctx context.Context, user *bridgev2.User, roomID id.RoomID) (RoomContext, error) // error = not an eligible room
    EligibleRooms func(ctx context.Context) ([]RoomRef, error) // sweep source: {RoomID, Title}
    HomeserverURL string; ServerName string
    Log         zerolog.Logger
}
```

The widget never calls SIP or the core directly, only through `CallAPI`.

---

## 9. UI (static HTML/JS, no framework)

One page, about 300 lines of hand-written JS with no build step. It includes its own minimal Widget API
client (our own code, about 120 lines): it answers `toWidget` requests (`capabilities` → `[]`,
`supported_api_versions`, `notify_capabilities`, `visibility`, `theme_change`, and anything else with an
empty ack), and sends `fromWidget` `get_openid`. matrix-widget-api is not vendored. If that ever changes,
it is Apache-2.0 and needs a CREDITS entry.

Layout (mobile first, works in a 300 px side panel):
- Status line (`aria-live="polite"`): "Ready", "Ringing your devices: answer to connect", "Calling
  +1…", "Connected 02:13", "Incoming call", or error text.
- **Portal:** a large "Call +1 555 123 4567" button (`POST call {}`). "Dial another number" opens the
  number field.
- **Number field** (`inputmode="tel"`, `autocomplete="tel"`): debounced (300 ms) `POST normalize`
  shows "Will call +15551234567" or the error text. The Call button stays disabled until the number is
  valid. The server is the single source of truth; the JS has no number-parsing rules of its own.
- **Keypad** 3×4 (`1-9 * 0 #`, with letter subtitles, A-D behind a toggle). When idle, a key adds to the
  number field. When `can_dtmf`, each press sends `POST dtmf` (presses are coalesced while one request is
  in flight, ≤32 digits). The keys pressed are shown only in the page, masked after 5 s.
- **Hang up** button (red, replaces Call while `call != null`). Inbound `incoming` shows "Decline".
- After `call {number}`: when `state.call.room_id` differs from the current room, show "Open room" as a
  `https://matrix.to/#/<room_id>` link (new tab). Optional progressive step: ask for the
  `org.matrix.msc2931.navigate` capability by renegotiation only when clicked (sable-next shows a dialog,
  then navigates in-app).
- Keyboard: digits, `*`, `#`, Enter = call, Escape = hang up. Buttons ≥48 px, visible focus ring, labels
  on every control, `prefers-reduced-motion` respected. Theme comes from the `theme` query parameter, with
  `prefers-color-scheme` as fallback.
- Error states: no Widget API parent ("Open this from the Dialpad widget in a Sable or gomuks room"),
  OpenID blocked, 401 (re-exchange once, then show the error), 403 refusal text, 429 ("Slow down").

---

## 10. Telemetry (per TELEMETRY-SPEC)

Add to the span catalogue (§5.11-style table, new §5.12) and the metric table (§4.3), plus the README
"Tracing and metrics" section, in the same change.

| Span | Kind | Class | Attributes |
|---|---|---|---|
| `widget.session` | SERVER | root | `matrix.sender` (on success), `matrix.room_id`, `widget.room.kind`, `outcome` (ok, bad_key, openid_invalid, openid_error, no_user, forbidden, rate_limited) |
| `widget.call` | SERVER | root | `matrix.sender`, `matrix.room_id`, `call.remote.e164`, `widget.call.form` (room, number), `outcome` = error code or ok. `call.outbound` links to it (PlaceCall reads the ctx span, as for `cmd.call`) |
| `widget.dtmf` | SERVER | root | `matrix.sender`, `matrix.room_id`, `dtmf.digit_count`, `outcome` |
| `widget.hangup` | SERVER | root | `matrix.sender`, `matrix.room_id`, `outcome` |
| `widget.openid.verify` | CLIENT | child-only (under `widget.session`) | `server.address`, `outcome`; the HTTP client span uses the fixed template |
| `bridge.widget.sweep` | INTERNAL | root (startup) / linked root (login attach); periodic passes start a span **only** when they have unseen rooms | `rooms.checked`, `rooms.installed`, `rooms.updated`, `rooms.failed`, `trigger` (startup, login, periodic, disable) |

`GET state`, `POST normalize` and static files get **no spans** (polling would flood traces); they
appear only in metrics. Root spans annotate the ctx logger, and handlers log through `obs.Logger(ctx,
&s.log)`.

| Metric | Instrument | Attributes |
|---|---|---|
| `mrtcsip.widget.requests` | Counter `{request}` | `widget.action` (session, state, normalize, call, dtmf, hangup), `outcome` (ok, unauthorized, forbidden, rate_limited, invalid, busy, not_ready, error) |
| `mrtcsip.widget.sessions` | ObservableGauge `{session}` | none |
| `mrtcsip.widget.installs` | Counter `{room}` | `outcome` (installed, updated, unchanged, removed, failed) |

No MXIDs, numbers or room IDs on metrics.

---

## 11. Tests

- `pkg/calls`: `Snapshot` follows each state transition (fake clock, existing fakes). The race test
  calls `Snapshot` while the actor runs. Busy calls are not reported.
- `pkg/hostkit`: action results for every error mapping. All existing command tests pass unchanged. The
  `widget` command with a fake installer.
- `pkg/widget` (httptest, a fake homeserver for `openid/userinfo`, fake `CallAPI`, fake `Deps`):
  - key: right key accepted; wrong key, other room's key and missing key → 401.
  - OpenID: valid; invalid (HS 401); `sub` on another server → refused; remote server with
    `allow_remote_users=false` → refused; HS timeout → `openid_error`.
  - User without permission or not existing → 403. A room the user does not own → 403. A management room
    with no login → session ok, `login.ready=false`.
  - Session expiry, eviction cap, and that the session binds the room (a request cannot switch rooms).
  - Origin mismatch and wrong content type → 403/415. Body over 4 KiB → 413.
  - Rate limits per key (fake clock), with `Retry-After`.
  - CSP and the other headers on HTML and API. Static assets served with the right types. Paths
    outside the prefix not handled. The prefix validation rejects `/` and `/_matrix/...`.
  - Privacy: a test logger captures all output during a dtmf request with digits `"159#"` and a known
    token, and asserts that neither string appears (log or span attributes via `internal/obstest`).
  - Installer: content builder (URL fragment encoding, key, title), idempotence (no send when equal),
    update on content change, `M_FORBIDDEN` counted once, queue full drops without blocking, removal
    sweep.
  - Telemetry: span names and classes. No spans for `state`. No orphan roots (obstest).
- `internal/importcheck`: `pkg/widget` is host-class, and core packages must not import it.
- JS: no toolchain in the repo, so it is checked manually in the live test. Keep the JS small and
  dependency-free.
- jabber (`sipcalls` tag): `ResolveRoom` refuses group MMS/MUC rooms and logins without a SIP account,
  and reports `ready=false` with the right text. The sweep covers only eligible rooms.

Build, vet and test with `-tags "nocrypto nolibopusfile"` (mrtc) and jabber's `sipcalls` build.

---

## 12. Task split and file ownership

Order: W1 and W2 in parallel (they lock the contracts) → W3 and W4 in parallel → W5 and W6 in parallel
→ an Opus review (security focus on W3) → W7 deploy and live test.

| Task | Tier | Owns (edit only these) | Contract |
|---|---|---|---|
| **W1** calls snapshot | Sonnet | `pkg/calls/snapshot.go` (new), `pkg/calls/call.go`, the `state` sites in `pkg/calls/*.go` (non-test), `pkg/calls/snapshot_test.go` | §8.2 |
| **W2** hostkit actions + config + `widget` command shell | Sonnet | `pkg/hostkit/actions.go` (new), `commands.go`, `config.go` (+ `WidgetConfig`, example, validation), `upgrade.go`, `*_test.go` there | §4, §6.5, §8.1. The `widget` command calls a `WidgetInstaller` interface field on `CommandDeps` (nil = disabled reply) |
| **W3** `pkg/widget` server, auth, API, installer, telemetry | **Opus** (security-sensitive) or Sonnet followed by a mandatory Opus review | `pkg/widget/*.go` (not `static/`), `internal/importcheck/importcheck_test.go`, `docs/TELEMETRY-SPEC.md` (§4.3, §5.12) | §3, §5, §6, §7, §8.3, §10 |
| **W4** UI | Sonnet | `pkg/widget/static/*` | §7 API, §9 |
| **W5** standalone wiring | Sonnet | `pkg/connector/*` (new `widget.go`: Deps, ResolveRoom via `commandLogin` rules plus `portalNumber`, EligibleRooms, mount on `AS.Router` in `Start`, ResolveCallRoom-created enqueue), `cmd/mrtc-sip-bridge/main.go` if needed, `README.md` (widget section, metrics/spans), `docs/PLAN.md` (ADR-008 progress) | §3.1, §5.4, §6.3 |
| **W6** jabber wiring (**local branch `local/sip-calls` only, never pushed**; commits only if the coordinator follows the Phase J local-commit practice) | Sonnet | `pkg/connector/sipcalls_widget.go` (new, `//go:build sipcalls`), `sipcalls_widget_test.go`, small hooks in `sipcalls.go` (start/stop), `sipcalls_host.go` (created enqueue, post-PL-sweep widget sweep), `sipcalls_commands.go` (installer dep). No untagged files. | §5.4 (isCallPortal + commandLoginWithAccount), §6.3 |
| **W7** deploy and live test | coordinator | `<synapse-host>:/opt/services/<stack>/docker-compose.yml` labels (§3.2), both bridge configs | §13 |

Docs: `docs/EMBEDDING.md` gets a short "Widget" subsection (W6 owner writes the text; W5 commits it in
this repo), and `docs/DECISIONS.md` gets ADR-012 "Dialpad widget: OpenID + room-bound key, served on
the AS listener", which records the auto-granted-OpenID finding (W3 owner).

---

## 13. Live test plan

Outbound calls go **only to +17377273232**. **Never call +15550001234** without warning the user first.
Run the standalone bridge first; jabbercalls takes over the SIP account only when the standalone one is
stopped (one SIP account, one bridge).

1. Deploy (labels in §3.2, `widget.enabled: true`). `curl -I https://example.org/sip-dialpad/` → 200
   with the CSP header. `curl https://example.org/sip-dialpad/api/v1/state` → 401.
   `curl https://example.org/_matrix/app/v1/ping` must **not** reach the bridge.
2. Logs: the startup sweep installs the widget in existing portals and the management room (`installs`
   metric, `bridge.widget.sweep` span). A restart → `unchanged` everywhere.
3. sable-next (web, then Android): in the +17377273232 portal, open widgets → Dialpad. No capability
   prompt. The status shows "Ready" and the number.
4. Click "Call +17377273232" → the devices ring (callback) → answer → "Connected". Press `1`,`2`,`#` →
   the remote IVR reacts. Logs and traces show only `dtmf.digit_count`.
5. Hang up from the widget → the call ends and the status returns to Ready.
6. Management room → Dialpad → type `737 727 3232` → preview "+17377273232" → Call → ring in the
   portal → answer → "Open room" link works → hang up from the widget.
7. Errors: `+1555` → invalid; a number outside `dial_allowed_prefixes` → not_allowed; Call twice →
   line_busy; DTMF before connect → not_connected.
8. gomuks web: steps 3-5 (note whether it shows an empty permission prompt).
9. Security: copy the widget URL without the fragment (or with another room's `k`) into another widget →
   401. Use the session from room A against room B → acts on A only. Burst calls → 429.
10. Element X: no widget is shown (expected). `call` still works.
11. Repeat 2-6 with `mautrix-jabbercalls` (standalone stopped) in a Cheogram 1:1 room. A group MMS room
    gets no widget.
12. `remove_when_disabled: true` + `enabled: false` → the widgets are removed. Re-enable to restore.
