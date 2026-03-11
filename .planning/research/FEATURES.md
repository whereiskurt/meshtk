# Feature Landscape

**Domain:** MQTT proxy credential cache with admin API
**Researched:** 2026-03-10

## Table Stakes

Features users (operators) expect. Missing = the cache system is not operationally viable.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| DynamoDB credential lookup | Source of truth for user auth; replaces current hardcoded seed-based validation in `inspect.go` lines 99-115 | Medium | Must match defcon.run schema exactly. Single GetItem by username. |
| In-memory cache with TTL expiry | Every MQTT CONNECT hits the auth path; DynamoDB on every connection is unacceptable latency (~5-15ms per call vs sub-ms cache hit) | Medium | Use `sync.Map` or sharded map with RWMutex. TTL per entry, not global flush. |
| Cache-miss backfill from DynamoDB | On cache miss, fetch from DynamoDB, populate cache, then validate. Transparent to the MQTT client. | Low | Must be synchronous in the CONNECT path -- client blocks until auth resolves. |
| CONNECT rejection with proper CONNACK | Invalid credentials must return MQTT CONNACK with return code 0x05 (not authorized) rather than silently dropping the connection | Low | Current code just returns (drops connection). Proper CONNACK is MQTT protocol compliance. |
| Credential swap on valid auth | After validating client creds, replace username/password with generic Mosquitto creds before forwarding to broker | Low | Already implemented for the seed-based flow. Refactor to use cache result instead. |
| Generic creds from config | Mosquitto shared credentials sourced from YAML config or env vars, not hardcoded `public`/`31337` as today | Low | Add fields to `Server` config struct. Env var override via existing `MESHTK_` prefix. |
| Cache eviction API endpoint | `DELETE /cache/credentials/{username}` -- evict a specific cached entry. Required for immediate revocation when a user is banned/removed. | Low | Single endpoint, mutex-protected delete from map. |
| Cache refresh API endpoint | `POST /cache/credentials/{username}/refresh` -- force re-fetch from DynamoDB for a specific user. Needed when password is changed externally. | Low | Delete + re-fetch on next CONNECT, or eager re-fetch. Eager is better for admin UX. |
| Cache stats API endpoint | `GET /cache/stats` -- current entry count, hit/miss counters, hit rate. Operators need visibility into whether the cache is working. | Low | Atomic counters for hits/misses. Entry count from map length. |
| Passthrough allowlist | Preserve existing behavior for hardcoded usernames (`ghosts`, `kph`, `ax`, `meshmap`) that bypass credential validation entirely | Low | Move to config rather than hardcoded. List of usernames that skip cache lookup. |
| Graceful degradation on DynamoDB failure | If DynamoDB is unreachable, serve from cache for existing entries. New users fail closed (reject). Log errors. Do not crash. | Medium | Timeout on DynamoDB calls (2-3 seconds). Cache entries survive DynamoDB outages until TTL expires. |

## Differentiators

Features that improve operational excellence beyond the minimum. Not expected day one, but high value.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Cache inspection endpoint | `GET /cache/credentials` -- list all cached usernames (not passwords) with their TTL remaining. Enables debugging "is user X cached?" without checking logs. | Low | Return username list with expiry timestamps. Never expose passwords. |
| Bulk eviction endpoint | `DELETE /cache/credentials` -- flush entire cache. Useful during incident response or after bulk credential rotation. | Low | Clear the map under write lock. All subsequent CONNECTs will re-validate from DynamoDB. |
| Negative caching | Cache failed lookups (username not found in DynamoDB) with a shorter TTL. Prevents repeated DynamoDB queries for bots hammering with invalid usernames. | Medium | Separate TTL for negative entries (e.g., 60s vs 300s for positive). Prevents DynamoDB cost spikes from brute-force attempts. |
| Health check endpoint | `GET /health` -- returns 200 if proxy is running, includes DynamoDB connectivity status. Useful for ECS health checks and monitoring. | Low | Already running on ECS; health endpoint enables proper container health checks. |
| Structured JSON logging for auth events | Log auth success/failure with username, source IP, cache hit/miss as structured JSON. Enables log aggregation and alerting. | Low | Extend existing `InspectorLogger` pattern. Add auth-specific log entries. |
| Rate limiting per source IP on failed auth | Track failed auth attempts per IP and slow/block after threshold. Prevents credential stuffing without burdening DynamoDB. | Medium | Extend existing `rateLimiter` pattern from `proxy.go`. Apply specifically to CONNECT failures. |
| Configurable TTL via config | TTL for cache entries configurable via YAML/env vars rather than hardcoded. Different deployments may want different freshness. | Low | Add `CacheTTLSecs` and `CacheNegativeTTLSecs` to `Server` config struct. |
| Prometheus metrics endpoint | `GET /metrics` -- expose cache hit/miss rates, DynamoDB latency, active connections as Prometheus metrics. | Medium | Standard for infrastructure services. But adds a dependency (prometheus client library). Defer unless monitoring stack demands it. |

