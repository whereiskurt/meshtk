# Phase 1: Foundation - Research

**Researched:** 2026-03-10
**Domain:** Go in-memory caching (Otter v2), AWS DynamoDB (SDK v2), Viper config extension
**Confidence:** HIGH

## Summary

Phase 1 builds credential cache infrastructure in isolation: config schema additions to `Server` struct, an in-memory cache wrapping Otter v2, and a DynamoDB adapter that scans the defcon.run table for MQTT credentials. No existing proxy behavior changes -- all new code lives in `internal/credcache/` with unit tests.

The two key dependencies are **Otter v2** (v2.3.0, latest stable) for the cache layer and **AWS SDK for Go v2** for DynamoDB access. Both are well-documented and straightforward to integrate. The existing Viper config pattern with embedded YAML + env var override (`MESHTK_` prefix) is the established approach for all new config fields.

**Primary recommendation:** Create `internal/credcache/` package with three files (`types.go`, `cache.go`, `store.go`), extend `Server` struct with `CredCache` nested struct and `ProxyUsername`/`ProxyPassword` fields, add defaults to embedded `meshtk.yaml`, and write comprehensive unit tests.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Table name: `run-human-electro` (configurable via `Server.CredCache.TableName`)
- Lookup method: Scan with FilterExpression on `mqttUsername` attribute (no GSI)
- Raw DynamoDB scan -- no ElectroDB key format coupling
- Attributes to fetch: `mqttUsername`, `mqttPassword`, `mqttUsertype`
- Password storage: plaintext 12-char hex strings (SHA256 slice), not hashed
- Password comparison: hex-to-hex (`fmt.Sprintf("%x", p.Password)` matches stored `mqttPassword`)
- Default region: `us-east-1`
- Configurable endpoint URL: `Server.CredCache.DynamoDBEndpoint` (empty = standard AWS SDK resolution)
- AWS SDK v2 for DynamoDB
- Library: Otter v2 (variable TTL, built-in stats, W-TinyLFU eviction, max memory size)
- Default TTL: 15 minutes (configurable)
- Default max size: 64 MB (configurable)
- Cache key: mqttUsername (string), cache value: full MQTT credential struct
- New nested struct: `Server.CredCache` with fields: TTLSecs, MaxSizeMB, TableName, TableRegion, DynamoDBEndpoint, Passthrough, TimeoutSecs
- Generic Mosquitto creds: `Server.ProxyUsername` and `Server.ProxyPassword`
- Admin API address: `Server.AdminListenAddress`
- New package: `internal/credcache/` with files: `types.go`, `cache.go`, `store.go`
- CredentialStore interface: `Fetch(username string) (*Credential, error)`
- Remove `generateMQTTPassword()` function and `USER_CREATION_SEED` dependency

