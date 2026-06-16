# Deploying Kiro-Go to a VPS (production)

This guide covers a hardened, internet-facing deployment. The proxy itself
terminates plain HTTP; **you must put a TLS-terminating reverse proxy in front
of it** so admin passwords, API keys, and OAuth tokens never travel in
cleartext.

## 1. Threat model & what the app does for you

The proxy ships these hardening defaults (no config needed):

- **Scoped CORS** — only the inference API (`/v1/messages`, `/v1/chat/completions`,
  `/v1/responses`, `/v1/models`, `/v1/key-status`) sends `Access-Control-Allow-Origin: *`
  for browser SDK callers. The admin panel, portal, and landing page are
  same-origin only.
- **Security headers** — CSP, `X-Frame-Options: DENY`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy`, and `Permissions-Policy` on the HTML surfaces;
  `nosniff` on the JSON API. `Strict-Transport-Security` is emitted automatically
  when the request arrives over TLS (or via a trusted proxy that sets
  `X-Forwarded-Proto: https`).
- **Admin brute-force lockout** — 10 failed passwords per IP in 5 minutes locks
  that IP out (in-memory; resets on restart).
- **Path-jailed static serving** — admin assets can't escape the `web/` dir.
- **`/health` is minimal** — returns only `{"status":"ok"}`, no version/uptime
  fingerprint.
- **Public key portal** — `/portal` lets a customer paste their key and see
  usage/limits. It never returns the raw key, the internal id, or the key's
  label, has its own per-IP rate limit (20/min), and gives a single
  indistinguishable `{"valid":false}` for unknown/disabled/expired keys.

What it does **not** do: terminate TLS, or run a stateful session for the admin
(the admin password is sent as a header per request). The reverse proxy below
covers TLS; keep the admin panel off the public internet if you can (see §4).

## 2. Required: set a strong admin password

```bash
# docker-compose.yml -> environment:
- ADMIN_PASSWORD=<a long random string>
```

The server refuses to start with the bundled default password unless
`KIRO_ALLOW_DEFAULT_PASSWORD=1` is set — **never set that in production.**

## 3. TLS termination with Caddy (recommended)

Caddy is the best fit for this proxy: automatic Let's Encrypt certificates with
near-zero config, native WebSocket upgrades, HTTP/2 (and optional HTTP/3) to the
client, and — importantly — it does **not** impose a default read/write timeout
that would sever a multi-minute streaming response (unlike nginx, whose
`proxy_read_timeout` defaults to 60s). It adds only sub-millisecond overhead; it
will not speed up or slow down generation (that is upstream-bound) and it does
**not** affect upstream 429s (those are handled entirely in the Go account pool).

`/etc/caddy/Caddyfile`:

```caddy
api.example.com {
    # Compress only NON-streaming responses. Compressing text/event-stream adds
    # CPU + latency for no gain and can trip some SSE intermediaries, so exclude
    # the streaming API paths and compress everything else (admin UI, JSON, static).
    @nostream {
        not path /v1/messages /v1/chat/completions /v1/responses
    }
    encode @nostream zstd gzip

    reverse_proxy 127.0.0.1:8989 {
        # Low-latency mode: disable response buffering so SSE/streamed chunks are
        # flushed to the client immediately. Caddy already auto-flushes when the
        # upstream sets Content-Type: text/event-stream (which this proxy does),
        # but -1 also covers non-SSE chunked output (e.g. batch JSON) — belt and
        # suspenders for a streaming workload.
        flush_interval -1

        # The Go server speaks plain HTTP/1.1 (no TLS / no h2c on its listener),
        # so pin the hop to 1.1. Do NOT use h2c:// here. keepalive is on by
        # default (2m); connections to the Go process are reused between turns.
        transport http {
            versions 1.1
        }

        header_up X-Forwarded-Proto https
        header_up X-Real-IP {remote_host}
    }
}
```

WebSocket endpoints (`/v1/responses` WS transport, the `/admin/ws/` dashboard
socket) need no special directives — Caddy v2 upgrades WebSocket connections
transparently.

**Optional — enable HTTP/3 (QUIC) to the client.** Helps on lossy/mobile
networks (no head-of-line blocking, 1-RTT handshakes); marginal on stable wired
links, and clients that don't support it fall back to HTTP/2 automatically.
Requires UDP/443 open in your firewall. Add a global options block at the top of
the Caddyfile:

```caddy
{
    servers {
        protocols h1 h2 h3
    }
}
```

Then bind the container to loopback only so the Go process is never directly
reachable from the internet:

```yaml
# docker-compose.yml
ports:
  - "127.0.0.1:8989:8080"
