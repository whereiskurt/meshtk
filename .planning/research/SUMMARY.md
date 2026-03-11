# Project Research Summary

**Project:** MeshTK MQTT Proxy Credential Cache
**Domain:** MQTT proxy authentication with DynamoDB-backed in-memory cache
**Researched:** 2026-03-10
**Confidence:** HIGH

## Executive Summary

This milestone adds DynamoDB-backed credential caching to an existing Go MQTT proxy. The proxy currently validates credentials inline using a hardcoded seed-based algorithm in `inspect.go`. The goal is to replace that with a lookup against a DynamoDB table (`defcon.run` schema), cache positive results in memory with TTL, and expose a minimal HTTP admin API for cache eviction, refresh, and stats. The existing Go 1.24.1 codebase with Cobra/Viper/Logrus provides everything needed -- only 3 new dependencies are required: otter/v2 (in-memory cache), aws-sdk-go-v2 (DynamoDB), and stdlib net/http (admin API).

The recommended approach isolates all credential logic in a new `internal/credcache/` package with four clean components: a pure in-memory cache, a DynamoDB store adapter behind an interface, an Authenticator orchestrator, and HTTP handlers. The integration point into the existing proxy is a single function call in `inspect.go` replacing ~15 lines of inline code. This decomposition enables unit-testable components, a clean mock store for tests, and a future-proof architecture if the backing store ever changes.

The primary risks are operational, not technical: the revocation window (stale cached credentials after DynamoDB deletion), cache stampede on TTL expiry under reconnect bursts, and brute-force amplification if negative results are not bounded. All three are solvable with well-established Go patterns (singleflight, bounded negative cache, short TTL + mandatory eviction API) that must be built in from the start, not retrofitted.

## Key Findings

### Recommended Stack

The existing Go 1.24.1 codebase needs only three new dependencies. For the in-memory cache, **maypok86/otter v2** (v2.3.0) is the clear choice: it outperforms ristretto and ttlcache on throughput and hit rate, provides variable per-item TTL with refresh-on-read, built-in stats (hit/miss/eviction counters), and a type-safe generics API. For DynamoDB access, **aws-sdk-go-v2** is mandatory -- aws-sdk-go v1 (already in the codebase for S3) reached end-of-support July 31, 2025, and building new DynamoDB integration on v1 would compound technical debt immediately. The two SDK versions coexist cleanly via different import paths. For the admin API, the **stdlib net/http** with Go 1.22+ ServeMux handles method routing and path parameters natively, making chi/gin/echo unnecessary for 4 endpoints.

**Core technologies:**
- **maypok86/otter v2** (v2.3.0): In-memory credential cache -- variable TTL, refresh-on-read, built-in stats, zero GC pressure
- **aws-sdk-go-v2/service/dynamodb**: DynamoDB credential lookups -- v2 is required (v1 is EOL); coexists with existing v1 S3 code
- **aws-sdk-go-v2/feature/dynamodb/attributevalue**: Marshal/unmarshal Go structs from DynamoDB items
- **net/http (stdlib)**: HTTP admin API -- Go 1.22+ ServeMux supports method+path routing natively
- **sirupsen/logrus** (existing): Structured logging for cache events -- no new dependency needed
- **spf13/viper** (existing): Config for DynamoDB table, TTL, generic creds, admin listen address

### Expected Features

The MVP order is driven by hard dependencies: generic creds and passthrough allowlist must be config-driven before the auth refactor, DynamoDB lookup must exist before the cache, and the cache must exist before the admin API.

**Must have (table stakes):**
- DynamoDB credential lookup -- replaces seed-based `generateMQTTPassword()` entirely
- In-memory cache with TTL + cache-miss backfill -- DynamoDB at ~10ms per CONNECT is not acceptable latency
- CONNECT rejection with proper MQTT CONNACK (0x05) -- current silent drop is protocol non-compliance
- Credential swap to generic Mosquitto creds from config -- replaces hardcoded `"public"/"31337"`
- Passthrough allowlist from config -- preserves existing behavior for `ghosts`, `kph`, `ax`, `meshmap`
- Cache eviction API (`DELETE /cache/{username}`) -- required for immediate credential revocation; this is a security control
- Cache stats API (`GET /cache/stats`) -- minimum operational visibility
- Cache refresh API (`POST /cache/{username}/refresh`) -- admin convenience; low effort after eviction endpoint exists
- Graceful degradation on DynamoDB failure -- serve cached entries, fail new unknown users closed

