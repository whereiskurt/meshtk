# Phase 4: Operational Hardening - Research

**Researched:** 2026-03-11
**Domain:** Otter v2 cache extensions, Go HTTP admin endpoints, negative caching
**Confidence:** HIGH

## Summary

Phase 4 adds three new admin endpoints (cache listing, bulk flush, health check) and negative caching to prevent DynamoDB cost spikes from unknown usernames. All changes build on well-established patterns from Phases 1-3: the existing admin Server, Cache wrapper, and CacheAuthenticator.

The critical technical finding is that Otter v2.3.0 provides all required APIs natively: `SetExpiresAfter()` for per-entry TTL override (negative entries get shorter TTL), `All()` iterator with `GetEntryQuietly()` for TTL-aware listing, `InvalidateAll()` for bulk flush, and `Entry.ExpiresAfter()` for remaining TTL calculation. No custom expiry calculator replacement is needed -- the existing `ExpiryWriting` calculator sets the default TTL on write, then `SetExpiresAfter()` overrides individual entries to shorter durations.

The existing code structure (admin Server with HandleFunc registrations, writeJSON/writeError helpers, CacheAuthenticator with isDegraded) means all changes are additive -- no refactoring required.

**Primary recommendation:** Use Otter v2's `SetExpiresAfter()` for per-entry negative TTL, `All()` iterator for listing, and `InvalidateAll()` for bulk flush. Add `Negative bool` field to Credential struct and update Verify() to check it.

<user_constraints>

## User Constraints (from CONTEXT.md)

### Locked Decisions
- Store negative entries in the same Otter v2 cache as positive entries using a sentinel value (`Negative: true` flag on Credential struct)
- Negative TTL: 60 seconds (configurable via `Server.CredCache.NegativeTTLSecs`, default 60)
- No separate size cap for negative entries -- Otter's W-TinyLFU eviction naturally prioritizes frequently-accessed real entries over one-shot negatives
- Negative entries are visible in GET /cache/credentials listing (marked with `negative: true`)
- Negative entries count toward cache Size() and stats
- On cache miss where DynamoDB returns ErrNotFound: store a negative entry with the short TTL
- Return all entries, no pagination -- bounded by MaxSizeMB, practical cache size is hundreds not millions
- Fields per entry: `username`, `ttl_remaining` (integer seconds), `negative` (boolean)
- No passwords or usertype in listing -- security principle
- Sorted by TTL remaining ascending (entries expiring soonest first)
- TTL remaining calculated via Otter v2's Range with ExpiresAt() on each entry
- Response includes top-level `count` field
- Envelope format: `{"data": {"count": N, "entries": [...]}, "timestamp": "..."}`
- Health check probes DynamoDB connectivity via circuit breaker state (CacheAuthenticator.IsDegraded()) -- no real DynamoDB call
- Always returns HTTP 200 -- ECS health check sees 200 either way
- Body indicates status: "healthy" or "degraded", dynamodb: "reachable" or "unreachable"
- Minimal response: status + dynamodb + cache_entries only
- Export existing `isDegraded()` as public `IsDegraded()` method on CacheAuthenticator
- Bulk eviction via InvalidateAll (not Range + Delete loop -- see research finding below)
- Stats counters preserved (cumulative lifetime counters, not reset by flush)
- Circuit breaker NOT reset by bulk eviction
- Response: `{"data": {"evicted_count": N, "stats_reset": false}, "timestamp": "..."}`

### Claude's Discretion
- Exact Credential struct field naming for the Negative flag
- Otter v2 Range API usage details for listing and bulk delete
- How to set per-entry TTL (negative vs positive) when calling cache.Set
- Test strategy for negative caching behavior
- Error handling for Range iteration edge cases

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope

</user_constraints>

<phase_requirements>

## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| ADMIN-04 | `GET /cache/credentials` lists cached usernames with TTL remaining (no passwords) | Otter v2 `All()` iterator + `GetEntryQuietly()` for TTL; new `Entries()` method on Cache wrapper; new admin handler |
| ADMIN-05 | `DELETE /cache/credentials` flushes entire cache | Otter v2 `InvalidateAll()` natively supported; new `DeleteAll()` method on Cache wrapper; new admin handler |
| ADMIN-06 | `GET /health` returns 200 with DynamoDB connectivity status | Export existing `isDegraded()` as `IsDegraded()`; new admin handler returning circuit breaker state |

