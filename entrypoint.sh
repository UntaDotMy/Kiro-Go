#!/bin/sh
set -e

# entrypoint.sh — runs as root briefly so we can fix data-volume ownership
# (which is owned by root when migrating from the upstream image), then
# drops to uid 1000 before exec'ing the binary.
#
# This is the standard Docker pattern for images that need a writable
# volume but don't want to run the application as root.

DATA_DIR="${DATA_DIR:-/app/data}"
RUN_UID="${RUN_UID:-1000}"
RUN_GID="${RUN_GID:-1000}"

# Ensure the data dir exists, then take ownership only if it is currently
# owned by a different user. Best-effort; ignore failures (e.g. when the
# volume is mounted read-only on purpose).
if [ -d "$DATA_DIR" ]; then
  current_uid="$(stat -c %u "$DATA_DIR" 2>/dev/null || echo 0)"
  if [ "$current_uid" != "$RUN_UID" ]; then
    chown -R "$RUN_UID:$RUN_GID" "$DATA_DIR" 2>/dev/null || true
  fi
fi

# Drop privileges to RUN_UID:RUN_GID and exec the binary via tini so signal
# handling and zombie reaping still work for graceful shutdown.
exec /sbin/tini -- /sbin/su-exec "$RUN_UID:$RUN_GID" ./kiro-go
