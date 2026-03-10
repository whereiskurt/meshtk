# Domain Pitfalls

**Domain:** MQTT proxy credential caching with DynamoDB backend
**Researched:** 2026-03-10

## Critical Pitfalls

Mistakes that cause security vulnerabilities, data corruption, or require rewrites.

### Pitfall 1: Negative Cache Poisoning via Brute Force

**What goes wrong:** An attacker sends MQTT CONNECT packets with thousands of invalid usernames. Each miss hits DynamoDB, and if you cache the "not found" result, the cache fills with negative entries. If you do NOT cache negatives, every invalid attempt hits DynamoDB, driving up costs and latency under attack.

**Why it happens:** The current code (inspect.go lines 99-115) already validates inline but has no rate limiting on auth attempts per username. Moving to a cache introduces a new failure mode: the cache itself becomes an attack surface. MQTT brute force tools like mqtt-pwn and Metasploit's `auxiliary/scanner/mqtt/connect` module specifically target this.

**Consequences:**
- Without negative caching: DynamoDB costs spike during brute force attacks; potential throttling blocks legitimate users
- With naive negative caching: Memory exhaustion from millions of fake usernames filling the cache
- Either way: legitimate credential lookups degraded during attack

**Prevention:**
- Cache negative results (username not found) with a SHORT TTL (30-60 seconds) -- just enough to absorb repeated attempts
- Cap negative cache entries with a bounded LRU (e.g., max 10,000 negative entries) separate from the positive cache
- Combine with the existing rate limiter (`pkg/network/limiter.go`) -- apply rate limiting BEFORE cache lookup, at the connection/IP level
- Track failed auth attempts per source IP and escalate to connection rejection

**Detection:**
- Monitor cache miss rate; a sudden spike in misses = brute force attempt
- Monitor negative cache size growth
- Alert on DynamoDB read throttling events

**Phase:** Must be addressed in the cache implementation phase, not deferred. The HTTP admin API for cache eviction should include negative cache stats.

---

### Pitfall 2: AWS SDK v1 End-of-Life -- Building on Deprecated Foundation

**What goes wrong:** The project currently uses `github.com/aws/aws-sdk-go v1.55.7`. AWS SDK for Go v1 reached end-of-support on July 31, 2025. Building a new DynamoDB integration on v1 means no security patches, no new features, and eventual breakage as AWS evolves its APIs.

**Why it happens:** The existing S3 upload code already uses v1, so it feels natural to add DynamoDB calls using the same SDK. But v1 is now past end-of-life -- it receives no updates at all.

**Consequences:**
- Security vulnerabilities in the SDK will not be patched
- New DynamoDB features (if needed later) will not be available
- Credential chain behavior (ECS task roles, EC2 instance profiles) may drift from AWS expectations
- Technical debt compounds -- migrating later means rewriting both S3 and DynamoDB code

**Prevention:**
- Use `aws-sdk-go-v2` for the new DynamoDB integration from day one
- The v2 SDK can coexist with v1 in the same binary -- no need to migrate S3 immediately
- v2 has a different import path (`github.com/aws/aws-sdk-go-v2/service/dynamodb`) so there are no conflicts
- Plan a follow-up task to migrate existing S3 code to v2

**Detection:**
- `go.mod` still importing only `github.com/aws/aws-sdk-go` (v1) for new DynamoDB code
- Dependabot/security scanners flagging the v1 dependency

**Phase:** Must be decided before writing any DynamoDB code. The DynamoDB client setup phase should use v2 exclusively.

**Confidence:** HIGH -- AWS officially announced end-of-support: https://aws.amazon.com/blogs/developer/announcing-end-of-support-for-aws-sdk-for-go-v1-on-july-31-2025/

---

### Pitfall 3: Cache Stampede on Popular Username Expiry

**What goes wrong:** When a cached credential entry expires (TTL), multiple concurrent MQTT CONNECT packets for the same username all see a cache miss simultaneously. All of them hit DynamoDB at the same time to re-fetch the credential. With Meshtastic devices reconnecting in bursts (e.g., after a mesh network partition heals), this can cause dozens of simultaneous lookups for the same user.

**Why it happens:** Simple TTL expiry with lazy loading -- check cache, miss, fetch from DynamoDB, store in cache. Between the miss and the store, every concurrent request also misses.

**Consequences:**
- DynamoDB read capacity consumed by redundant identical queries
- Increased latency for all concurrent CONNECT packets during stampede
- Under high concurrency, potential DynamoDB throttling that cascades to all users