</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| maypok86/otter/v2 | v2.3.0 | In-memory cache with per-entry TTL, iteration, bulk invalidation | Already in use; provides all needed APIs natively |
| net/http (stdlib) | Go 1.22+ | Admin HTTP server with method routing | Already in use; Go 1.22+ ServeMux pattern matching |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| encoding/json (stdlib) | - | JSON response serialization | Already used by admin handlers |
| sort (stdlib) | - | Sort entries by TTL remaining | For listing endpoint response ordering |
| time (stdlib) | - | TTL calculation from ExpiresAtNano | For computing remaining TTL |
| log (stdlib) | - | Logging in admin and credcache packages | Established convention |

### Alternatives Considered
None -- all dependencies are already in the project.

## Architecture Patterns

### Recommended Changes Structure
```
internal/credcache/types.go      # Add Negative bool to Credential
internal/credcache/cache.go      # Add DeleteAll(), Entries(), SetWithTTL() methods
internal/credcache/auth.go       # Add IsDegraded(), update Verify() for negative caching
internal/admin/server.go         # Add 3 new route handlers
pkg/config/config.go             # Add NegativeTTLSecs to CredCacheConfig
```

### Pattern 1: Per-Entry TTL via SetExpiresAfter
**What:** Otter v2 supports per-entry TTL override after Set. The global ExpiryCalculator sets the default (900s), then `SetExpiresAfter()` overrides specific entries to shorter durations.
**When to use:** Negative cache entries that need 60s TTL instead of the default 900s.
**Example:**
```go
// Source: Otter v2.3.0 cache.go line 218
// After Set, override the TTL for this specific entry
c.inner.Set(username, cred)
c.inner.SetExpiresAfter(username, negativeTTL)
```

**Important:** `SetExpiresAfter` is called AFTER `Set`. The entry must exist in the cache first. This is a two-step operation that is safe for concurrent use.

### Pattern 2: Cache Entry Listing via All() Iterator
**What:** Otter v2.3.0 provides `All() iter.Seq2[K, V]` for iterating all entries AND `GetEntryQuietly(key)` to get Entry metadata (including ExpiresAtNano) without side effects.
**When to use:** Building the cache listing endpoint.
**Example:**
```go
// Source: Otter v2.3.0 cache.go lines 359-361, 114-116
// All() iterates key-value pairs; GetEntryQuietly gets TTL metadata
type CacheEntry struct {
    Username     string
    TTLRemaining int
    Negative     bool
}

var entries []CacheEntry
for username, cred := range c.inner.All() {
    entry, ok := c.inner.GetEntryQuietly(username)
    if !ok {
        continue // entry evicted between iteration and lookup
    }
    ttlRemaining := int(time.Until(entry.ExpiresAt()).Seconds())
    if ttlRemaining < 0 {
        ttlRemaining = 0
    }
    entries = append(entries, CacheEntry{
        Username:     username,
        TTLRemaining: ttlRemaining,
        Negative:     cred.Negative,
    })
}
// Sort by TTL ascending
sort.Slice(entries, func(i, j int) bool {
    return entries[i].TTLRemaining < entries[j].TTLRemaining
})
```

**Note on CONTEXT.md correction:** The CONTEXT.md mentions "Range with ExpiresAt()" but Otter v2.3.0 uses `All()` (Go 1.23 iter.Seq2 pattern), not a Range method on Cache. The `Range` method exists on the internal hashmap only. The public API is `All()`, `Keys()`, `Values()`, `GetEntry()`, `GetEntryQuietly()`.

### Pattern 3: Bulk Invalidation via InvalidateAll
**What:** Otter v2.3.0 provides `InvalidateAll()` which iterates all entries internally and invalidates each. This is simpler than manual Range+Delete.
**When to use:** Flush endpoint.
**Example:**
```go
// Source: Otter v2.3.0 cache.go line 385-387
// Get count before flush, then invalidate
count := c.inner.EstimatedSize()
c.inner.InvalidateAll()
return count
```

### Pattern 4: Negative Cache Entry in Verify Flow
**What:** When DynamoDB returns ErrNotFound, store a negative Credential with `Negative: true` and short TTL. On cache hit, check the Negative flag before comparing passwords.
**When to use:** Updating the CacheAuthenticator.Verify() and fetchWithSingleflight() methods.
**Example:**
```go
// In fetchWithSingleflight, after ErrNotFound:
if errors.Is(err, ErrNotFound) {
    negCred := &Credential{Username: username, Negative: true}
    a.cache.Set(username, negCred)
    a.cache.SetExpiresAfter(username, a.negativeTTL)
    return nil, nil
}

// In Verify, after cache hit:
cred, ok := a.cache.Get(username)
if ok {
    if cred.Negative {
        return false, nil // cached negative result
    }
    return comparePassword(cred.Password, password), nil
}
```