```

> **Why not let Caddy time out long streams?** Caddy has no default
> write/read timeout, so an active multi-minute generation is never cut. Its
> idle default (5m) applies only to *idle* keep-alive connections, not active
> streams. This complements the Go server's own 30-minute write ceiling and the
> 2-minute rolling write deadline on the streaming handlers.

### Or with nginx

```nginx
# Keep warm keep-alive connections to the Go process. Without this block (and the
# proxy_http_version 1.1 + Connection "" in `location /` below) nginx proxies as
# HTTP/1.0 with `Connection: close`, opening and tearing down a fresh TCP+TLS
# connection to Go on EVERY request — which defeats the server's own keep-alive
# reuse (IdleTimeout) and adds per-request setup latency under load.
upstream kiro {
    server 127.0.0.1:8989;
    keepalive 64;
}

server {
    listen 443 ssl http2;
    server_name api.example.com;

    ssl_certificate     /etc/letsencrypt/live/api.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.example.com/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    # The realtime dashboard needs WebSocket upgrade passthrough.
    location /admin/ws/ {
        proxy_pass http://127.0.0.1:8989;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 300s;
    }

    location / {
        proxy_pass http://kiro;
        proxy_http_version 1.1;     # required for upstream keep-alive reuse
        proxy_set_header Connection "";   # clear "close" so connections are pooled
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 1800s;   # streaming responses can run for minutes
    }
}
```

> **Note on the WebSocket dashboard password:** the admin dashboard authenticates
> its status WebSocket by sending the admin password in the
> `Sec-WebSocket-Protocol` header. If your reverse proxy logs request/response
> headers, that password will appear in the access log. Either strip the header
> from logs, or disable the `/admin/ws/` route to force the dashboard onto its
> 10-second polling fallback.

## 4. Tell the app it's behind a proxy

Once a reverse proxy is in front, set `KIRO_TRUSTED_PROXIES` so the brute-force
limiter keys on the real client IP (from `X-Forwarded-For`/`X-Real-IP`) instead
of the proxy's IP, and so `X-Forwarded-Proto: https` is honored for HSTS.

```yaml
# docker-compose.yml -> environment:
# CIDR(s) or bare IPs of YOUR reverse proxy. For a same-host proxy this is loopback.
- KIRO_TRUSTED_PROXIES=127.0.0.1/32
```

**Only set this to proxies you control.** If you set it to `0.0.0.0/0`, any
client can spoof `X-Forwarded-For` and defeat the per-IP lockout.

## 5. Lock down the admin surface (recommended)

The admin panel is powerful (it can export OAuth tokens). Restrict it at the
edge to your own IP(s). With nginx:

```nginx
location /admin/ {
    allow 203.0.113.4;   # your office / VPN IP
    deny all;
    proxy_pass http://127.0.0.1:8989;
    # ...same proxy_set_header lines as above...
}
```

The public landing (`/`) and customer portal (`/portal`) stay open; only
`/admin` is gated.

## 6. Firewall

```bash
ufw default deny incoming
ufw allow OpenSSH
ufw allow 443/tcp
ufw enable
```

Do **not** expose port 8989/8080 publicly — only the reverse proxy on 443
should be reachable.

## 7. Backups

`data/config.json` holds accounts, OAuth tokens, API keys, and settings — treat
it as a secret (it's written `0600`). `data/stats.db` is usage history (safe to
lose). Hot-copy the `data/` dir for a backup; for a guaranteed-consistent
snapshot stop the container first.

## 8. Post-deploy verification

```bash
# Health should be minimal (no version/uptime)
curl -s https://api.example.com/health        # -> {"status":"ok"}

# Admin requires the password
curl -s -o /dev/null -w '%{http_code}\n' https://api.example.com/admin/api/status   # -> 401

# Security headers present on admin
curl -sI https://api.example.com/admin | grep -i -E 'content-security-policy|x-frame-options|strict-transport'

