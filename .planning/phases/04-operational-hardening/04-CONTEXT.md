# Phase 4: Operational Hardening - Context

**Gathered:** 2026-03-11
**Status:** Ready for planning

<domain>
## Phase Boundary

The proxy handles brute-force attempts without DynamoDB cost spikes, operators can inspect and bulk-clear the cache during incidents, and the ECS health check has a real endpoint to target. Adds three new admin endpoints (list, flush, health) and negative caching to the auth flow.

</domain>

<decisions>
## Implementation Decisions

### Negative Caching Strategy
- Store negative entries in the same Otter v2 cache as positive entries using a sentinel value (`Negative: true` flag on Credential struct)
- Negative TTL: 60 seconds (configurable via `Server.CredCache.NegativeTTLSecs`, default 60)
- No separate size cap for negative entries — Otter's W-TinyLFU eviction naturally prioritizes frequently-accessed real entries over one-shot negatives
- Negative entries are visible in GET /cache/credentials listing (marked with `negative: true`)
- Negative entries count toward cache Size() and stats
- On cache miss where DynamoDB returns ErrNotFound: store a negative entry with the short TTL

### Cache Listing (GET /cache/credentials)
- Return all entries, no pagination — bounded by MaxSizeMB, practical cache size is hundreds not millions
- Fields per entry: `username`, `ttl_remaining` (integer seconds), `negative` (boolean)
- No passwords or usertype in listing — security principle (carried from Phase 3)
- Sorted by TTL remaining ascending (entries expiring soonest first)
- TTL remaining calculated via Otter v2's Range with ExpiresAt() on each entry
- Response includes top-level `count` field
- Envelope format: `{"data": {"count": N, "entries": [...]}, "timestamp": "..."}`

### Health Check (GET /health)
- Probe DynamoDB connectivity via circuit breaker state (CacheAuthenticator.IsDegraded()) — no real DynamoDB call
- Always returns HTTP 200 — ECS health check sees 200 either way, won't kill the task during DynamoDB outages
- Body indicates status: "healthy" or "degraded", dynamodb: "reachable" or "unreachable"
- Minimal response: status + dynamodb + cache_entries only (full stats at GET /cache/stats)
- Export existing `isDegraded()` as public `IsDegraded()` method on CacheAuthenticator

### Bulk Eviction (DELETE /cache/credentials)
- Implemented via Range + Delete loop: `Cache.DeleteAll()` iterates keys then Invalidate each
- Stats counters preserved (cumulative lifetime counters, not reset by flush)
- Circuit breaker NOT reset by bulk eviction — independent operations (use POST /refresh to test DynamoDB and reset CB)
- Immediate execution, no confirmation step — programmatic API, follows same pattern as single-entry DELETE
- Response: `{"data": {"evicted_count": N, "stats_reset": false}, "timestamp": "..."}`

### Claude's Discretion
- Exact Credential struct field naming for the Negative flag
- Otter v2 Range API usage details for listing and bulk delete
- How to set per-entry TTL (negative vs positive) when calling cache.Set
- Test strategy for negative caching behavior
- Error handling for Range iteration edge cases

</decisions>

<specifics>
## Specific Ideas

- Otter v2's ExpiryCalculator is set globally in NewCache — negative entries need per-entry TTL. May need to use Otter's variable expiry feature or SetWithExpiry if available
- The existing admin.Server already has `cache`, `store`, and `auth` fields — new endpoints are just more HandleFunc registrations on the ServeMux
- DeleteAll should return count so the response can report `evicted_count`
- Circuit breaker state is already tracked atomically — IsDegraded() is a one-line public wrapper

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `internal/admin/server.go`: Admin Server with Handler() ServeMux — add 3 new routes
- `internal/admin/server.go`: writeJSON/writeError helpers — reuse for all new endpoints
- `internal/admin/server.go`: withLogging middleware — already wraps all routes
- `internal/credcache/cache.go`: Cache.Get/Set/Delete/Stats/Size — extend with DeleteAll and Range-based listing
- `internal/credcache/auth.go`: CacheAuthenticator with isDegraded() — export as IsDegraded()
- `internal/credcache/types.go`: Credential struct — add Negative bool field

### Established Patterns
- Go 1.22+ ServeMux with method routing: `mux.HandleFunc("GET /health", handler)`
- Response envelope: `{"data": {...}, "timestamp": "..."}` / `{"error": "...", "timestamp": "..."}`
- Constructor pattern: NewServer() already takes cache + store + auth + logger
- Unexported handler methods: handleEvict, handleRefresh, handleStats pattern

### Integration Points
- `internal/admin/server.go`: Register 3 new routes in Handler()
- `internal/credcache/cache.go`: Add DeleteAll() and Entries() methods
- `internal/credcache/auth.go`: Add IsDegraded() public method, update Verify() for negative cache entries
- `internal/credcache/types.go`: Add Negative field to Credential struct
- `pkg/config/config.go`: Add NegativeTTLSecs to CredCache config struct

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 04-operational-hardening*
*Context gathered: 2026-03-11*