### Anti-Patterns to Avoid
- **Custom ExpiryCalculator replacement:** Do NOT replace the global ExpiryWriting calculator with a custom one that inspects the Credential value. Use `SetExpiresAfter()` per-entry instead -- simpler and the ExpiryCalculator interface's `ExpireAfterCreate` receives the entry before the value is fully initialized.
- **Storing negative entries in a separate map:** Violates the decision to use the same Otter cache. Separate maps would not benefit from W-TinyLFU eviction.
- **Calling GetEntry instead of GetEntryQuietly for listing:** `GetEntry` updates access statistics and eviction policy, inflating hit counts and distorting cache behavior. Use `GetEntryQuietly` for read-only introspection.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Per-entry TTL | Custom expiry map or wrapper | `cache.SetExpiresAfter(key, duration)` | Otter v2 handles it atomically and thread-safely |
| Bulk invalidation | Range + Delete loop | `cache.InvalidateAll()` | Built-in, handles concurrent modification safely |
| Entry iteration | hashmap.Range (internal API) | `cache.All()` (public iter.Seq2) | Public, stable API; weakly consistent for concurrent use |
| TTL remaining | Track insertion time manually | `Entry.ExpiresAt()` via `GetEntryQuietly()` | Otter tracks per-entry expiration natively |

**Key insight:** Otter v2.3.0 provides all needed primitives natively. Every operation in this phase maps to a single Otter method call.

## Common Pitfalls

