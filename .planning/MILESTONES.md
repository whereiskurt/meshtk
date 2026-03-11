# Milestones

## v1.0 MeshTK MQTT Proxy Credential Cache (Shipped: 2026-03-11)

**Phases:** 4 | **Plans:** 8 | **Timeline:** 2026-03-10 → 2026-03-11
**Requirements:** 21/21 satisfied | **Audit:** passed

**Key accomplishments:**
1. Config schema and credential cache infrastructure — CredCacheConfig with Otter v2 in-memory cache and DynamoDB adapter
2. CacheAuthenticator with singleflight and circuit breaker — deduplicates concurrent DynamoDB fetches, gracefully degrades on outage
3. MQTT CONNECT credential validation — validates every connection, swaps to generic Mosquitto creds, rejects invalid with CONNACK 0x05
4. Admin HTTP API — evict, refresh, stats, list, flush, and health endpoints on configurable address
5. Negative caching for brute-force protection — unknown usernames cached with short TTL to prevent DynamoDB cost spikes
6. Health endpoint for ECS — reports DynamoDB connectivity status via circuit breaker state

**Git range:** `feat(01-01)..feat(04-02)` | **Files changed:** 48 | **LOC:** +7,588 / -115

---