### Claude's Discretion
- Exact Otter v2 API usage and configuration
- DynamoDB scan pagination handling (if table grows large)
- Test fixture strategy (embedded test data vs local DynamoDB)
- Internal struct field naming

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CONF-01 | Cache TTL and max memory size configurable via YAML | Otter v2 Options struct supports MaximumSize/MaximumWeight and ExpiryCalculator; Viper nested struct with `default:` tags |
| CONF-02 | Admin API listen address configurable via YAML | Simple string field on Server struct, follows existing pattern |
| CONF-03 | DynamoDB table name configurable via YAML | String field in CredCache nested struct with default tag |
| CONF-04 | DynamoDB region configurable via YAML | String field in CredCache nested struct; aws config.LoadDefaultConfig supports region override |
| CRED-02 | Credential lookups cached in-memory with configurable max size | Otter v2 MaximumWeight with Weigher function or MaximumSize; v2.3.0 verified |
| CRED-03 | Cached entries expire after configurable TTL | Otter v2 ExpiryCreating/ExpiryWriting calculators with variable TTL support |
| AUTH-02 | Generic Mosquitto credentials from config (not hardcoded) | ProxyUsername/ProxyPassword fields on Server struct replacing hardcoded "public"/"31337" |
| AUTH-05 | Passthrough allowlist from config (not hardcoded) | []string field in CredCache struct replacing hardcoded list in inspect.go |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| github.com/maypok86/otter/v2 | v2.3.0 | In-memory cache with TTL, stats, W-TinyLFU eviction | User-locked decision; adaptive eviction, variable TTL, built-in stats, zero GC pressure |
| github.com/aws/aws-sdk-go-v2/service/dynamodb | latest | DynamoDB Scan operations | User-locked decision; v1 is EOL |
| github.com/aws/aws-sdk-go-v2/config | latest | AWS SDK configuration loading | Required by aws-sdk-go-v2 |
| github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression | latest | Type-safe filter/projection expressions | Avoids string-based expression bugs |
| github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue | latest | Unmarshal DynamoDB items to Go structs | Standard approach for SDK v2 |
| github.com/spf13/viper | v1.20.0 (existing) | Config: YAML + env var override | Already in project |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/aws/aws-sdk-go-v2/aws | latest | Core AWS types (aws.String, etc.) | Always with SDK v2 |
| github.com/maypok86/otter/v2/stats | (bundled) | Cache hit/miss/eviction counters | Stats recording for admin API |
| testing (stdlib) | - | Unit tests | All test files |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Otter v2 | N/A | Locked decision |
| AWS SDK v2 | N/A | Locked decision |
| Expression builder | Raw string expressions | Expression builder prevents typos and handles attribute aliasing |

**Installation:**
```bash
go get github.com/maypok86/otter/v2@v2.3.0
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/service/dynamodb
go get github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression
go get github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue
go get github.com/aws/aws-sdk-go-v2/aws
```

## Architecture Patterns

### Recommended Project Structure
```
internal/credcache/
  types.go          # Credential struct, CredentialStore interface
  cache.go          # Otter v2 wrapper: NewCache(), Get(), Set(), Stats(), Close()
  store.go          # DynamoDBStore implementing CredentialStore
  cache_test.go     # Unit tests for cache TTL, hit/miss, expiry
  store_test.go     # Unit tests for DynamoDB adapter (mock or local endpoint)
pkg/config/
  config.go         # Extended Server struct with CredCache, ProxyUsername, ProxyPassword, AdminListenAddress
  meshtk.yaml       # New default values for CredCache fields
```

### Pattern 1: CredentialStore Interface
**What:** Small interface for credential fetching, enabling mock testing and future backend swaps.
**When to use:** All DynamoDB access goes through this interface.
**Example:**
```go
// internal/credcache/types.go
type Credential struct {
    Username string `dynamodbav:"mqttUsername"`
    Password string `dynamodbav:"mqttPassword"`
    Usertype string `dynamodbav:"mqttUsertype"`
}

type CredentialStore interface {
    Fetch(ctx context.Context, username string) (*Credential, error)
}
```

### Pattern 2: Otter v2 Cache Wrapper
**What:** Thin wrapper around Otter providing typed access and hiding cache internals.
**When to use:** All cache operations go through this wrapper.
**Example:**
```go
// internal/credcache/cache.go
// Source: https://pkg.go.dev/github.com/maypok86/otter/v2
type Cache struct {
    inner *otter.Cache[string, *Credential]
}

func NewCache(ttlSecs int, maxSizeMB int) (*Cache, error) {
    counter := stats.NewCounter()
    c, err := otter.New[string, *Credential](&otter.Options[string, *Credential]{
        MaximumSize:      maxSizeMB * 1024, // approximate entry count from MB
        ExpiryCalculator: otter.ExpiryWriting[string, *Credential](time.Duration(ttlSecs) * time.Second),
        StatsRecorder:    counter,
    })
    if err != nil {
        return nil, err
    }
    return &Cache{inner: c}, nil
}

func (c *Cache) Get(username string) (*Credential, bool) {
    return c.inner.GetIfPresent(username)
}

func (c *Cache) Set(username string, cred *Credential) {
    c.inner.Set(username, cred)
}

func (c *Cache) Stats() stats.Stats {
    return c.inner.Stats()
}
```

