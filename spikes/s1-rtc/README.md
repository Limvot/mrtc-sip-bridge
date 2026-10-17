# Spike S1: MatrixRTC ring from an appservice ghost

Throwaway program that proves an appservice ghost can make the owner's Element X and Element Web **ring**
via MatrixRTC, and after the owner answers, be a LiveKit participant publishing audible audio attributed to
the ghost while receiving the owner's audio. Wire spec and rationale: `../../docs/spikes/S1-matrixrtc-wire-spec.md`
(cited below as "spec §N"). Nothing in the spec had been exercised against live clients before this spike.
It is its own Go module (mautrix v0.32.0, server-sdk-go v2.19.0, hraban/opus).

The appservice is **outbound only**: the registration has `url: null`, there is no HTTP listener, and all
Matrix traffic is client-server API with the AS token and `?user_id=` masquerading. The program polls room
state for the owner's `call.member` (answer) and `/messages` for decline events.

## Build

```bash
cd spikes/s1-rtc
go build -tags nolibopusfile -o s1-rtc .   # Go 1.24+, cgo, libopus headers (pkg-config opus)
```

`-tags nolibopusfile` is needed unless `opusfile` dev headers are installed: hraban/opus links opusfile by
default, but the encoder does not use it. (Alternatively `export GOFLAGS=-tags=nolibopusfile`.)

## Generate the registration

```bash
./s1-rtc gen-registration -server-name example.org -out out/registration.yaml
```

Prints the path only (the file, mode 0600, holds the tokens; `out/` is gitignored). It uses id
`mrtc-sip-spike`, `url: null`, sender `sipspikebot`, an exclusive namespace `@sipspike_.*:<server>`,
`rate_limited: false`. Copy its `as_token` into `MATRIX_AS_TOKEN` in the env file.

## What the human does on the homeserver

Full checklist: spec §8. Summary (Synapse):

1. Add the registration to `app_service_config_files` in `homeserver.yaml`.
2. `experimental_features`: `msc4143_enabled`, `msc4222_enabled`, `msc4075_enabled`; set
   `max_event_delay_duration: 24h`; `matrix_rtc.transports` with `type: livekit` and the public
   `livekit_service_url` (no `url:`).
3. lk-jwt-service (v0.9.0+) with `LIVEKIT_FULL_ACCESS_HOMESERVERS=<your server_name>` (otherwise the ghost's
   token has `canPublish=false`), whole `/livekit/jwt/` prefix proxied; the federation/openid endpoint must be
   reachable from lk-jwt-service for OpenID verification.
4. Restart Synapse. **First confirm two normal Element accounts can call each other** (rules out
   transport/proxy problems). Confirm an @-mention pushes to the phone while Element X is backgrounded.

## Settings

Environment, optionally loaded from `-env-file` (default `~/.config/mrtc-sip-bridge/s1.env`; must be mode
0600 or the program refuses to start; variables already set in the environment win). Tokens are never logged.

```
MATRIX_HOMESERVER_URL=https://matrix.example.org   # client-server API base
MATRIX_SERVER_NAME=example.org
MATRIX_AS_TOKEN=...                                # as_token from out/registration.yaml
OWNER_MXID=@you:example.org
LIVEKIT_SERVICE_URL=https://matrix-rtc.example.org/livekit/jwt   # same as matrix_rtc.transports; /sfu/get is appended
```

## Usage

```
s1-rtc ring [flags]
```

Flags: `-ghost` (localpart, default `sipspike_15550001234`), `-room` (reuse a room id; the ghost must be able to
join it, otherwise a new unencrypted `private_chat` DM-style room is created and the owner invited),
`-device-id` (`MRTCSPIKE`), `-ring-timeout` (30s, also the notification `lifetime`), `-prejoin` (join LiveKit
before the owner answers), `-max-duration` (5m), `-env-file`.

Flow: register ghost, set displayname, create/join room, set `call.member` PL to 0 (spec §1.5), wait for the
owner to join, send ghost `org.matrix.msc3401.call.member` (spec §1.3) then `org.matrix.msc4075.rtc.notification`
(spec §2.2), poll every second for answer/decline/timeout. On answer: OpenID token as the ghost, `POST
/sfu/get`, log the JWT `sub` and `video.room` (never the token), connect, publish a 440 Hz beep (400 ms on,
600 ms off, Opus 48k mono, 20 ms frames, Source MICROPHONE), subscribe to remote audio, log
`rtc stats` (tx frames, rx packets, participants) every 5 s. Ends on owner leaving, max duration or Ctrl-C;
always clears the membership (`{}`) and, if unanswered, redacts the notification.

## Test procedure (human)

1. Owner signed in on Element X (phone, notifications on, app backgrounded) **and** Element Web (open).
2. `./s1-rtc ring`. When the log says "waiting for owner to join", accept the room invite in Element.
3. Expected: ring on phone (full-screen incoming call) and an incoming call toast on Web. Answer on one;
   confirm the other stops ringing. Expected log: `owner ANSWERED`, `jwt claims`, `livekit connected`,
   `published tone track`. You hear the beeps; speak, and `rx_packets` in `rtc stats` keeps rising.
   Hang up from the client and confirm `call ended` plus cleanup.
4. Repeat, this time **declining** (on phone, then Web). Expected: `owner DECLINED`, notification redacted,
   membership cleared; both devices stop ringing. Also let one run to `ring timeout`.
5. Repeat with `-prejoin`, answering on each client. Compare audio quality and one-way audio against the
   default run.

If nothing rings, check in order: push for a plain mention, the room is unencrypted, call.member PL log line,
`/sfu/get` errors, and rerun with the ring on the other client only.

## Results checklist (spec §9)

- [ ] Q1: When the owner answers on an Element Call 0.28 client, does Element X on the owner's *other* device
      stop ringing (missing `livekit_alias` vs ruma)? Note device/client versions. If not, how long until it stops?
- [ ] Q2: Does the ghost's audio render and sound in Element X and in Element Web (identity match)? Record the
      logged `sub` (== `@ghost:server:DEVICEID`?), `video.room` (== expected hash?) and `canPublish`.
- [ ] Q3: Does the default join-on-answer run give two-way audio, or is `-prejoin` required? Compare runs.
- [ ] Q4: `RequestOpenIDToken` on a masqueraded ghost client worked (mautrix v0.32.0 `client.go:1995`)? Yes if
      the log shows `openid token minted for ghost`.
- [ ] Extra: ring on Element X (Android/iOS) and Web; the other device stops on answer; decline works from
      both; ring time from send to toast; the owner's `rx_packets` > 0 while speaking; audio is not silent
      (no E2EE in the room).

## Notes and known gaps

- Cleanup after a crash: a stale ghost membership expires after 4 h. Rerunning with the same `-device-id`
  overwrites it; a membership with identical content is dropped by Synapse, but `created_ts` always differs.
- Declines are matched on the `/messages` 50 most recent events; fine for a quiet test room.
- LiveKit connection state, participants and remote tracks are logged, but no RTP quality stats.
