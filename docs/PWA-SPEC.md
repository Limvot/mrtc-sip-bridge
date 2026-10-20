# Dialpad PWA spec (installable quick-call app)

Status: **design, not implemented** (2026-10-17, branch `widget-dialpad`). Extends the Dialpad widget
(`docs/WIDGET-SPEC.md`, ADR-012) for both hosts: the standalone bridge and the embedded jmpchat-matrix-bridge (formerly mautrix-jabber)
host (local branch `local/sip-calls`, never pushed). Proposed ADR text: §14.

Goal: the user gets a special link from the bridge bot, opens it on their phone and installs the dialpad
as an app (Chrome or Firefox on Android, Safari on iOS). The app starts from the home screen in standalone
mode and acts as a quick dialer: type a number, tap Call, the phone rings in the Matrix client, answer,
talk. It also does DTMF and hang up.

Outside a Matrix client there is no Widget API and so no OpenID token. The PWA needs its own long-lived
credential, issued by the bridge.

**What the PWA is not:** it is not an audio endpoint. It is a remote control for the existing callback
ring flow (PHASE3-SPEC D6): `call` makes the bridge ring the user's Matrix clients (Element X, Sable) in
the number portal; the user answers there; only then does the bridge dial the number. Audio stays in the
Matrix client's Element Call (MatrixRTC). The UI must say this plainly (§9.4).

---

## 1. Decisions at a glance

| Topic | Decision |
|---|---|
| Credential | Two stages. The bot command gives a **one-time enrollment code** (15 min, single use) inside a link. The PWA redeems it, after the user confirms, for a **rotating device token** (256 bit). The device token is never shown in Matrix. |
| Renewal | **Refresh-token rotation, no forced reinstall.** Every session exchange (app open, hourly session renewal) presents the current token and gets a successor; the client stores it and confirms it; confirmation supersedes the predecessor, which stays valid for a short grace window (default 120 s). Expiry is by **inactivity** (default 90 days without use); an absolute lifetime exists but is **off by default**. |
| Reuse detection | A superseded token presented after its grace window revokes the device's whole token family, logs a Warn and notifies the management room (it means a copy was stolen, or a very old backup was restored). |
| Storage | Two small tables of our own in the bridge's database (`dbutil` child DB with its own version table), owned by `pkg/widget`: one row per device (family), one row per issued token. Only `sha256(secret)` is stored. Works for both hosts, survives restarts. |
| Session flow | The PWA exchanges (and thereby rotates) the device token for a normal short-lived widget session (the existing in-memory store and limits). All the API calls use the session. |
| Scope | Management-room scope for the login the link was issued for: dial any allowed number, status, DTMF, hang up. No room-specific actions, no other rooms. |
| Revocation | `dialpad-links` lists, `dialpad-revoke <id\|all>` deletes the family and kills live sessions immediately; "Forget this device" in the app revokes its own family. Families of deleted logins are purged. |
| Link delivery | In the private management room only, the secret part in a code span (no linkify or URL preview), sent with logging suppressed, redacted by the bot when used or expired. |
| Hosting | The same widget server and prefix: `<prefix>app/` (page, manifest, service worker, icons) and `<prefix>api/v1/device/...`. No new listener or proxy rule. |

---

## 2. Credential design

### 2.1 Why two stages

A single long-lived token inside the link would sit for months in places we do not control:

- the **homeserver database**: management rooms are unencrypted (the bridges build without crypto), so the
  message body is stored in plain text and visible to server admins and in backups;
- the **bridge's own debug log**: mautrix `Client.LogRequestDone` logs `req_body` of every send at Debug
  level (mautrix v0.32.0 `client.go:417-423`; only `SensitiveContent` requests are masked), and the dev
  bridges run at debug;
- **URL previews**: a client that previews a linkified URL sends the whole URL, fragment included, to
  `/_matrix/media/v3/preview_url?url=...`; the homeserver logs it and caches the preview;
- the **browser history and sync** of the phone that opened it (Chrome keeps fragments in history; whether
  `history.replaceState` rewrites the stored entry is **unverified**, so assume it does not);
- every other device of the user that synced the management room.

With a one-time code, everything above only ever holds a secret that is dead after 15 minutes or after
first use, whichever comes first. The device token goes straight from the bridge's HTTPS response into
the PWA's storage and is never displayed, logged or stored in Matrix. Because it rotates on every
exchange (§2.7), any single token value is short-lived too: a copy taken from the phone stops working
(and is detected) as soon as the real app renews.

Costs: a code burned by opening the link in the wrong browser (for example Safari on iOS, see §6.4) means
asking for a new link. That is cheap, so we accept it.

### 2.2 Formats

- Enrollment code: `mrtcsip_dpc_` + base64url(32 random bytes) (43 chars). Token: `mrtcsip_dpt_` + 43
  chars. The prefixes let secret scanners recognize them and let the paste box (§9.2) find them in any
  pasted text. Entropy is 256 bits, so brute force is out of reach. `crypto/rand` only.
- Link ID: 6 chars of lowercase base32 (`[a-z2-7]`), random and unique per bridge. Public: shown in
  `dialpad-links`, allowed on spans and logs, never on metrics.
- Device name: optional command argument, 1-32 chars of `[\p{L}\p{N} ._-]`, default `device-<id>`.
  Escaped in Markdown replies and always rendered with `textContent`.

### 2.3 Storage

**Decision: an own table through a `dbutil` child database**, in `pkg/widget/devicestore.go`:

```go
db := br.DB.Database.Child("mrtcsip_dialpad_version", upgrades.Table, dbutil.ZeroLogger(log))
```

with `upgrades` embedded in `pkg/widget/devicestore_upgrades/` (`00-latest.sql`, v1). Schema
(portable between SQLite and Postgres; times are unix milliseconds):

```sql
-- One row per device = one token family.
CREATE TABLE mrtcsip_dialpad_device (
    bridge_id     TEXT    NOT NULL,
    id            TEXT    NOT NULL,          -- public link id
    state         TEXT    NOT NULL,          -- 'pending' | 'active' | 'revoked'
    code_hash     TEXT,                      -- hex sha256 of the enrollment code (kept 24 h after use)
    code_expires  BIGINT,                    -- enrollment deadline
    user_mxid     TEXT    NOT NULL,
    login_id      TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    client        TEXT    NOT NULL DEFAULT '', -- closed enum from the UA at enrollment (§2.6)
    created_at    BIGINT  NOT NULL,
    enrolled_at   BIGINT,
    last_used_at  BIGINT,                    -- inactivity expiry is last_used_at + inactivity_days
    hard_expiry   BIGINT,                    -- informational: enrolled_at + max_lifetime_days at enrollment (expiry is computed at read time)
    revoked_at    BIGINT,
    revoked_why   TEXT,                      -- 'reuse' (kept 30 days so the app can say why); other revokes delete the row
    notice_room   TEXT,                      -- the bot's link message, until redacted
    notice_event  TEXT,
    PRIMARY KEY (bridge_id, id)
);
CREATE UNIQUE INDEX mrtcsip_dialpad_device_code ON mrtcsip_dialpad_device (bridge_id, code_hash);
CREATE INDEX mrtcsip_dialpad_device_user ON mrtcsip_dialpad_device (bridge_id, user_mxid);

-- One row per issued device token (rotation chain).
CREATE TABLE mrtcsip_dialpad_token (
    bridge_id     TEXT    NOT NULL,
    token_hash    TEXT    NOT NULL,          -- hex sha256 of the token
    device_id     TEXT    NOT NULL,
    parent_hash   TEXT,                      -- the token it was rotated from (NULL for the first)
    issued_at     BIGINT  NOT NULL,
    confirmed_at  BIGINT,                    -- NULL = issued but the client has not confirmed it yet
    superseded_at BIGINT,                    -- set when a child is confirmed; grace runs from here
    PRIMARY KEY (bridge_id, token_hash),
    FOREIGN KEY (bridge_id, device_id) REFERENCES mrtcsip_dialpad_device (bridge_id, id) ON DELETE CASCADE
);
CREATE INDEX mrtcsip_dialpad_token_device ON mrtcsip_dialpad_token (bridge_id, device_id);
```

