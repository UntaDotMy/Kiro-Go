# builder stage always runs on the build machine's native platform and
# cross-compiles the target binary using Go's GOOS / GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

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

# Runtime: a slim Debian base. The binary is a static (CGO_ENABLED=0) Go build, so
# we only need ca-certificates for outbound TLS plus tini/gosu for clean signal
# handling and privilege drop.
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates wget tini gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 kiro \
    && useradd -u 1000 -g 1000 -d /app -m kiro

WORKDIR /app
COPY --from=builder /app/kiro-go .
COPY --from=builder /app/web ./web

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 8080
VOLUME /app/data

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