### Pattern 3: DynamoDB Scan with Expression Builder
**What:** Type-safe scan filtering on `mqttUsername` with projection to fetch only needed attributes.
**When to use:** Every cache miss triggers this.
**Example:**
```go
// internal/credcache/store.go
// Source: https://docs.aws.amazon.com/code-library/latest/ug/go_2_dynamodb_code_examples.html
func (s *DynamoDBStore) Fetch(ctx context.Context, username string) (*Credential, error) {
    filt := expression.Name("mqttUsername").Equal(expression.Value(username))
    proj := expression.NamesList(
        expression.Name("mqttUsername"),
        expression.Name("mqttPassword"),
        expression.Name("mqttUsertype"),
    )
    expr, err := expression.NewBuilder().WithFilter(filt).WithProjection(proj).Build()
    if err != nil {
        return nil, fmt.Errorf("build expression: %w", err)
    }

    result, err := s.client.Scan(ctx, &dynamodb.ScanInput{
        TableName:                 aws.String(s.tableName),
        FilterExpression:          expr.Filter(),
        ExpressionAttributeNames:  expr.Names(),
        ExpressionAttributeValues: expr.Values(),
        ProjectionExpression:      expr.Projection(),
    })
    if err != nil {
        return nil, fmt.Errorf("dynamodb scan: %w", err)
    }

    if len(result.Items) == 0 {
        return nil, ErrNotFound
    }

    var cred Credential
    if err := attributevalue.UnmarshalMap(result.Items[0], &cred); err != nil {
        return nil, fmt.Errorf("unmarshal credential: %w", err)
    }
    return &cred, nil
}
```

### Pattern 4: Config Extension (Existing Viper Pattern)
**What:** Nested struct in Server with `default:` tags, embedded YAML defaults, env var override.
**When to use:** All new configuration.
**Example:**
```go
// pkg/config/config.go -- add to Server struct
type CredCacheConfig struct {
    TTLSecs          int      `default:"900"`
    MaxSizeMB        int      `default:"64"`
    TableName        string   `default:"run-human-electro"`
    TableRegion      string   `default:"us-east-1"`
    DynamoDBEndpoint string   `default:""`
    Passthrough      []string `default:"['ghosts', 'kph', 'ax', 'meshmap']"`
    TimeoutSecs      int      `default:"5"`
}

type Server struct {
    // ... existing fields ...
    CredCache          CredCacheConfig `json:"CredCache"`
    ProxyUsername      string          `default:"public"`
    ProxyPassword      string          `default:"31337"`
    AdminListenAddress string          `default:"localhost:9090"`
}
```

### Anti-Patterns to Avoid
- **Coupling to ElectroDB key format:** Scan on `mqttUsername` directly, never parse `pk`/`sk` composite keys.
- **Using aws-sdk-go v1 for new code:** The project has v1 for existing S3 code, but new DynamoDB code must use v2. Both can coexist in go.mod.
- **Hardcoding AWS credentials:** Use default credential chain (env vars, instance profile, SSO). Never hardcode.
- **Using MaximumWeight for MB limit without Weigher:** If using MaximumWeight, you must provide a Weigher function. MaximumSize (entry count) is simpler if approximate sizing is acceptable.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| In-memory cache with TTL | Custom map + goroutine reaper | Otter v2 | Eviction policy, thread safety, memory management, stats -- hundreds of edge cases |
| DynamoDB filter expressions | String concatenation | `feature/dynamodb/expression` builder | Handles attribute aliasing, prevents injection-like bugs |
| DynamoDB item marshaling | Manual map[string]AttributeValue parsing | `feature/dynamodb/attributevalue` | Struct tag-based, type-safe, handles nested types |
| Config env var binding | os.Getenv() calls | Viper AutomaticEnv + MESHTK_ prefix | Already wired, dot-to-underscore mapping works for nested structs |
| Cache statistics | Atomic counters | Otter stats.Counter | Thread-safe, already integrated with cache internals |

