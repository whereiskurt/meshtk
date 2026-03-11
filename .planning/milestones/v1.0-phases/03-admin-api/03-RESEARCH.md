# Phase 3: Admin API - Research

**Researched:** 2026-03-10
**Domain:** Go stdlib net/http admin server for cache management
**Confidence:** HIGH

## Summary

Phase 3 adds an HTTP admin API for cache eviction, forced refresh, and stats inspection. The implementation is straightforward: a new `internal/admin/` package with a Server struct that wraps Cache, CredentialStore, and CacheAuthenticator dependencies. All three endpoints (DELETE, POST, GET) map directly to existing Cache and Store methods with minimal new logic.

The primary complexity is the refresh endpoint's interaction with the circuit breaker -- `recordSuccess()` on CacheAuthenticator is unexported and needs a public `ResetCircuitBreaker()` method. Everything else is standard Go HTTP handler patterns with Go 1.22+ ServeMux routing.

**Primary recommendation:** Build a thin admin.Server with `Handler() http.Handler` returning a Go 1.22+ ServeMux, using httptest for tests. Add `Cache.Size()` (wrapping `inner.EstimatedSize()`) and `CacheAuthenticator.ResetCircuitBreaker()` as the only modifications to existing code.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- All responses use consistent envelope: `{"data": {...}, "timestamp": "2026-03-10T12:00:00Z"}` for success
- Error responses use: `{"error": "message", "timestamp": "2026-03-10T12:00:00Z"}` with appropriate HTTP status codes
- DELETE /cache/credentials/{username} returns 200 always (idempotent) -- `{"data": {"username": "x", "evicted": true/false}, "timestamp": "..."}`
- GET /cache/stats returns `{"data": {"entries": N, "hits": N, "misses": N, "hit_rate": 0.957, "evictions": N}, "timestamp": "..."}`
- All JSON responses include `Content-Type: application/json`
- Admin HTTP server launches as a goroutine inside ProxyServer() -- starts alongside the proxy
- Uses main logger (`n.Config.Log`) -- not InspectorLogger
- Log every admin request at INFO level: method, path, status code, duration
- Simple close on SIGINT -- no graceful drain timeout
- Binds to `Server.AdminListenAddress` (default `localhost:9090`, already in config from Phase 1)
- POST /cache/credentials/{username}/refresh returns confirmation only: `{"data": {"username": "x", "refreshed": true}, "timestamp": "..."}`
- No credential details in refresh response -- security principle
- If username not found in DynamoDB: return 404 AND evict from cache if present
- Refresh bypasses the circuit breaker -- explicit operator action
- Successful refresh resets the circuit breaker failure counter
- New package: `internal/admin/`
- Admin Server struct takes Cache + CredentialStore + CacheAuthenticator + Logger as constructor params
- Exposes single `Handler() http.Handler` method -- encapsulates all routing via Go 1.22+ ServeMux
- Handlers are unexported methods on admin.Server (handleEvict, handleRefresh, handleStats)

### Claude's Discretion
- Request logging middleware implementation (wrapper vs per-handler)
- Exact admin.Server struct field names and constructor signature
- Whether to add a method to CacheAuthenticator for circuit breaker reset, or expose atomic fields
- Test strategy (httptest.Server with mock dependencies)
- Error message wording

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| ADMIN-01 | `DELETE /cache/credentials/{username}` evicts a specific cached entry | Cache.Delete() exists; Cache.Get() needed to check if entry existed for evicted:true/false response |
| ADMIN-02 | `POST /cache/credentials/{username}/refresh` force re-fetches from DynamoDB | Store.Fetch() bypasses authenticator; need public ResetCircuitBreaker() on CacheAuthenticator |
| ADMIN-03 | `GET /cache/stats` returns entry count, hit/miss counters, hit rate | Cache.Stats() returns Hits/Misses/Evictions/HitRate; need Cache.Size() wrapping EstimatedSize() for entry count |
| ADMIN-07 | Admin HTTP server binds to configurable address (default localhost) | Server.AdminListenAddress already configured at `localhost:9090` in config.go |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| net/http | stdlib | HTTP server and routing | Go 1.22+ ServeMux supports method+path routing natively; no framework needed for 3 endpoints |
| encoding/json | stdlib | JSON encoding/decoding | Standard for JSON API responses |
| net/http/httptest | stdlib | Test HTTP handlers | Standard Go testing approach for HTTP handlers |
| log (logrus) | v1.9.3 | Request logging | Already used project-wide via `n.Config.Log` |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| time | stdlib | Timestamps in responses | Every response envelope includes ISO 8601 timestamp |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| stdlib ServeMux | chi, gorilla/mux | Overkill for 3 endpoints; project decision locks stdlib |

