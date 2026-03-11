---
phase: 04-operational-hardening
plan: 01
subsystem: auth
tags: [credcache, negative-caching, circuit-breaker, otter]

# Dependency graph
requires:
  - phase: 02-cache-auth
    provides: Cache, CacheAuthenticator, singleflight, circuit breaker
provides:
  - Credential.Negative field for negative cache entries
  - Cache.SetWithTTL, DeleteAll, Entries methods for admin API
  - CacheEntry struct for admin listing
  - CacheAuthenticator.IsDegraded() exported method
  - Negative caching on ErrNotFound with configurable TTL
  - NegativeTTLSecs config field
affects: [04-02 admin endpoints, health endpoint]

# Tech tracking
tech-stack:
  added: []
  patterns: [negative caching via Credential.Negative flag, SetWithTTL two-step otter API, GetEntryQuietly for stats-safe iteration]

key-files:
  created: []
  modified:
    - internal/credcache/types.go
    - internal/credcache/cache.go
    - internal/credcache/cache_test.go
    - internal/credcache/auth.go
    - internal/credcache/auth_test.go
    - pkg/config/config.go

key-decisions:
  - "Otter v2 ExpiresAtNano (int64) used for TTL calculation instead of ExpiresAt() method"
  - "Negative entries stored inside singleflight callback for concurrent request deduplication"
  - "GetEntryQuietly used in Entries() to avoid inflating hit stats"

patterns-established:
  - "Negative caching: Credential.Negative flag with SetWithTTL for short-lived rejection cache"
  - "Admin iteration: GetEntryQuietly + ExpiresAtNano for stats-safe entry listing"

requirements-completed: [ADMIN-04, ADMIN-05, ADMIN-06]

# Metrics
duration: 3min
completed: 2026-03-11
---

# Phase 4 Plan 01: Credential Cache Extensions Summary

**Negative caching with 60s TTL on unknown users, cache list/flush primitives, and exported IsDegraded for admin health**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-11T13:15:44Z
- **Completed:** 2026-03-11T13:18:48Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Credential struct extended with Negative bool for marking rejected-user cache entries
- Cache gains SetWithTTL, DeleteAll, Entries methods -- foundation for admin list/flush endpoints
- Verify short-circuits on negative cache hits without password comparison or DynamoDB call
- IsDegraded exported for admin health endpoint visibility
- NegativeTTLSecs config field (default 60) added to CredCacheConfig
- 11 new tests added, all 40 credcache tests pass

## Task Commits

Each task was committed atomically:

1. **Task 1: Extend credcache types, Cache methods, and config**
   - `6a3fcb7` (test) - failing tests for SetWithTTL, DeleteAll, Entries
   - `ffdd0cd` (feat) - Negative field, SetWithTTL/DeleteAll/Entries, NegativeTTLSecs

2. **Task 2: Negative caching in Verify and IsDegraded export**
   - `8ac5d15` (test) - failing tests for negative caching and IsDegraded
   - `d82d6ec` (feat) - negative caching in Verify, export IsDegraded

_TDD: each task has RED (test) then GREEN (feat) commits._

## Files Created/Modified
- `internal/credcache/types.go` - Added Negative bool field to Credential struct
- `internal/credcache/cache.go` - Added CacheEntry struct, SetWithTTL, DeleteAll, Entries methods
- `internal/credcache/cache_test.go` - 7 new tests for cache extension methods
- `internal/credcache/auth.go` - negativeTTL field, WithNegativeTTL option, negative caching in Verify, exported IsDegraded
- `internal/credcache/auth_test.go` - 4 new tests for negative caching and IsDegraded
- `pkg/config/config.go` - NegativeTTLSecs field added to CredCacheConfig

## Decisions Made
- Used Otter v2 ExpiresAtNano (int64 nanoseconds) for TTL calculation -- ExpiresAt() method does not exist in v2
- Negative entries stored inside singleflight callback to ensure concurrent requests for the same unknown user all see the cached negative result
- GetEntryQuietly used in Entries() iteration to avoid inflating cache hit stats during admin listing

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Corrected Otter v2 Entry API usage**
- **Found during:** Task 1 (Entries implementation)
- **Issue:** Plan referenced `entry.ExpiresAt()` method, but Otter v2 uses `entry.ExpiresAtNano` int64 field
- **Fix:** Used `time.Unix(0, entry.ExpiresAtNano)` for TTL calculation
- **Files modified:** internal/credcache/cache.go
- **Verification:** TestEntries_ReturnsAllWithTTL passes with correct TTL values
- **Committed in:** ffdd0cd

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Minor API surface correction. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All cache primitives ready for admin endpoint wiring (list, flush, health)
- IsDegraded exported for health endpoint
- Negative caching active -- brute-force protection for unknown users

---
*Phase: 04-operational-hardening*
*Completed: 2026-03-11*
