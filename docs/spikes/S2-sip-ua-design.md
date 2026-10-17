# Spike S2: SIP UA against JMP.chat, design and research

Status: **research and design only, no code written.** Written 2026-10-16. Source-level facts below were
read from the module sources at the pinned versions (downloaded via the Go module proxy, not from memory).
Anything marked **UNVERIFIED** has to be confirmed against a live JMP account or by running the code.

Versions examined:

| Module | Version | License | Notes |
|---|---|---|---|
| `github.com/emiago/sipgo` | **v1.6.0** (proxy `@latest` reported v1.5.0 on 2026-10-16, but `@v/list` has v1.6.0 and diago v0.41.0 requires it) | BSD-2-Clause | |
| `github.com/emiago/diago` | **v0.41.0** (2026-10-06) | MPL-2.0 | `go 1.24.0`; pre-1.0 so the API still moves (see its `WEBRTC_PION_CHANGES.md`) |
| `github.com/livekit/media-sdk` | **v0.1.1** (2026-09-07) | Apache-2.0 | |

pkg.go.dev: `https://pkg.go.dev/github.com/emiago/sipgo@v1.6.0`, `.../diago@v0.41.0`, `.../livekit/media-sdk@v0.1.1`.

---

## 1. sipgo (v1.6.0)

### Setup

```go
ua, err := sipgo.NewUA(
    sipgo.WithUserAgent("mrtc-sip-spike"),
    sipgo.WithUserAgentHostname(publicIPOrHost), // used in Via/Contact defaults
)
client, err := sipgo.NewClient(ua,
    sipgo.WithClientHostname(publicIP),      // host placed in Via/Contact
    sipgo.WithClientPort(5060),              // local port (source port of UDP)
    sipgo.WithClientNAT(),                   // sets rport on Via (RFC 3581)
)
srv, err := sipgo.NewServer(ua)
srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) { ... })
srv.OnAck(...); srv.OnBye(...); srv.OnCancel(...); srv.OnOptions(...)
go srv.ListenAndServe(ctx, "udp", "0.0.0.0:5060")
```

Relevant constructors and options (all in `ua.go`, `client.go`, `server.go`): `NewUA`, `WithUserAgent*`,
`NewClient`, `WithClientHostname/Port/ConnectionAddr/NAT/Addr/Logger`, `NewServer`, `Server.OnInvite/OnAck/
OnBye/OnCancel/OnOptions/OnRegister/OnRequest`, `Server.ListenAndServe(ctx, network, addr)`, `ServeUDP(net.PacketConn)`.
`ServeUDP` lets us hand in our own socket so the client and server share one UDP source port, which is
**required behind NAT** (one binding for REGISTER and for incoming INVITEs).
Debug: `sip.SIPDebug = true` dumps raw SIP (will contain digest responses, never enable in shared logs).

### REGISTER with digest auth

There is **no one-call "register" helper in sipgo itself**. The building blocks are:

- `sipgo.ClientRequestRegisterBuild` (a `ClientRequestOption`): fills Via/Contact/Expires defaults for REGISTER.
- `client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)`: send and wait for the final response.
- **`client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{Username, Password})`**: given the 401/407 response,
  rebuilds the request with the `Authorization`/`Proxy-Authorization` header (uses `github.com/icholy/digest`)
  and returns the final response. `TransactionDigestAuth` is the transaction variant.
- Pattern (from `sipgo@v1.6.0/example/register/client/main.go`): build `sip.NewRequest(sip.REGISTER, uri)`,
  append `Contact`, `req.SetTransport("UDP")`, send; if 401, `DoDigestAuth`.
