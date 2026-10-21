# Build stage (CGO required: sqlite3 and libopus)
# Go >= 1.27.3 / 1.26.10: crypto/x509 blocks the compromised eMudhra intermediates (CVE-2026-111943).
FROM golang:1.27.3-alpine3.24 AS builder

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
# nolibopusfile: hraban/opus without opusfile (stream decoding unused).
RUN CGO_ENABLED=1 go build -tags "nocrypto nolibopusfile" -ldflags "-s -w \
	-X main.Commit=${GIT_COMMIT} -X main.Tag=${GIT_TAG} \
	-X main.RepositoryURL=${GIT_REPOSITORY_URL} -X main.BuildTime=${BUILD_TIME}" \
	-o mrtc-sip-bridge ./cmd/mrtc-sip-bridge

# Runtime stage
FROM alpine:3.24

ARG GIT_COMMIT=unknown
ARG GIT_REPOSITORY_URL=unknown
LABEL org.opencontainers.image.revision=${GIT_COMMIT} \
	org.opencontainers.image.source=${GIT_REPOSITORY_URL}

# --upgrade pulls the current CA bundle (>= 20261020: emSign ECC Root CA - G3, Root CA - C1 and ECC Root CA - C3 removed)
# and any other security fixes newer than the base image.
RUN apk upgrade --no-cache && apk add --no-cache --upgrade ca-certificates tzdata sqlite-libs opus opusfile

# Run as non-root
RUN adduser -D -h /app -s /sbin/nologin bridge
WORKDIR /app

COPY --from=builder /build/mrtc-sip-bridge .
COPY entrypoint.sh /app/entrypoint.sh
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/mrtc-sip-bridge/
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