**Prevention:**
- Implement singleflight pattern (`golang.org/x/sync/singleflight`) -- only one goroutine fetches from DynamoDB for a given username; others wait for the result
- Consider "refresh-ahead" / "early refresh": when a cached entry is within 10-20% of its TTL, trigger an async background refresh while still serving the cached value
- The singleflight approach is idiomatic Go and straightforward to implement

**Detection:**
- Monitor DynamoDB read metrics; bursts of identical GetItem requests for the same key
- Log when multiple goroutines attempt concurrent fetches for the same username

**Phase:** Must be built into the cache implementation from the start. Retrofitting singleflight is harder than building it in.

---

### Pitfall 4: Stale Credentials After Revocation -- The Revocation Window

**What goes wrong:** A credential is revoked in DynamoDB (by the defcon.run admin system), but the proxy continues to accept connections using the cached (now-invalid) credential until the TTL expires. During this window, a revoked user retains full access.

**Why it happens:** In-memory cache with TTL-based expiry is eventually consistent by design. The proxy has no way to know a credential was revoked in DynamoDB until it re-fetches. A 5-minute TTL means up to 5 minutes of unauthorized access after revocation.

**Consequences:**
- Revoked users maintain access for up to TTL duration
- Security incident response is delayed -- you cannot immediately lock out a compromised account
- Erodes trust in the credential management system

**Prevention:**
- The HTTP admin API for cache eviction is the primary mitigation -- this is not optional, it is a security control. When defcon.run revokes a credential, it should call the eviction endpoint
- Keep TTL short (2-5 minutes) as a safety net for cases where the eviction call fails
- The eviction API must be synchronous and return confirmation -- fire-and-forget is not acceptable for security operations
- Consider: if the proxy runs multiple instances (future scaling), you need a way to evict from all instances (out of scope now but design the eviction API to be callable externally)

**Detection:**
- Audit log when eviction API is called vs. when credential was revoked in DynamoDB -- gap should be minimal
- Monitor for CONNECT successes from usernames that no longer exist in DynamoDB (periodic reconciliation)

**Phase:** The HTTP admin API phase is a security-critical deliverable, not a nice-to-have. It should be implemented immediately after the cache itself.

---

### Pitfall 5: Mutex Deadlock Between Cache Operations and HTTP Admin API

**What goes wrong:** The cache uses a `sync.RWMutex` for concurrent access. The HTTP admin API handler acquires a write lock to evict an entry. Meanwhile, the MQTT proxy goroutine holds a read lock and tries to call a function that also needs the write lock (or vice versa). Deadlock -- the proxy freezes, all MQTT connections hang.

**Why it happens:** The existing codebase already uses `ConnMutex` (a `sync.Mutex`) for connection tracking. Adding a second mutex for the credential cache, plus HTTP handlers running in separate goroutines, creates multiple lock ordering opportunities for deadlocks. Go's `sync.Mutex` is not reentrant -- a goroutine that holds a lock and tries to acquire it again will deadlock.

**Consequences:**
- Complete proxy freeze -- all MQTT connections stop, no new connections accepted
- Silent failure -- no error logged, process appears alive but unresponsive
- Requires process restart to recover

