# Credits, sources and licensing

Licenses were checked on 2026-10-16 via the GitHub API (`license.spdx_id`) and the repos' LICENSE/NOTICE
files. Re-check when adding a dependency or borrowing code. This project's own license is **MIT** (`LICENSE`,
copyright William Kray). Dependency licenses remain as listed below, and a summary is in
`THIRD-PARTY-NOTICES.md` (also shipped in the Docker image). MPL-2.0 dependencies (mautrix, go.mau.fi/util,
diago) are used unmodified and linked only. Every dependency below is compatible with MIT distribution and
with being imported by jmpchat-matrix-bridge (formerly mautrix-jabber) (ADR-006).

## Planned dependencies

| Project | License | Use | Obligations |
|---|---|---|---|
| [maunium.net/go/mautrix](https://github.com/mautrix/go) (bridgev2) | MPL-2.0 | Bridge framework. Spike S1 uses v0.32.0 (client API only, linked). | File-level copyleft. Changes to *its* files must stay MPL. Fine to link. |
| [emiago/sipgo](https://github.com/emiago/sipgo) | BSD-2-Clause | SIP stack | Keep the copyright notice. Spike S2 uses v1.6.0. |
| [emiago/diago](https://github.com/emiago/diago) | MPL-2.0 | SIP UA. **Adopted** (ADR-009): v0.41.0, linked, no files copied, used by `pkg/sipua` only. | Same as mautrix. |
| [livekit/media-sdk](https://github.com/livekit/media-sdk) | Apache-2.0 | **Not used.** The G.711 u-law table in `pkg/media` is our own implementation of the public ITU-T G.711 algorithm. | n/a |
| [livekit/server-sdk-go](https://github.com/livekit/server-sdk-go) | Apache-2.0 | LiveKit participant. Used by `pkg/rtc`: v2.18.1 (forced by diago's pion/ice, spike S1 used v2.19.0). Linked, no files copied; Apache-2.0 verified via GitHub API on 2026-10-16. | Keep NOTICE/attribution. |
| [hraban/opus](https://github.com/hraban/opus) | MIT | cgo Opus binding. Used directly by `pkg/media` (Opus encode/decode) as `gopkg.in/hraban/opus.v2` v2.0.0-20230925203106-0188a62cb302, built with `-tags nolibopusfile` (no opusfile needed). | Keep the notice. |
| [pion/webrtc](https://github.com/pion/webrtc), [livekit/protocol](https://github.com/livekit/protocol) | MIT / Apache-2.0 | Direct imports in `pkg/rtc` (`webrtc.RTPCodecCapability`, `media.Sample`, `livekit.TrackSource`): pion/webrtc v4.2.15 (MIT), livekit/protocol v1.49.0 (Apache-2.0); both verified via GitHub API on 2026-10-16 | Keep the notices. |
| [libopus (xiph/opus)](https://github.com/xiph/opus) | BSD-3-Clause (Xiph COPYING; GitHub shows NOASSERTION) | Opus codec, linked via cgo | Keep the copyright notice in binary distributions/images. |
| [pion/sdp](https://github.com/pion/sdp), [pion/rtp](https://github.com/pion/rtp) | MIT | SDP (indirect); RTP packets (`pion/rtp` v1.10.2, direct import of `pkg/rtc`) | Keep the notice. |
| [pion/stun](https://github.com/pion/stun) | MIT | STUN public-IP discovery (Spike S2 uses v3.1.7) | Keep the notice. |
| [rs/zerolog](https://github.com/rs/zerolog) | MIT | Structured logging (pkg/core, same version as jmpchat-matrix-bridge: v1.35.1) | Keep the notice. |
| [go.mau.fi/util](https://github.com/mautrix/go-util) | MPL-2.0 | Shared utilities used by the bridge (v0.11.0, same as jmpchat-matrix-bridge) | Same as mautrix. |
| [gopkg.in/yaml.v3](https://github.com/go-yaml/yaml) | MIT and Apache-2.0 | YAML (config handling and tests; v3.0.1) | Keep the notices. |
| [open-telemetry/opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go) | Apache-2.0 | OpenTelemetry v1.47.0 (same version as jmpchat-matrix-bridge): API modules `otel`, `attribute`, `codes`, `trace`, `metric`, `propagation` in core `pkg/obs`; `sdk`, `sdk/metric` and the OTLP exporters (`otlptrace`, `otlptracegrpc`, `otlptracehttp`, `otlpmetricgrpc`, `otlpmetrichttp`) in the host `pkg/telemetry` only | Keep NOTICE/attribution. No source copied. |
| [open-telemetry/opentelemetry-go-contrib](https://github.com/open-telemetry/opentelemetry-go-contrib) | Apache-2.0 | `instrumentation/runtime` v0.72.0: Go runtime metrics, standalone host only | Same. |
| [open-telemetry/opentelemetry-proto-go](https://github.com/open-telemetry/opentelemetry-proto-go) | Apache-2.0 | `go.opentelemetry.io/proto/otlp`, indirect (OTLP exporters) | Same. |
| [go-logr/logr](https://github.com/go-logr/logr), [go-logr/stdr](https://github.com/go-logr/stdr) | Apache-2.0 | `otel.SetLogger` sink in `pkg/telemetry` (logr v1.4.4); stdr indirect | Same. |
| [grpc/grpc-go](https://github.com/grpc/grpc-go) | Apache-2.0 | Indirect: gRPC transport of the OTLP exporters | Same. |
| [protocolbuffers/protobuf-go](https://github.com/protocolbuffers/protobuf-go) | BSD-3-Clause | Indirect (`google.golang.org/protobuf`) | Keep the notice. |
| [grpc-ecosystem/grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway) | BSD-3-Clause | Indirect (OTLP exporters) | Keep the notice. |
| [cenkalti/backoff](https://github.com/cenkalti/backoff) | MIT | Indirect (`backoff/v5`, exporter retry) | Keep the notice. |

`pkg/sipua/net.go`'s STUN helper is ported from our own spike (`spikes/s2-sip`), so no outside code is involved.

`THIRD-PARTY-NOTICES.md` lists the direct runtime dependencies; regenerate it from `go.mod` (for example
with `go-licenses`) when dependencies change. `LICENSE` and `THIRD-PARTY-NOTICES.md` are copied into the
Docker image under `/usr/share/doc/mrtc-sip-bridge/`.

## Prior art and references (ideas, not code)

| Project | License | How we use it |
|---|---|---|
| [lhns/matrix-sip-bridge](https://github.com/lhns/matrix-sip-bridge) by Pierre Kisters | Apache-2.0 | Closest prior art: MatrixRTC ghost membership, MSC4075 ring, MSC4310 decline, the encrypted-room silent-call finding, bridgev2 call-event handling. **If code is copied or adapted:** keep its copyright header, mark modified files as changed (Apache-2.0 §4b), and carry its NOTICE attribution in ours. |
| [MTRNord/sip-bridge-mx](https://github.com/MTRNord/sip-bridge-mx) | Apache-2.0 | Reference only (WebRTC-SIP ↔ MatrixRTC, inactive since 2023). |
| [gymnae/mumabridge](https://github.com/gymnae/mumabridge) | **No license** (all rights reserved) | Reference for the pattern only. **Don't copy code.** |
| [element-hq/element-call](https://github.com/element-hq/element-call) | AGPL-3.0 | The reference MatrixRTC client. Read it to learn event shapes. Take event schemas from the MSCs (MSC3401, MSC4143, MSC4195, MSC4075, MSC4310), not by copying source. **Don't copy code** unless this project adopts AGPL-3.0. |
| [element-hq/lk-jwt-service](https://github.com/element-hq/lk-jwt-service) | AGPL-3.0 | Deployed as a separate service. We only call its HTTP API, so there's no licensing impact. |
| [livekit/sip](https://github.com/livekit/sip) | Apache-2.0 | Reference for how LiveKit bridges SIP media. REGISTER is not supported ([PR #774](https://github.com/livekit/sip/pull/774)). |

## Written sources

- François Marier, ["Setting up a JMP SIP account on Asterisk"](https://feeding.cloud.geek.nz/posts/jmp-sip-account-on-asterisk/) (2021): JMP registrar, port, codecs, DTMF, NAT notes.
- battlepenguin, ["Moving my phone numbers from Google Hangouts/Voice to an SIP/XMPP Service"](https://battlepenguin.com/tech/moving-my-phone-number-from-google-hangouts-voice-to-an-sip-xmpp-service/) (2021): JMP multi-registration behaviour and SIP minute limit.
- [LiveKit telephony docs](https://docs.livekit.io/telephony/): LiveKit SIP does not support REGISTER.
- [element-call issue #3933](https://github.com/element-hq/element-call/issues/3933): `MISSING_MATRIX_RTC_TRANSPORT` pitfall.
- Sibling project [jmpchat-matrix-bridge](https://github.com/williamkray/jmpchat-matrix-bridge) (same author): phone normalisation and telemetry conventions get reimplemented here.