**Installation:**
No new dependencies required. Everything is stdlib or already in go.mod.

## Architecture Patterns

### Recommended Project Structure
```
internal/
  admin/
    server.go          # Server struct, New(), Handler(), unexported handlers
    server_test.go     # httptest-based tests with mock dependencies
  credcache/
    cache.go           # + Size() method (wraps EstimatedSize())
    auth.go            # + ResetCircuitBreaker() public method
```

### Pattern 1: Admin Server Constructor
**What:** Server struct with dependency injection via constructor
**When to use:** Standard for this project
**Example:**
```go
// internal/admin/server.go
package admin

import (
    "encoding/json"
    "net/http"
    "time"

    log "github.com/sirupsen/logrus"
    "github.com/whereiskurt/meshtk/internal/credcache"
)

type Server struct {
    cache *credcache.Cache
    store credcache.CredentialStore
    auth  *credcache.CacheAuthenticator
    log   *log.Logger
}

func NewServer(cache *credcache.Cache, store credcache.CredentialStore, auth *credcache.CacheAuthenticator, logger *log.Logger) *Server {
    return &Server{
        cache: cache,
        store: store,
        auth:  auth,
        log:   logger,
    }
}

func (s *Server) Handler() http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("DELETE /cache/credentials/{username}", s.handleEvict)
    mux.HandleFunc("POST /cache/credentials/{username}/refresh", s.handleRefresh)
    mux.HandleFunc("GET /cache/stats", s.handleStats)
    return s.withLogging(mux)
}
```

### Pattern 2: Response Envelope
**What:** Consistent JSON envelope for all responses
**When to use:** Every admin endpoint response
**Example:**
```go
type successResponse struct {
    Data      interface{} `json:"data"`
    Timestamp string      `json:"timestamp"`
}

type errorResponse struct {
    Error     string `json:"error"`
    Timestamp string `json:"timestamp"`
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(successResponse{
        Data:      data,
        Timestamp: time.Now().UTC().Format(time.RFC3339),
    })
}

func writeError(w http.ResponseWriter, status int, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(errorResponse{
        Error:     msg,
        Timestamp: time.Now().UTC().Format(time.RFC3339),
    })
}
```

### Pattern 3: Logging Middleware
**What:** Wrap the ServeMux with request logging
**When to use:** All admin requests (low volume, INFO level)
**Example:**
```go
func (s *Server) withLogging(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rw := &statusWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(rw, r)
        s.log.Infof("admin %s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
    })
}

type statusWriter struct {
    http.ResponseWriter
    status int
}

func (w *statusWriter) WriteHeader(code int) {
    w.status = code
    w.ResponseWriter.WriteHeader(code)
}
```

### Pattern 4: Go 1.22+ Path Parameters
**What:** Extract path parameters using `r.PathValue("username")`
**When to use:** DELETE and POST handlers that need {username}
**Example:**
```go
func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
    username := r.PathValue("username")
    // ...
}
```

### Anti-Patterns to Avoid
- **Exposing credential data in responses:** Never include password or usertype in any admin response
- **Using CacheAuthenticator.Verify() for refresh:** Refresh must call Store.Fetch() directly to bypass singleflight and circuit breaker
- **Graceful shutdown complexity:** Admin handlers are sub-millisecond cache operations; simple close is correct
- **Using InspectorLogger:** Admin logs go to main logger, not inspector log (keeps MQTT traffic logs clean)

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| HTTP routing | Custom router | Go 1.22+ ServeMux | Native method+path routing with wildcards |
| JSON serialization | Custom encoder | encoding/json | Standard, handles all types |
| Test HTTP servers | Manual TCP listener | net/http/httptest | Standard Go HTTP testing |
| Response status capture | Custom ResponseWriter | Minimal statusWriter wrapper | Only need to capture status code for logging |