**Should have (differentiators):**
- Cache inspection endpoint (`GET /cache/credentials`) -- debug "is user X cached?" without log diving
- Bulk eviction endpoint (`DELETE /cache/credentials`) -- incident response and mass credential rotation
- Negative caching -- bounded LRU with short TTL to absorb brute-force attempts without DynamoDB cost spikes
- Health check endpoint (`GET /health`) -- enables ECS container health checks
- Structured JSON auth event logging -- feed log aggregation and alerting

**Defer (v2+):**
- Prometheus metrics endpoint -- JSON stats endpoint covers initial needs; add only if monitoring stack demands it
- Rate limiting per source IP on failed auth -- existing connection-level rate limiter in `proxy.go` already provides baseline protection
- Automatic cache warming on startup -- lazy population is sufficient; startup scan is expensive and mostly unnecessary

### Architecture Approach

The architecture cleanly decomposes the existing inline credential logic into four components in a new `internal/credcache/` package. The single integration point is `inspect.go`'s `ConnectPacket` case, where ~15 lines of inline code become one call to `Authenticator.ValidateAndSwap(p)`. All other proxy pipeline code is untouched. The CredentialCache is a pure data structure (no external dependencies, trivially testable). DynamoDBStore implements a `CredentialStore` interface, enabling mock stores in unit tests. CredCacheHTTP runs on a separate goroutine and accesses only the cache (not the proxy pipeline), eliminating lock contention concerns between the admin API and MQTT handling. The build order follows hard dependencies: cache first, then config, then DynamoDB adapter, then Authenticator, then inspect.go integration, then CONNACK rejection, then HTTP admin API.

**Major components:**
1. **CredentialCache** (`internal/credcache/cache.go`) -- `map[string]*CachedCredential` + `sync.RWMutex` + TTL expiry + hit/miss/eviction stats; pure Go, no external deps
2. **DynamoDBStore** (`internal/credcache/dynamo.go`) -- implements `CredentialStore` interface; uses AWS SDK v2 `GetItem` with `ConsistentRead: true`
3. **Authenticator** (`internal/credcache/auth.go`) -- orchestrates cache lookup -> DynamoDB fallback -> cache populate -> password verify -> credential swap; called from `inspect.go`
4. **CredCacheHTTP** (`internal/credcache/http.go`) -- stdlib HTTP handlers for evict/refresh/stats; runs on separate goroutine; accesses cache only
5. **Config extensions** (`pkg/config/config.go`) -- DynamoDB table, region, TTL, max entries, generic creds, admin listen address

### Critical Pitfalls

1. **AWS SDK v1 used for DynamoDB** -- Use aws-sdk-go-v2 exclusively for DynamoDB from day one. v1 is EOL (July 2025), no security patches. v2 and v1 coexist cleanly via different import paths; do not touch existing S3 code.
2. **Cache stampede on TTL expiry** -- Implement `golang.org/x/sync/singleflight` in the Authenticator so only one goroutine fetches DynamoDB on a cache miss; concurrent requests for the same username wait for the result. Meshtastic reconnect bursts make this a real risk, not theoretical.
3. **Revocation window with no eviction mechanism** -- The HTTP admin API eviction endpoint is a security control, not a feature. It must be implemented immediately after the cache. TTL must be short (2-5 minutes) as the safety net. defcon.run should call the eviction endpoint on credential deletion.
4. **Negative cache poisoning under brute force** -- Cache "not found" results with a bounded LRU (max 10,000 entries) at a short TTL (30-60 seconds). Without this, every invalid username attempt hits DynamoDB; with unbounded negative caching, memory is exhausted by fake usernames.
5. **Mutex deadlock between cache and HTTP admin API** -- Never hold the credential cache mutex while making DynamoDB calls or log writes. Keep critical sections minimal: lock, read/write map, unlock. Never nest the cache mutex with `ConnMutex`. Run with `-race` flag during development.

## Implications for Roadmap

Based on the dependency chain established in the architecture research, a 4-phase structure is recommended. Each phase is independently deliverable and testable.

### Phase 1: Foundation -- Config, Cache Data Structure, and DynamoDB Adapter

