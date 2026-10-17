# Build stage (CGO required: sqlite3 and libopus)
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
# nolibopusfile: hraban/opus without opusfile (stream decoding unused).
RUN CGO_ENABLED=1 go build -tags "nocrypto nolibopusfile" -ldflags "-s -w \
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