**Key insight:** The entire cache infrastructure is built from two well-tested libraries (Otter v2, AWS SDK v2) wrapped in a thin interface layer. The only custom code is the glue.

## Common Pitfalls

### Pitfall 1: Viper Nested Struct Env Var Mapping
**What goes wrong:** Nested struct fields like `Server.CredCache.TTLSecs` may not auto-bind to `MESHTK_SERVER_CREDCACHE_TTLSECS` without explicit binding.
**Why it happens:** Viper's AutomaticEnv with SetEnvKeyReplacer replaces `.` with `_`, but nested struct field names in YAML must exactly match the env var path after prefix stripping.
**How to avoid:** Ensure the embedded `meshtk.yaml` has the nested structure (`Server.CredCache.TTLSecs`) so Viper knows about the keys. Test env var override explicitly in unit tests.
**Warning signs:** Config values always return defaults despite env vars being set.

### Pitfall 2: DynamoDB Scan Returns All Items
**What goes wrong:** FilterExpression is applied after the scan reads all items from the table -- you pay for read capacity on every item.
**Why it happens:** Scan always reads the full table (or up to 1MB per page), then applies the filter. This is fine for small tables but expensive at scale.
**How to avoid:** For Phase 1 this is acceptable (the table is small). The cache minimizes scan frequency. Document that a GSI on `mqttUsername` would eliminate this cost for Phase 2+ optimization.
**Warning signs:** High DynamoDB read cost, slow lookups.

### Pitfall 3: Otter v2 MaximumSize vs MaximumWeight
**What goes wrong:** Using MaximumSize (entry count) when the config says "max 64 MB" -- these are different things.
**Why it happens:** MaximumSize bounds by entry count. MaximumWeight with a Weigher function bounds by weight (which can represent bytes).
**How to avoid:** For credential structs (small, uniform size), MaximumSize with a reasonable estimate is simplest. A Credential struct is approximately 50-100 bytes, so 64MB / 100 bytes = ~640K entries. Set MaximumSize to a sensible upper bound. Alternatively, use MaximumWeight with a Weigher that returns approximate byte size.
**Warning signs:** Cache either holds too few entries or uses too much memory.

### Pitfall 4: AWS SDK v2 Config Loading Region
**What goes wrong:** `config.LoadDefaultConfig` uses `AWS_REGION` env var or `~/.aws/config` region, which may differ from the DynamoDB table's region.
**Why it happens:** The SDK's default config picks up whatever region is configured globally.
**How to avoid:** Explicitly set the region when creating the DynamoDB client: `dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.Region = tableRegion })`.
**Warning signs:** "ResourceNotFoundException" errors because the client queries the wrong region.

### Pitfall 5: Existing aws-sdk-go v1 Coexistence
**What goes wrong:** Import path conflicts or confusing which SDK version to use.
**Why it happens:** go.mod already has `github.com/aws/aws-sdk-go` (v1) for S3 code.
**How to avoid:** Both v1 and v2 coexist fine in go.mod (different module paths). Never import v1 DynamoDB packages. Leave existing S3 code on v1 untouched.
**Warning signs:** Accidentally importing `github.com/aws/aws-sdk-go/service/dynamodb` (v1 path).

### Pitfall 6: Otter v2 Requires Go 1.22+
**What goes wrong:** Build failures if Go version is too old.
**Why it happens:** Otter v2 uses Go generics and iter.Seq (Go 1.23+).
**How to avoid:** Project uses Go 1.24.1 (confirmed in go.mod), so this is fine. Just be aware.
**Warning signs:** Compile errors mentioning generics or iterators.

## Code Examples

