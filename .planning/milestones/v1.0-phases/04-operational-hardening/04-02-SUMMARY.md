---
phase: 04-operational-hardening
plan: 02
subsystem: admin
tags: [admin-api, health-check, cache-management, endpoints]

# Dependency graph
requires:
  - phase: 04-operational-hardening
    plan: 01
    provides: Cache.Entries(), Cache.DeleteAll(), CacheAuthenticator.IsDegraded()
provides:
  - GET /cache/credentials endpoint (list cached entries)
  - DELETE /cache/credentials endpoint (bulk flush)
  - GET /health endpoint (service health with DynamoDB status)
affects:
  - internal/admin/server.go (new handlers + route registration)
  - internal/admin/server_test.go (10 new tests)

# Tech stack
tech-stack:
  added: []
  patterns: [Go 1.22+ ServeMux exact-path routing, TDD red-green]

# Key files
key-files:
  modified:
    - internal/admin/server.go
    - internal/admin/server_test.go

# Decisions
key-decisions:
  - "Bulk DELETE /cache/credentials registered before wildcard DELETE /cache/credentials/{username} -- Go 1.22+ exact match takes priority"
  - "Health endpoint always returns HTTP 200 -- ECS health check should not kill task during DynamoDB outages"
  - "Stats counters preserved on flush (stats_reset: false) -- cumulative lifetime counters per user decision"

# Metrics
metrics:
  duration: 2min
  completed: "2026-03-11T13:23:48Z"
---

# Phase 4 Plan 2: Admin HTTP Endpoints Summary

Three admin HTTP endpoints for cache listing, bulk flush, and health check using existing Cache and CacheAuthenticator methods from Plan 01.

## What Was Built

### Task 1: List, Flush, and Health Admin Endpoints (TDD)

**Commits:**
- `9eeeed0` test(04-02): add failing tests for list, flush, and health admin endpoints
- `4c40019` feat(04-02): implement list, flush, and health admin endpoints

**Endpoints added:**

| Method | Path | Purpose |
|--------|------|---------|
| GET | /cache/credentials | List all cached entries (username, ttl_remaining, negative flag -- no passwords) |
| DELETE | /cache/credentials | Bulk flush entire cache, returns evicted_count |
| GET | /health | Service health with DynamoDB reachability status |

**Tests added (10):**
- TestListCredentials_ReturnsEntries
- TestListCredentials_NoPasswords
- TestListCredentials_WithNegativeEntry
- TestListCredentials_SortedByTTL
- TestListCredentials_Empty
- TestFlushCredentials_ClearsAll
- TestFlushCredentials_EmptyCache
- TestHealth_Healthy
- TestHealth_Degraded
- TestRouteDisambiguation

## Deviations from Plan

None - plan executed exactly as written.

## Verification

- All 10 new tests pass
- All existing admin tests pass (evict, refresh, stats, content-type)
- All credcache tests pass
- Full `go test ./...` passes
- `go vet ./...` clean