**Rationale:** The CredentialCache and DynamoDBStore have no dependencies on each other or on the existing proxy code. They can be built, tested, and reviewed before any proxy behavior changes. Establishing the config schema first prevents rework in subsequent phases.
**Delivers:** New `internal/credcache/` package with cache.go (full unit tests), dynamo.go (integration test against real DynamoDB), config.go extensions for all credential cache settings. Zero changes to existing proxy behavior.
**Addresses:** Generic creds from config, passthrough allowlist from config, DynamoDB table/region config, TTL and max-entries config.
**Avoids:** DynamoDB schema mismatch (verify schema in this phase before building on top), wrong concurrency primitive (sync.Map vs RWMutex -- decide here), unbounded cache growth (hard cap built in from the start).

### Phase 2: Authenticator and Proxy Integration

**Rationale:** With the cache and DynamoDB adapter complete, the Authenticator is straightforward orchestration. The inspect.go integration is the single riskiest change (touching existing proxy behavior) and must be isolated to its own phase for review.
**Delivers:** `auth.go` with `ValidateAndSwap()` (unit tested with MockStore), refactored `ConnectPacket` case in `inspect.go` (15 lines -> 3 lines), preserved passthrough allowlist behavior, credential swap using config-sourced generic creds, and proper MQTT CONNACK rejection (0x05) instead of silent credential clobber.
**Addresses:** DynamoDB credential lookup, in-memory cache with TTL + cache-miss backfill, CONNECT rejection with proper CONNACK, credential swap, graceful degradation on DynamoDB failure.
**Avoids:** Cache stampede (singleflight must be wired in here), credential swap step forgotten (validate and rewrite are explicit named methods in Authenticator), timing attack on password comparison (use `crypto/subtle.ConstantTimeCompare`).
**Uses:** otter v2 (via CredentialCache), aws-sdk-go-v2 (via DynamoDBStore), golang.org/x/sync/singleflight.

### Phase 3: HTTP Admin API

**Rationale:** The HTTP admin API runs on a separate goroutine and accesses only the CredentialCache -- it has no dependency on the proxy pipeline. It can be built in parallel with Phase 2 if desired, but sequencing it after Phase 2 ensures the cache is battle-tested before exposing administrative control over it. The eviction endpoint is a security control and must not be deferred past this phase.
**Delivers:** HTTP server on configurable port (`:8080`), `DELETE /cache/{username}` (evict), `POST /cache/{username}/refresh` (force re-fetch from DynamoDB), `GET /cache/stats` (hit/miss/size/hit rate), bound to localhost or internal interface, initialized in `NewServer()` alongside existing components.
**Addresses:** Cache eviction API, cache stats API, cache refresh API, partial mitigation of revocation window.
**Avoids:** Deadlock between cache mutex and HTTP handlers (minimal critical sections, DynamoDB calls outside lock), admin API without authentication (bind to localhost; rely on ECS/VPC network isolation as documented in anti-features).

### Phase 4: Operational Hardening

**Rationale:** These features improve production resilience and observability but are not blocking for the core credential cache function. They are grouped here so Phase 2 and Phase 3 can ship and be validated before adding more complexity.
**Delivers:** Negative caching (bounded LRU, 30-60s TTL for "not found" results), cache inspection endpoint (`GET /cache/credentials`), bulk eviction endpoint (`DELETE /cache/credentials`), health check endpoint (`GET /health` with DynamoDB connectivity status), structured JSON logging for auth events.
**Addresses:** Negative cache poisoning under brute force, debugging visibility, incident response tooling, ECS health check integration.

### Phase Ordering Rationale

- Phase 1 before Phase 2: Cache and DynamoDB adapter must exist before Authenticator can be written. Config must be defined before any component consumes it.
- Phase 2 before Phase 3: Admin API acts on the cache, which must be in use by the proxy before admin operations are meaningful to test end-to-end.
- Phase 3 before Phase 4: Eviction API must exist before negative caching is added (negative cache entries must also be evictable). The health check endpoint logically belongs after all components are running.
- Singleflight in Phase 2: Must be built in at Authenticator creation, not retrofitted after. Cache stampede is a structural concern, not an optimization.
- CONNACK rejection in Phase 2: Protocol compliance fix that requires the new auth path to be working first.

### Research Flags

Phases with well-documented patterns (skip research-phase):
- **Phase 1 (cache data structure):** Standard Go RWMutex + map pattern; otter v2 API is well-documented; aws-sdk-go-v2 DynamoDB GetItem is straightforward.
- **Phase 3 (HTTP admin API):** stdlib net/http with Go 1.22+ ServeMux is well-documented; 4 endpoints with no middleware.

