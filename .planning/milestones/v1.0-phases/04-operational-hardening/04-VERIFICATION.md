---
phase: 04-operational-hardening
verified: 2026-03-11T00:00:00Z
status: passed
score: 9/9 must-haves verified
re_verification: false
gaps: []
human_verification: []
---

# Phase 4: Operational Hardening Verification Report

**Phase Goal:** The proxy handles brute-force attempts without DynamoDB cost spikes, operators can inspect and bulk-clear the cache during incidents, and the ECS health check has a real endpoint to target.
**Verified:** 2026-03-11
**Status:** passed
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| #  | Truth                                                                                   | Status     | Evidence                                                                                          |
|----|-----------------------------------------------------------------------------------------|------------|---------------------------------------------------------------------------------------------------|
| 1  | Cache stores negative entries with short TTL when DynamoDB returns ErrNotFound          | VERIFIED   | `auth.go:116-120` — singleflight callback stores `Credential{Negative:true}` via `SetWithTTL`    |
| 2  | Negative cache hits reject auth without calling DynamoDB                                | VERIFIED   | `auth.go:87-90` — `cred.Negative` checked before `comparePassword`; store not called             |
| 3  | Cache can list all entries with TTL remaining and negative flag                         | VERIFIED   | `cache.go:103-128` — `Entries()` iterates via `GetEntryQuietly`, builds `CacheEntry` slice       |
| 4  | Cache can flush all entries in one call                                                 | VERIFIED   | `cache.go:95-99` — `DeleteAll()` calls `InvalidateAll()` and returns approximate count           |
| 5  | CacheAuthenticator exposes circuit breaker state via IsDegraded()                       | VERIFIED   | `auth.go:155-163` — `IsDegraded()` is exported, reads `consecutiveFailures` atomically           |
| 6  | GET /cache/credentials returns entries with username, ttl_remaining, negative (no pwd) | VERIFIED   | `server.go:87-93` — calls `s.cache.Entries()`; `CacheEntry` struct has no password/usertype      |
| 7  | DELETE /cache/credentials flushes entire cache and reports evicted count               | VERIFIED   | `server.go:97-103` — calls `s.cache.DeleteAll()`, returns `evicted_count` + `stats_reset:false`  |
| 8  | GET /health returns 200 with healthy/degraded status and DynamoDB reachable/unreachable | VERIFIED   | `server.go:107-119` — always HTTP 200, checks `s.auth.IsDegraded()` for status string            |
| 9  | Repeated CONNECT attempts with unknown usernames do not cause unbounded DynamoDB calls  | VERIFIED   | Negative entries cached inside singleflight callback; subsequent calls short-circuit at cache hit |

**Score:** 9/9 truths verified

---

## Required Artifacts

### Plan 01 Artifacts

| Artifact                             | Expected                                              | Status     | Details                                                                       |
|--------------------------------------|-------------------------------------------------------|------------|-------------------------------------------------------------------------------|
| `internal/credcache/types.go`        | Credential struct with Negative bool field            | VERIFIED   | Line 16: `Negative bool` — no dynamodbav tag, comment explains intent        |
| `internal/credcache/cache.go`        | SetWithTTL, DeleteAll, Entries methods + CacheEntry   | VERIFIED   | Lines 82-128: all four declared and substantive (no stubs)                    |
| `internal/credcache/auth.go`         | IsDegraded() public method, negative caching in Verify | VERIFIED  | Lines 79-103 (Verify), 155-163 (IsDegraded) — both substantive               |
| `pkg/config/config.go`               | NegativeTTLSecs config field                          | VERIFIED   | Line 59: `NegativeTTLSecs int \`default:"60"\``                               |

### Plan 02 Artifacts

| Artifact                             | Expected                                                           | Status     | Details                                                           |
|--------------------------------------|--------------------------------------------------------------------|------------|-------------------------------------------------------------------|
| `internal/admin/server.go`           | handleListCredentials, handleFlushCredentials, handleHealth + routes | VERIFIED | Lines 37-43 (routes), 87-119 (handlers) — all three implemented  |
| `internal/admin/server_test.go`      | Tests for all 3 new endpoints                                      | VERIFIED   | TestListCredentials through TestRouteDisambiguation — 10 tests    |

---

## Key Link Verification

### Plan 01 Key Links

| From                          | To                            | Via                               | Pattern                       | Status   | Details                                                                  |
|-------------------------------|-------------------------------|-----------------------------------|-------------------------------|----------|--------------------------------------------------------------------------|
| `internal/credcache/auth.go`  | `internal/credcache/cache.go` | SetWithTTL for negative entries   | `cache\.SetWithTTL`           | WIRED    | `auth.go:119` calls `a.cache.SetWithTTL(username, negCred, a.negativeTTL)` |
| `internal/credcache/auth.go`  | `internal/credcache/types.go` | Credential.Negative flag in Verify | `cred\.Negative`             | WIRED    | `auth.go:88` checks `cred.Negative` before password comparison            |

### Plan 02 Key Links

