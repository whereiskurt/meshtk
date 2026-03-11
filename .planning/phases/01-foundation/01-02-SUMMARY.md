---
phase: 01-foundation
plan: 02
subsystem: cache
tags: [otter, dynamodb, aws-sdk-go-v2, credential-cache, in-memory-cache]

# Dependency graph
requires: []
provides:
  - "Credential struct and CredentialStore interface"
  - "Otter v2 cache wrapper with TTL, stats, Get/Set/Delete"
  - "DynamoDBStore adapter with Scan + FilterExpression + pagination"
  - "ErrNotFound sentinel error"
affects: [02-proxy-integration]

# Tech tracking
tech-stack:
  added: [otter/v2@v2.3.0, aws-sdk-go-v2/service/dynamodb, aws-sdk-go-v2/config, aws-sdk-go-v2/feature/dynamodb/expression, aws-sdk-go-v2/feature/dynamodb/attributevalue]
  patterns: [CredentialStore interface for backend swappability, mock DynamoDB client for testing, Otter v2 Options struct cache creation]

key-files:
  created: [internal/credcache/types.go, internal/credcache/cache.go, internal/credcache/store.go, internal/credcache/cache_test.go, internal/credcache/store_test.go]
  modified: [go.mod, go.sum]

key-decisions:
  - "Used MaximumSize (entry count) instead of MaximumWeight for cache sizing -- simpler, credentials are uniform small size"
  - "Used Otter ExpiryWriting calculator for TTL -- resets on write, not read"
  - "DynamoDBStore.Fetch implements pagination via LastEvaluatedKey loop for safety with large tables"
  - "DynamoDBAPI interface enables mock testing without real AWS connection"

patterns-established:
  - "CredentialStore interface: small interface with Fetch(ctx, username) for backend swappability"
  - "Mock DynamoDB client: struct implementing DynamoDBAPI with captured ScanInput for assertion"
  - "Cache wrapper pattern: thin typed wrapper over otter.Cache with CacheStats export"

requirements-completed: [CRED-02, CRED-03]

# Metrics
duration: 3min
completed: 2026-03-11
---

# Phase 1 Plan 2: Credential Cache Package Summary

**Otter v2 in-memory cache with TTL/stats and DynamoDB store adapter using Scan with FilterExpression on mqttUsername**

## Performance

- **Duration:** 3 min
- **Started:** 2026-03-11T01:28:33Z
- **Completed:** 2026-03-11T01:31:58Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Credential struct with dynamodbav tags for mqttUsername, mqttPassword, mqttUsertype
- Otter v2 cache wrapper with configurable TTL expiry, max size, and stats recording
- DynamoDB store adapter with FilterExpression, ProjectionExpression, and pagination
- 13 unit tests covering cache hit/miss/TTL/stats/delete and store fetch/notfound/error/expressions

## Task Commits

Each task was committed atomically:

1. **Task 1: Create types.go and cache.go with Otter v2 wrapper and unit tests**
   - `af81309` (test: failing cache tests - RED)
   - `b71b124` (feat: cache implementation - GREEN)
2. **Task 2: Create DynamoDB store adapter with mock-based unit tests**
   - `bcd28d5` (test: failing store tests - RED)
   - `644faf2` (feat: store implementation - GREEN)

## Files Created/Modified
- `internal/credcache/types.go` - Credential struct, CredentialStore interface, ErrNotFound sentinel
- `internal/credcache/cache.go` - Otter v2 cache wrapper with Get/Set/Delete/Stats/Close
- `internal/credcache/cache_test.go` - 8 unit tests for cache behavior including TTL expiry
- `internal/credcache/store.go` - DynamoDBStore implementing CredentialStore with Scan
- `internal/credcache/store_test.go` - 5 unit tests with mock DynamoDB client
- `go.mod` - Added otter/v2 and aws-sdk-go-v2 dependencies
- `go.sum` - Updated checksums

## Decisions Made
- Used MaximumSize (entry count = maxSizeMB * 10000) instead of MaximumWeight -- credentials are small uniform structs, approximate sizing is sufficient
- Used ExpiryWriting calculator -- TTL resets on write operations, appropriate for credential cache refreshes
- DynamoDBStore.Fetch paginates via LastEvaluatedKey loop for safety with large tables
- DynamoDBAPI interface with just Scan method enables mock testing without real AWS
- Cache.Delete wraps otter.Invalidate (v2 API renamed Delete to Invalidate)
- Cache.Close wraps StopAllGoroutines (v2 API for cleanup)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- Vendor directory required `go mod vendor` sync after adding new dependencies -- standard Go vendoring behavior, resolved immediately

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- credcache package complete with types, cache, and store
- Ready for Phase 2 to wire into proxy CONNECT path via Authenticator
- DynamoDBStore can be tested against local DynamoDB on port 8080 for integration testing

## Self-Check: PASSED

- All 5 source/test files exist
- All 4 task commits verified (af81309, b71b124, bcd28d5, 644faf2)
- 13/13 tests pass
- Project builds cleanly
- No vet warnings

---
*Phase: 01-foundation*
*Completed: 2026-03-11*
