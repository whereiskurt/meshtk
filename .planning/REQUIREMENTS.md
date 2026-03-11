# Requirements: MeshTK Credential Cache

**Defined:** 2026-03-10
**Core Value:** Every MQTT CONNECT is validated against cached credentials with minimal latency — invalid clients are rejected before reaching the broker, valid clients are transparently forwarded with generic creds.

## v1 Requirements

Requirements for initial release. Each maps to roadmap phases.

### Credential Store

- [ ] **CRED-01**: Proxy looks up MQTT username/password in DynamoDB using defcon.run schema
- [x] **CRED-02**: Credential lookups are cached in-memory using Otter v2 (or equivalent) with configurable max memory size
- [x] **CRED-03**: Cached entries expire automatically after configurable TTL (per-entry variable TTL supported)
- [ ] **CRED-04**: Cache miss triggers transparent DynamoDB fetch and cache population
- [ ] **CRED-05**: Proxy continues serving cached entries when DynamoDB is unreachable

### Authentication Flow

- [ ] **AUTH-01**: On valid credentials, proxy swaps username/password with generic Mosquitto creds before forwarding
- [x] **AUTH-02**: Generic Mosquitto credentials are sourced from YAML config or env vars (not hardcoded)
- [ ] **AUTH-03**: On invalid or missing credentials, proxy sends CONNACK with return code 0x05 (not authorized)
- [ ] **AUTH-04**: Configured passthrough usernames bypass credential validation entirely
- [x] **AUTH-05**: Passthrough allowlist is sourced from YAML config (not hardcoded)

### Admin API

- [ ] **ADMIN-01**: `DELETE /cache/credentials/{username}` evicts a specific cached entry
- [ ] **ADMIN-02**: `POST /cache/credentials/{username}/refresh` force re-fetches from DynamoDB
- [ ] **ADMIN-03**: `GET /cache/stats` returns entry count, hit/miss counters, hit rate
- [ ] **ADMIN-04**: `GET /cache/credentials` lists cached usernames with TTL remaining (no passwords)
- [ ] **ADMIN-05**: `DELETE /cache/credentials` flushes entire cache
- [ ] **ADMIN-06**: `GET /health` returns 200 with DynamoDB connectivity status
- [ ] **ADMIN-07**: Admin HTTP server binds to configurable address (default localhost)

### Configuration

- [x] **CONF-01**: Cache TTL and max memory size are configurable via `Server.CacheTTLSecs` and `Server.CacheMaxSizeMB` in YAML config
- [x] **CONF-02**: Admin API listen address is configurable via `Server.AdminListenAddress`
- [x] **CONF-03**: DynamoDB table name is configurable via `Server.CredentialTableName`
- [x] **CONF-04**: DynamoDB region is configurable via `Server.CredentialTableRegion`

## v2 Requirements

Deferred to future release. Tracked but not in current roadmap.

### Operational Hardening

- **OPS-01**: Negative caching for failed lookups with shorter TTL to prevent DynamoDB cost spikes
- **OPS-02**: Auth-specific rate limiting per source IP on failed CONNECT attempts
- **OPS-03**: Prometheus metrics endpoint for cache hit/miss rates and DynamoDB latency
- **OPS-04**: Structured JSON logging for auth events (username, source IP, cache hit/miss)

## Out of Scope

| Feature | Reason |
|---------|--------|
| Credential CRUD (create/update/delete) | Managed in defcon.run — proxy is a consumer, not authority |
| Persistent cache (Redis/Memcached) | Single-process proxy; in-memory + DynamoDB is sufficient |
| Topic-based authorization | Username/password validation only; topic ACLs are a different problem |
| OAuth/JWT/token auth | Meshtastic MQTT clients send username/password only |
| Distributed cache sync | Each proxy instance maintains own cache; DynamoDB is shared truth |
| Admin API authentication | Internal API on ECS; network-level security (VPC/security groups) is sufficient |
| Automatic cache warming on startup | Lazy population on first CONNECT is sufficient; DynamoDB scan is expensive |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| CRED-01 | Phase 2 | Pending |
| CRED-02 | Phase 1 | Complete |
| CRED-03 | Phase 1 | Complete |
| CRED-04 | Phase 2 | Pending |
| CRED-05 | Phase 2 | Pending |
| AUTH-01 | Phase 2 | Pending |
| AUTH-02 | Phase 1 | Complete |
| AUTH-03 | Phase 2 | Pending |
| AUTH-04 | Phase 2 | Pending |
| AUTH-05 | Phase 1 | Complete |
| ADMIN-01 | Phase 3 | Pending |
| ADMIN-02 | Phase 3 | Pending |
| ADMIN-03 | Phase 3 | Pending |
| ADMIN-04 | Phase 4 | Pending |
| ADMIN-05 | Phase 4 | Pending |
| ADMIN-06 | Phase 4 | Pending |
| ADMIN-07 | Phase 3 | Pending |
| CONF-01 | Phase 1 | Complete |
| CONF-02 | Phase 1 | Complete |
| CONF-03 | Phase 1 | Complete |
| CONF-04 | Phase 1 | Complete |

**Coverage:**
- v1 requirements: 21 total
- Mapped to phases: 21
- Unmapped: 0

---
*Requirements defined: 2026-03-10*
*Last updated: 2026-03-10 — traceability mapped after roadmap creation*
