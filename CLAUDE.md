# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MikroTik Rule Toggle — remote control panel for MikroTik firewall rules and kid-control. Go web server (API + PWA UI) with toggles, timers, audit log. RouterOS 7 script syncs state by comment/name tags (`hook:<name>`).

## Build & Run

```bash
# Build
go build -o hook-server ./server/

# Run locally
AUTH_TOKEN=test ./hook-server

# Docker via Make (pulls pre-built image from ghcr.io)
make up

# Docker manually
HOST_IP=10.0.0.5 docker compose up -d

# Local build (without CI)
make build-local

# Update on server (pull new image + restart)
make restart

# Test API
curl -H "Authorization: Bearer test" http://localhost:8080/api/state
```

## Architecture

- `server/main.go` — HTTP server, routing, auth middleware, heartbeat, script version check, graceful shutdown (SIGINT/SIGTERM)
- `server/state.go` — `Store` struct with RWMutex-protected JSON file read/write, timer logic, pending timers
- `server/audit.go` — `AuditLog` with buffered writes (5s flush), RWMutex, graceful Flush(), daily analytics
- `server/static/index.html` — single-page vanilla JS PWA, pull-to-refresh, countdown timers, bar charts
- `server/static/manifest.json` + `sw.js` — PWA support
- `server/static/icon.svg` — MikroTik logo (Simple Icons), used as favicon
- `server/static/icon-192.svg` — MikroTik logo on blue background, used as PWA/apple-touch-icon
- `mikrotik/remote-hook.rsc` — RouterOS 7 script, in-memory fetch, conntrack clearing, temp-block, auto-update, sends `X-Script-Version` header

- `entrypoint.sh` — replaces `your-server` and `token` placeholders in .rsc with `HOST_IP` and `AUTH_TOKEN` env at container startup
- `Dockerfile` — multi-stage build (golang → alpine)
- `.github/workflows/build.yml` — CI: builds Docker image, pushes to ghcr.io on push to master
- `Makefile` — `make up/down/logs/pull/restart/build-local`, auto-detects host IP via `ip route` (Linux) or `ipconfig` (macOS)

Single `main` package, no internal packages. Static files embedded via `//go:embed`. MikroTik scripts served from disk (`/mikrotik/` in container, copied via Dockerfile).

## MikroTik Script Conventions

- Firewall rules: matched by `comment` containing `hook:<param-name>`
- Kid-control: matched by `name` containing `hook:<param-name>` (inverted logic)
- `invertedSections` array controls which sections have inverted logic
- Scans configurable `sections` array (firewall filter/nat/mangle, kid-control)
- Uses `:parse` to dynamically build commands — intentional due to RouterOS limitations
- JSON parsing via string search (`:find`) — RouterOS has no JSON parser
- Conntrack clearing: resolves address-lists to IPs via `/ip/firewall/address-list`, exact match for IPs, regex for CIDR
- Temp-block: collects src IPs (from src-address-list, src-address, or conntrack scan), adds to `_temp-block` with 30s TTL, kills all connections. Drop rule auto-created before established/related accept
- Pre-collection: src IPs gathered BEFORE rule enable (connections may disappear after drop activates)
- Fetch: `output=user as-value` (in-memory, no disk writes), `duration=10` (10s timeout)
- Fail-safe: any fetch/parse error → script aborts, no rules changed
- `scriptVersion` variable — increment on every .rsc change (server compares with router's `X-Script-Version` header)

## Key Design Decisions

- State stored as JSON file (`data/state.json`), audit log in `data/audit.json` (max 2000 entries, buffered 5s)
- Auth: optional Bearer token via `AUTH_TOKEN` env; applies only to `/api/*` routes
- UI stores token in localStorage
- Timer: `TempRelease` creates pending timer (`timer_duration`); countdown starts only after router fetches state (`disabled_until`). Active timers can be extended.
- Inverted params (kid-control): `enabled=true` in API → `disabled=yes` on MikroTik
- Docker image built by GitHub Actions, pushed to `ghcr.io/mogilevich/mikrotik-rule-toggle`
- `docker compose` uses pre-built image; `make build-local` for local builds
- Graceful shutdown: SIGINT/SIGTERM → flush audit → stop server