| From                          | To                            | Via                               | Pattern                           | Status   | Details                                                              |
|-------------------------------|-------------------------------|-----------------------------------|-----------------------------------|----------|----------------------------------------------------------------------|
| `internal/admin/server.go`    | `internal/credcache/cache.go` | Entries() and DeleteAll() calls   | `s\.cache\.(Entries\|DeleteAll)` | WIRED    | `server.go:88` `s.cache.Entries()`, `server.go:98` `s.cache.DeleteAll()` |
| `internal/admin/server.go`    | `internal/credcache/auth.go`  | IsDegraded() for health check     | `s\.auth\.IsDegraded`            | WIRED    | `server.go:110` calls `s.auth.IsDegraded()`                          |

---

## Requirements Coverage

| Requirement | Source Plan  | Description                                                         | Status    | Evidence                                                              |
|-------------|--------------|---------------------------------------------------------------------|-----------|-----------------------------------------------------------------------|
| ADMIN-04    | 04-01, 04-02 | `GET /cache/credentials` lists cached usernames with TTL (no pwds) | SATISFIED | Route registered at `server.go:37`; handler returns `CacheEntry` fields without password/usertype |
| ADMIN-05    | 04-01, 04-02 | `DELETE /cache/credentials` flushes entire cache                   | SATISFIED | Route registered at `server.go:38`; `handleFlushCredentials` calls `DeleteAll()` |
| ADMIN-06    | 04-01, 04-02 | `GET /health` returns 200 with DynamoDB connectivity status        | SATISFIED | Route registered at `server.go:43`; always returns HTTP 200; reports healthy/degraded |

No orphaned requirements — REQUIREMENTS.md Traceability table maps ADMIN-04, ADMIN-05, ADMIN-06 to Phase 4 and all three are now implemented.

Note: The plans also claim credit for negative caching which satisfies the v2 requirement OPS-01 ("Negative caching for failed lookups with shorter TTL to prevent DynamoDB cost spikes"). This was delivered ahead of schedule and is a bonus — OPS-01 is marked v2/deferred in REQUIREMENTS.md and was not a contracted Phase 4 requirement.

---

## Anti-Patterns Found

No anti-patterns detected. Scan of all phase 4 modified files:

- `internal/credcache/types.go` — no TODOs, no stubs, no empty returns
- `internal/credcache/cache.go` — all three new methods have substantive implementations
- `internal/credcache/auth.go` — `IsDegraded()` is substantive; negative caching path wired through singleflight
- `pkg/config/config.go` — field added with correct default
- `internal/admin/server.go` — all three handlers return real data from cache/auth; no static returns
- `internal/admin/server_test.go` — 10 new tests with real assertions; no placeholder tests

---

## Test Verification

Tests executed and confirmed passing:

```
ok  github.com/whereiskurt/meshtk/internal/credcache  2.523s  (41 tests)
ok  github.com/whereiskurt/meshtk/internal/admin       0.466s  (17 tests)
go vet ./...  (clean — no output)
```

**credcache new tests (Plan 01):**
- TestSetWithTTL, TestDeleteAll, TestDeleteAll_ReturnsCount
- TestEntries_ReturnsAllWithTTL, TestEntries_SortedByTTL, TestEntries_IncludesNegativeFlag, TestEntries_Empty
- TestVerify_NegativeCache_RejectsWithoutDynamoCall, TestNegativeCache_StoredOnErrNotFound
- TestNegativeCache_DoesNotAffectValidEntries, TestIsDegraded_ExportedMethod

**admin new tests (Plan 02):**
- TestListCredentials_ReturnsEntries, TestListCredentials_NoPasswords
- TestListCredentials_WithNegativeEntry, TestListCredentials_SortedByTTL, TestListCredentials_Empty
- TestFlushCredentials_ClearsAll, TestFlushCredentials_EmptyCache
- TestHealth_Healthy, TestHealth_Degraded, TestRouteDisambiguation

---

## Human Verification Required

None. All observable behaviors can be verified programmatically through the test suite.

The health endpoint's "always returns HTTP 200" decision is confirmed by the test (`TestHealth_Degraded` asserts `w.Code == http.StatusOK` even when degraded) and by the comment in the handler.

---

## Summary

Phase 4 goal is fully achieved. All three pillars are in place:

1. **Brute-force protection without DynamoDB cost spikes** — negative entries are cached inside the singleflight callback (so concurrent requests for the same unknown username share one DynamoDB miss and all see the cached negative result). The 60-second negative TTL is configurable via `NegativeTTLSecs`.

2. **Operator tooling for cache inspection and bulk-clear** — `GET /cache/credentials` exposes entries with username, TTL, and negative flag (no passwords); `DELETE /cache/credentials` performs a one-call flush with approximate evicted count. Route disambiguation between bulk-flush and single-entry evict is verified by test.

3. **Real health endpoint for ECS** — `GET /health` always returns HTTP 200 (ECS policy: don't kill task during DynamoDB outages), with `status` and `dynamodb` fields reflecting the circuit breaker state from `IsDegraded()`.

All 9 must-have truths are VERIFIED. All 6 key links are WIRED. Requirements ADMIN-04, ADMIN-05, ADMIN-06 are SATISFIED. No gaps.

---

_Verified: 2026-03-11_
_Verifier: Claude (gsd-verifier)_