### Creating the DynamoDB Client with Custom Endpoint
```go
// Source: https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html
func NewDynamoDBStore(tableName, region, endpoint string) (*DynamoDBStore, error) {
    cfg, err := config.LoadDefaultConfig(context.TODO())
    if err != nil {
        return nil, fmt.Errorf("load aws config: %w", err)
    }

    opts := []func(*dynamodb.Options){
        func(o *dynamodb.Options) {
            o.Region = region
        },
    }
    if endpoint != "" {
        opts = append(opts, func(o *dynamodb.Options) {
            o.BaseEndpoint = aws.String(endpoint)
        })
    }

    client := dynamodb.NewFromConfig(cfg, opts...)
    return &DynamoDBStore{
        client:    client,
        tableName: tableName,
    }, nil
}
```

### Cache with Stats and TTL
```go
// Source: https://pkg.go.dev/github.com/maypok86/otter/v2
func NewCache(ttlSecs int, maxEntries int) (*Cache, error) {
    counter := stats.NewCounter()
    inner, err := otter.New[string, *Credential](&otter.Options[string, *Credential]{
        MaximumSize:      maxEntries,
        ExpiryCalculator: otter.ExpiryWriting[string, *Credential](time.Duration(ttlSecs) * time.Second),
        StatsRecorder:    counter,
    })
    if err != nil {
        return nil, err
    }
    return &Cache{inner: inner}, nil
}
```

### Config YAML Defaults (to add to meshtk.yaml)
```yaml
Server:
  # ... existing fields ...
  ProxyUsername: "public"
  ProxyPassword: "31337"
  AdminListenAddress: "localhost:9090"
  CredCache:
    TTLSecs: 900
    MaxSizeMB: 64
    TableName: "run-human-electro"
    TableRegion: "us-east-1"
    DynamoDBEndpoint: ""
    TimeoutSecs: 5
    Passthrough:
      - "ghosts"
      - "kph"
      - "ax"
      - "meshmap"
```

