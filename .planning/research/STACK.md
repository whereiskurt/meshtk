# Technology Stack: Credential Cache Milestone

**Project:** MeshTK MQTT Proxy Credential Cache
**Researched:** 2026-03-10
**Overall Confidence:** HIGH

## Decision Context

This stack is for a **new milestone** on an existing Go 1.24.1 codebase. The existing project uses Cobra/Viper, Logrus, aws-sdk-go v1 (S3), and OpenTelemetry. The credential cache needs: DynamoDB backend, in-memory cache with TTL, and an HTTP admin API.

## Recommended Stack

### In-Memory Cache

| Technology | Version | Purpose | Why | Confidence |
|------------|---------|---------|-----|------------|
| **maypok86/otter/v2** | v2.3.0 | TTL-based credential cache | Best-in-class Go cache library as of 2025. Generics-based API, per-item variable TTL, TTL refresh on access (ideal for credential sessions), built-in stats collection for admin API hit/miss reporting, W-TinyLFU eviction policy. Outperforms ristretto in both throughput and hit rate. | HIGH |

**Rationale:** Otter v2 is the clear winner for this use case:
- **Variable TTL** via `WithVariableTTL()` -- credentials can have per-entry TTL based on DynamoDB data or a global default.
- **TTL refresh on read** via `ExpiryAccessing` -- every successful credential check resets the TTL, keeping active users cached while idle entries expire. This is exactly the behavior wanted for MQTT credential caching.
- **Built-in stats** via `stats.NewCounter()` -- cache hit/miss/eviction counts available without custom instrumentation, feeds directly into the HTTP admin API.
- **Generics** -- type-safe `Cache[string, Credential]` with no interface{} casting.
- **No GC pressure** -- designed for high-throughput with minimal allocations.

### DynamoDB Client

| Technology | Version | Purpose | Why | Confidence |
|------------|---------|---------|-----|------------|
| **aws-sdk-go-v2** | latest (config v1.29+) | DynamoDB credential lookups | AWS SDK v1 reached end-of-support July 2025. SDK v2 offers ~50% CPU reduction and ~58% fewer allocations for DynamoDB operations. Module paths are different (`github.com/aws/aws-sdk-go-v2/...`) so v1 (S3) and v2 (DynamoDB) coexist cleanly in the same go.mod. | HIGH |
| **aws-sdk-go-v2/service/dynamodb** | v1.56+ | DynamoDB API client | GetItem for single credential lookups, Scan/Query for bulk cache warming if needed. | HIGH |
| **aws-sdk-go-v2/feature/dynamodb/attributevalue** | latest | Marshal/Unmarshal Go structs | Replaces v1's `dynamodbattribute` package. Struct tags via `dynamodbav` for clean mapping to defcon.run schema. | HIGH |
| **aws-sdk-go-v2/config** | latest | AWS credential chain loading | Uses default credential chain (ECS task role, EC2 instance profile, env vars) -- matches existing deployment model. | HIGH |

**Rationale: Why SDK v2 for DynamoDB despite v1 for S3:**
- SDK v1 is **end-of-support** since July 2025. No security patches, no new features.
- v1 and v2 have **different Go module paths** -- they coexist without conflict in the same binary. No need to migrate S3 to v2 as part of this milestone.
- DynamoDB is a **new integration** -- no existing v1 DynamoDB code to migrate.
- Performance matters: every cache miss triggers a DynamoDB call on the MQTT connection hot path.

### HTTP Admin API

| Technology | Version | Purpose | Why | Confidence |
|------------|---------|---------|-----|------------|
| **net/http (stdlib)** | Go 1.24.1 | HTTP admin endpoints | Go 1.22+ ServeMux supports method routing (`"DELETE /cache/{username}"`) and path variables natively. The admin API has ~4 endpoints total -- no middleware framework needed. Zero dependencies. Matches Go idiom for small internal APIs. | HIGH |

**Rationale: Why stdlib over chi/gin:**
- The admin API is **tiny**: evict entry, refresh cache, get stats, health check. Four handlers at most.
- Go 1.22+ `http.ServeMux` now supports `"GET /cache/stats"` and `"DELETE /cache/{username}"` patterns natively -- the exact features that previously required chi.
- No middleware requirements beyond basic logging (Logrus handler wrapper is trivial).
- Adding chi or gin to a Cobra/Viper CLI app for 4 endpoints adds dependency weight with zero benefit.
- The existing codebase has **no HTTP server** -- adding stdlib keeps the dependency footprint minimal.

### Supporting Libraries

| Library | Version | Purpose | When to Use | Confidence |
|---------|---------|---------|-------------|------------|
| **encoding/json (stdlib)** | Go 1.24.1 | JSON responses for admin API | All admin API responses (stats, errors, confirmation) | HIGH |
| **sync (stdlib)** | Go 1.24.1 | Mutex for cache operations if needed | Protecting cache initialization/teardown -- Otter itself is concurrent-safe | HIGH |
| **context (stdlib)** | Go 1.24.1 | Request cancellation, DynamoDB timeouts | Pass context through from HTTP handlers to DynamoDB calls | HIGH |
| **sirupsen/logrus** | v1.9.3 (existing) | Structured logging | Already in codebase, use for cache hit/miss/error logging | HIGH |
| **spf13/viper** | v1.20.0 (existing) | Configuration | Cache TTL duration, DynamoDB table name, admin API listen address, generic Mosquitto creds | HIGH |

**No new supporting dependencies needed.** The existing stack (Logrus, Viper, Cobra) covers logging, config, and CLI integration.

## Alternatives Considered