- Re-registration, expiry handling, rport/received contact rewriting: **not in sipgo**, you write the loop.
  (diago's `register_transaction.go` is a ready reference implementation, MPL-2.0.)

### Inbound INVITE and dialogs

- `sipgo.NewDialogServerCache(client, contactHDR)` returns a `*DialogServerCache`; also a lower-level
  `DialogUA{Client, ContactHDR, RewriteContact bool}`. `RewriteContact` sends in-dialog requests to the
  packet source instead of the Contact URI: **set it true behind NAT**.
- In the `OnInvite` handler: `dlg, err := cache.ReadInvite(req, tx)`; `dlg.Respond(180, "Ringing", nil)`;
  `dlg.RespondSDP(sdpAnswer)` sends 200 OK with the SDP body (then wait for ACK via `OnAck` →
  `cache.ReadAck(req, tx)`); `dlg.Respond(486/603, ...)` to reject; `dlg.Bye(ctx)` to hang up;
  `OnBye` → `cache.ReadBye(req, tx)`; `dlg.Context()`/`dlg.OnState` signal DialogStateEnded.
  `cache.MatchDialogRequest(req)` finds the dialog for in-dialog requests.
- Outbound: `sipgo.NewDialogClientCache(client, contactHDR)`; `dc.Invite(ctx, recipientURI, sdpBody, headers...)`
  returns `*DialogClientSession`; then `WaitAnswer(ctx, sipgo.AnswerOptions{Username, Password})` (handles the
  digest challenge on INVITE), `Ack(ctx)`, `Bye(ctx)`.
- Raw SDP is a `[]byte`; sipgo does not parse or generate SDP. Use `github.com/pion/sdp/v3` (as planned).

---

## 2. diago (v0.41.0)

### Verdict on scope

diago **is** the ready-made thing: a registering UA (with re-register loop), inbound handler, outbound INVITE
with digest auth, SDP generation/negotiation, RTP session (RTCP SR/RR, jitter buffer option), PCMU/PCMA/Opus,
RFC 4733 DTMF read/write, WAV playback, WAV recording, symmetric-RTP learning. It sits on sipgo v1.6.0.

Heavy transitive deps (pion/webrtc v3, ice, srtp, dtls fork, `hraban/opus.v2` via cgo, `zaf/g711`) are pulled
in even if we only use plain RTP. Acceptable for a spike and CI; revisit for production.

### Key API (read from source)

- `diago.NewDiago(ua, opts...)`; options: `WithTransport(diago.Transport{...})`, `WithAuth`, `WithMediaConfig`,
  `WithLogger(*slog.Logger)`, `WithServer`, `WithClient`, `WithServerRequestMiddleware`.
- `diago.Transport` fields (`diago.go:52`): `Transport` ("udp"/"udp4"/...), `BindHost`, `BindPort`,
  **`ExternalHost`, `ExternalPort`** (what goes in Contact), **`MediaExternalIP`** (what goes in SDP `c=`),
  `RewriteContact`, `MediaSRTP`, TLS fields. If `ExternalHost` is empty it falls back to the resolved bind IP,
  and `MediaExternalIP` defaults to the external host when that is an IP.
- `diago.MediaConfig{Codecs []media.Codec}`; codecs: `media.CodecAudioUlaw` (PT 0), `CodecAudioAlaw` (8),
  `CodecAudioOpus`, `CodecTelephoneEvent8000` (PT 101).
- **RTP port range:** package globals `media.RTPPortStart`, `media.RTPPortEnd` (default 0 = ephemeral).
- **Symmetric RTP:** `AnswerOptions.RTPNAT` / `media.RTPNATSymetric` (=1): the media session "learns" a
  changed source address and sends back to it. Also `media.RTPSourceLock` (security, locks to first source).
- `dg.Register(ctx, recipientURI, diago.RegisterOptions{Username, Password, Expiry, RetryInterval, OnRegistered})`:
  blocks, re-registers (default at 0.75 x server `Expires`, or `RetryInterval`), unregisters on ctx cancel;
  returns on 401/5xx. `dg.RegisterTransaction(...)` gives `.Register`, `.QualifyLoop`, `.Unregister`
  for finer control. **NAT-aware:** `createClient` always uses `sipgo.WithClientNAT()`, and after the
  200/401 it reads Via `rport`/`received` and rewrites the Contact (`register_transaction.go`, RFC 3581 §9).
  Caveat: only the Contact is rewritten, **not the SDP** (set `MediaExternalIP` yourself).
- Server: `dg.Serve(ctx, func(d *diago.DialogServerSession){...})` or `ServeBackground` / `HandleFunc`.
  The handler is the call scope: returning ends the dialog.
- `DialogServerSession`: `Trying()`, `Ringing()`, `Progress()`, `Answer(diago.AnswerOptions{Codecs, RTPNAT})`
  → `*DialogMedia`, `Hangup(ctx)`, `Close()`, `d.InviteRequest` (From/To/Call-ID), `d.Context()`.
- `DialogMedia`: `AudioReader()`, `AudioWriter()` (**encoded**, see below), `AudioReaderDTMF()` /
  `AudioWriterDTMF()` (+ `DTMFReader.OnDTMF`, `DTMFWriter.WriteDTMF`), `PlaybackCreate()` (WAV/PCM file or
  reader → encodes and paces), `AudioStereoRecordingCreate(*os.File)` (WAV), `Echo()`, `Listen()` /
  `ListenContext`, `MediaSession()`, `RTPSession()`.
- Outbound: `dg.Invite(ctx, recipient sip.Uri, diago.InviteOptions{Username, Password, Headers, ...})`
  → `(*DialogClientSession, *DialogMedia, error)`; or the low-level
  `dg.NewDialog(recipient, NewDialogOptions{})` → `dialog.Invite(ctx, InviteClientOptions{Username, Password,
  Headers, EarlyMediaDetect})` → `*DialogMedia`, then `dialog.Ack(ctx)`. `InviteClientOptions.WithCaller(name,
  callerID, host)` sets the From (needed for Bandwidth's "username as caller ID" rule, UNVERIFIED).
  Caller must `Ack` when using the low-level API and `Close` both dialog and media.

### Minimal code (shape only, **untested**, from the examples and signatures above)

```go
ua, _ := sipgo.NewUA(sipgo.WithUserAgent("mrtc-s2"))
media.RTPPortStart, media.RTPPortEnd = 40000, 40100
dg := diago.NewDiago(ua,
    diago.WithTransport(diago.Transport{
        Transport: "udp4", BindHost: "0.0.0.0", BindPort: 5060,
        ExternalHost: publicIP, ExternalPort: 5060,   // Contact; see section 3
        MediaExternalIP: net.ParseIP(publicIP),       // SDP c=
        RewriteContact: true,
    }),
    diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{
        media.CodecAudioUlaw, media.CodecTelephoneEvent8000}}),
)

// Serve inbound calls
go dg.Serve(ctx, func(d *diago.DialogServerSession) {
    d.Trying(); d.Ringing()
    med, err := d.Answer(diago.AnswerOptions{RTPNAT: media.RTPNATSymetric})
    // READ audio: payload is PCMU, 160 bytes per 20 ms. Use a buffer >= media.RTPBufSize.
    r, _ := med.AudioReader()
    buf := make([]byte, media.RTPBufSize)
    n, _ := r.Read(buf)                    // n bytes of PCMU
    pcm := make([]byte, n*2)
    audio.DecodeUlawTo(pcm, buf[:n])       // diago/audio: ulaw bytes → LE PCM16 at 8 kHz
    // WRITE audio: either play a WAV/PCM file ...
    pb, _ := med.PlaybackCreate(); pb.PlayFile("tone.wav")
    // ... or write PCMU bytes (20 ms = 160 bytes) to med.AudioWriter()
})

// Register (blocks, re-registers, unregisters on cancel)
var reg sip.Uri; sip.ParseUri("sip:"+host+":5008", &reg) // domain/host from the user's ATA config
go dg.Register(ctx, reg, diago.RegisterOptions{Username: u, Password: p, Expiry: 120*time.Second})

// Outbound
var dst sip.Uri; sip.ParseUri("sip:+15551230000@"+host, &dst)
opts := diago.InviteOptions{Username: u, Password: p}
dlg, med, err := dg.Invite(ctx, dst, opts)   // then dlg.Ack(ctx) if the high-level API does not
```

Examples in the module: `examples/register`, `examples/wav_record` (answer + stereo WAV recording + echo),
`examples/playback`, `examples/dtmf`, `examples/readmedia`, `examples/183ringing`. Copy shape from these
(MPL-2.0: file-level copyleft only applies if we copy the files; calling the library is fine).

### PCM vs encoded audio, and extraction for LiveKit

- `AudioReader()`/`AudioWriter()` carry **encoded payload** (PCMU bytes), not PCM. PCM helpers: package
  `diago/audio` (`DecodeUlawTo`, `EncodeUlawTo`, `DecodeAlawTo`, `OpusDecoder/Encoder`, `MonitorPCMReader/
  Writer`, `WavWriter`, `PCMMix`), and `PlaybackCreate()` accepts WAV/PCM and does encoding + pacing.
- **Raw RTP is accessible:** `med.MediaSession()` → `*media.MediaSession` has `ReadRTP(buf, *rtp.Packet)`,
  `ReadRTPRaw`, `WriteRTP(*rtp.Packet)`, `WriteRTPRaw`, and `med.RTPSession()` wraps it with RTCP stats.
  Packets are `pion/rtp` types, which match `livekit/media-sdk/rtp`. So extraction into the later LiveKit
  pipeline is straightforward either via `AudioReader` bytes (PCMU → media-sdk g711 decode) or raw RTP into
  media-sdk's `rtp.HandleLoop`. `RTPDtmfReader` / `media.DTMFDecode` expose RFC 4733 events.
- Conclusion: diago's media layer can be used as-is as the SIP-side media endpoint, with the LiveKit side
  using media-sdk. Risk: diago's `AudioWriter` has a running sample clock and expects frames matching codec
  sample duration (20 ms = 160 bytes). Our Opus→PCMU path must emit exact 20 ms frames (use
  media-sdk `FullFrames`).

### diago vs raw sipgo + pion/rtp for S2

- Raw sipgo S2 needs, beyond REGISTER/INVITE: our own re-register loop, SDP build/parse, RTP socket + packetizer
  (seq/ts/SSRC), jitter handling, G.711 codec, WAV writer, tone generator, DTMF 4733, symmetric RTP
  logic. Roughly 800 to 1200 lines.
- diago S2: roughly 150 to 250 lines. **diago makes S2 several times smaller.**
- Cost: pre-1.0 API churn, large dep tree, and a dependency on one maintainer's opinions about media
  threading. Mitigation: pin the version, wrap in our `pkg/sipua` interface per PLAN.md so it can be swapped for
  raw sipgo + media-sdk later.

**Recommendation: use diago for S2.** Keep the spike's SIP/media code behind a tiny interface so the
decision for production (diago vs sipgo + media-sdk) is made after S2 with real data. If diago's NAT or
RTP behaviour proves to be the blocker, fall back to sipgo (`DoDigestAuth` + `DialogServerCache`) and
`ListenUDPPortRange` from media-sdk `rtp`.

---

## 3. NAT strategy (laptop behind home NAT, single public IPv4, no port forwards)

Principle: for UDP SIP the NAT only passes inbound packets from the **same remote ip:port the laptop sent
to**, and only while the binding is fresh. So everything the bridge needs from Bandwidth must arrive on a
pinhole we created outbound. Sources of truth are the registrar response (`received`/`rport`) and STUN.

1. **Use one UDP socket for everything SIP** (REGISTER and INVITE handling on the same local port, e.g. 5060).
   diago does this when `BindPort` is fixed and the client reuses the address (`createClient`). Otherwise
   inbound INVITEs would arrive on a port the NAT never mapped.
2. **rport (RFC 3581):** send `Via: ...;rport`. sipgo does this with `WithClientNAT()`, which diago always sets.
   The registrar's response has `received=<public ip>;rport=<mapped port>`. diago rewrites the Contact from it
   on REGISTER. For raw sipgo we do the same by hand. Registering the mapped ip:port as Contact makes
   Bandwidth send the INVITE to the pinhole. **UNVERIFIED:** whether Bandwidth honours `received/rport`
   or just replies to the source anyway. In practice most registrars send to the registered Contact and some
   ignore the Contact URI and use the source of the REGISTER (called "fix NAT" mode).
3. **Keep the binding alive.** Consumer NAT UDP timeouts are commonly 30 to 120 s. Options, cheapest first:
   - Short re-register: `RegisterOptions{Expiry: 60s, RetryInterval: 25s}` (diago default is 0.75 x
     `Expires`, which could be far too long; **force RetryInterval ~20 to 30 s for the spike**). Note the server
     may answer with its own `Expires`, which diago adopts for the next interval, so pin `RetryInterval`.
   - **UNVERIFIED** whether Bandwidth accepts an Expires as low as 60 s and a REGISTER every 25 s
     (some SBCs clamp to a minimum of 60 or 300). Log the `Expires` in the 200 OK.
   - Alternative: send a periodic CRLF keepalive (RFC 5626 style double-CRLF) or `OPTIONS` to the registrar
     every 20 s from the same socket. A bare CRLF is cheap but not every SBC tolerates it over UDP. OPTIONS is
     safer but only helps if Bandwidth replies (a reply is not required to refresh the outbound binding).
     Make it a flag: `-keepalive=none|options|crlf`, default `options`, interval `-keepalive-interval=20s`.
4. **Contact:** `sip:<username>@<public-ip>:<mapped-port>` (from `received/rport`, or STUN), **not** the
   LAN IP. Set `ExternalHost`/`ExternalPort` to the public values; diago also rewrites from the response.
   Transport param: `;transport=udp` (default).
5. **SDP:** `c=IN IP4 <public-ip>` and `m=audio <rtp-port> RTP/AVP 0 101`. Port must be the **externally mapped**
   RTP port. If the NAT preserves ports (most home routers try to), advertising the local RTP port
   works. If it remaps, we need STUN on the actual RTP socket. Therefore:
   - Discover the public IP with STUN (pion/stun, or `stun.l.google.com:19302`, `-stun` flag). To learn the
     *RTP* mapping, send the STUN binding request **from the RTP socket itself** (same local port) before
     advertising it. diago does not do STUN for plain RTP (its STUN use is in the WebRTC path), so for the
     spike either (a) use a fixed RTP port range and rely on port-preserving NAT, advertising the public IP
     only (**-public-ip**), or (b) do our own STUN probe before the call and set `MediaExternalIP`.
   - Private IP in SDP will not work (Bandwidth sends RTP to an unroutable address), unless symmetric RTP
     latching replaces it. **UNVERIFIED** whether Bandwidth latches (some carriers do, some ignore the SDP
     entirely when behind a SBC, so media may work even with a wrong `c=`).
6. **Symmetric RTP latching:** our side: enable `RTPNATSymetric` so we reply to wherever the first
   packet came from (diago implements this). The far side: **UNVERIFIED**; the 2021 Asterisk setup
   used `rtp_symmetric`, which implies the near end latched, not necessarily Bandwidth. Test.
7. **Early RTP to open the pinhole:** on answering (or on 183), immediately send a few PCMU packets of
   silence (e.g. 5 packets of 0xFF = digital silence in µ-law, 20 ms apart) to the remote `c=`/`m=` address
   so the NAT creates an outbound mapping toward Bandwidth's media IP:port before they start sending.
   Without this, the first seconds of inbound media can be dropped (clipped audio, "one-way audio for the
   first N seconds"). diago starts writing RTP when the writer is used; **we should explicitly write silence
   immediately after `Answer`** (UNVERIFIED that diago does it by itself). For outbound calls send silence
   right after the 183/200 SDP is received.
8. **RTP source filter:** diago's `RTPSourceLock` can lock to the first source IP; **leave it off in the
   spike** (media may come from different Bandwidth IPs, 2021 notes list 67.231.2.13 and 216.82.238.135,
   and ports 5004 to 5005).
9. **No port forward fallback:** if inbound INVITE/RTP is blocked despite all this, the documented fallback
   is a UPnP/NAT-PMP mapping or a VPS relay. Out of scope for the spike. Record the result.
10. **IPv6:** force `udp4` and IPv4-only SDP; Bandwidth over IPv6 is untested.

### Recommended spike knobs

| Flag | Default | Meaning |
|---|---|---|
| `-registrar` | env `SIP_REGISTRAR` | `host:port`, user's ATA value, e.g. `jmp.cbcbc7.auth.bandwidth.com:5008` (2021, **may be stale**) |
| `-domain` | registrar host | SIP domain in From/To/R-URI if different from the registrar host |
| `-bind` | `0.0.0.0:5060` | local SIP UDP bind (single socket for REGISTER+INVITE) |
| `-public-ip` | auto via STUN | IP for Contact and SDP `c=` |
| `-public-sip-port` | learned from `rport` | Override Contact port |
| `-stun` | `stun.l.google.com:19302` | STUN server; `-stun=off` disables |
| `-rtp-port-range` | `40000-40100` | `media.RTPPortStart/End`; keep small, same range open on the host firewall |
| `-expiry` | `60s` | REGISTER Expires requested |
| `-reregister` | `25s` | forced re-register interval (diago `RetryInterval`) |
| `-keepalive` | `options` | `none`, `options`, `crlf` |
| `-codecs` | `pcmu` | `pcmu`, `pcmu,pcma`; never G.729 |
| `-symmetric-rtp` | `true` | `RTPNATSymetric` |
| `-early-rtp` | `5` | number of silence packets sent on answer |
| `-record-dir` | `./out` | where WAVs go |
| `-tone-hz` / `-tone-ms` | `440` / `2000` | playback tone |
| `-dial` | empty | E.164 to call once registered |
| `-answer` | `true` | auto-answer inbound |
| `-v` / `-sip-debug` | off | `-sip-debug` prints raw SIP (redacted, see section 5) |

---

## 4. livekit/media-sdk (v0.1.1, Apache-2.0)

Import root `github.com/livekit/media-sdk` (package `media`). Note go.mod pulls `gopkg.in/hraban/opus.v2`
(cgo, libopus needed). Resampling picks `resample_soxr.go` (cgo) or a pure-Go beep resampler when `!cgo`
(`resample_nocgo.go`).

Core types (`media.go`, `pcm.go`, `processor.go`, `pipe.go`):
- `media.PCM16Sample` (`[]int16`), `media.PCM16Writer` = `WriteCloser[PCM16Sample]`, generic
  `media.Writer[T]`, `media.WriteCloser[T]`, `media.Reader[T]`, `media.Processor[T]`.
- `media.Resample(dst, dstRate, src, srcRate, opts...)` and `media.ResampleWriter(w PCM16Writer, rate)`
  (wraps a writer so input at another rate is converted). Also `WithPredictableResample`.
- `media.MonoToStereo` / `StereoToMono`, `media.NewSwitchWriter`, `media.Pipe`, `media.PlayAudio`,
  `media.FullFrames(w, frameSize)` (re-chunks into exact frames, needed for 20 ms RTP).
- `media.MultiWriter`, `media/mixer`, `media/jitter`, `media/ring`.

G.711 (`g711`): `g711.DecodeULaw(w media.PCM16Writer) g711.ULawWriter` (bytes in, PCM16 out);
`g711.EncodeULaw(w g711.ULawWriter) media.PCM16Writer` (PCM16 in, bytes out); A-law equivalents
`DecodeALaw`/`EncodeALaw`; low-level `g711.DecodeULawTo(out []int16, in []byte)` and `EncodeULawTo`
(also A-law). Types `ULawSample`, `ALawSample`.

Opus (`opus`): `opus.Decode(w media.PCM16Writer, targetChannels int, log) (opus.Writer, error)`,
`opus.Encode(w opus.Writer, channels int, log) (media.PCM16Writer, error)`, `opus.EncodeWith(...,
EncodeOptions{...})`, type `opus.Sample` (`[]byte`). Needs libopus (cgo).

RTP (`rtp`): `rtp.Packet`/`rtp.Header` (aliases of pion/rtp), `rtp.Reader` (`ReadRTP()`), `rtp.Writer`,
`rtp.Handler(Closer)` (`HandleRTP(h, payload)`), `rtp.HandleLoop(r, h)`, `rtp.NewSeqWriter`, `rtp.Stream`,
`rtp.NewMediaStreamIn[T](media.Writer[T])`, `rtp.NewMediaStreamOut[T](stream, rate)`, `rtp.NewMux`,
`rtp.HandleJitter(h, opts...)`, `rtp.NewSession`, `rtp.ListenUDPPortRange(min,max,ip)` and
`ListenUDPEvenPortRange`. Also `media/dtmf` (`DecodeRTP`, `Encode`, `Write`), `media/tones` (`Generate`,
`Play`), `media/sdp`.

Chain for the bridge (later):
- **PSTN → Matrix:** RTP PCMU 8 kHz → (jitter) → `g711.DecodeULaw` → PCM16 8 kHz →
  `media.ResampleWriter(opusEncoderPCM, 48000)` → `opus.Encode` → LiveKit track (via server-sdk-go).
- **Matrix → PSTN:** LiveKit Opus → `opus.Decode` → PCM16 48 kHz → `ResampleWriter(.., 8000)` →
  `media.FullFrames(.., 160)` → `g711.EncodeULaw` → RTP PCMU via `rtp.Stream`/`MediaStreamOut`
  (or diago's `AudioWriter`).

---

## 5. Spike program design (`spikes/s2-sip/`)

Not written yet (task constraint). Proposed layout:

```
spikes/s2-sip/
  README.md          how to run (short), points to this doc
  main.go            flags, env, wiring, signal handling
  stun.go            STUN binding request (pion/stun) for public IP and optional RTP-socket probe
  redact.go          log helpers: credentials and URIs with passwords never logged
  record.go          WAV recording (use diago MonitorPCM/AudioStereoRecordingCreate; mono inbound only)
  tone.go            generates a WAV/PCM 440 Hz tone (8 kHz, 16-bit) or uses media-sdk tones
  .env.example       variable names only, no values
  .gitignore         .env, *.wav, out/
  go.mod             separate module so the spike deps do not enter the main module (or a go.work entry)
```

Add `spikes/s2-sip/.env` and `spikes/s2-sip/out/` to the repo `.gitignore` before writing code.

### Credentials

Environment variables (from `.env` loaded by the program if the file exists, file is gitignored, mode 0600
enforced by a check that refuses to start otherwise):

- `SIP_USERNAME`: numeric JMP SIP username
- `SIP_PASSWORD`: password (spaces allowed, do not word-split in shell, quote in `.env`)
- `SIP_REGISTRAR`: `host:port` from the user's ATA config
- `SIP_DOMAIN`: optional, if different from the registrar host
- `SIP_CALLER_ID`: optional From user for outbound (defaults to username)

Rules: never print the password, never print the `Authorization` header, never enable raw SIP debug without
the redaction wrapper. Print `username` only as `ab***yz`. Never log audio or DTMF digits, phone numbers are
allowed (project convention in `CLAUDE.md`).

### Log output (one line per event, `slog` text, no secrets)

`startup flags (redacted)`, `stun public=<ip> mapped_sip_port=<n>`, `register attempt N`,
`register 401 challenge realm=<realm> algo=<MD5|SHA-256>`, `register ok expires=<n> contact=<uri>
received=<ip> rport=<n>`, `reregister ok`, `invite from=<number> call_id=<id> offered_codecs=[...]
remote_sdp=<ip>:<port>`, `answer codec=PCMU local_rtp=<port> advertised=<ip>:<port>`,
`rtp first-packet-from=<ip>:<port> after=<ms>` (shows symmetric latching evidence),
`rtp rx packets=<n> bytes=<n> loss=<n>` every 5 s, `dtmf digit-count=<n>` (counts only), `tone sent`,
`bye by=remote|local`, `wav closed path=<p> seconds=<s>`, `outbound invite to=<number>`,
`outbound 183|180|200 after=<ms>`.

### Step-by-step human test procedure

0. Prereq: the user's current JMP SIP username, password, registrar host:port (from the ATA config; do not
   trust the 2021 values). Pause or power off the ATA first: JMP says two registrations on one account are not
   officially supported (RESEARCH.md).
1. Create `spikes/s2-sip/.env` (chmod 600) with the five variables. Confirm `git status` does not list it.
2. Build and run `go run . -registrar $SIP_REGISTRAR -sip-debug=false`. Expected: STUN discovers the public
   IP, `register ok expires=...`. If 401 repeats: wrong password/username or realm mismatch (try `-domain`).
3. Leave running 10 minutes. Expected: `reregister ok` every ~25 s. Check from another network that
   registration remains (ask JMP support/portal, or call the number in step 4). If it drops after N
   minutes, lower `-reregister` or set `-keepalive=crlf`.
4. **Inbound test:** call the JMP number from a mobile (other carrier). Expected: `invite from=...`,
   `answer`, `rtp first-packet`. Speak for 15 s, DTMF a few digits, hang up. Expected: WAV in `out/` that
   plays your voice (listen to it), plus the tone played back audibly on the phone.
5. **If silence or one-way audio:** rerun with `-sip-debug` (redacted) and capture `rtp first-packet-from`.
   Variations, one at a time: `-symmetric-rtp=false`, `-early-rtp=20`, `-public-ip` set manually,
   a different `-rtp-port-range`; or enable the router's UPnP / forward the RTP range (record this as the
   result). Run `tcpdump -ni any udp and port 5060 or portrange 40000-40100` on the laptop to see if
   Bandwidth's packets arrive.
6. **Outbound test:** `go run . -dial +1XXXXXXXXXX` (own mobile). Expected: 100/180/183, 200, ACK, hear
   tone on the mobile, hang up. Check the caller ID (Bandwidth may require the SIP username as From user,
   UNVERIFIED) and whether the digest challenge on INVITE (401/407) is handled.
7. **NAT-longevity test:** idle 30 min then call in (checks that the pinhole survived), then 2 hours.
8. **Reconnect test:** disconnect Wi-Fi for 60 s, reconnect, confirm re-registration and a successful inbound
   call. Then change the public IP if possible (router reboot) and confirm Contact updates.
9. Record findings in `docs/PLAN.md` (S2 status) and update RESEARCH.md: actual registrar host/port, Expires
   accepted, codecs offered in the INVITE SDP, whether `rport/received` is honoured, whether Bandwidth
   latches, minimum working keepalive interval, G.729-only offers (if any), DTMF payload type (101?).

### Exit criteria for S2

- Registration stable for 1 hour behind NAT without port forwards, via rport + keepalive.
- Inbound call: two-way audio, WAV recording intelligible, 2 s tone heard by the caller.
- Outbound call to a real number connects with two-way audio.
- A written answer for each **UNVERIFIED** item in this document, or a decision to work around it.
- Go/no-go for diago as the SIP/media layer (record in DECISIONS.md as a new ADR if adopted).

---

## Open questions and sources

- UNVERIFIED: current Bandwidth registrar host/port, minimum Expires, G.729-only offers, symmetric RTP
  latching behaviour, early RTP behaviour in diago, whether CRLF keepalive is tolerated.
- Add to `docs/CREDITS.md` when code starts: `pion/stun` (MIT), and the diago transitive deps
  (`pion/webrtc`, `pion/ice`, `pion/srtp`, `emiago/dtls` fork, `zaf/g711`, `go-audio/riff`, `icholy/digest`).
- Sources (read at the pinned versions): `sipgo@v1.6.0` `client.go`, `dialog_server.go`, `dialog_client.go`,
  `dialog_ua.go`, `example/register`; `diago@v0.41.0` `diago.go`, `register_transaction.go`,
  `dialog_media.go`, `dialog_server_session.go`, `dialog_client_session.go`, `media/media_session.go`,
  `media/codec.go`, `audio/`, `examples/`, `WEBRTC_PION_CHANGES.md`; `media-sdk@v0.1.1` `g711`, `opus`, `rtp`,
  `resample*.go`, `pcm.go`.
- RFC 3581 (rport), RFC 4733 (telephone-event), RFC 5626 (keepalive), RFC 3261 (SIP).
