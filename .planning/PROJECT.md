# MeshTK — MQTT Proxy Credential Cache

## What This Is

An MQTT proxy enhancement for MeshTK that validates Meshtastic client connections against DynamoDB-backed credentials with sub-millisecond in-memory caching. Valid clients are transparently forwarded with generic Mosquitto credentials; invalid clients are rejected with CONNACK 0x05 before reaching the broker. Operators manage the cache via an HTTP admin API with eviction, refresh, stats, listing, flush, and health endpoints.

## Core Value

Every MQTT CONNECT is validated against cached credentials with minimal latency — invalid clients are rejected before reaching the broker, and valid clients are transparently forwarded with generic creds.

## Requirements

### Validated

- ✓ TCP proxy listener accepts MQTT connections — existing
- ✓ MQTT packet parsing extracts username, password, clientID — existing
- ✓ Rule-based packet inspection with allow/block/kill/slow decisions — existing
- ✓ Proxy forwarding to backend MQTT broker — existing
- ✓ Connection tracking via ConnTrack — existing
- ✓ S3-backed inspector logging with rotation — existing
- ✓ YAML config with env var overrides — existing
- ✓ DynamoDB credential lookup using defcon.run schema — v1.0
- ✓ In-memory credential cache (Otter v2) with configurable TTL and max size — v1.0
- ✓ TTL-based automatic cache expiry — v1.0
- ✓ CONNECT interception: validate → swap generic creds → forward — v1.0
- ✓ CONNECT rejection: CONNACK 0x05 for invalid/missing creds — v1.0
- ✓ Passthrough allowlist from YAML config — v1.0
- ✓ Generic Mosquitto credentials from YAML/env — v1.0
- ✓ Singleflight-deduplicated DynamoDB fetch on cache miss — v1.0
- ✓ Circuit breaker for DynamoDB outage graceful degradation — v1.0
- ✓ Admin API: evict, refresh, stats, list, flush, health — v1.0
- ✓ Negative caching for brute-force protection — v1.0
- ✓ Health endpoint for ECS health checks — v1.0
- ✓ All config fields (TTL, max size, admin address, table, region, negative TTL) configurable — v1.0

### Active

(None — planning next milestone)

### Out of Scope

- Topic-based authorization — not needed, just username/password validation
- Credential CRUD (create/update/delete) — managed externally in defcon.run
- OAuth/token-based auth — MQTT username/password only
- Changes to fleet simulation or nodeinfo commands — proxy only
- Persistent cache (Redis, etc.) — in-memory with DynamoDB as source of truth
- Admin API authentication — internal API on ECS; network-level security sufficient
- Rate limiting per source IP — deferred to v2
- Prometheus metrics — deferred to v2
- Structured JSON logging for auth events — deferred to v2

## Context

Shipped v1.0 with 29,299 LOC Go (2,461 new/modified for credential cache).
Tech stack: Go, Cobra CLI, Paho MQTT, Otter v2, AWS SDK v2, stdlib net/http.
4 phases, 8 plans, 21 requirements — all satisfied and verified.

Known tech debt:
- Admin package uses stdlib `log.Logger` instead of project's logrus logger
- DynamoDB table schema (attribute names, password format) verified against live table pattern but not production-tested

## Constraints

- **Tech stack**: Go — must integrate with existing codebase patterns
- **AWS**: DynamoDB access via standard AWS SDK credential chain (ECS/EC2/default)
- **Schema**: Must match existing defcon.run DynamoDB table schema
- **Performance**: Cache lookups must be sub-millisecond; DynamoDB only hit on cache miss

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| In-memory cache (Otter v2) over Redis | Single-process proxy, avoid infra dependency | ✓ Good — sub-ms lookups, zero GC pressure |
| TTL + manual eviction API | Balance freshness with admin control | ✓ Good — 900s default TTL + 6 admin endpoints |
| Reject invalid at proxy level | Don't burden Mosquitto with bad connections | ✓ Good — CONNACK 0x05 before broker sees it |
| Swap creds on forward | Clients never auth directly to Mosquitto | ✓ Good — transparent to broker |
| Singleflight for concurrent fetches | Prevent DynamoDB stampede on cache miss | ✓ Good — verified by test: 10 goroutines, 1 fetch |
| Circuit breaker for DynamoDB | Graceful degradation, not cascading failure | ✓ Good — cache hits continue during outage |
| Negative caching with short TTL | Prevent DynamoDB cost spikes from brute-force | ✓ Good — 60s configurable TTL |
| Health endpoint always HTTP 200 | ECS should not kill task during DynamoDB outage | ✓ Good — status field reports healthy/degraded |
| stdlib log in admin/credcache | Avoid logrus dependency in new packages | ⚠️ Revisit — inconsistent with proxy logging |
| aws-sdk-go-v2 for DynamoDB | v1 EOL July 2025; v1 S3 code untouched | ✓ Good |
| Go 1.22+ ServeMux for admin | No framework needed for 6 endpoints | ✓ Good — exact match before wildcard |

---
*Last updated: 2026-03-11 after v1.0 milestone*
