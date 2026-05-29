# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Convert Kiro accounts to OpenAI / Anthropic compatible API service.

[English](README.md) | [中文](README_CN.md)

If this project helps you, a Star would mean a lot.

## Features

- Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` and `/v1/responses` (Codex CLI)
- Multi-account pool with round-robin load balancing
- Auto token refresh, SSE streaming, Web admin panel
- Multiple auth: AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, local cache, credentials JSON
- Usage tracking, account import/export, i18n (CN / EN)
- Support configuring outbound proxy (SOCKS5 / HTTP)

## Quick Start

### Docker Compose (Recommended)

```bash
git clone https://github.com/UntaDotMy/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker compose up -d
```

By default the compose file maps host port **8989** to container `8080` so this fork can run side-by-side with the upstream image (which uses `8080`). Edit `docker-compose.yml` to change the host port if you prefer.

### Docker Run

```bash
docker run -d \
  --name kiro-go-patch \
  -p 8989:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/untadotmy/kiro-go:latest
```

### Build from Source

```bash
git clone https://github.com/UntaDotMy/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go
```

Config is auto-created at `data/config.json`. Mount `/app/data` for persistence. The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8989/admin` (or whichever host port you mapped), log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8989/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI Chat Completions
curl http://localhost:8989/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI Responses (Codex CLI)
curl http://localhost:8989/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"claude-opus-4-7","input":"Hello!"}'
```

## Thinking Mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude-compatible requests that include a top-level `thinking` config such as `{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}` also enable thinking mode automatically. Configure output format in the admin panel under Settings - Thinking Mode.

## Outbound Proxy

For users in restricted network regions, configure an outbound proxy in the admin panel under **Settings - Outbound Proxy Settings**. Supports SOCKS5 and HTTP proxies.

The setting takes effect immediately without restarting.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |
| `LOG_LEVEL` | Log verbosity: `debug` / `info` / `warn` / `error` | `info` |
| `KIRO_API_REGION` | AWS region for the Kiro/CodeWhisperer endpoints (e.g. `us-east-1`, `eu-west-1`, `ap-northeast-1`). Overrides the admin-UI setting. | `us-east-1` |
| `KIRO_API_REGIONS` | Comma-separated cross-region failover list (e.g. `us-east-1,eu-west-1`). When set, the proxy tries every endpoint in the first region before falling through to the next region. Empty = single-region (`KIRO_API_REGION`). | - |
| `KIRO_ALLOW_DEFAULT_PASSWORD` | Set to `1` to allow startup with the default password (not recommended in production) | - |
| `KIRO_WS_ALLOW_ANY_ORIGIN` | Set to `1` to revert WebSocket Origin check to permissive (older A11 behaviour) | - |
| `DATA_DIR` | (entrypoint) Path to chown to runtime UID/GID before drop | `/app/data` |
| `RUN_UID` / `RUN_GID` | (entrypoint) UID/GID to drop privileges to | `1000` / `1000` |

## Reverse proxy notes

The realtime dashboard at `/admin/ws/status` requires WebSocket upgrade-header passthrough. Reverse proxies in front of Kiro-Go must forward `Upgrade` and `Connection`:

```nginx
# nginx
location /admin/ws/ {
    proxy_pass http://kiro-go:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 300s;
}
```

The dashboard authenticates the WebSocket via `Sec-WebSocket-Protocol` (browsers can't set custom headers on WS upgrades). **If your reverse proxy logs request headers, the admin password will appear in the access log.** Either strip the header from logs or terminate the WebSocket internally. If logs are uncomfortable, the dashboard automatically falls back to 10-second polling — disable the WebSocket route to force that path.

## Backup and restore

Two files in `data/` carry all state:

- `data/config.json` — accounts, OAuth tokens, API keys, every setting. **Treat as a secret.** File mode 0600 is enforced on save.
- `data/stats.db` — persistent usage history (SQLite WAL). Safe to lose; the proxy will recreate it.

For a hot backup, just copy the directory while the service is running. SQLite's WAL plus our atomic `temp + rename` save mean you'll get a consistent snapshot 99 % of the time. For a guaranteed-consistent snapshot, stop the container first:

```bash
docker stop kiro-go-patch
cp -r ./data ./data.backup-$(date +%F)
docker start kiro-go-patch
```

Restore is symmetric: stop, replace `data/`, start. Container UID 1000 must own the files (the entrypoint chowns on first start).

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Friend Links

- [LINUX DO](https://linux.do)

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