Phases likely needing targeted investigation during planning:
- **Phase 2 (proxy integration):** The exact DynamoDB schema for defcon.run credentials must be confirmed before writing the DynamoDB adapter. The attribute names, partition key name, and password storage format (hash algorithm, encoding) are not documented in the research files -- this must be validated against the live table before Phase 1 code is finalized.
- **Phase 4 (negative caching):** The interaction between otter v2's eviction API and a separate negative cache (or a unified cache with positive/negative entry types) needs design work. Whether to use one otter cache or two is a decision deferred to this phase.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | All technology choices backed by official docs, benchmarks, and AWS announcements. Otter v2 released December 2025 with known stable API. SDK v2 EOL for v1 is official. |
| Features | HIGH | Based on direct codebase analysis (`inspect.go`, `proxy.go`, `config.go`) and well-established MQTT proxy patterns. Feature list is grounded in existing code, not speculation. |
| Architecture | HIGH | Architecture is derived directly from existing code patterns (`ServerCmd` composition, `ConnMutex`+map pattern, S3Mover adapter pattern). Integration point is unambiguous. |
| Pitfalls | HIGH | Critical pitfalls are well-known distributed caching problems (stampede, revocation window, brute force) with established mitigations. Mutex deadlock analysis is specific to the existing codebase structure. |

**Overall confidence:** HIGH

### Gaps to Address

- **DynamoDB table schema for defcon.run credentials:** The research identifies that the proxy must match the existing table schema exactly, but the actual attribute names, partition key, sort key structure, and password storage format (bcrypt? argon2? plaintext?) are not known. This must be confirmed against the live table or defcon.run source code before writing `DynamoDBStore.Fetch()`. Risk: if the schema assumption is wrong, the DynamoDB adapter requires a rewrite.
- **Password comparison semantics:** The architecture assumes the proxy compares a stored credential directly against the MQTT CONNECT password. If defcon.run stores passwords as bcrypt or argon2 hashes, the comparison requires a hash operation (not just `subtle.ConstantTimeCompare`). This affects the Authenticator's `verifyPassword()` implementation and potentially performance on the hot path.
- **Multiple proxy instances:** PITFALLS.md notes that if the proxy scales horizontally, each instance has an independent cache and the eviction API only affects one instance. The admin API design should document this limitation now, even if multi-instance eviction is out of scope for this milestone.

## Sources

### Primary (HIGH confidence)
- Existing codebase: `internal/app/server/inspect.go`, `proxy.go`, `cmd.go`, `pkg/config/config.go` -- direct analysis of integration points and existing patterns
- [AWS SDK Go v1 EOL announcement](https://aws.amazon.com/blogs/developer/announcing-end-of-support-for-aws-sdk-for-go-v1-on-july-31-2025/) -- end-of-support confirmed July 31, 2025
- [AWS DynamoDB read consistency](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html) -- ConsistentRead behavior documented
- [Otter v2 GitHub releases](https://github.com/maypok86/otter/releases) -- v2.3.0 released December 22, 2025
- [Otter v2 pkg.go.dev](https://pkg.go.dev/github.com/maypok86/otter/v2) -- API documentation
- [Go sync.Map documentation](https://pkg.go.dev/sync#Map) -- optimal use cases explicitly documented
- [AWS SDK Go v2 DynamoDB package](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/dynamodb) -- v1.56+

### Secondary (MEDIUM confidence)
- [Otter cache evolution blog](https://maypok86.github.io/otter/blog/cache-evolution/) -- benchmarks vs ristretto and bigcache
- [Go ServeMux vs chi comparison](https://www.calhoun.io/go-servemux-vs-chi/) -- Go 1.22+ routing capabilities
- [Go HTTP router comparison](https://www.alexedwards.net/blog/which-go-router-should-i-use) -- framework selection guidance
- [Go thread-safe cache patterns](https://dev.to/vearutop/implementing-robust-in-memory-cache-with-go-196e) -- RWMutex + map patterns
- [Go caching strategies with TTL](https://oneuptime.com/blog/post/2026-02-01-go-caching-strategies/view) -- TTL implementation patterns

### Tertiary (reference)
- [MQTT brute force tooling (mqtt-pwn)](https://mqtt-pwn.readthedocs.io/en/latest/plugins/brute.html) -- attack surface context for negative cache design
- [Go mutex deadlock patterns](https://iximiuz.com/en/posts/go-http-handlers-panic-and-deadlocks/) -- deadlock scenario examples

---
*Research completed: 2026-03-10*
*Ready for roadmap: yes*