| Category | Recommended | Alternative | Why Not |
|----------|-------------|-------------|---------|
| Cache | **Otter v2** | dgraph-io/ristretto | Ristretto v1 API is clunky (no generics), v2 exists but Otter v2 has better benchmarks, cleaner API, and built-in stats. Ristretto also had maintenance gaps in 2024. |
| Cache | **Otter v2** | jellydator/ttlcache v3 | Simpler API but no admission/eviction policy -- just LRU. Otter's W-TinyLFU achieves significantly higher hit rates under mixed workloads. |
| Cache | **Otter v2** | patrickmn/go-cache | Unmaintained since 2022, no generics, naive expiration (scans entire map). Not suitable for production. |
| Cache | **Otter v2** | allegro/bigcache | Designed for large data volumes with minimal GC. No per-item TTL support, byte-oriented API requires manual serialization. Wrong tool for structured credential data. |
| Cache | **Otter v2** | sync.Map + time.AfterFunc | Hand-rolled TTL cache is tempting but gets TTL refresh, eviction, stats, and concurrency wrong. Not worth the bugs. |
| DynamoDB | **aws-sdk-go-v2** | aws-sdk-go v1 | End-of-support. No security patches. 50% worse performance on DynamoDB operations. |
| DynamoDB | **aws-sdk-go-v2** | guregu/dynamo | Nice high-level API but adds another abstraction layer. For simple GetItem calls, the SDK v2 attributevalue package is sufficient and keeps us closer to AWS docs/examples. |
| HTTP | **net/http stdlib** | go-chi/chi v5 | Overkill for 4 endpoints. Chi's value is middleware composition and route grouping -- neither needed here. |
| HTTP | **net/http stdlib** | gin-gonic/gin | Even more overkill. Gin's custom context, binding, and validation are for full web apps. Adds heavy transitive dependencies. Non-idiomatic for internal admin APIs. |
| HTTP | **net/http stdlib** | labstack/echo | Same rationale as gin. Framework-level features not needed for a handful of admin endpoints. |

## Version Pinning Strategy

```
# New dependencies to add
go get github.com/maypok86/otter/v2@v2.3.0
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/service/dynamodb@latest
go get github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue@latest
```

**Note on vendoring:** The existing project vendors dependencies (`vendor/` directory present). After adding new deps, run `go mod vendor` to keep vendor directory in sync.

## What NOT to Use

| Technology | Why Not |
|------------|---------|
| **Redis/Memcached** | Explicitly out of scope (PROJECT.md). Single-process proxy -- external cache adds latency and infrastructure complexity for zero benefit. |
| **GORM/database ORM** | DynamoDB is not SQL. The SDK v2 attributevalue package handles marshaling natively. |
| **go-cache** | Abandoned. Last meaningful update was 2022. |
| **aws-sdk-go v1 for DynamoDB** | End-of-support. Would accumulate tech debt on day one. |
| **gRPC for admin API** | The admin API is for human operators (curl, browser). HTTP+JSON is the right interface. gRPC is already used for the inspector protocol -- don't conflate the two. |
| **Prometheus client** | The admin API serves cache stats directly. Adding Prometheus for 3 counters (hit/miss/eviction) is over-engineering. If OTel metrics are wanted later, the existing OTel setup covers it. |

## Integration Notes

### AWS SDK v1 + v2 Coexistence

The existing codebase uses `github.com/aws/aws-sdk-go` (v1) for S3. The new DynamoDB code will use `github.com/aws/aws-sdk-go-v2/...`. These are entirely separate Go modules with different import paths -- they coexist without conflict. There is no need to migrate S3 to v2 as part of this milestone. A future milestone could unify on v2, but it is not a prerequisite.

### Configuration Integration

New Viper config keys to add:

```yaml
Server:
  CredentialCache:
    Enabled: true
    DynamoDBTable: "defcon-credentials"    # DynamoDB table name
    DynamoDBRegion: "us-east-1"            # AWS region for DynamoDB
    TTL: "15m"                             # Default cache TTL
    MaxEntries: 10000                      # Maximum cache size
    AdminListenAddress: ":8080"            # HTTP admin API bind address
  GenericMQTT:
    Username: "proxy-generic"              # Swapped into forwarded CONNECT
    Password: "proxy-generic-pass"         # Swapped into forwarded CONNECT
```

### Cache Type Definition

```go
// Credential represents a cached DynamoDB credential entry
type Credential struct {
    Username     string
    PasswordHash string    // bcrypt or argon2 hash from DynamoDB
    ExpiresAt    time.Time // Optional per-credential expiry from DynamoDB
}

// Cache type: otter.Cache[string, Credential]
// Key: MQTT username (string)
// Value: Credential struct
```

## Sources

- [Otter v2 GitHub releases](https://github.com/maypok86/otter/releases) -- v2.3.0 released Dec 22, 2025
- [Otter cache evolution blog](https://maypok86.github.io/otter/blog/cache-evolution/) -- benchmarks vs ristretto, bigcache
- [Otter v2 pkg.go.dev](https://pkg.go.dev/github.com/maypok86/otter/v2)
- [AWS SDK Go v2 migration guide](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/migrate-gosdk.html)
- [AWS SDK Go v1 end-of-support announcement](https://aws.amazon.com/blogs/developer/upgrading-your-aws-sdk-for-go-from-v1-to-v2-with-amazon-q-developer/)
- [AWS SDK Go v2 DynamoDB package](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/dynamodb) -- v1.56+
- [AWS SDK Go v2 attributevalue](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue)
- [Go 1.22 ServeMux enhancements](https://www.calhoun.io/go-servemux-vs-chi/) -- method routing, path variables
- [Go HTTP router comparison](https://www.alexedwards.net/blog/which-go-router-should-i-use)