### Pitfall 1: SetExpiresAfter Requires Existing Entry
**What goes wrong:** Calling `SetExpiresAfter` before `Set` does nothing (entry doesn't exist yet).
**Why it happens:** Two-step operation -- Set creates the entry, SetExpiresAfter overrides its TTL.
**How to avoid:** Always call `Set` first, then `SetExpiresAfter`. The Cache wrapper method should encapsulate both steps.
**Warning signs:** Negative entries expiring at the default TTL (900s) instead of 60s.

### Pitfall 2: GetEntry vs GetEntryQuietly for Listing
**What goes wrong:** Using `GetEntry` in the listing endpoint counts every listed entry as a cache "hit", inflating hit rate statistics.
**Why it happens:** `GetEntry` is the normal read path with side effects. `GetEntryQuietly` skips stats recording and eviction policy updates.
**How to avoid:** Always use `GetEntryQuietly` for admin introspection endpoints.
**Warning signs:** Hit rate jumps every time the listing endpoint is called.

### Pitfall 3: Route Conflict Between Single-Entry and Bulk Operations
**What goes wrong:** `DELETE /cache/credentials` (bulk flush) conflicts with `DELETE /cache/credentials/{username}` (single evict) if routing is ambiguous.
**Why it happens:** Go 1.22+ ServeMux handles this correctly -- exact path `/cache/credentials` matches before wildcard `/cache/credentials/{username}`. BUT only if the exact path is registered.
**How to avoid:** Register `DELETE /cache/credentials` (exact, no trailing slash) for bulk flush. The existing `DELETE /cache/credentials/{username}` pattern requires a non-empty `{username}` segment. Go 1.22+ ServeMux routes exact matches before wildcard patterns.
**Warning signs:** Bulk flush returning single-entry evict response format, or 404 for the bulk path.

### Pitfall 4: Negative Entry Password Comparison
**What goes wrong:** Negative entries have empty Password field. If Verify doesn't check Negative flag first, `comparePassword("", rawPassword)` would return false anyway, but wastes CPU on hex encoding + constant-time compare.
**Why it happens:** Forgetting to check the Negative flag before the password comparison path.
**How to avoid:** Check `cred.Negative` immediately after cache hit, before `comparePassword`. Return `(false, nil)` for negative hits.
**Warning signs:** No functional bug, but unnecessary computation.

### Pitfall 5: EstimatedSize for evicted_count Is Approximate
**What goes wrong:** `EstimatedSize()` may not reflect the exact count of entries that will be invalidated.
**Why it happens:** Otter's `EstimatedSize` is approximate due to concurrent operations and pending evictions.
**How to avoid:** Accept this is an approximation. Document in response that `evicted_count` is approximate. Alternatively, count entries during iteration, but `InvalidateAll` doesn't return a count.
**Warning signs:** `evicted_count` occasionally off by 1-2 entries.

### Pitfall 6: Negative Entry Stored After Singleflight Returns nil
**What goes wrong:** Current code returns `(nil, nil)` for ErrNotFound without caching anything. Multiple concurrent requests for the same unknown user each trigger a DynamoDB call (singleflight only deduplicates in-flight, not cached results).
**Why it happens:** The whole point of negative caching is to prevent this. The fix is to cache the negative result inside the singleflight callback before returning.
**How to avoid:** Store the negative entry inside the singleflight `Do` callback, so subsequent requests find it in cache.
**Warning signs:** DynamoDB call count for unknown usernames not decreasing after the first lookup.

## Code Examples

### New Cache Methods

```go
// SetWithTTL stores a credential with a specific TTL override.
// Source: Otter v2.3.0 cache.go lines 123, 218
func (c *Cache) SetWithTTL(username string, cred *Credential, ttl time.Duration) {
    c.inner.Set(username, cred)
    c.inner.SetExpiresAfter(username, ttl)
}

// DeleteAll invalidates all entries and returns the approximate count evicted.
// Source: Otter v2.3.0 cache.go lines 385-387, 413
func (c *Cache) DeleteAll() int {
    count := c.inner.EstimatedSize()
    c.inner.InvalidateAll()
    return count
}

// CacheEntry represents a single cache entry for the listing endpoint.
type CacheEntry struct {
    Username     string `json:"username"`
    TTLRemaining int    `json:"ttl_remaining"`
    Negative     bool   `json:"negative"`
}

// Entries returns all cache entries with TTL metadata, sorted by TTL ascending.
// Source: Otter v2.3.0 cache.go lines 106-116, 359-361
func (c *Cache) Entries() []CacheEntry {
    var entries []CacheEntry
    for username, cred := range c.inner.All() {
        entry, ok := c.inner.GetEntryQuietly(username)
        if !ok {
            continue
        }
        ttl := int(time.Until(entry.ExpiresAt()).Seconds())
        if ttl < 0 {
            ttl = 0
        }
        entries = append(entries, CacheEntry{
            Username:     username,
            TTLRemaining: ttl,
            Negative:     cred.Negative,
        })
    }
    sort.Slice(entries, func(i, j int) bool {
        return entries[i].TTLRemaining < entries[j].TTLRemaining
    })
    return entries
}
```

### Updated Verify Flow with Negative Caching

```go
// In CacheAuthenticator.Verify:
cred, ok := a.cache.Get(username)
if ok {
    if cred.Negative {
        return false, nil // cached negative -- no DynamoDB call
    }
    return comparePassword(cred.Password, password), nil
}

// In fetchWithSingleflight, update ErrNotFound handling:
if errors.Is(err, ErrNotFound) {
    negCred := &Credential{Username: username, Negative: true}
    a.cache.SetWithTTL(username, negCred, a.negativeTTL)
    return nil, nil
}
```

### New Admin Handlers

```go
// handleListCredentials returns all cached entries with TTL.
func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
    entries := s.cache.Entries()
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "count":   len(entries),
        "entries": entries,
    })
}

// handleFlushCredentials evicts all cache entries.
func (s *Server) handleFlushCredentials(w http.ResponseWriter, r *http.Request) {
    count := s.cache.DeleteAll()
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "evicted_count": count,
        "stats_reset":   false,
    })
}

// handleHealth returns service health status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    degraded := s.auth.IsDegraded()
    status := "healthy"
    dynamo := "reachable"
    if degraded {
        status = "degraded"
        dynamo = "unreachable"
    }
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "status":        status,
        "dynamodb":      dynamo,
        "cache_entries": s.cache.Size(),
    })
}

// Route registration in Handler():
mux.HandleFunc("GET /cache/credentials", s.handleListCredentials)
mux.HandleFunc("DELETE /cache/credentials", s.handleFlushCredentials)
mux.HandleFunc("GET /health", s.handleHealth)
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Otter v1 Range() callback | Otter v2 All() iter.Seq2 | v2.0.0 (2024) | Uses Go 1.23 range-over-func; cleaner iteration |
| No per-entry TTL override | SetExpiresAfter() | v2.0.0 (2024) | Eliminates need for custom ExpiryCalculator hacks |
| GetEntry for all reads | GetEntryQuietly for introspection | v2.0.0 (2024) | Admin reads don't pollute cache statistics |

## Open Questions

1. **Go 1.23 iter.Seq2 requirement**
   - What we know: Otter v2.3.0's `All()` returns `iter.Seq2[K, V]` which requires Go 1.23+ range-over-func support
   - What's unclear: Whether the project's go.mod specifies Go 1.23+
   - Recommendation: Verify `go.mod` requires Go 1.23+. If not, the `All()` iterator is still usable but with explicit `next` calls instead of range syntax. Check at implementation time.

2. **SetExpiresAfter atomicity with Set**
   - What we know: `Set` and `SetExpiresAfter` are two separate calls. Between them, a concurrent `Get` could see the entry with the default TTL.
   - What's unclear: Whether this race matters in practice (the window is nanoseconds).
   - Recommendation: Accept the tiny race. The worst case is one request sees a negative entry with the longer TTL, which self-corrects on expiry.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | None -- Go's built-in test runner |
| Quick run command | `go test ./internal/credcache/ ./internal/admin/ -count=1 -v` |
| Full suite command | `go test ./... -count=1` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| ADMIN-04 | GET /cache/credentials returns entries with TTL, no passwords | unit | `go test ./internal/admin/ -run TestListCredentials -count=1 -v` | No -- Wave 0 |
| ADMIN-04 | Entries sorted by TTL ascending | unit | `go test ./internal/credcache/ -run TestEntries_SortedByTTL -count=1 -v` | No -- Wave 0 |
| ADMIN-04 | Negative entries visible with negative:true flag | unit | `go test ./internal/admin/ -run TestListCredentials_WithNegative -count=1 -v` | No -- Wave 0 |
| ADMIN-05 | DELETE /cache/credentials flushes all entries | unit | `go test ./internal/admin/ -run TestFlushCredentials -count=1 -v` | No -- Wave 0 |
| ADMIN-05 | Subsequent GETs trigger fresh DynamoDB lookups after flush | unit | `go test ./internal/credcache/ -run TestDeleteAll -count=1 -v` | No -- Wave 0 |
| ADMIN-06 | GET /health returns 200 with healthy status | unit | `go test ./internal/admin/ -run TestHealth_Healthy -count=1 -v` | No -- Wave 0 |
| ADMIN-06 | GET /health returns 200 with degraded status when CB open | unit | `go test ./internal/admin/ -run TestHealth_Degraded -count=1 -v` | No -- Wave 0 |
| NEG-01 | Negative entries cached with short TTL on ErrNotFound | unit | `go test ./internal/credcache/ -run TestVerify_NegativeCache -count=1 -v` | No -- Wave 0 |
| NEG-02 | Repeated unknown username lookups don't call DynamoDB after first | unit | `go test ./internal/credcache/ -run TestNegativeCache_PreventsRepeatedDynamoCalls -count=1 -v` | No -- Wave 0 |
| NEG-03 | Negative entries expire and allow fresh lookup | unit | `go test ./internal/credcache/ -run TestNegativeCache_Expiry -count=1 -v` | No -- Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/credcache/ ./internal/admin/ -count=1 -v`
- **Per wave merge:** `go test ./... -count=1`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/credcache/cache_test.go` -- add tests for SetWithTTL, DeleteAll, Entries methods
- [ ] `internal/credcache/auth_test.go` -- add tests for negative caching in Verify, IsDegraded public method
- [ ] `internal/admin/server_test.go` -- add tests for handleListCredentials, handleFlushCredentials, handleHealth

## Sources

### Primary (HIGH confidence)
- Otter v2.3.0 source code (local: `$GOMODCACHE/github.com/maypok86/otter/v2@v2.3.0/`) -- verified All(), InvalidateAll(), SetExpiresAfter(), GetEntryQuietly(), Entry.ExpiresAt() APIs
- Otter v2.3.0 `entry.go` -- verified Entry struct fields: ExpiresAtNano, SnapshotAtNano, ExpiresAt() method
- Otter v2.3.0 `expiry_calculator.go` -- verified ExpiryWriting, ExpiryCreating, ExpiryCreatingFunc patterns
- Project source: `internal/credcache/cache.go`, `auth.go`, `types.go` -- current implementation verified
- Project source: `internal/admin/server.go` -- current handler patterns and helpers verified

### Secondary (MEDIUM confidence)
- None needed -- all findings verified against local source code

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in use, APIs verified in local source
- Architecture: HIGH -- extending established patterns, Otter v2 APIs verified line-by-line
- Pitfalls: HIGH -- derived from actual API constraints observed in source code

**Research date:** 2026-03-11
**Valid until:** 2026-04-11 (stable -- Otter v2.3.0 pinned in go.mod)
