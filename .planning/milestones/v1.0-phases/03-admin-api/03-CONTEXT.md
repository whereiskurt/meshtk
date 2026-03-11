# Phase 3: Admin API - Context

**Gathered:** 2026-03-10
**Status:** Ready for planning

<domain>
## Phase Boundary

HTTP server for cache eviction, force refresh, and stats. Operators can evict specific cached credentials, force a DynamoDB re-fetch, and inspect cache performance — all via HTTP on a configurable local address. No authentication (network-level security via VPC/security groups, per out-of-scope decision).

</domain>

<decisions>
## Implementation Decisions

### Response Format & Structure
- All responses use consistent envelope: `{"data": {...}, "timestamp": "2026-03-10T12:00:00Z"}` for success
- Error responses use: `{"error": "message", "timestamp": "2026-03-10T12:00:00Z"}` with appropriate HTTP status codes
- DELETE /cache/credentials/{username} returns 200 always (idempotent) — `{"data": {"username": "x", "evicted": true/false}, "timestamp": "..."}`
- GET /cache/stats returns `{"data": {"entries": N, "hits": N, "misses": N, "hit_rate": 0.957, "evictions": N}, "timestamp": "..."}`
- All JSON responses include `Content-Type: application/json`

### Server Lifecycle
- Admin HTTP server launches as a goroutine inside ProxyServer() — starts alongside the proxy
- Uses main logger (`n.Config.Log`) — not InspectorLogger (keeps inspector logs clean for MQTT traffic)
- Log every admin request at INFO level: method, path, status code, duration (low volume — operators only)
- Simple close on SIGINT — no graceful drain timeout (admin handlers are sub-millisecond cache operations)
- Binds to `Server.AdminListenAddress` (default `localhost:9090`, already in config from Phase 1)

### Refresh Behavior
- POST /cache/credentials/{username}/refresh returns confirmation only: `{"data": {"username": "x", "refreshed": true}, "timestamp": "..."}`
- No credential details (password, usertype) in refresh response — security principle
- If username not found in DynamoDB: return 404 `{"error": "username not found in DynamoDB"}` AND evict from cache if present (credential was revoked)
- Refresh bypasses the circuit breaker — explicit operator action, useful for incident response and DynamoDB connectivity probing
- Successful refresh resets the circuit breaker failure counter (proves DynamoDB is reachable, resumes automatic auth flow immediately)

### Package Placement
- New package: `internal/admin/`
- Admin Server struct takes Cache + CredentialStore + CacheAuthenticator + Logger as constructor params
- Exposes single `Handler() http.Handler` method — encapsulates all routing via Go 1.22+ ServeMux
- ServerCmd creates admin.New(...) and calls `go http.ListenAndServe(addr, adminSrv.Handler())` in ProxyServer
- Handlers are unexported methods on admin.Server (handleEvict, handleRefresh, handleStats)

### Claude's Discretion
- Request logging middleware implementation (wrapper vs per-handler)
- Exact admin.Server struct field names and constructor signature
- Whether to add a method to CacheAuthenticator for circuit breaker reset, or expose atomic fields
- Test strategy (httptest.Server with mock dependencies)
- Error message wording

</decisions>

<specifics>
## Specific Ideas

- Cache.Stats() already returns Hits, Misses, Evictions, HitRate — map directly to stats endpoint
- Cache.Delete() and Cache.Set() already exist for evict and refresh
- Store.Fetch() bypasses CacheAuthenticator entirely (no singleflight, no circuit breaker) — use directly for refresh
- CacheAuthenticator.recordSuccess() resets circuit breaker — need to expose this or add a public method for admin refresh to call

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `internal/credcache/cache.go`: Cache.Get/Set/Delete/Stats — direct use in admin handlers
- `internal/credcache/store.go`: DynamoDBStore.Fetch — direct call for refresh (bypasses circuit breaker)
- `internal/credcache/auth.go`: CacheAuthenticator.recordSuccess — needs public method for CB reset
- `pkg/config/config.go:78`: Server.AdminListenAddress already configured (default "localhost:9090")

### Established Patterns
- Go 1.22+ ServeMux with method routing: `mux.HandleFunc("DELETE /path/{param}", handler)`
- Constructor pattern: `New[Type]()` returns pointer
- Single-letter receiver names: `(s *Server)`
- json.NewEncoder(w).Encode() for responses

### Integration Points
- `internal/app/server/cmd.go`: NewServer() — create admin.Server and store on ServerCmd
- `internal/app/server/cmd.go`: ProxyServer()/StartProxyServer() — launch admin HTTP in goroutine
- `internal/credcache/auth.go` — may need public ResetCircuitBreaker() method

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 03-admin-api*
*Context gathered: 2026-03-10*