## Anti-Features

Features to explicitly NOT build.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| Credential CRUD via admin API | Credentials are managed in defcon.run. Dual-write creates consistency nightmares. The proxy is a consumer, not an authority. | Expose eviction/refresh only. CRUD stays in defcon.run. |
| Persistent cache (Redis/Memcached) | Single-process proxy on ECS. Adding Redis adds infrastructure, failure modes, and network hops for no benefit. DynamoDB is already the durable store. | In-memory cache with DynamoDB as source of truth. Cache rebuilds on restart. |
| Topic-based authorization | Out of scope per PROJECT.md. Username/password validation only. Topic ACLs are a different problem with different complexity. | Pass all topic subscriptions through to Mosquitto as today. |
| OAuth/JWT/token auth | MQTT clients (Meshtastic devices) send username/password. There is no token exchange protocol in the Meshtastic MQTT flow. | Stick with username/password validation against DynamoDB. |
| Distributed cache synchronization | If multiple proxy instances run, cache sync is tempting. But it adds massive complexity (gossip protocol, eventual consistency, conflict resolution). | Each proxy instance maintains its own cache. TTL ensures eventual consistency. DynamoDB is the shared source of truth. |
| Admin API authentication | For an internal API on ECS (not exposed to internet), adding auth adds complexity with minimal security benefit. Network-level security (security groups) is sufficient. | Bind admin HTTP server to localhost or internal interface. Rely on ECS/VPC network isolation. |
| Automatic cache warming on startup | Pre-loading all credentials from DynamoDB on startup sounds helpful but DynamoDB scan is expensive and the proxy handles cold cache gracefully (miss -> fetch -> cache). | Lazy population on first CONNECT per user. Cache fills naturally within minutes of deployment. |
| Password hashing in cache | Storing hashed passwords in cache and comparing hashes adds latency for no security benefit -- the password already came over the wire in plaintext MQTT and lives in DynamoDB in whatever form defcon.run stores it. | Store the credential as-is from DynamoDB. Compare directly. |

## Feature Dependencies

```
DynamoDB credential lookup
  --> Cache-miss backfill (requires DynamoDB client)
  --> In-memory cache with TTL (cache stores DynamoDB results)
    --> CONNECT rejection with CONNACK (requires validation result)
    --> Credential swap on valid auth (requires validation result)
    --> Cache eviction API (requires cache to exist)
    --> Cache refresh API (requires cache + DynamoDB client)
    --> Cache stats API (requires cache counters)
    --> Cache inspection endpoint (requires cache)
    --> Bulk eviction endpoint (requires cache)

Generic creds from config (independent, needed by credential swap)

Passthrough allowlist (independent, checked before cache lookup)

Negative caching (requires cache + DynamoDB lookup, adds after core cache works)

Health check endpoint (independent)

Rate limiting on failed auth (requires auth flow to be working first)
```

## MVP Recommendation

Prioritize in this order:

1. **Generic creds from config** -- trivial config change, removes hardcoded values, unblocks credential swap
2. **Passthrough allowlist from config** -- move hardcoded usernames to config, preserves existing behavior
3. **DynamoDB credential lookup** -- core capability, the reason this milestone exists
4. **In-memory cache with TTL + cache-miss backfill** -- makes DynamoDB lookup performant
5. **CONNECT rejection with proper CONNACK** -- protocol compliance, replaces silent drop
6. **Credential swap using cache result** -- refactor existing swap logic to use cache instead of seed
7. **Cache eviction API** -- minimum admin control for credential revocation
8. **Cache stats API** -- minimum operational visibility
9. **Cache refresh API** -- admin convenience, low incremental effort after eviction endpoint exists
10. **Graceful degradation on DynamoDB failure** -- operational resilience

Defer to post-MVP:
- **Negative caching**: Only needed under brute-force load. Add when traffic patterns demand it.
- **Health check endpoint**: Valuable but not blocking the credential cache feature itself.
- **Cache inspection endpoint**: Nice for debugging but stats endpoint covers most needs.
- **Bulk eviction**: Edge case (mass credential rotation). Single-user eviction is sufficient initially.
- **Prometheus metrics**: Only if monitoring stack is already Prometheus-based. JSON stats endpoint covers initial needs.
- **Rate limiting on failed auth**: The existing rate limiter in `proxy.go` already provides connection-level protection. Auth-specific limiting is a refinement.

## Sources

- Existing codebase analysis: `internal/app/server/inspect.go` (current auth flow), `proxy.go` (connection handling), `cmd.go` (server structure), `pkg/config/config.go` (configuration patterns)
- PROJECT.md requirements and constraints
- MQTT 3.1.1 specification for CONNACK return codes (HIGH confidence -- well-established protocol)
- DynamoDB operational patterns (HIGH confidence -- standard AWS SDK usage)
