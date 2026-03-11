# Project Retrospective

*A living document updated after each milestone. Lessons feed forward into future planning.*

## Milestone: v1.0 — MeshTK MQTT Proxy Credential Cache

**Shipped:** 2026-03-11
**Phases:** 4 | **Plans:** 8 | **Sessions:** ~4

### What Was Built
- DynamoDB-backed MQTT credential validation with Otter v2 in-memory cache
- CacheAuthenticator with singleflight dedup, circuit breaker, negative caching
- MQTT CONNECT interception with CONNACK 0x05 rejection and credential swap
- Admin HTTP API with 6 endpoints (evict, refresh, stats, list, flush, health)
- Configurable negative caching for brute-force protection
- ECS-compatible health endpoint using circuit breaker state

### What Worked
- TDD approach caught wiring issues early — tests written before implementation in every plan
- Wave-based execution parallelized independent work effectively
- Phase verification (gsd-verifier) caught the AUTH-05 hardcoded passthrough gap in Phase 1 before it became a problem
- Integration checker caught the NegativeTTLSecs wiring gap at milestone audit — 2-line fix vs potential production bug
- Otter v2 API research (reading local source code) gave high-confidence findings — no surprises at implementation time

### What Was Inefficient
- Phase 1 VERIFICATION.md flagged AUTH-05 as partial but execution continued — gap carried forward to Phase 2 where it was naturally resolved, but could have been fixed in 1 line during Phase 1
- ROADMAP.md plan checkboxes stayed `[ ]` even for completed plans in Phases 1-3 (only Phase 4 marked `[x]` on completion) — cosmetic but visible inconsistency
- Admin package logger mismatch (stdlib vs logrus) carried as tech debt — should have been decided in Phase 3 context gathering

### Patterns Established
- Consumer-defined interfaces (Authenticator in server package, not credcache)
- Singleflight wrapping inside authenticator layer (not cache layer)
- JSON envelope `{data, timestamp}` for all admin responses
- Circuit breaker with atomic counters for lock-free degradation detection
- GetEntryQuietly for read-only iteration (avoid inflating stats)

### Key Lessons
1. Research local source code (e.g., Otter v2 in GOMODCACHE) rather than trusting docs — APIs diverge from documentation
2. Integration checking at milestone level catches cross-phase wiring gaps that per-phase verification misses
3. Negative caching must be inside singleflight callback to handle concurrent unknown-user requests correctly
4. Config fields without runtime wiring are silent bugs — audit the config→constructor path

### Cost Observations
- Model mix: ~70% opus (executor), ~30% sonnet (verifier, checker, integration)
- Sessions: ~4 context windows
- Notable: Research agents paid for themselves — zero API surprises at implementation time

---

## Cross-Milestone Trends

### Process Evolution

| Milestone | Sessions | Phases | Key Change |
|-----------|----------|--------|------------|
| v1.0 | ~4 | 4 | Initial milestone — established TDD, wave execution, verification patterns |

### Cumulative Quality

| Milestone | Tests | Coverage | Zero-Dep Additions |
|-----------|-------|----------|-------------------|
| v1.0 | 58+ | credcache + admin + server + config | singleflight (1 new dep) |

### Top Lessons (Verified Across Milestones)

1. Research local source code, not documentation — APIs evolve faster than docs
2. Integration checking catches wiring gaps that unit tests miss
