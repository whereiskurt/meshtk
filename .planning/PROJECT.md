# MeshTK — MQTT Proxy Credential Cache

## What This Is

An enhancement to the existing MeshTK server proxy that adds DynamoDB-backed MQTT credential validation with an in-memory application cache. When Meshtastic mobile clients (iPhone/Android) connect via MQTT, the proxy validates their username/password against cached credentials (backed by DynamoDB), then swaps them with generic shared credentials before forwarding to the Mosquitto broker.

## Core Value

Every MQTT CONNECT is validated against cached credentials with minimal latency — invalid clients are rejected before reaching the broker, and valid clients are transparently forwarded with generic creds.

## Requirements

### Validated

<!-- Inferred from existing codebase -->

- ✓ TCP proxy listener accepts MQTT connections — existing (`internal/app/server/cmd.go`)
- ✓ MQTT packet parsing extracts username, password, clientID — existing (`internal/app/server/inspect.go`)
- ✓ Rule-based packet inspection with allow/block/kill/slow decisions — existing (`internal/app/server/decider.go`)
- ✓ Proxy forwarding to backend MQTT broker — existing (`internal/app/server/proxy.go`)
- ✓ Connection tracking via ConnTrack — existing
- ✓ S3-backed inspector logging with rotation — existing
- ✓ YAML config with env var overrides — existing (`pkg/config/`)

### Active

- [ ] DynamoDB credential lookup using defcon.run schema
- [ ] In-memory credential cache keyed by MQTT username
- [ ] TTL-based automatic cache expiry
- [ ] HTTP API for manual cache eviction (evict specific entry)
- [ ] HTTP API for cache refresh (force re-fetch from DynamoDB)
- [ ] HTTP API for cache stats/inspection (current entries, hit/miss rates)
- [ ] CONNECT interception: validate creds → swap with generic creds → forward
- [ ] CONNECT rejection: send CONNACK auth failure for invalid/missing creds
- [ ] Generic Mosquitto credentials sourced from config/env vars

### Out of Scope

- Topic-based authorization — not needed, just username/password validation
- Credential CRUD (create/update/delete) — managed externally in defcon.run
- OAuth/token-based auth — MQTT username/password only
- Changes to fleet simulation or nodeinfo commands — proxy only
- Persistent cache (Redis, etc.) — in-memory with DynamoDB as source of truth

## Context

- Existing Go codebase with Cobra CLI, Paho MQTT client, protobuf-based inspection
- The proxy already parses MQTT packets and extracts credentials in the inspection layer
- DynamoDB schema follows the defcon.run project (username/password stored there)
- Process running the proxy has AWS credentials (ECS task role or EC2 instance profile)
- Every packet can trigger a lookup, so caching is critical for performance at scale
- Generic Mosquitto creds are static — configured via YAML or env vars

## Constraints

- **Tech stack**: Go — must integrate with existing codebase patterns
- **AWS**: DynamoDB access via standard AWS SDK credential chain (ECS/EC2/default)
- **Schema**: Must match existing defcon.run DynamoDB table schema for credential lookups
- **Performance**: Cache lookups must be sub-millisecond; DynamoDB only hit on cache miss

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| In-memory cache over Redis | Single-process proxy, avoid infrastructure dependency | — Pending |
| TTL + manual eviction API | Balance freshness with admin control | — Pending |
| Reject invalid at proxy | Don't burden Mosquitto with bad connections | — Pending |
| Swap creds on forward | Clients never auth directly to Mosquitto | — Pending |

---
*Last updated: 2026-03-10 after initialization*