(SQLite only cascades with `PRAGMA foreign_keys=ON`; whether the bridges' SQLite DSN sets it is
**unverified**, so the store always deletes token rows explicitly in the same transaction and does not
rely on the cascade.)

Why not the alternatives:

- **bridgev2 KV store** (`br.DB.KV`): one string per key, so all links would be one JSON blob with racy
  read-modify-write, no lookup by hash, no listing per user, and `KVQuery.Set` logs the **value** on
  error (`kvstore.go:51-58`).
- **User or login metadata**: there's no per-user metadata in bridgev2; login metadata is a host type
  (jabber's lives in public, untagged files, which Phase J must not touch, WIDGET-SPEC §6.4), it is saved
  as a blob by the framework (racy with our writes), and it can't be indexed by hash.
- **In memory only**: does not survive restarts, and the whole point is a long-lived credential.

The child DB is created and upgraded by the host in `Start` (after the bridge's own upgrade, before
`widget.New`), only when `widget.pwa.enabled`. `bridge_id = br.ID` because several bridges may share a
database. In jabber the table exists only in `sipcalls` builds. It is harmless to the public build
(dbutil only refuses Synapse's tables as foreign).

Lookups are by hash (indexed equality on a hash of a 256-bit secret). A plain sha256 is right here: a
slow KDF is only needed for low-entropy secrets, and timing reveals nothing usable. Every multi-row
change below runs in one DB transaction (`db.DoTxn`).

### 2.4 Lifecycle

1. **Issue** (`dialpad-link`, §4): insert a `pending` device with `code_hash`, `code_expires = now +
   enroll_minutes`. The per-user limit (`max_links_per_user`, default 5) counts pending and active
   devices; at the limit the command refuses and lists the IDs to revoke. Global cap 1000 devices per
   bridge.
2. **Enroll** (`POST device/enroll`, §7): single use and race-free:
   `UPDATE mrtcsip_dialpad_device SET state='active', enrolled_at=$now, last_used_at=$now,
   hard_expiry=$h, client=$c WHERE bridge_id=$b AND code_hash=$codeHash AND state='pending' AND
   code_expires>$now`; one affected row means success, and the same transaction inserts the first token
   row with `confirmed_at = now` (the enroll response is the only copy; if it is lost the user just
   requests a new link, there is nothing to supersede). Then the bot redacts its link message (async,
   §4.2) and, if `notify_on_enroll`, posts "Dialpad link `k3m9qa` (Pixel) was set up on Chrome Android
   just now. Not you? `dialpad-revoke k3m9qa`."
3. **Use and rotate** (`POST device/session` then `POST device/confirm`): §2.7.
4. **Inactivity expiry**: a device is expired when `now > last_used_at + inactivity_days` (default 90) or,
   only if `max_lifetime_days > 0`, `now > enrolled_at + max_lifetime_days` (computed at read time from
   the current config, so enabling or changing the limit applies to existing devices; the `hard_expiry`
   column is informational). There is no other expiry: an app that is opened
   at least once per `inactivity_days` never needs re-enrollment. An expired device answers
   `device_expired` (so the app can say "expired" rather than "invalid") and is deleted with its tokens
   30 days later. An expired pending device is deleted after its message is redacted.
5. **Revoke** (command or "Forget this device"): delete the device and all its token rows, then
   `sessions.revokeDevice(id)` (§3). Effective immediately. **Reuse revoke** (§2.7) instead sets
   `state='revoked', revoked_why='reuse'`, deletes all token rows except the hashes needed to recognize
   the family (kept, all marked superseded), and kills sessions; the row is deleted after 30 days.
6. **Orphans**: the janitor (hourly) deletes devices whose `login_id` no longer exists
   (`br.DB.UserLogin.GetByID`) or whose user lost command permission. Request-time resolution already
   refuses them (§3.2); the purge only cleans up. This needs no logout hook, which matters for jabber,
   whose logout code lives in untagged files.
7. **Token pruning**: superseded token rows are kept for `inactivity_days` after `superseded_at` (so
   reuse of an old copy is still recognized), then deleted; later, an old copy is just
   `device_invalid`. Unconfirmed children older than 1 day are deleted.

### 2.5 Binding

A link is bound to `(user_mxid, login_id)` at issue time: the login that `CommandDeps.Login` picks for a
command in the management room (standalone: default login; jabber: `commandLoginWithAccount`, so a
login with a SIP account). A user with two logins issues one link per login.

### 2.6 `client` enum

At enrollment the server maps `User-Agent` to one of `chrome_android`, `firefox_android`,
`safari_ios`, `chrome_desktop`, `firefox_desktop`, `other`. The raw UA and the IP address are never
stored. The enum shows in `dialpad-links` and the enrollment notice, so the user can spot a link set up
somewhere unexpected. It is computed again on every rotation; if it differs from the stored value, the
bot posts a notice ("Dialpad `k3m9qa` (Pixel) was used from Firefox Desktop, set up on Chrome Android.
Not you? `dialpad-revoke k3m9qa`.") at most once per day per device. This is a signal, not a revoke
(UAs change on updates).

### 2.7 Rotation (refresh-token rotation with confirm-by-use)

Token states, per token row: **unconfirmed** (`confirmed_at IS NULL`), **current** (confirmed, not
superseded), **in grace** (superseded less than `rotation_grace_seconds` ago, default 120), **dead**
(superseded longer ago).

**Exchange** (`POST device/session {device_token: T}`), in one transaction:

1. Find the row of `sha256(T)`; none → 401 `device_invalid` (failure limiter). Load its device: missing
   → `device_invalid`; `revoked` → 401 `device_revoked` (`reason: "reuse"` when it was a reuse revoke);
   expired (§2.4.4) → 401 `device_expired`.
2. Classify `T`:
   - **current**: OK.
   - **unconfirmed**: load its parent. Parent pruned → `device_invalid`. Parent **dead** → reuse (as
     below): another branch won more than the grace window ago, so this child is a stale fork (for
     example a thief rotated a copied `T` once and kept the unconfirmed child for later). Parent current
     or in grace → OK: the client stored a successor but its confirm was lost; presenting it counts as
     confirming it (as **Confirm** below).
   - **in grace**: OK (another tab, or a retry after a lost confirm, §9.2.1).
   - **dead**: **reuse detected** → reuse revoke (§2.4.5), Warn log `"Dialpad device token reuse
     detected; device revoked"` with `user`, `device_id`, `client` (never the token), management-room
     notice ("Dialpad `k3m9qa` (Pixel) was signed out: an old copy of its sign-in was used, which can
     mean it was copied from your phone. Set it up again with `dialpad-link` if this was you."), span
     outcome `reuse`, metric event `reuse_revoked`; answer 401 `device_revoked` with `reason: "reuse"`.
3. Issue a successor `C` (new random token) as an **unconfirmed** child of `T`. Delete older unconfirmed
   children of `T` beyond the newest 2 (a client that keeps losing responses can't grow the table).
4. Update `last_used_at = now` (sliding inactivity window), create the session (§3), and answer
   `{session, expires_in, next_token: C, device: {...}}`.

**Confirm** (`POST device/confirm {device_token: C}`), idempotent:

- `C` unconfirmed and its parent current or in grace → set `confirmed_at = now`, and set
  `superseded_at = now` on **every other current token of the device** (its parent and any sibling
  branch; their grace windows start here, not at issue time). Answer `{ok: true}`. Parent dead → reuse;
  parent pruned → `device_invalid` (as step 2).
- `C` already confirmed (current or in grace) → `{ok: true}` (a retried confirm).
- `C` dead or unknown → handled as in step 1/2 (dead = reuse).

Why confirm-by-use rather than superseding the old token at issue time: if the exchange response is
lost (the phone went offline mid-request, the app was killed), the client still holds `T`, and `T` must
keep working however long the phone stays offline. With supersede-at-issue, a lost response plus more
than `grace` offline would look exactly like theft and revoke the user's own device. With
confirm-by-use, `T` is superseded only once the server has proof that the client stored `C`.

Live branches: exactly one current token per device. Confirming a token supersedes every other current
token of the device, so a fork made inside a grace window (two exchanges of `T`, both confirmed) cannot
persist: the branch confirmed first goes to grace when the other is confirmed, then dead, and presenting
it after the grace window is reuse. Without this, a thief who rotated inside the window would hold a
permanent second branch that reuse detection never sees.

Tradeoff: two tabs **without** Web Locks that rotate the same token concurrently and store their
successors in the opposite order of their confirms can leave the stored token on the losing branch; the
next renew after the grace window then signs the device out as reuse (a rare false alarm, with the reuse
notice). All target browsers support Web Locks (§9.2.1), and `renewLocked` re-reads storage inside the
lock, so in practice rotations of one browser profile never overlap.

Stolen-copy outcomes (why reuse detection works): a thief copies `T` from the phone.
- The real app rotates first (it opens at least hourly while in use): `T` is superseded; when the thief
  presents `T` after the grace window → reuse revoke. Both are signed out and the user is told.
- The thief rotates first and confirms: the thief's `C'` supersedes `T`; the real app presents `T` on its
  next open (after grace) → reuse revoke, both signed out, user told. Inside the grace window (≤ 120 s)
  both would keep working until the next rotation, which then trips the same detection.
- The real app is never opened again: the thief keeps access until inactivity expiry or `dialpad-revoke`.
  That residue is inherent to bearer credentials; the client-change notice (§2.6) and `dialpad-links`
  ("last used") are the user's signals.

---

## 3. Session flow

**Decision: exchange the device token for a normal widget session, rotating the token on every
exchange (§2.7).** The device token is not sent on every request.

Justification:
- The app polls `state` every 1-5 s. A per-request token would be a DB lookup per poll and would put the
  device secret in every request, which multiplies exposure in proxies and memory. Rotation on every
  request would be absurd. The session is a 1 h in-memory secret, and a DB transaction (rotation) happens
  once per app open or session renewal.
- Exchanges happen when the app opens (or returns to the foreground with an expired session) and every
  hour while it stays open, which is exactly "normal use": that is what keeps the inactivity window
  sliding and the token rotating.
- All of the existing API (handlers, rate limits, `authenticate`, telemetry) works unchanged with a
  session. Only the authentication step at the start differs.
- Revocation is still immediate because sessions remember their link (below), so the shorter-lived
  session costs nothing in revocation latency.

### 3.1 Session store changes (`pkg/widget/auth.go`)

`session` gains `device string` (link ID, "" for widget sessions) and `login networkid.UserLoginID`.
`room` is "" for device sessions. New `sessionStore.revokeDevice(id)` and `revokeUser(mxid)` delete
matching sessions; `revokeDevice` also records the revocation time per device (kept 1 h), and
`createDevice(..., notBefore)` refuses (401 `device_revoked`) when the device was revoked at or after
`notBefore`, the clock time the exchange took before `Rotate`. TTL = 1 h (or less if the hard expiry is
closer). Per-user and global caps are shared with
widget sessions (8 per user, 1000 overall); an evicted PWA session simply re-exchanges.

### 3.2 Authorization on every request

`authenticate` branches on `ss.device != ""`:
1. `lookup(user)` exists and has `Permissions.Commands` (as today);
2. `Deps.ResolveDevice(ctx, user, loginID)` (new, required when PWA is enabled) returns a `RoomContext`
   with `Kind = KindDevice` ("device"): `Login` set when the login exists, belongs to the user and is
   ready (standalone: `loginClient`; jabber: real `*xmppClient` with a SIP account); otherwise `Login`
   nil and `Refusal` = the same text as the commands. A missing or foreign login → 401 `device_revoked`
   (the janitor will purge the row).

`KindDevice` behaves like `KindManagement` everywhere: `commandRoom` = "" (hangup/dtmf target the
login's active call in any room), `call {}` is refused ("Enter a number to call."), and **no installer
enqueue** (`handleCall` currently enqueues `a.ss.room`; skip when it's ""). `state.room.kind` is
`"device"`. The device and the management-room privacy check do not apply: the device credential does
not come from room state, and the management room's privacy was checked when the link was issued.

---

## 4. Bot commands (shared, `pkg/hostkit`)

Registered with the other call commands when `CommandDeps.Dialpad != nil`. All are `RequiresLogin`,
wrapped in `WithSpan` (`cmd.dialpad_link`, `cmd.dialpad_links`, `cmd.dialpad_revoke`; arguments never
recorded), and **management room only**: in any other room the reply is "Run `$cmdprefix dialpad-link`
in your management room (your DM with the bridge bot)." `ce.Sudo` doesn't bypass this (links are
personal).

```go
// DialpadLinks is implemented by pkg/widget (nil = the PWA is disabled on this host).
type DialpadLinks interface {
    Issue(ctx context.Context, user id.UserID, login networkid.UserLoginID, room id.RoomID, name string) (LinkIssue, error)
    AttachNotice(ctx context.Context, linkID string, room id.RoomID, evt id.EventID) error
    List(ctx context.Context, user id.UserID) ([]LinkInfo, error)
    Revoke(ctx context.Context, user id.UserID, linkID string) (int, error) // linkID "all" = every link of user
}
type LinkIssue struct { ID, Name, Link, AppURL string; ExpiresAt time.Time }
type LinkInfo struct { ID, Name, State, Client string; Created, LastUsed, Expires time.Time }
var ErrDialpadLinkLimit, ErrDialpadRoomNotPrivate error
```

### 4.1 `dialpad-link [name]`

1. Management room check; `CommandDeps.Login(ce)` (refusal → its text).
2. `Issue` checks `widget.PrivateRoom(management room, user)`. A shared management room would show the
   link to others, so it refuses with `MsgWidgetNotPrivate`.
3. The reply is sent by hostkit itself with `ce.Bot.SendMessage` and a **ctx whose zerolog logger is
   `zerolog.Nop()`** (the span is kept), so mautrix's Debug `req_body` line is dropped. Test: §12.
   Markdown:

   > Dialpad link for **Pixel** (`k3m9qa`). It works **once** and expires in **15 minutes**:
   >
   > `https://example.org/sip-dialpad/app/#c=mrtcsip_dpc_…`
   >
   > On your phone, open <https://example.org/sip-dialpad/app/>, tap **Paste link**, and paste this
   > message (or copy the link above into the phone's browser). Then install it: Chrome: ⋮ → *Add to
   > Home screen* / *Install app*; Firefox: ⋮ → *Install*. iPhone: add the app to the Home Screen
   > **first**, open it from there, then paste. The bridge rings your Matrix app for each call; answer
   > there. I will delete this message once the link is used or expires. Devices: `$cmdprefix
   > dialpad-links`.

   The secret link is in a code span on purpose: clients don't linkify code, so there's no tappable
   link and no URL preview request with the code in it. Whether Element X, Sable and gomuks honor this
   is **unverified** (live test §13 step 3). The clickable URL carries no secret.
4. `AttachNotice(id, room, eventID)` stores the event for redaction.

### 4.2 Redaction of the link message: yes

Evaluation: redaction removes the code from every client's timeline and, after Synapse's
`redaction_retention_period` (default 7 d), from the database. It cannot remove it from homeserver
backups, the HS's preview cache or devices that already synced. With a one-time 15-minute code that
residue is harmless, so redaction is hygiene, not the main defense. Policy: redact **as soon as the code
is used** (after the enroll response is written, async) or **at expiry**. A timer is set at issue;
because timers do not survive restarts, the janitor (every minute) also redacts every row with
`notice_event` set that is enrolled or past `code_expires`, then clears the columns. Redaction failure
(for example the bot has PL 0 in a user-created management room) logs at Debug and is retried at most 3
times. The user's own command message has no secret and is left alone.

### 4.3 `dialpad-links`

Lists the user's links (all logins) as a table: id, name, state (`waiting for setup` / `active` /
`expired`), client, created, last used (date only), expires. Empty: "No Dialpad links. Use `$cmdprefix
dialpad-link`."

### 4.4 `dialpad-revoke <id|all>`

Deletes the user's link(s) (only the caller's own; an unknown id says "No link `x` of yours"), kills
their sessions, redacts pending notices. Reply: "Revoked 1 link. That device is signed out now." Pending
codes can be revoked too.

---

## 5. Threat model and mitigations

| Threat | Mitigation | Residual |
|---|---|---|
| **Link leakage** (HS DB, logs, previews, history, other synced devices) | Code is single use and expires in 15 min; delivered only in a private management room; code span (no linkify/preview); bot send with logging suppressed; redacted on use or expiry; the device token is never displayed | Someone who reads the room within the 15 minutes and redeems first wins. The user then sees "already used" and gets an enrollment notice they didn't expect. Revoke and reissue. |
| **Device token theft** (phone or browser storage, backups) | Token only in `localStorage` on our origin; **rotation on every exchange with reuse detection** (§2.7): whichever of thief and real app rotates second trips a family revoke, a Warn log and a management-room notice; client-change notices; inactivity expiry (90 d); revocation from Matrix; "Forget this device"; `dialpad-links` shows client and last use | A thief is undetected only while the real app stays unused (until inactivity expiry or revoke), and for at most the grace window (120 s) when both use it at once. The credential can only place calls through the user's own SIP account (toll risk), not read Matrix; `dial_allowed_prefixes` bounds where calls go. |
| **Forked families** (a thief rotates a copied token once and keeps the unconfirmed child, or confirms a sibling inside the grace window) | An unconfirmed child is usable only while its parent is current or in grace; presenting it after its parent died is reuse. Confirming any token supersedes every other current token of the device, so a sibling branch goes to grace, then dead, and is caught as reuse (§2.7) | Two lock-less tabs whose stores and confirms cross can trip a false reuse sign-out (see next row). |
| **False reuse alarms** (lost responses, two tabs, app killed mid-rotation) | Confirm-by-use: the old token is superseded only after the client proves it stored the new one; Web Locks serialize rotation across tabs and `renewLocked` re-reads storage inside the lock; a grace window covers lock-less races (§2.7, §9.2.1) | Restoring a phone backup older than the last rotation signs the device out (by design: indistinguishable from a stolen copy). A browser without Web Locks running two tabs that rotate at the same moment can, rarely, sign the device out as reuse (one current token per device, §2.7 "Live branches"); all target browsers support Web Locks. |
| **Eviction of a stored-but-unconfirmed successor** (accepted) | none specific | A thief holding the current token `T`, while the real app stored `C` but its confirm was lost, can rotate `T` until `C` falls out of the newest 2 unconfirmed children (§2.7 step 3), or confirm a child so `T` dies; the real app then gets `device_invalid` (or reuse) for `C` and has to re-enroll. In the `device_invalid` case there is no notice. Accepted: it needs the stolen current token, gains the thief nothing beyond what that token already gives, and the user sees the sign-out. |
| **Session after revoke** (an exchange racing a revoke or reuse detection) | The session store remembers device revocations for 1 h (the session TTL); a device session is refused when its device was revoked at or after the moment its exchange started (§3.1) | |
| **`previous_token` abuse** (a client-supplied token at enroll) | The server only looks it up (no confirm, no rotation, no reuse detection, no last-use update) and revokes its device only when it is a different device of the same user | |
| **No forced expiry** (a long-lived family) | Inactivity expiry; optional absolute lifetime (`max_lifetime_days`, off by default); each token value lives only until the next rotation | A device used regularly stays signed in indefinitely, which is the requirement. Revocation is the control. |
| **Shared origin** (other apps on the same host could read `localStorage` through their own XSS) | The token is per origin, not per path. Recommend a **dedicated hostname** for production (e.g. `dialpad.example.com`); the server logs one Warn at start when PWA is enabled and the public URL path is not `/` | Dev runs under `example.org/sip-dialpad/`; the other content there is static (a redirect and nginx `.well-known`). Accepted for dev. |
| **Phishing** (user lured into giving away the link, or a fake app) | The command text says to open it only on your own phone; the link works once; the app shows the bound MXID on every screen | Social engineering of a still-valid code is possible within 15 min. |
| **Enrollment CSRF / account confusion** (attacker sends the victim a link with the attacker's code, the victim's dialed numbers then show in the attacker's state) | `enroll` is two steps: `confirm:false` returns `{user, name}`; the app shows "Set up this device for **@attacker:hs** (Pixel)?" and redeems only on tap. An already enrolled app asks before it replaces its device. The MXID is shown in the header | Only bridge users with command permission can issue codes at all. |
| **Link scanners that run JS** (corporate or AV prefetchers) | Code in the fragment (never sent to a server); redeeming needs a user tap, not page load | |
| **CSRF on the API** | No cookies: bearer token in a header from JS only. POSTs need `application/json` plus same-origin `Origin` (existing `checkPost`); no CORS | |
| **XSS** | Strict CSP (§8.3), no inline script, no `innerHTML`/`eval`/`document.write` (test-enforced, §12), names and server messages rendered with `textContent` | |
| **Clickjacking** of the app page (it has a live session) | `frame-ancestors 'none'` and `X-Frame-Options: DENY` on `app/` only (the widget page stays framable) | |
| **Token in URLs** | Code only in the **fragment** (`#c=`), never a query; the app reads it, then `history.replaceState(null, "", location.pathname)` **before any network request**; the code is sent in a POST body; `Referrer-Policy: no-referrer`; the device token never appears in any URL | Browser history may keep the first URL (unverified). The code is dead in 15 min. |
| **Brute force / guessing** | 256-bit secrets; failed `enroll`/`device/session` count in the existing per-IP failure limiter (5/min, then 15 min lockout); malformed input (wrong prefix or length) is rejected before the DB | |
| **Revocation latency** | Zero: delete the row, then kill in-memory sessions by link ID in the same process. Per-request re-resolution also catches logout, sip-logout and permission loss | A request already in flight completes. |
| **Logging hygiene** | Never log or trace codes, tokens, sessions, request bodies, device names, DTMF digits; the access logger is not used on these routes; no secret on metrics; privacy test (§12) | |

---

## 6. PWA mechanics

### 6.1 Routes (added to `serveHTTP`, only when `pwa.enabled`; otherwise 404)

| Path | Content |
|---|---|
| `<prefix>app/` | `static/app/index.html` (the app page) |
| `<prefix>app/manifest.webmanifest` | manifest, `application/manifest+json` |
| `<prefix>app/sw.js` | service worker, `text/javascript`, `Cache-Control: no-cache` |
| `<prefix>app/<asset>` | `app.js`, `app.css`, `icon.svg`, `icon-192.png`, `icon-512.png`, `icon-maskable-512.png` (same name regex as `static/`) |
| `<prefix>api/v1/device/{enroll,session,forget}` | §7 |

`<prefix>app` (no slash) redirects to `<prefix>app/`. Assets live in `pkg/widget/static/app/` (the
existing `go:embed static/*` must become `static` or `static/* static/app/*`). The app reuses the shared
`static/app.css` rules where it can, but has its own `app/app.js`, because the widget's Widget API and
OpenID code don't apply. The common dialpad logic (keypad, polling, rendering) moves to
`static/dialpad.js` and both pages include it, so it isn't duplicated. The SW's scope is `<prefix>app/`
(its own directory, so no `Service-Worker-Allowed` header is needed); it still sees the requests of
controlled pages for `../static/dialpad.js`.

### 6.2 Manifest

```json
{
  "name": "Dialpad (<bridge display name>)",
  "short_name": "Dialpad",
  "id": "./",
  "start_url": "./",
  "scope": "./",
  "display": "standalone",
  "orientation": "portrait",
  "background_color": "#101418",
  "theme_color": "#1f6f43",
  "icons": [
    {"src": "icon-192.png", "sizes": "192x192", "type": "image/png"},
    {"src": "icon-512.png", "sizes": "512x512", "type": "image/png"},
    {"src": "icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
    {"src": "icon.svg", "sizes": "any", "type": "image/svg+xml"}
  ]
}
```

URLs are relative to the manifest, so the prefix never has to be templated. The name comes from a
static file: the bridge name is not substituted (one embedded file, no templating). `start_url` has **no
fragment or token**. Two bridges on two prefixes are two distinct apps (different `id`/scope).

### 6.3 Service worker (`sw.js`, about 40 lines)

- Registration: `app.js` registers `sw.js` only after it removed the fragment with `replaceState`, so
  the worker's fetch of `sw.js` (and its client URL) never sees the link code.
- Cache name: `dialpad-<s>-<v>` where `<s>` is a short hash (FNV-1a, 8 hex) of `registration.scope`, so
  several bridges or paths on one origin keep separate caches.
- `install`: `caches.open("dialpad-<s>-<v>")` and `addAll` of the fixed shell list (`./`, `app.js`,
  `app.css`, `../static/dialpad.js`, `../static/app.css`, icons). `<v>` is a constant string that the
  build bumps (a Go test checks that it equals a hash of the shell files, so a change to the shell can't
  ship with a stale cache).
- `activate`: delete other caches with the same `dialpad-<s>-` prefix (never another scope's),
  `clients.claim()`.
- `fetch`: **only** same-origin GETs whose path is in the shell list → cache first, then network. Every
  other request (all of `api/`, any POST) is not intercepted (no `respondWith`), so API responses are
  never cached. Navigation to `./` when offline → the cached shell, which shows "Offline: no connection
  to the bridge".
- Recent Chrome no longer needs a service worker with a fetch handler to install; older versions and
  WebAPK minting did (**version boundary unverified**). The SW costs nothing and gives an offline shell,
  so ship it.

### 6.4 Install (documented in the README and the command text)

- **Chrome Android**: open the app URL → ⋮ → *Install app* (or *Add to Home screen*). Chrome builds a
  WebAPK; it shares storage with Chrome, so a device enrolled in the tab is enrolled in the app.
- **Firefox Android**: ⋮ → *Install* (or *Add to Home screen* on older versions). Shares storage with the
  Firefox profile.
- **iOS / iPadOS Safari**: Share → *Add to Home Screen*. Home Screen web apps have **storage separate
  from Safari**, so enroll **inside** the installed app: install from the bare app URL first, open it,
  tap *Paste link*. A code redeemed in Safari is gone; request a new one. iOS Safari ignores some
  manifest fields; `apple-touch-icon` (`icon-192.png`) and `<meta name="apple-mobile-web-app-capable">`
  are added to the page.
- Desktop browsers work too (Chrome/Edge install), but the feature targets phones.

### 6.5 Icons (in-repo, no external assets)

`static/app/icon.svg` is hand-written: a rounded square in the theme green with a white 3×4 grid of dots
(a keypad) and a handset glyph. The PNGs are generated by `go run ./internal/genicons` (stdlib
`image/draw` only, the same drawing) and committed. A test checks their dimensions and that the maskable
icon keeps its content inside the 80% safe zone (transparent-free background).

---

## 7. HTTP API additions (`<prefix>api/v1/device/`)

All POST, JSON, through the existing `checkPost`, `decodeJSON` (4 KiB, strict) and `Cache-Control:
no-store`.

| Path | Body | Response | Errors |
|---|---|---|---|
| `enroll` | `{code, confirm: bool}` | `confirm:false` → `{user, name, expires_in}` (no state change); `confirm:true` → `{device_token, device: {id, name, user}}` (the first token, already confirmed) | 401 `link_invalid` (unknown, malformed, revoked), 410 `link_used`, 410 `link_expired`, 429 |
| `session` | `{device_token}` | `{session, expires_in, next_token, room: {kind: "device"}, device: {id, name, user, inactive_expiry_at}}` (rotation, §2.7) | 401 `device_invalid`, 401 `device_expired`, 401 `device_revoked` (+ `reason: "reuse"`), 403 `forbidden` (user lost permission), 429 |
| `confirm` | `{device_token}` (the `next_token` just stored) | `{ok: true}` (idempotent) | as `session` |
| `forget` | `{}` with `Authorization: Bearer <session>` (device session only) | `{ok: true}`: deletes its own device and tokens, kills its sessions | 401 |

`link_used` vs `link_expired` vs `link_invalid` is only distinguishable for a caller who holds the
code: the server finds the device by `code_hash` (kept for 24 h after enrollment, then set to NULL)
and checks its state and `code_expires`; after 24 h a used code is just `link_invalid`. The existing `state`, `normalize`, `call`, `dtmf`, `hangup` accept device sessions
unchanged (§3.2).

### 7.1 Rate limits (new rows for WIDGET-SPEC §5.6)

| Key | Limit |
|---|---|
| `enroll` per client IP | 5/min |
| `device/session` per client IP | 10/min |
| failed `enroll` / `device/session` per IP | shared with the existing failure limiter: 5/min, then 15 min lockout |
| `device/session` per device | 30/h (each one rotates; a normal app does 1-2 per hour) |
| `device/confirm` per device | 60/h |
| `dialpad-link` per user (command) | 5/h (prevents flooding the table and the room) |

Action limits (`call` 5/min, `dtmf`, `hangup`, `contacts` 5/s) are per MXID and shared with widget sessions,
which is right because they protect the same SIP account.

---

## 8. Server and config changes

### 8.1 Config (`hostkit.WidgetConfig`, nested `pwa` block)

```yaml
widget:
    # ... existing keys ...
    # Installable phone app (PWA) at <public_url>app/, signed in with links from `dialpad-link`.
    # Needs enabled: true. A dedicated hostname for public_url is recommended (the app keeps a
    # long-lived sign-in in the browser storage of that host).
    pwa:
        enabled: false
        # A set-up device is signed out after this many days without being used. Each use renews
        # its sign-in (the stored credential is replaced on every use), so a device in use never
        # needs to be set up again.
        inactivity_days: 90
        # Optional hard limit after setup (days). 0 = none (default).
        max_lifetime_days: 0
        # After a device's credential is replaced, the previous one keeps working this long
        # (seconds), for a second open tab. Using it later signs the device out (stolen copy).
        rotation_grace_seconds: 120
        # How long a new link can be used to set up a device (minutes). The bot deletes the message
        # when it is used or expires.
        enroll_minutes: 15
        # Links (waiting or active) per user.
        max_links_per_user: 5
        # Post a notice in the management room when a link is set up.
        notify_on_enroll: true
```

Validation (only when `widget.enabled && pwa.enabled`): `inactivity_days` 1-3650;
`max_lifetime_days` 0 or ≥ `inactivity_days`; `rotation_grace_seconds` 10-3600; `enroll_minutes`
1-60; `max_links_per_user` 1-50.
`pwa.enabled` without `widget.enabled` is a config error. `hostkit.CopyKeys` copies all keys (both
upgraders); the example goes into `pkg/hostkit/example-config.yaml`.

### 8.2 `pkg/widget`

- `devicestore.go` (+ `devicestore_upgrades/`): `NewDeviceStore(db *dbutil.Database, bridgeID
  networkid.BridgeID, cfg, clk, log)`, `Upgrade(ctx)`, `Issue`, `Peek`, `Enroll`, `Rotate(ctx, token,
  client) (RotateResult{Device, Next string, Reuse bool}, error)`, `Confirm(ctx, token)`, `List`,
  `Revoke`, `AttachNotice`, `DueNotices`, `ClearNotice`, `PurgeExpired` (devices and token pruning),
  `PurgeOrphans(ctx, exists func(user, login) bool)`, `Counts()` (cached for the gauge).
  The DB is reached only through this file.
- `device.go`: the `enroll`/`session`/`confirm`/`forget` handlers, reuse handling (Warn log, notice), `DialpadLinks` implementation (privacy check,
  link URL building `<public_url>app/#c=<code>`), the redaction worker (uses `Deps.State`-like seam
  `Deps.Redact`), enrollment notice sender.
- `Deps` additions: `Devices *DeviceStore` (nil = PWA off), `ResolveDevice func(ctx, user, login
  networkid.UserLoginID) (RoomContext, error)`, `LoginExists func(ctx, login networkid.UserLoginID)
  bool` (janitor; default `br.DB.UserLogin.GetByID`), `Notify func(ctx, room id.RoomID, markdown
  string) (id.EventID, error)` and `Redact func(ctx, room id.RoomID, evt id.EventID) error` (defaults:
  bridge bot, logging suppressed for `Notify`). `KindDevice = "device"`.
- `server.go`: the `app/` routes, `.webmanifest` content type, a per-route CSP, a janitor that also
  runs device purges (hourly) and due redactions (every minute).
- `api.go`: the `authenticate` branch, the `commandRoom`/`call {}`/enqueue changes for `KindDevice`,
  span attribute `widget.client`.

### 8.3 Headers for `app/`

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:;
  connect-src 'self'; manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'none';
  frame-ancestors 'none'
X-Frame-Options: DENY
Cross-Origin-Opener-Policy: same-origin
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
```

The widget page keeps its CSP (`frame-ancestors <widget.frame_ancestors>`). `frame-ancestors` does not
matter for a top-level PWA except to forbid framing, which is what `'none'` does.

---

## 9. UI (`static/app/`, plain JS, no build step)

### 9.1 Same dialpad as the management-room widget

Number field with debounced live normalization (`POST normalize`, "Will call +1…"), 3×4 keypad (A-D
toggle), Call, Hang up / Decline, DTMF while `can_dtmf` (masked after 5 s), status line with
`aria-live`, the `phase` texts from WIDGET-SPEC §7/§9, polling (1 s during a call or 10 s after an
action, 5 s idle, paused while `document.hidden`; on `visibilitychange` to visible, one immediate poll).
Big touch targets (≥ 56 px in standalone mode), `inputmode="tel"`, `theme-color` meta matching the
manifest, `prefers-color-scheme`, safe-area insets (`env(safe-area-inset-*)`).

### 9.2 Device states

| State | Screen |
|---|---|
| No device, no code | "Set up this app" with **Paste link** (a textarea; the app extracts `mrtcsip_dpc_…` from any pasted text with `/mrtcsip_dpc_[A-Za-z0-9_-]{43}/`), and "Send `!sip dialpad-link` to the bridge bot in your management room to get one." (the command prefix is from `GET` of nothing: the page cannot know it, so the text says "your bridge bot's `dialpad-link` command") |
| Code in fragment or pasted | Fragment removed first. `enroll {confirm:false}` → "Set up this device for **@user:hs** (*Pixel*)? This app can place calls on your phone account." [Set up] [Cancel]. Already enrolled → "This replaces the current sign-in (@…)." |
| Enrolled | Header: "@user:hs · Pixel" (tap: device info, expiry, **Forget this device**, and **Install** hint when `beforeinstallprompt` fired) |
| `device_expired` / `device_revoked` / `device_invalid` | Clear storage (token only; keep recent numbers unless the user clears them), show "This device was signed out (not used for 90 days / revoked). Get a new link with `dialpad-link` in your management room." plus Paste link. With `reason: "reuse"`: "Signed out for safety: an old copy of this app's sign-in was used somewhere else." |
| `link_used` / `link_expired` | "This link was already used / has expired. Request a new one." |
| 403 `login_required` | Refusal text (for example "No SIP account is set up for this login…") |
| Offline / 5xx / 429 | "No connection to the bridge" / "Slow down", retry with backoff |

Storage: one key, `localStorage["mrtcsip.dialpad.device.v1:<prefix hash>"]` = JSON `{token, id,
name, user, gen}` (the prefix hash keeps two bridges on one host apart; `gen` is a local counter that
increases on every stored rotation). The whole object is written with a single `setItem`, which is
atomic per key: a reader sees either the old or the new credential, never a mix. The session lives in
memory only (per tab).

### 9.2.1 Rotation in the client: locking, races, lost responses

`renew()` runs on app load, on `visibilitychange` to visible when the session is missing or expires
within 5 min, 5 min before session expiry while open, and once on a 401 from any API call:

```js
async function renew() {
  return navigator.locks
    ? navigator.locks.request(LOCK_NAME, renewLocked)   // LOCK_NAME includes the prefix hash
    : renewLocked();                                     // no Web Locks: rely on the grace window
}
async function renewLocked() {
  const cur = load();                          // re-read inside the lock: another tab may have rotated
  const r = await post("device/session", {device_token: cur.token});  // may throw (network)
  store({...cur, token: r.next_token, gen: cur.gen + 1});  // 1. persist the successor FIRST
  session = r.session;                                     // 2. then use the session
  confirmSoon(r.next_token);                               // 3. confirm (retried, idempotent)
}
```

- **Lock**: Web Locks (`navigator.locks`, supported by Chrome/Firefox Android and Safari 15.4+)
  serialize rotations of all tabs and the installed app of one browser profile (they share the origin).
  Inside the lock the tab re-reads `localStorage`, so a tab that waited uses the token the other tab just
  stored instead of rotating a superseded one. Sessions are per tab, so the waiting tab still does its
  own exchange, with the fresh token.
- **`storage` event**: tabs listen for changes of the key. A change to another `token` only updates the
  in-memory copy; removal (forget/revoke in another tab) signs this tab out.
- **Without Web Locks**: two tabs may rotate the same token concurrently. Both get successors; the
  server allows it (siblings inside the grace window, §2.7), and the last `setItem` wins. The tab whose
  successor was overwritten keeps its session (valid for its hour) and, at its next renew, reads the
  winner's token from storage. The overwritten successor stays unconfirmed and is pruned.
  If the overwritten successor's confirm reached the server after the stored one's, the stored token is
  on the losing branch and the next renew after the grace window is a (false) reuse sign-out (§2.7 "Live
  branches"); only lock-less browsers can hit this.
- **Lost exchange response** (network error after the server rotated): nothing was stored, the client
  still has `T`. `T` is still current (only confirmation supersedes it), so the retry works at any time
  later, even days offline. The orphan unconfirmed child is pruned server-side.
- **Response received, app killed before `setItem`**: same as above.
- **Stored `C`, confirm lost or app killed before confirm**: the next renew presents `C`; the server
  treats a presented unconfirmed token as confirmed (§2.7 step 2). Meanwhile `T` stays valid, so nothing
  breaks. `confirmSoon` retries with backoff (1 s, 5 s, 30 s) and stops on success or on a 401.
- **Storage write fails** (quota, private mode): do not use the session's successor flow; keep `T`, show
  "Could not save the sign-in on this device", and skip the confirm, so `T` remains current.
- **Retry discipline**: network errors and 5xx retry with backoff, always with the token currently in
  storage (re-read each time). A 401 is final: no retry with an older token, ever (the client never keeps
  previous tokens, so it cannot trigger a false reuse by itself).

### 9.3 Recent numbers (optional, on by default, local only)

The last 10 dialed E.164 numbers (after normalization, as the server confirmed them), newest first,
tap to fill and call. Stored in `localStorage` on the phone only, never sent anywhere. Privacy note in
the settings sheet: "Recent numbers stay on this phone. Anyone who can unlock it can see them." A toggle
turns them off (and clears them), plus a **Clear** button; **Forget this device** asks whether to clear
them too. No contact names (no address-book access; the Contact Picker API is not used).

### 9.5 Tabs and Contacts

When the server reports `features.contacts` (WIDGET-SPEC §7.5, §9) the main screen has a Dial / Contacts tab
bar (Dial by default; hidden during a call). The Dial tab keeps everything above, including the local
Recent list (§9.3); the Contacts tab is the same as in the room widget: search, paging, and a Call and a
Message button per row. The list, the query and the request sequence live in memory only. They are cleared
by `stopApp`, so every sign-out path wipes them: Forget this device, a revoked, expired or reused device,
a blocked sign-in, and a sign-out in another tab (`storage` event). Nothing about contacts is written to
`localStorage`, IndexedDB or the service worker cache (`sw.js` never touches `api/`). The shell changed,
so `sw.js` `VERSION` is bumped.

### 9.4 Callback caveat and the Matrix client

After `call`, the status reads "Ringing your Matrix app: answer there to connect", then the usual
phases. A one-line explainer sits under the Call button on first use: "Calls ring in your Matrix app
(Element X, Sable). Answer there; this app only dials and controls the call."

Deep link (informational, optional button "Open in Matrix"): `state.call.room_id` makes a
`https://matrix.to/#/<room_id>` link. On Android, Element X declares an intent filter for `matrix.to`
links, so the link may open the room in Element X directly (**unverified for Element X Android/iOS
versions in use**; otherwise matrix.to's chooser page opens). It is not needed for the main flow: the
callback ring already reaches Element X as an incoming-call notification (full-screen ring on Android
when the app is allowed to), so the user answers from the notification. A deep link into the ringing
*call* (rather than the room) is not available in any client; don't build anything on it.

---

## 10. Telemetry (TELEMETRY-SPEC; spans and metrics added to the catalogue and README in the same change)

| Span | Kind | Class | Attributes |
|---|---|---|---|
| `widget.device.enroll` | SERVER | root | `matrix.sender` (once known), `widget.device.id`, `widget.device.confirm` (bool), `outcome` (ok, peek, invalid, used, expired, rate_limited, error) |
| `widget.device.session` | SERVER | root | `matrix.sender`, `widget.device.id`, `widget.device.token_state` (current, unconfirmed, grace), `outcome` (ok, invalid, expired, revoked, reuse, forbidden, rate_limited, error) |
| `widget.device.confirm` | SERVER | root | `matrix.sender`, `widget.device.id`, `outcome` (ok, repeat, invalid, revoked, reuse, error) |
| `widget.device.forget` | SERVER | root | `matrix.sender`, `widget.device.id`, `outcome` |
| `cmd.dialpad_link`, `cmd.dialpad_links`, `cmd.dialpad_revoke` | CONSUMER | root (`WithSpan`) | `cmd.name` only; `Issue`/`Revoke` add `widget.device.id` and `outcome` as events, never the name or code |
| `bridge.widget.device_janitor` | INTERNAL | root, **only** when it purged or redacted something | `devices.purged`, `notices.redacted`, `notices.failed` |
| existing `widget.call`/`widget.dtmf`/`widget.hangup` | | | add `widget.client` (`widget`, `pwa`) and `widget.device.id` for PWA sessions |

Metrics (closed enums only; no MXIDs, numbers, link IDs or names):

| Metric | Instrument | Attributes |
|---|---|---|
| `mrtcsip.widget.requests` (existing) | Counter | `widget.action` gains `enroll`, `device_session`, `device_confirm`, `forget`; `outcome` gains `expired`, `used`, `reuse`; new `widget.client` (`widget`, `pwa`) |
| `mrtcsip.widget.device_links` | ObservableGauge `{link}` | `state` (`pending`, `active`); from the store's cached counts (refreshed by the janitor), no I/O in the callback |
| `mrtcsip.widget.device_events` | Counter `{event}` | `event` (`issued`, `enrolled`, `rotated`, `grace_used`, `reuse_revoked`, `revoked`, `expired`, `orphan_purged`, `notice_redacted`) |

`GET state` from the PWA stays span-free (polling).

---

## 11. Host wiring

### 11.1 Standalone (`pkg/connector/widget.go`)

- In `Start`, when `widget.enabled && widget.pwa.enabled`: `NewDeviceStore(c.br.DB.Database, c.br.ID,
  …)`, `Upgrade(ctx)` (an error fails start, like other DB errors), and pass it as `Deps.Devices`.
- `ResolveDevice`: `GetCachedUserLoginByID(login)`, `UserMXID == user.MXID` (else revoked), `loginClient`
  for readiness (refusal "You are not logged in…").
- `CommandDeps.Dialpad`: a late-bound adapter like `widgetInstaller` (commands are registered in
  `PostInit`, the server exists only after `Start`), answering "The Dialpad app is not enabled on this
  bridge." until then or when disabled.

### 11.2 Jabber (`local/sip-calls` only, `//go:build sipcalls` files, never pushed)

- `sipcalls_widget.go`: the same store creation in the widget start path (jabber DB `x.br.DB.Database`);
  `ResolveDevice` with `commandLoginWithAccount` rules (a real `*xmppClient` login of the user, SIP
  account attached; refusal texts from `sipcalls_commands.go`); `Dialpad` adapter.
- `sipcalls_commands.go`: pass the adapter into `hostkit.CommandDeps`.
- No untagged file changes. Logout needs no hook (janitor orphan purge, §2.4).

---

## 12. Tests

- `devicestore` (SQLite in-memory via `dbutil`, fake clock): schema upgrade from empty and idempotent
  re-run; issue/peek/enroll single-use under concurrency (two goroutines redeem one code, exactly one
  wins); expiry of pending devices; inactivity expiry slides with each rotation; optional `hard_expiry`
  (off by default); per-user limit counting pending+active; revoke one/all only for
  the owner; orphan purge; `bridge_id` isolation (two bridge IDs in one DB); only hashes in the table
  (scan every column for the plaintext).
- Rotation (fake clock, same store): exchange returns an unconfirmed successor and the old token stays
  current; confirm supersedes the parent and starts grace; parent inside grace → OK; parent after grace →
  reuse revoke of the whole family (all tokens refused, sessions killed, `reason: "reuse"`, Warn log
  without the token, notice sent once); presenting an unconfirmed successor confirms it; lost exchange
  response (discard the successor) → the old token still works days later, no reuse; confirm is
  idempotent; two concurrent exchanges of one token → two siblings, both usable inside grace, the one
  confirmed first goes to grace then dead (reuse after grace); an unconfirmed child whose parent is
  dead → reuse (thief fork PoC), while its parent is current or in grace → OK; at most 2 unconfirmed children kept; pruning of superseded rows after
  `inactivity_days`; a revoked family's tokens never rotate; client-change notice at most once per day.
- `device.go` handlers (httptest, fake store clock, fake Deps): peek has no side effects; confirm
  enrolls; reused/expired/garbage codes → right codes; malformed codes never hit the DB; failure lockout
  shared with widget sessions; device session → `KindDevice` session; revoke kills a live session (next
  `state` → 401); user permission removed → 403; login gone → `device_revoked`; `call {}` refused,
  `call {number}` ok, `hangup`/`dtmf` use room ""; no installer enqueue for device sessions; `forget`
  only with a device session; Origin/content-type checks; rate limits with `Retry-After`.
- Routes/headers: `app/` 404 when disabled; app CSP (`frame-ancestors 'none'`, `manifest-src`,
  `worker-src`) and `X-Frame-Options: DENY` only on `app/`; widget page CSP unchanged; manifest valid JSON
  with relative `start_url`/`scope` and the three PNG sizes; `sw.js` served no-cache; SW cache version
  equals the shell hash; icons decode with the right dimensions.
- Static lint (Go test over embedded files): no `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `eval`,
  `new Function`, `document.write`; no `http:`/external URLs; `sw.js` never `respondWith` for `api/`
  (string check on the route list).
- hostkit commands (fake `DialpadLinks`): management-room-only (also with sudo); private-room refusal;
  limit reply; list/revoke formatting; the reply is sent with a Nop-logger ctx: a test logger at Trace
  around the command captures nothing that contains `mrtcsip_dpc_`; `AttachNotice` gets the event ID.
- Redaction: on enroll; at expiry (fake clock); after "restart" (new store over the same DB) the janitor
  redacts due notices; failure retried at most 3 times.
- Privacy (obstest + captured logs): a full flow (issue, enroll, session, call, dtmf `"159#"`, revoke)
  never shows the code, token, session or digits in logs or span attributes, and metrics have only the
  enum attributes.
- Telemetry: span names and classes; janitor span only when it did work; no spans for `state`.
- Config: defaults, ranges, `pwa` without `widget` refused, upgrader copies keys (both hosts).
- jabber (`sipcalls`): `ResolveDevice` refuses non-xmpp logins and reports `ready=false` without a SIP
  account; the commands are wired; the store migrates in the jabber DB.
- JS: no toolchain; checked in the live test, including the two-tab and lost-response cases (§15).

Build, vet and test: `-tags "nocrypto nolibopusfile"` (mrtc) and jabber's `sipcalls` build.

---

## 13. Task split and file ownership

Order: P1 ∥ P2 (contracts) → P3 ∥ P4 → P5 ∥ P6 → Opus security review (P1, P3, P4 commands, P4 UI token
handling) → P7.

| Task | Tier | Owns (edit only these) | Contract |
|---|---|---|---|
| **P1** device store incl. rotation | **Opus** (rotation and reuse logic are security-critical) | `pkg/widget/devicestore.go`, `pkg/widget/devicestore_upgrades/*`, `pkg/widget/devicestore_test.go` | §2.3-2.7, §8.2 store API |
| **P2** hostkit config + commands | Sonnet | `pkg/hostkit/config.go` (`PWAConfig`), `example-config.yaml`, `upgrade.go`, `commands.go` (or new `dialpad.go`), `*_test.go` there | §4, §8.1; `DialpadLinks` interface exactly as §4 |
| **P3** widget server: device API, auth branch, routes, headers, janitor, telemetry | **Opus** (security-sensitive) | `pkg/widget/device.go` (new), `auth.go`, `api.go`, `server.go`, `ratelimit.go`, `telemetry.go`, their tests, `internal/genicons/` (new), `docs/TELEMETRY-SPEC.md` | §3, §5, §6.1, §7, §8.2-8.3, §10 |
| **P4** PWA front end | Sonnet, Opus review of `renew()`/storage | `pkg/widget/static/app/*` (new), `static/dialpad.js` (extracted), `static/app.js` + `index.html` (switch to `dialpad.js`, no behavior change) | §6.2-6.5, §7 API, §9 (incl. §9.2.1) |
| **P5** standalone wiring + docs | Sonnet | `pkg/connector/widget.go`, `widget_test.go`, `start.go`/`commands.go` hooks if needed, `README.md` (PWA section: install steps, dedicated-host advice, metrics/spans), `docs/PLAN.md`, `docs/DECISIONS.md` (ADR-013 from §14), `docs/EMBEDDING.md` (PWA note) | §11.1 |
| **P6** jabber wiring (**local branch `local/sip-calls` only; never push; commit only per the Phase J local-commit practice**) | Sonnet | `pkg/connector/sipcalls_widget.go`, `sipcalls_widget_test.go`, `sipcalls_commands.go` (adapter only); no untagged files | §11.2 |
| **P7** deploy + live test | coordinator | both bridge configs (`widget.pwa`) | §15 |

---

## 14. Proposed ADR-013 (added by P5)

**ADR-013: Dialpad PWA: one-time enrollment link, bridge-issued device tokens.** Supersedes ADR-012's
"would revisit if a standalone PWA ... replaces the widget" note only partly: the widget stays; the PWA
is an extra for clients without widgets (Element X). Context: outside a Widget API host there is no
OpenID. Decision: `dialpad-link` (management room only, private room only) issues a 15-minute single-use
code in a fragment link; the PWA redeems it after user confirmation for a 256-bit device token (stored
hashed in own `dbutil` child tables). Every exchange for the existing 1 h in-memory session (management-
room scope, one login) rotates the token; the predecessor is superseded once the client confirms the
successor and stays valid for a 120 s grace window; a superseded token used after grace revokes the
whole family (stolen-copy detection). Expiry is by inactivity (90 days), with an optional absolute
lifetime that is off by default, so a device in use never has to be reinstalled. Revocation deletes the
family and the live sessions. Consequences: a stolen copy works only until the real app's next
rotation trips detection, or indefinitely if the real app is never used again (until inactivity expiry
or revoke); restoring an old browser backup signs the device out; the origin of `public_url` holds a long-lived secret in `localStorage`,
so a dedicated hostname is recommended; the PWA does not carry audio (callback ring). Would revisit if:
Element X gains room widgets, a Matrix login inside the PWA becomes practical (then the device token
could be replaced by the user's own Matrix session plus OpenID), or Web Push for call status is wanted.

---

## 15. Live test plan

Outbound calls go **only to +17377273232**. **Never call +15550001234** without warning the user first.
Standalone first; `mautrix-jabbercalls` takes over the SIP account only when the standalone bridge is
stopped.

1. Deploy with `widget.pwa.enabled: true` (no Traefik change: `/sip-dialpad/` already routed).
   `curl -I https://example.org/sip-dialpad/app/` → 200 with `frame-ancestors 'none'`, `manifest-src`;
   `.../app/manifest.webmanifest` → `application/manifest+json`; `.../api/v1/device/session` with a
   garbage token → 401; six garbage tries → 429 lockout. Start log: the shared-origin Warn.
2. `dialpad-link Pixel` in a portal room → refused (management room only). In the management room →
   notice with the code span. Check the bridge log at debug: no `mrtcsip_dpc_` anywhere. `dialpad-links`
   shows `waiting for setup`.
3. In Element X, Sable and gomuks: is the code span linkified or previewed? (Record the results; if a
   client linkifies code, note it in the spec.) Check the Synapse log for `preview_url` with the code.
4. Android Chrome: open the bare app URL → Paste link → confirmation shows the right MXID → Set up. The
   link notice is redacted within seconds; the enrollment notice appears with `chrome_android`.
   Install (⋮ → Install app); open from the home screen: standalone window, still enrolled.
5. In the app: type `737 727 3232` → "Will call +17377273232" → Call → Element X rings → answer →
   "Connected" in the app → keys `1`,`2`,`#` reach the IVR (logs show only `dtmf.digit_count`) → Hang up
   from the app → Ready. Recent numbers shows +17377273232; Clear works.
6. Reuse the same link in another browser → "already used". Wait 15 min with a fresh unused link → the
   message is redacted, the link says expired. Restart the bridge in between once: redaction still happens
   (janitor) and the enrolled app keeps working after the restart (silent re-exchange).
6a. Rotation: open the app twice (two tabs in Chrome, or app + tab) and reload both several times →
   never signed out; DB shows one family with one or two current tokens. Airplane mode right after
   tapping reload (lost response), wait > 2 min, reconnect → still signed in. Copy the stored token
   from DevTools (desktop Chrome with an enrolled test device), let the app rotate, wait > 2 min, then
   `curl` `device/session` with the copy → 401 `reason: "reuse"`, Warn log, notice in the management
   room, the app shows "Signed out for safety".
7. Firefox Android: enroll with a second link, Install, call once (steps 4-5 abbreviated).
8. iOS Safari (if a device is available): Add to Home Screen first, enroll inside the app, call once.
   Confirm that a code redeemed in Safari tab does not carry over (expected).
9. `dialpad-revoke <id>` during an idle app → the next poll shows "signed out (revoked)". Revoke during an
   active call → the app signs out; the call continues in the Matrix client (revocation does not hang
   up; hang up there).
10. "Forget this device" in the app → `dialpad-links` no longer lists it.
11. `max_links_per_user` reached → the command refuses and lists IDs. `sip-logout` (jabber) or logout
    (standalone) → the app shows the refusal text / `device_revoked`; after the janitor runs the link is
    gone from the list.
12. Repeat 2, 4, 5, 9 with `mautrix-jabbercalls` (standalone stopped) and its prefix `/jabber-dialpad/`;
    note that it is a separate installed app.