### Env Var Mapping (Viper Pattern)
```
MESHTK_SERVER_CREDCACHE_TTLSECS=300
MESHTK_SERVER_CREDCACHE_TABLENAME=my-table
MESHTK_SERVER_CREDCACHE_TABLEREGION=us-west-2
MESHTK_SERVER_CREDCACHE_DYNAMODBENDPOINT=http://localhost:8080
MESHTK_SERVER_PROXYUSERNAME=myuser
MESHTK_SERVER_PROXYPASSWORD=mypass
MESHTK_SERVER_ADMINLISTENADDRESS=0.0.0.0:9090
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| aws-sdk-go v1 | aws-sdk-go-v2 | v1 EOL July 2025 | Must use v2 for new code; v1 coexists for S3 |
| Otter v1 (builder pattern) | Otter v2 (Options struct) | June 2025 | API is different: `otter.Must(&Options{})` not `otter.MustBuilder()` |
| DynamoDB string expressions | Expression builder package | Available since SDK v2 GA | Type-safe, handles aliasing automatically |

**Deprecated/outdated:**
- `generateMQTTPassword()` with `USER_CREATION_SEED`: being removed in this phase (DynamoDB-only auth)
- Hardcoded passthrough usernames in inspect.go: moving to config
- Hardcoded "public"/"31337" Mosquitto creds: moving to config

## Open Questions

1. **MaximumSize vs MaximumWeight for Otter v2**
   - What we know: Config says "64 MB max". Otter supports both entry-count bounds and weight-based bounds.
   - What's unclear: Whether the user wants strict memory bounding (requires Weigher) or approximate entry count.
   - Recommendation: Use MaximumSize with a generous entry count (e.g., 100,000). Credential structs are tiny (~100 bytes each), so even 100K entries = ~10MB. The 64MB config value can be converted to an approximate entry count. This is Claude's discretion per CONTEXT.md.

2. **DynamoDB Scan Pagination**
   - What we know: A single Scan returns up to 1MB of data. If the table has many items, pagination may be needed.
   - What's unclear: Current table size.
   - Recommendation: Implement pagination in the initial Scan (loop on LastEvaluatedKey) to be safe. This is Claude's discretion per CONTEXT.md.

3. **Viper Default Tag for Slice Fields**
   - What we know: The project uses `default:"['value1', 'value2']"` syntax for slice defaults in struct tags (see Fleet.BehaviourTag).
   - What's unclear: Whether this syntax works reliably with Viper for the Passthrough []string field.
   - Recommendation: Set defaults in the embedded YAML file (more reliable for slices) rather than relying solely on struct tags.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go stdlib `testing` (no existing test framework in project) |
| Config file | None -- Go tests work out of the box |
| Quick run command | `go test ./internal/credcache/ -v -count=1` |
| Full suite command | `go test ./... -v -count=1` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| CONF-01 | TTL and max size configurable via YAML/env | unit | `go test ./pkg/config/ -run TestCredCacheConfig -v` | No -- Wave 0 |
| CONF-02 | AdminListenAddress configurable | unit | `go test ./pkg/config/ -run TestAdminListenAddress -v` | No -- Wave 0 |
| CONF-03 | TableName configurable | unit | `go test ./pkg/config/ -run TestCredCacheConfig -v` | No -- Wave 0 |
| CONF-04 | TableRegion configurable | unit | `go test ./pkg/config/ -run TestCredCacheConfig -v` | No -- Wave 0 |
| CRED-02 | Cache stores/returns credentials, max size | unit | `go test ./internal/credcache/ -run TestCache -v` | No -- Wave 0 |
| CRED-03 | Cache entries expire after TTL | unit | `go test ./internal/credcache/ -run TestCacheTTL -v` | No -- Wave 0 |
| AUTH-02 | ProxyUsername/ProxyPassword from config | unit | `go test ./pkg/config/ -run TestProxyCreds -v` | No -- Wave 0 |
| AUTH-05 | Passthrough allowlist from config | unit | `go test ./pkg/config/ -run TestPassthrough -v` | No -- Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/credcache/ -v -count=1`
- **Per wave merge:** `go test ./... -v -count=1`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/credcache/cache_test.go` -- covers CRED-02, CRED-03
- [ ] `internal/credcache/store_test.go` -- covers DynamoDB adapter (mock client)
- [ ] `pkg/config/config_test.go` -- covers CONF-01 through CONF-04, AUTH-02, AUTH-05
- [ ] No framework install needed -- Go stdlib `testing` is built in

## Sources

### Primary (HIGH confidence)
- [Otter v2 pkg.go.dev](https://pkg.go.dev/github.com/maypok86/otter/v2) - Full API reference: Cache methods, Options struct, stats package
- [Otter v2 Getting Started](https://maypok86.github.io/otter/user-guide/v2/getting-started/) - Cache creation and basic operations
- [Otter GitHub Releases](https://github.com/maypok86/otter/releases) - v2.3.0 is latest (Dec 2025)
- [AWS SDK Go v2 DynamoDB examples](https://docs.aws.amazon.com/code-library/latest/ug/go_2_dynamodb_code_examples.html) - Scan with filter expressions
- [AWS SDK Go v2 expression package](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression) - Expression builder API
- [AWS SDK Go v2 endpoint configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html) - BaseEndpoint for custom endpoints

### Secondary (MEDIUM confidence)
- [DynamoDB Go SDK Scan operations](https://dynobase.dev/dynamodb-golang-query-examples/) - Verified patterns against official docs

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries locked by user decisions, versions verified against releases
- Architecture: HIGH - Package structure locked by user decisions, patterns verified against API docs
- Pitfalls: HIGH - Based on known DynamoDB scan behavior and Viper config patterns from existing codebase
- Validation: MEDIUM - No existing test infrastructure to validate against; test strategy based on Go stdlib conventions

**Research date:** 2026-03-10
**Valid until:** 2026-04-10 (stable libraries, 30-day window)