**Key insight:** This phase is deliberately simple -- 3 endpoints, no auth, no middleware frameworks. The stdlib handles everything.

## Common Pitfalls

### Pitfall 1: Cache.Get() Does Not Distinguish "Not Found" from "Expired"
**What goes wrong:** Using Cache.Get() to check if entry exists before Delete() may return false for entries that exist but expired.
**Why it happens:** Otter evicts expired entries lazily.
**How to avoid:** For the evict endpoint, attempt Delete() regardless. Use Get() only to determine the `evicted` boolean in the response (if Get returns true, the entry was present; if false, it may have expired or never existed -- either way `evicted: false` is correct).
**Warning signs:** Inconsistent `evicted` field values.

### Pitfall 2: Refresh Must Handle "Not Found in DynamoDB" Correctly
**What goes wrong:** Returning 200 when DynamoDB says user doesn't exist.
**Why it happens:** Not checking for ErrNotFound from Store.Fetch().
**How to avoid:** Check for `credcache.ErrNotFound` -- return 404 AND evict from cache (credential was revoked). This is a locked decision.
**Warning signs:** Stale cache entries surviving after credential revocation.

### Pitfall 3: Circuit Breaker Reset Requires Public Method
**What goes wrong:** Can't reset circuit breaker from admin package because `recordSuccess()` is unexported.
**Why it happens:** Phase 2 made recordSuccess private (only called internally).
**How to avoid:** Add `ResetCircuitBreaker()` public method on CacheAuthenticator that resets `consecutiveFailures` to 0 with appropriate logging.
**Warning signs:** Compile error when admin tries to call unexported method.

### Pitfall 4: Missing Cache.Size() for Entry Count
**What goes wrong:** GET /cache/stats needs `entries` count but Cache has no Size method.
**Why it happens:** Phase 1 didn't need entry count reporting.
**How to avoid:** Add `Size() int` method to Cache that returns `c.inner.EstimatedSize()`. Otter v2's `EstimatedSize()` is already available.
**Warning signs:** Missing `entries` field in stats response.

### Pitfall 5: Race Between Get and Delete on Evict
**What goes wrong:** Between checking Get() and calling Delete(), the entry could expire.
**Why it happens:** Non-atomic check-then-delete.
**How to avoid:** This is acceptable -- the `evicted` field is best-effort. The entry will be gone either way. Don't add locking for this.
**Warning signs:** Overthinking consistency for an admin debugging endpoint.

## Code Examples

### Evict Handler
```go
func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
    username := r.PathValue("username")
    _, existed := s.cache.Get(username)
    s.cache.Delete(username)
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "username": username,
        "evicted":  existed,
    })
}
```

### Refresh Handler
```go
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
    username := r.PathValue("username")
    cred, err := s.store.Fetch(r.Context(), username)
    if err != nil {
        if errors.Is(err, credcache.ErrNotFound) {
            s.cache.Delete(username) // Evict revoked credential
            writeError(w, http.StatusNotFound, "username not found in DynamoDB")
            return
        }
        writeError(w, http.StatusBadGateway, "DynamoDB fetch failed")
        return
    }
    s.cache.Set(username, cred)
    s.auth.ResetCircuitBreaker() // Proves DynamoDB is reachable
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "username":  username,
        "refreshed": true,
    })
}
```

### Stats Handler
```go
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
    cs := s.cache.Stats()
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "entries":   s.cache.Size(),
        "hits":      cs.Hits,
        "misses":    cs.Misses,
        "hit_rate":  cs.HitRate,
        "evictions": cs.Evictions,
    })
}
```

### Integration in cmd.go (StartProxyServer)
```go
// In StartProxyServer(), before the goroutine that accepts connections:
adminAddr := n.Config.Server.AdminListenAddress
if adminAddr != "" {
    adminSrv := admin.NewServer(cache, store, auth, n.Config.Log)
    go func() {
        n.Config.Log.Infof("Admin API listening on %s", adminAddr)
        if err := http.ListenAndServe(adminAddr, adminSrv.Handler()); err != nil {
            n.Config.Log.Errorf("Admin server error: %v", err)
        }
    }()
}
```

