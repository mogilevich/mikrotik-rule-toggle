# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MikroTik Remote Hook — two-component system: a Go web server (API + UI) for managing toggle parameters, and a MikroTik RouterOS script that fetches state and enables/disables rules by comment tags (`hook:<name>`).

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

- `server/main.go` — HTTP server, routing, auth middleware, embeds static files via `//go:embed`, serves `mikrotik/*.rsc` from disk
- `server/state.go` — `Store` struct with mutex-protected JSON file read/write
- `server/static/index.html` — single-page vanilla JS UI, communicates with API via fetch
- `mikrotik/remote-hook.rsc` — RouterOS 7 script, parses JSON response with `:find` string operations

The Go server is a single `main` package (no internal packages). Static files are embedded at compile time. MikroTik scripts are served from disk (`/mikrotik/` in container, copied via Dockerfile).

## MikroTik Script Conventions

- Rules are matched by comment containing `hook:<param-name>`
- The script scans configurable `sections` array (firewall filter/nat/mangle, kid-control)
- Uses `:parse` to dynamically build commands for each section — this is intentional due to RouterOS limitations
- JSON parsing is done via string search (`:find`) since RouterOS has no JSON parser

## Key Design Decisions

- State is stored as a single JSON file (`data/state.json`), not a database
- Auth is optional Bearer token — if `AUTH_TOKEN` env is empty, API is open
- Auth applies only to `/api/*` routes; UI and `/mikrotik/*.rsc` download are public
- UI stores token in localStorage and sends it with every API request
- MikroTik script uses ROS7 syntax (`/ip/firewall/filter` not `/ip firewall filter`)
- `docker compose up` does NOT rebuild — always use `docker compose up --build` after changes
