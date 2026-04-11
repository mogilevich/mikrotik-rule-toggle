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

# Docker (always use --build after code changes)
docker compose up --build

# Test API
curl -H "Authorization: Bearer test" http://localhost:8080/api/state
```

## Architecture

- `server/main.go` — HTTP server, routing, auth middleware, heartbeat, graceful shutdown (SIGINT/SIGTERM)
- `server/state.go` — `Store` struct with RWMutex-protected JSON file read/write, timer logic
- `server/audit.go` — `AuditLog` with buffered writes (5s flush), RWMutex, graceful Flush()
- `server/static/index.html` — single-page vanilla JS PWA, pull-to-refresh, countdown timers
- `server/static/manifest.json` + `sw.js` — PWA support
- `mikrotik/remote-hook.rsc` — RouterOS 7 script, in-memory fetch, conntrack clearing

Single `main` package, no internal packages. Static files embedded via `//go:embed`. MikroTik scripts served from disk (`/mikrotik/` in container, copied via Dockerfile).

## MikroTik Script Conventions

- Firewall rules: matched by `comment` containing `hook:<param-name>`
- Kid-control: matched by `name` containing `hook:<param-name>` (inverted logic)
- `invertedSections` array controls which sections have inverted logic
- Scans configurable `sections` array (firewall filter/nat/mangle, kid-control)
- Uses `:parse` to dynamically build commands — intentional due to RouterOS limitations
- JSON parsing via string search (`:find`) — RouterOS has no JSON parser
- Conntrack clearing: reads `src-address-list` and `dst-address-list` from rule, clears matching connections after successful enable
- Fetch: `output=user as-value` (in-memory, no disk writes), `duration=10` (10s timeout)
- Fail-safe: any fetch/parse error → script aborts, no rules changed

## Key Design Decisions

- State stored as JSON file (`data/state.json`), audit log in `data/audit.json` (max 200 entries, buffered 5s)
- Auth: optional Bearer token via `AUTH_TOKEN` env; applies only to `/api/*` routes
- UI stores token in localStorage
- Timer: `TempRelease` sets param + `disabled_until` timestamp; background ticker restores every 10s
- Inverted params (kid-control): `enabled=true` in API → `disabled=yes` on MikroTik
- `docker compose up` does NOT rebuild — always use `docker compose up --build`
- Graceful shutdown: SIGINT/SIGTERM → flush audit → stop server
