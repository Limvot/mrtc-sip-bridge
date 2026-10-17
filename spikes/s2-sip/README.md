# Spike S2: SIP UA against JMP.chat

Throwaway program that proves a laptop behind home NAT (no port forwards) can register to a SIP account,
receive and place calls, and exchange PCMU audio. Spec and rationale: `../../docs/spikes/S2-sip-ua-design.md`.
It is its own Go module (diago v0.41.0, sipgo v1.6.0, pion/stun v3.1.7) so the repo root stays clean.

> **WARNING: registering can disrupt your existing ATA.** JMP does not officially support two
> registrations on one account. Pause or power off the ATA (or whatever else is registered with these
> credentials) before running `register`, and expect it to need a reboot afterwards.

## Build

```bash
cd spikes/s2-sip
go build -o s2-sip .      # needs Go 1.24+ and cgo (diago pulls in opus via cgo, libopus headers required)
```

## Credentials

Only via environment variables, optionally loaded from an env file (`-env-file`, default
`~/.config/mrtc-sip-bridge/spike.env`). The file must be mode 0600 or the program refuses to start. Variables
already set in the environment win over the file.

```
SIP_USERNAME=...        # numeric JMP SIP username
SIP_PASSWORD='...'      # quote it, spaces allowed
SIP_REGISTRAR=host:port # from your ATA config (do not trust 2021 values)
SIP_DOMAIN=             # optional, only if different from the registrar host
SIP_CALLER_ID=          # optional From user for outbound (default: username)
```

The password is never logged; the username is logged masked (`ab***yz`). `-sip-debug` prints raw SIP with
`Authorization`/`Proxy-Authorization` values replaced by `[REDACTED]`. Phone numbers are logged, DTMF digits
are not (count only). Recordings (`out/*.wav`) contain real call audio; they are gitignored, keep them local.

## Usage

```
s2-sip register [flags]           register, stay registered, auto-answer inbound calls
s2-sip call [flags] <number>      outbound call (flags BEFORE the number), hang up after -call-seconds
```

Key flags (all have defaults, `s2-sip register -h` lists them): `-registrar`, `-domain`, `-bind`
(0.0.0.0:5060; unspecified IP becomes the LAN IP), `-public-ip` (manual, skips STUN), `-stun`
(`stun.l.google.com:19302`, `off` disables), `-public-sip-port`, `-rtp-port-range` (40000-40100),
`-expiry` (60s), `-reregister` (25s), `-symmetric-rtp` (true), `-early-rtp` (5 silence packets on answer),
`-record-dir` (./out), `-tone-hz`/`-tone-ms`, `-call-seconds`, `-sip-debug`.

## Test procedure (human)

0. Get your current JMP SIP username, password and registrar host:port from the ATA config. Pause or power
   off the ATA first (see warning above).
1. Create the env file with those variables, `chmod 600` it. Confirm `git status` does not list it.
2. `./s2-sip register`. Expected: `stun result public=...`, `digest challenge ... realm=... algo=...`,
   `register ok expires=... contact=<public ip>:<port>`. Repeated 401: wrong username/password or realm
   mismatch (try `-domain`).
3. Leave it 10 minutes. Expected: `reregister ok` about every 25 s. If it drops, lower `-reregister`.
4. Inbound test: call the JMP number from a mobile on another carrier. Expected: `invite from=...
   offered_codecs=...`, `answer`, `early rtp sent`, `rtp stats` lines every 5 s. Speak 15 s, press a few
   DTMF keys, hang up. Expected: `out/in-<timestamp>.wav` (stereo: caller on one channel, our tone on the
   other) with your voice, a 2 s 440 Hz tone heard on the phone, `dtmf received digit-count=N`, `bye by=remote`.
5. Silence or one-way audio: rerun with `-sip-debug`. Vary one at a time: `-symmetric-rtp=false`,
   `-early-rtp=20`, `-public-ip <ip>`, a different `-rtp-port-range`; or enable router UPnP / forward the RTP
   range (record that as the result). Watch the wire:
   `tcpdump -ni any 'udp port 5060 or portrange 40000-40100'`. Look for `rtp remote address changed
   (latching)` in the logs.
6. Outbound test: `./s2-sip call +1XXXXXXXXXX` (your own mobile). Expected: `outbound response status=100/180/183`,
   `outbound answered`, tone on the mobile, hang up after `-call-seconds`. Check the caller ID (Bandwidth may
   require the SIP username as From user, set `SIP_CALLER_ID` otherwise) and that the INVITE digest challenge
   (401/407) is handled.
7. NAT longevity: idle 30 min, then call in; then 2 hours.
8. Reconnect: disconnect Wi-Fi 60 s, reconnect, confirm re-registration and an inbound call. Then change the
   public IP if possible (router reboot) and confirm the Contact updates (restart may be needed, see notes).
9. Record findings in `docs/PLAN.md` (S2 status) and `docs/RESEARCH.md`: actual registrar host/port, Expires
   accepted, codecs offered, whether rport/received is honoured, whether the carrier latches, minimum working
   re-register interval, DTMF payload type.

Exit criteria are in the design doc, section 5.

## Notes and known gaps

- `call` mode also listens on the SIP port (diago needs the socket to receive responses); inbound calls are
  rejected with 486. Do not run `register` and `call` at the same time on the same bind port.
- `-keepalive` (OPTIONS/CRLF) from the design doc is not implemented; the forced re-register is the keepalive.
- The public IP is discovered once at startup; after an IP change, restart the program.
