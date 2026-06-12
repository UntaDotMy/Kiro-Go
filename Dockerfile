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

# Camoufox-fetch stage: use Camoufox's own maintained Python launcher to download
# the correct Camoufox build for THIS image's architecture into a known path. We use
# the launcher's `fetch` (not a hardcoded release URL) so the binary always matches
# the camoufox package version. Python is only used at BUILD time; the running
# container has no Python.
FROM python:3.12-bookworm AS camoufox-fetch
ENV CAMOUFOX_INSTALL=/opt/camoufox
RUN pip install --no-cache-dir camoufox \
    && python -m camoufox fetch \
    # The launcher unpacks into the python user cache; locate it and stage it at a
    # fixed path. Resolve via the package so we don't guess the cache layout.
    && python - <<'PY'
import shutil, os
from camoufox import pkgman
src = pkgman.installdir() if hasattr(pkgman, "installdir") else None
# Fallbacks across camoufox versions:
if not src or not os.path.isdir(src):
    for c in (os.path.expanduser("~/.cache/camoufox"),
              "/root/.cache/camoufox"):
        if os.path.isdir(c):
            src = c
            break
assert src and os.path.isdir(src), f"camoufox install dir not found ({src})"
shutil.copytree(src, "/opt/camoufox", dirs_exist_ok=True)
print("staged camoufox from", src)
PY

# Runtime: Debian/glibc (golang base gives us `go` for the Playwright driver install
# plus glibc). NOT Alpine — both the Playwright Node driver and Camoufox (a Firefox
# build) are glibc binaries and do not run on musl.
#
# Engine note: we drive Camoufox (a stealth Firefox), not Chromium. We still need
# the Playwright DRIVER (Node + cli.js) to speak the Firefox protocol, and we need
# Firefox's system libraries — both provided by `playwright install --with-deps
# firefox` below (we install Playwright's Firefox only to pull its system deps,
# which Camoufox shares; the app launches Camoufox via CAMOUFOX_PATH, not that one).
FROM golang:1.25-bookworm AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates wget tini gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 kiro \
    && useradd -u 1000 -g 1000 -d /app -m kiro

# Playwright driver at a FIXED shared path so uid 1000 can read it at run time.
ENV PLAYWRIGHT_DRIVER_PATH=/ms-playwright-driver
# Where the app finds Camoufox (resolveCamoufoxPath honors CAMOUFOX_PATH first).
ENV CAMOUFOX_PATH=/opt/camoufox/camoufox

WORKDIR /app
COPY --from=builder /app/kiro-go .
COPY --from=builder /app/web ./web
COPY --from=camoufox-fetch /opt/camoufox /opt/camoufox

# Install the Playwright driver + Firefox's system dependencies at BUILD time so
# "docker compose up" needs no network. `--with-deps firefox` apt-installs the
# shared libs Firefox/Camoufox need and downloads the driver to PLAYWRIGHT_DRIVER_PATH.
# Version MUST match the playwright-go version in go.mod (v0.5700.1 → driver 1.57.0).
RUN GOCACHE=/tmp/gocache GOPATH=/tmp/gopath \
        go run github.com/playwright-community/playwright-go/cmd/playwright@v0.5700.1 install --with-deps firefox \
    && rm -rf /var/lib/apt/lists/* /tmp/gocache /tmp/gopath \
    && chmod -R a+rx /opt/camoufox \
    && chown -R 1000:1000 /ms-playwright-driver /opt/camoufox

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 8080
VOLUME /app/data

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