### Required Additions to Existing Code

**Cache.Size() in cache.go:**
```go
// Size returns the approximate number of entries in the cache.
func (c *Cache) Size() int {
    return c.inner.EstimatedSize()
}
```

**CacheAuthenticator.ResetCircuitBreaker() in auth.go:**
```go
// ResetCircuitBreaker resets the circuit breaker failure counter.
// Called by admin refresh to signal DynamoDB is reachable.
func (a *CacheAuthenticator) ResetCircuitBreaker() {
    prev := a.consecutiveFailures.Swap(0)
    if prev > 0 {
        log.Printf("[INFO] Circuit breaker reset by admin (was at %d consecutive failures)", prev)
    }
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| gorilla/mux for routing | Go 1.22+ ServeMux with method routing | Go 1.22 (Feb 2024) | No external dependency for `"DELETE /path/{param}"` patterns |
| Manual path parsing | `r.PathValue("param")` | Go 1.22 (Feb 2024) | Built-in path parameter extraction |

**Deprecated/outdated:**
- gorilla/mux: Archived in 2022, un-archived but stdlib now covers its core use case

## Open Questions

1. **ServerCmd needs access to Cache, Store, and CacheAuthenticator separately for admin wiring**
   - What we know: Currently `NewServer()` creates cache and store locally, then creates `CacheAuthenticator` and stores it as `n.Authenticator` (interface type). The cache and store are not stored on ServerCmd.
   - What's unclear: None -- this is a known refactor needed.
   - Recommendation: Store cache, store, and CacheAuthenticator (concrete type, not interface) as fields on ServerCmd so admin.NewServer() can receive them in StartProxyServer(). This is a small but necessary change to cmd.go.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) + httptest |
| Config file | None needed -- `go test ./...` |
| Quick run command | `go test ./internal/admin/ -v -count=1` |
| Full suite command | `go test ./... -count=1` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| ADMIN-01 | DELETE evicts entry, returns 200 with evicted:true/false | unit | `go test ./internal/admin/ -run TestEvict -v -count=1` | No -- Wave 0 |
| ADMIN-02 | POST refresh re-fetches from DynamoDB, resets CB | unit | `go test ./internal/admin/ -run TestRefresh -v -count=1` | No -- Wave 0 |
| ADMIN-03 | GET stats returns entries/hits/misses/hit_rate/evictions | unit | `go test ./internal/admin/ -run TestStats -v -count=1` | No -- Wave 0 |
| ADMIN-07 | Admin server binds to configured address | integration | `go test ./internal/admin/ -run TestHandler -v -count=1` | No -- Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/admin/ -v -count=1`
- **Per wave merge:** `go test ./... -count=1`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/admin/server_test.go` -- all admin handler tests using httptest + mock cache/store
- [ ] `Cache.Size()` method -- needed before stats test can verify entry count
- [ ] `CacheAuthenticator.ResetCircuitBreaker()` -- needed before refresh test can verify CB reset

## Sources

### Primary (HIGH confidence)
- Project source code: `internal/credcache/cache.go`, `auth.go`, `store.go`, `types.go` -- direct inspection
- Project source code: `internal/app/server/cmd.go` -- integration point inspection
- Project source code: `vendor/github.com/maypok86/otter/v2/cache.go` -- `EstimatedSize()` method confirmed at line 413
- Go 1.22+ stdlib: ServeMux method routing and `r.PathValue()` -- verified in Go docs

### Secondary (MEDIUM confidence)
- Go 1.25.5 runtime confirmed via `go version` -- all Go 1.22+ features available

### Tertiary (LOW confidence)
None -- all findings verified against source code.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all stdlib, no new dependencies
- Architecture: HIGH -- direct inspection of existing code, clear integration points
- Pitfalls: HIGH -- identified from actual code gaps (missing Size(), unexported recordSuccess())

**Research date:** 2026-03-10
**Valid until:** 2026-04-10 (stable -- stdlib-only, no external dependency changes)