# Portal rejects an unknown key without leaking anything
curl -s -H 'Authorization: Bearer sk-kg-nope' https://api.example.com/v1/key-status  # -> {"valid":false}
```

Run your config through [securityheaders.com](https://securityheaders.com) and
[Mozilla Observatory](https://observatory.mozilla.org) after deploy.

## 9. Throughput & avoiding 429s (read this if it's slow)

**The #1 cause of slowness and constant 429s is account capacity, not the
balancer.** Kiro bills per-account monthly *credits*. Once an account's
`usageCurrent` reaches its `usageLimit`, every further request is **overage**
traffic, which AWS throttles far more aggressively. An over-quota account shows
an **Over quota** badge in the admin Accounts table; a throttled one shows a
**Cooling** badge with a countdown.

If most of your accounts are over quota, no scheduler can make them fast — the
upstream is rate-limiting them on purpose. The fixes, in order of impact:

### 9.1 Add more healthy accounts (biggest lever)

The pool scales near-linearly. With the default `least-request` strategy each
new request is steered to the least-busy account, and failover rotates off a
just-throttled one to a healthy peer. Two exhausted accounts give the dispatcher
nothing to fail over to; 4-6 in-quota accounts changes everything. Watch the
**Over quota** / **Cooling** badges and replace or rest exhausted accounts.

### 9.2 Give each account its own egress IP (per-account proxy)

AWS appears to layer a **per-IP** throttle on top of the per-identity one, so
running every account out of one VPS IP correlates their throttling — they trip
together. Each account has a `proxyURL` you can set in the admin **account
detail** panel (or via `PUT /admin/api/accounts/{id}`), e.g.
`socks5://user:pass@host:1080` or `http://host:8080`. Route each account through
a different egress IP to decorrelate the IP-level throttle. Leave it blank to use
the global proxy (Settings → Proxy) or a direct connection.

> The proxy URL is SSRF-validated (scheme + link-local guard). HTTP/2 is
> auto-disabled for proxied connections (they can't negotiate h2); the direct
> path keeps HTTP/2 with active PING health-checks.

### 9.3 Pool strategy & multi-region

- **Strategy** (Settings → Pool Strategy): keep **`least-request`** (default). It
  is concurrency-aware and applies a per-account AIMD concurrency limit that
  self-tunes toward AWS's hidden token-bucket size — additive +1 on success,
  ×3/4 on a 429. The alternatives (`swr`, `least-used`, `random`) do **not**
  reserve in-flight slots and will burst harder into 429s under parallel load.
- **Multi-region (per-account pinning)**: set `KIRO_API_REGIONS=us-east-1,eu-west-1`
  (comma separated) to spread accounts across regions. Each account is pinned to
  **one** region for its whole life — chosen deterministically by hashing the
  account ID across the list — so different accounts land on different regional
  rate buckets while **no single account ever changes region**. This spreads load
  the safe way: across accounts, not within one identity. Within its pinned
  region an account still tries the 3 AWS service actions (Kiro IDE /
  CodeWhisperer / AmazonQ) in order, using the primary and falling back to the
  others only on a 429. Leave the variable unset to keep every account on the
  single default region (`KIRO_API_REGION`, default `us-east-1`).

  > Why per-account pinning instead of a per-request region chain: one identity
  > hopping regions request-to-request (or cascading across regions on a 429) is
  > implausible for a single real client. Pinning keeps each account's traffic
  > consistent — one region, one host, stable keep-alive connections — while the
  > pool as a whole still uses every configured region's rate buckets.

### 9.4 What's already tuned for you (no action needed)

- Cross-account **failover** up to 3 accounts per request, honoring upstream
  `Retry-After`.
- **Decorrelated-jitter cooldowns** so accounts sharing an AWS identity don't
  recover in lockstep and re-stampede.
- **Connection reuse**: 100 idle keep-alive conns/host, HTTP/2 PINGs on the
  direct path, OS TCP keep-alive on the proxied path.
- **Proactive token refresh** (120s before expiry, single-flight per account) so
  OAuth refresh almost never sits on the request hot path.

If after adding healthy accounts + per-account proxies you still see 429s, you
are simply asking for more throughput than your Kiro plans allow — upgrade the
plans (higher `usageLimit`) or add accounts.

