# Roadmap: MeshTK MQTT Proxy Credential Cache

## Overview

This milestone adds DynamoDB-backed credential caching to the existing MeshTK MQTT proxy. The work decomposes into four natural delivery boundaries driven by hard dependencies: configuration and data structures must exist before the Authenticator, the Authenticator must be in the proxy before the admin API is meaningful to test end-to-end, and operational hardening builds on all prior phases. Each phase delivers an independently verifiable capability.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

- [ ] **Phase 1: Foundation** - Config schema, in-memory cache data structure, and DynamoDB adapter with zero proxy behavior changes
- [ ] **Phase 2: Authenticator and Proxy Integration** - Wire credential validation into the proxy CONNECT path with proper CONNACK rejection and credential swap
- [ ] **Phase 3: Admin API** - HTTP server for cache eviction, refresh, and stats (security control + operational visibility)
- [ ] **Phase 4: Operational Hardening** - Negative caching, cache inspection, bulk eviction, and health check endpoint

## Phase Details

### Phase 1: Foundation
**Goal**: The credential cache infrastructure exists and is tested in isolation — config schema covers all new settings, the in-memory cache manages TTL and stats correctly, and the DynamoDB adapter can fetch a credential from the live table.
**Depends on**: Nothing (first phase)
**Requirements**: CONF-01, CONF-02, CONF-03, CONF-04, CRED-02, CRED-03, AUTH-02, AUTH-05
**Success Criteria** (what must be TRUE):
  1. YAML config and env vars expose `CacheTTLSecs`, `CacheMaxSizeMB`, `AdminListenAddress`, `CredentialTableName`, `CredentialTableRegion`, generic Mosquitto creds, and passthrough allowlist — all with defaults
  2. The in-memory cache stores a credential, returns it on lookup, expires it after TTL, and reports accurate hit/miss counters — verified by unit tests
  3. The DynamoDB adapter fetches a credential from the live defcon.run table using the correct schema and returns it as a typed Go struct
  4. No existing proxy behavior changes — the binary starts, connects, and operates identically to before this phase
**Plans:** 2 plans
Plans:
- [ ] 01-01-PLAN.md — Config schema extension with CredCache, ProxyUsername/Password, AdminListenAddress + remove legacy generateMQTTPassword
- [ ] 01-02-PLAN.md — Credential cache package: types, Otter v2 cache wrapper, DynamoDB store adapter with unit tests

### Phase 2: Authenticator and Proxy Integration
**Goal**: Every MQTT CONNECT is validated against cached credentials — valid clients are transparently forwarded with generic Mosquitto creds, invalid clients receive a proper CONNACK 0x05 rejection, and passthrough usernames bypass validation entirely.
**Depends on**: Phase 1
**Requirements**: CRED-01, CRED-04, CRED-05, AUTH-01, AUTH-03, AUTH-04
**Success Criteria** (what must be TRUE):
  1. A Meshtastic client with valid credentials connects successfully and the proxy forwards it to Mosquitto using the configured generic username/password (not the client's credentials)
  2. A client with invalid or missing credentials receives a CONNACK with return code 0x05 and the connection is closed — Mosquitto never sees the attempt
  3. A client whose username is in the passthrough allowlist connects without credential validation, using its own credentials forwarded as-is
  4. On DynamoDB unavailability, clients with credentials already in cache connect successfully; clients not in cache receive a CONNACK rejection
  5. Concurrent CONNECT attempts for the same uncached username result in exactly one DynamoDB fetch (singleflight — no stampede)
**Plans:** 2 plans
Plans:
- [ ] 02-01-PLAN.md — CacheAuthenticator implementation with singleflight, circuit breaker, constant-time password comparison, and unit tests
- [ ] 02-02-PLAN.md — Proxy integration: Authenticator interface, inspect.go/proxy.go/cmd.go wiring, CONNACK 0x05 rejection, credential swap

### Phase 3: Admin API
**Goal**: An operator can evict specific cached credentials immediately, force a cache refresh, and inspect current cache stats — all via HTTP on a configurable local address.
**Depends on**: Phase 2
**Requirements**: ADMIN-01, ADMIN-02, ADMIN-03, ADMIN-07
**Success Criteria** (what must be TRUE):
  1. `DELETE /cache/credentials/{username}` removes the named entry from cache; subsequent CONNECTs for that username trigger a fresh DynamoDB lookup
  2. `POST /cache/credentials/{username}/refresh` fetches the credential from DynamoDB and updates the cache entry immediately
  3. `GET /cache/stats` returns current entry count, hit counter, miss counter, and hit rate as JSON
  4. The admin HTTP server binds only to the configured address (default localhost) and starts alongside the proxy without affecting MQTT throughput
**Plans**: TBD

### Phase 4: Operational Hardening
**Goal**: The proxy handles brute-force attempts without DynamoDB cost spikes, operators can inspect and bulk-clear the cache during incidents, and the ECS health check has a real endpoint to target.
**Depends on**: Phase 3
**Requirements**: ADMIN-04, ADMIN-05, ADMIN-06
**Success Criteria** (what must be TRUE):
  1. `GET /cache/credentials` returns a list of cached usernames with TTL remaining — no passwords exposed
  2. `DELETE /cache/credentials` (no username) flushes the entire cache; subsequent CONNECTs for all users trigger fresh DynamoDB lookups
  3. `GET /health` returns HTTP 200 with a JSON body indicating DynamoDB connectivity status (reachable / unreachable)
  4. Repeated CONNECT attempts with unknown usernames do not cause unbounded DynamoDB calls — negative results are cached with a short TTL and a bounded entry cap
**Plans**: TBD

## Progress

**Execution Order:**
Phases execute in numeric order: 1 → 2 → 3 → 4

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Foundation | 0/2 | Planning complete | - |
| 2. Authenticator and Proxy Integration | 0/2 | Planning complete | - |
| 3. Admin API | 0/TBD | Not started | - |
| 4. Operational Hardening | 0/TBD | Not started | - |