**Prevention:**
- Use `sync.RWMutex` for the credential cache (readers don't block each other -- critical for MQTT throughput)
- Never hold the credential cache mutex while calling external functions (DynamoDB calls, logging that could block)
- Keep critical sections minimal: lock, read/write map, unlock. Do DynamoDB fetches OUTSIDE the lock
- Never nest locks: do not hold `ConnMutex` while acquiring the cache mutex or vice versa
- Run with `go build -race` and `go test -race` during development to catch races early
- Consider a channel-based design for cache updates if the locking gets complex

**Detection:**
- Health check endpoint that acquires a read lock with a timeout -- if it cannot acquire within 1 second, the cache is deadlocked
- Monitor goroutine count; deadlocked goroutines accumulate
- Use `runtime/pprof` or `net/http/pprof` to dump goroutine stacks on demand

**Phase:** Architecture decision in the cache design phase. The HTTP admin API must be designed with the locking strategy in mind from day one.

---

## Moderate Pitfalls

### Pitfall 6: DynamoDB Eventually Consistent Reads Returning Stale Credentials

**What goes wrong:** DynamoDB's default read mode is eventually consistent. After a credential is created or updated in defcon.run, a GetItem call from the proxy may return stale data (the old or missing credential) for a brief window.

**Why it happens:** DynamoDB replicates writes across three replicas asynchronously. An eventually consistent read may hit the replica that has not yet received the write. The inconsistency window is typically under 1 second but is not guaranteed.

**Prevention:**
- Use `ConsistentRead: true` on GetItem calls for credential lookups -- this costs 2x the read capacity but guarantees you see the latest data
- Since credential lookups only happen on cache miss (not every MQTT packet), the 2x cost is negligible
- This is especially important for the cache refresh API endpoint -- a forced refresh that returns stale data defeats the purpose

**Detection:**
- A newly created user cannot connect even after the cache should have been populated
- Intermittent "credential not found" for recently created users

**Phase:** DynamoDB client setup phase. Set `ConsistentRead: true` as the default for credential table reads.

**Confidence:** HIGH -- documented AWS behavior: https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html

---

### Pitfall 7: sync.Map vs RWMutex+Map -- Wrong Concurrency Primitive

**What goes wrong:** Choosing `sync.Map` for the credential cache because it "sounds concurrent-safe" leads to performance issues. `sync.Map` is optimized for append-only workloads (keys written once, read many times). A credential cache with TTL expiry, manual eviction, and periodic cleanup involves frequent deletes -- exactly the workload where `sync.Map` underperforms.

**Why it happens:** `sync.Map` appears in many Go tutorials as the "concurrent map" solution. But its internal structure (separate read-only and dirty maps with promotion) adds overhead for write-heavy access patterns.

**Prevention:**
- Use a plain `map[string]*CachedCredential` protected by `sync.RWMutex`
- This gives you type safety (no `interface{}` casts), better performance for mixed read/write/delete, and explicit control over locking granularity
- The existing codebase already uses this pattern (`ConnMutex` + `ConnTrack map`) -- stay consistent

**Detection:**
- Benchmark under load: if cache operations show unexpected latency, check the concurrency primitive
- Profile with `go tool pprof` looking for time spent in sync.Map internals

**Phase:** Cache data structure design. Decide before writing cache code.

**Confidence:** HIGH -- Go documentation explicitly states sync.Map's optimal use cases: https://pkg.go.dev/sync#Map

---

### Pitfall 8: Hardcoded Credential Swap Breaking Transparency

**What goes wrong:** The current code (inspect.go lines 108-111) swaps valid credentials with hardcoded `"public"` / `"31337"`. If the new cache-based auth path forgets to perform this swap, or swaps to different generic creds than the existing path, legitimate Meshtastic clients either fail to connect or connect with wrong permissions on Mosquitto.

**Why it happens:** The credential swap is buried inside the CONNECT packet inspection, not in a clearly separated "auth then rewrite" pipeline. When refactoring to use a cache, it is easy to validate the credential but forget the rewrite step, or to apply the rewrite in a different code path than the validation.

**Prevention:**
- Extract credential validation and credential rewriting into separate, clearly named functions: `ValidateCredential(username, password) (bool, error)` and `RewriteToGenericCreds(packet *ConnectPacket)`
- The generic credentials ("public"/"31337" or whatever replaces them) should come from config, not be hardcoded
- Test the full pipeline: invalid creds rejected, valid creds accepted AND rewritten, passthrough users (ghosts/kph/ax/meshmap) unchanged
- The PROJECT.md already specifies generic creds from config/env vars -- follow through on this

**Detection:**
- Integration test: connect with valid cached credential, verify the backend Mosquitto sees the generic credential, not the original
- Monitor Mosquitto auth logs for unexpected usernames leaking through

**Phase:** Core implementation phase. Must be tested as a unit: validate + rewrite as an atomic operation.

---

### Pitfall 9: Unbounded Cache Growth from Credential Accumulation

**What goes wrong:** Every unique username that successfully authenticates gets cached. Over time, the cache grows without bound if entries are only removed by TTL expiry (and the TTL ticker has bugs or the eviction goroutine is starved).

**Why it happens:** The existing `ConnTrack` map (inspect.go lines 366-387) already has a cleanup goroutine that runs every 5 minutes. If the credential cache uses a similar pattern but the cleanup goroutine panics, gets stuck, or falls behind, the map grows indefinitely, consuming memory.

**Prevention:**
- Set a hard cap on cache entries (e.g., 50,000) -- reject caching new entries if at capacity (still validate via DynamoDB, just don't cache)
- Use the TTL cleanup pattern already proven in `SetupTracker()` but add recovery from panics
- Monitor cache size as a metric exposed via the HTTP stats endpoint
- Consider using an LRU eviction policy on top of TTL -- least recently used entries evicted first when at capacity

**Detection:**
- Memory usage growth over time correlated with cache size
- HTTP stats endpoint showing cache entry count trending upward without plateau

**Phase:** Cache implementation phase. Build the size cap from the start; adding it later requires refactoring the data structure.

---

## Minor Pitfalls

### Pitfall 10: HTTP Admin API Without Authentication

**What goes wrong:** The cache eviction and stats endpoints are exposed without authentication. Anyone who can reach the proxy's HTTP port can evict credentials (denial of service) or enumerate cached usernames (information disclosure).

**Prevention:**
- Bind the admin HTTP listener to localhost only (127.0.0.1) or a private interface
- Add a shared secret / API key header for eviction operations
- Stats/read-only endpoints can be less restricted but should still not be public
- If running in ECS, use security groups to restrict access to the admin port

**Phase:** HTTP admin API phase. Security configuration, not a feature add-on.

---

### Pitfall 11: Password Comparison Timing Attacks

**What goes wrong:** Using `==` to compare cached password hashes allows timing-based side-channel attacks. The comparison short-circuits on the first differing byte, leaking information about how many prefix bytes match.

**Prevention:**
- Use `crypto/subtle.ConstantTimeCompare()` for all password/credential comparisons
- The current code (inspect.go line 108) uses `==` for password comparison -- this should be fixed as part of the refactor
- Store hashed passwords in the cache, not plaintext (depends on defcon.run schema)

**Phase:** Core credential validation implementation.

---

### Pitfall 12: DynamoDB Table Schema Mismatch with defcon.run

**What goes wrong:** The DynamoDB table key structure assumed by the proxy code does not match the actual defcon.run schema. GetItem returns nothing because the partition key name, sort key, or attribute names are wrong.

**Prevention:**
- Document the exact DynamoDB table name, partition key, sort key, and attribute names before writing any code
- Write a small integration test that performs a real GetItem against the defcon.run table
- Make table name and key attribute names configurable (YAML/env), not hardcoded

**Phase:** DynamoDB client setup phase. Verify schema before building the cache layer on top.

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| DynamoDB client setup | Using aws-sdk-go v1 (EOL) | Use v2 from the start; v1 and v2 can coexist |
| DynamoDB client setup | Eventually consistent reads | Set ConsistentRead: true for credential lookups |
| DynamoDB client setup | Schema mismatch with defcon.run | Verify table schema before coding; make configurable |
| Cache data structure | sync.Map misuse | Use map + sync.RWMutex, matching existing codebase patterns |
| Cache data structure | Unbounded growth | Hard cap + LRU eviction + TTL cleanup |
| Cache implementation | Cache stampede | Use golang.org/x/sync/singleflight |
| Cache implementation | Negative cache poisoning | Bounded negative cache with short TTL |
| Credential validation | Timing attacks on comparison | Use crypto/subtle.ConstantTimeCompare |
| Credential validation | Swap step forgotten | Extract validate and rewrite into separate named functions |
| HTTP admin API | Deadlock with cache mutex | Minimal critical sections; never nest locks; health check with timeout |
| HTTP admin API | No authentication | Bind to localhost; add API key for mutations |
| Cache eviction | Revocation window too long | Short TTL + mandatory eviction API + synchronous confirmation |

## Sources

- AWS SDK for Go v1 EOL announcement: https://aws.amazon.com/blogs/developer/announcing-end-of-support-for-aws-sdk-for-go-v1-on-july-31-2025/
- DynamoDB read consistency: https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html
- Go sync.Map documentation and use cases: https://pkg.go.dev/sync#Map
- MQTT brute force tooling (mqtt-pwn): https://mqtt-pwn.readthedocs.io/en/latest/plugins/brute.html
- DynamoDB best practices: https://dynomate.io/blog/dynamodb-best-practices/
- Go mutex deadlock patterns: https://iximiuz.com/en/posts/go-http-handlers-panic-and-deadlocks/
- Go thread-safe cache patterns: https://dev.to/vearutop/implementing-robust-in-memory-cache-with-go-196e
- Go caching strategies with TTL: https://oneuptime.com/blog/post/2026-02-01-go-caching-strategies/view
- DynamoDB race conditions: https://awsfundamentals.com/blog/understanding-and-handling-race-conditions-at-dynamodb
