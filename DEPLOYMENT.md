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

## 3. TLS termination with Caddy (simplest)

Caddy gets you automatic Let's Encrypt certificates with near-zero config.

`/etc/caddy/Caddyfile`:

```caddy
api.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8989 {
        header_up X-Forwarded-Proto https
        header_up X-Real-IP {remote_host}
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

### Or with nginx

```nginx
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
        proxy_pass http://127.0.0.1:8989;
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
