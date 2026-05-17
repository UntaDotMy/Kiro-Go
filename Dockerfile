# builder stage always runs on the build machine's native platform (typically
# amd64) and cross-compiles the target binary using Go's GOOS / GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o kiro-go .

FROM alpine:3.19
# tini reaps zombies and forwards signals; su-exec drops privileges before
# starting the application; wget is for the HEALTHCHECK; ca-certificates
# for outbound TLS to Kiro / Bedrock.
RUN apk --no-cache add ca-certificates wget tini su-exec && \
    addgroup -g 1000 kiro && \
    adduser -D -u 1000 -G kiro -h /app kiro

WORKDIR /app
COPY --from=builder /app/kiro-go .
COPY --from=builder /app/web ./web
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Run as root briefly so the entrypoint can fix data-volume ownership when
# migrating from the upstream image (which ran the proxy as root). The
# entrypoint then drops to uid 1000:1000 before exec'ing the binary, so the
# application itself never runs as root. Override RUN_UID / RUN_GID env
# vars to use a different non-root identity.
EXPOSE 8080
VOLUME /app/data

# Container-level health check so orchestrators (Docker, Compose, Kubernetes,
# Watchtower) can tell whether the proxy is actually serving traffic.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
