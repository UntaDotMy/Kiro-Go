# builder stage always runs on the build machine's native platform (typically
# amd64) and cross-compiles the target binary using Go's GOOS / GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.21-alpine AS builder

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
RUN apk --no-cache add ca-certificates wget tini && \
    addgroup -g 1000 kiro && \
    adduser -D -u 1000 -G kiro -h /app kiro

WORKDIR /app
COPY --from=builder --chown=kiro:kiro /app/kiro-go .
COPY --from=builder --chown=kiro:kiro /app/web ./web

# Run unprivileged. The data volume is owned by uid 1000 too.
USER 1000:1000

EXPOSE 8080
VOLUME /app/data

# Container-level health check so orchestrators (Docker, Compose, Kubernetes,
# Watchtower) can tell whether the proxy is actually serving traffic.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/health || exit 1

# tini reaps zombies and forwards signals so graceful shutdown works.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["./kiro-go"]
