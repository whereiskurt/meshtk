# Phase 1: Foundation - Context

**Gathered:** 2026-03-10
**Status:** Ready for planning

<domain>
## Phase Boundary

Config schema, in-memory cache data structure, and DynamoDB adapter — zero proxy behavior changes. The binary must start and operate identically to before. This phase builds the credential cache infrastructure in isolation so Phase 2 can wire it into the proxy CONNECT path.

</domain>

<decisions>
## Implementation Decisions

### DynamoDB Schema & Lookup
- Table name: `run-human-electro` (configurable via `Server.CredCache.TableName`)
- Lookup method: Scan with FilterExpression on `mqttUsername` attribute (no GSI exists for mqttUsername)
- Raw DynamoDB scan — no ElectroDB key format coupling
- Attributes to fetch: `mqttUsername`, `mqttPassword`, `mqttUsertype`
- Password storage: plaintext 12-char hex strings (SHA256 slice), not hashed
- Password comparison: hex-to-hex (`fmt.Sprintf("%x", p.Password)` matches stored `mqttPassword`)
- Exact password encoding (raw bytes vs UTF-8 string) to be verified against local DynamoDB during implementation
- Default region: `us-east-1`
- Configurable endpoint URL: `Server.CredCache.DynamoDBEndpoint` (empty string = standard AWS SDK resolution, non-empty = local dev like `http://localhost:8080`)
- AWS SDK v2 for DynamoDB (v1 is EOL)

### Cache Behavior
- Library: Otter v2 (variable TTL, built-in stats, W-TinyLFU eviction, max memory size)
- Default TTL: 15 minutes (configurable)
- Default max size: 64 MB (configurable)
- Cache key: mqttUsername (string)
- Cache value: full MQTT credential struct (username + password + usertype)
- Usertype stored but no behavior difference in Phase 1 — for future use
- Cache miss: block CONNECT with 5-second timeout on DynamoDB scan, reject on timeout
- DynamoDB unavailable: serve from cache for existing entries, reject uncached users (fail closed)

### Config Layout
- New nested struct: `Server.CredCache` containing:
  - `TTLSecs` (int, default 900)
  - `MaxSizeMB` (int, default 64)
  - `TableName` (string, default "run-human-electro")
  - `TableRegion` (string, default "us-east-1")
  - `DynamoDBEndpoint` (string, default "" — empty means standard AWS)
  - `Passthrough` ([]string, default ["ghosts", "kph", "ax", "meshmap"])
  - `TimeoutSecs` (int, default 5)
- Generic Mosquitto creds: `Server.ProxyUsername` and `Server.ProxyPassword` (replaces hardcoded "public"/"31337")
- Admin API address: `Server.AdminListenAddress` (default "localhost:9090" or similar)
- All fields support env var override via existing `MESHTK_` prefix

### Migration
- Remove `generateMQTTPassword()` function entirely — DynamoDB-only auth
- Remove `USER_CREATION_SEED` env var dependency
- Clean break, no seed-based fallback

### Package Structure
- New package: `internal/credcache/`
- Files organized by concern:
  - `types.go` — Credential struct, CredentialStore interface
  - `cache.go` — Otter v2 wrapper with TTL, stats, max size
  - `store.go` — DynamoDBStore implementing CredentialStore interface
- CredentialStore interface: `Fetch(username string) (*Credential, error)` — enables mock testing and future backend swaps
- DynamoDB adapter in same package (not separate pkg/)
- Full unit tests: cache TTL/hit/miss behavior + DynamoDB adapter with mock/local endpoint

### Claude's Discretion
- Exact Otter v2 API usage and configuration
- DynamoDB scan pagination handling (if table grows large)
- Test fixture strategy (embedded test data vs local DynamoDB)
- Internal struct field naming

</decisions>

<specifics>
## Specific Ideas

- User has a local DynamoDB docker on port 8080 (from defcon.run.34 stack) with the schema and test records — use for development and integration testing
- ElectroDB entity is `RunUser` in service `run`, version `1` — the DynamoDB items will have ElectroDB composite key prefixes on pk/sk fields but we scan on the raw `mqttUsername` attribute directly
- The `mqttUsertype` field has values: "rabbit", "admin", "wildhare", "og" — store all in cache for future phase use
- Current hardcoded passthrough usernames: "ghosts", "kph", "ax", "meshmap" — move to config as defaults

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/config/config.go`: Server struct at line 51 — add CredCache nested struct and ProxyUsername/ProxyPassword fields here
- `pkg/config/meshtk.yaml` (embedded): Add default values for new config fields
- `internal/app/server/inspect.go:65-70`: `generateMQTTPassword()` — to be removed
- `internal/app/server/inspect.go:99-115`: Current auth logic in ConnectPacket handler — will be replaced in Phase 2

### Established Patterns
- Config uses Viper with YAML + env var override (`MESHTK_` prefix, dot-to-underscore mapping)
- Struct tags use `default:"value"` pattern for defaults
- Single-letter receiver names: `(n *ServerCmd)`, `(c *Config)`
- Constructor pattern: `New[Type]()` returns pointer
- Interface pattern: small interfaces (see `Decider` in decider.go)
- Error handling: `if err != nil { log.Errorf(...); return }` pattern

### Integration Points
- `pkg/config/config.go` — Server struct gets new fields
- `internal/app/cmdargs.go` — Register new flags if needed (env var override may suffice)
- `go.mod` — Add otter v2 and aws-sdk-go-v2 dependencies
- No proxy code changes in Phase 1 — all new code is additive in `internal/credcache/`

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-foundation*
*Context gathered: 2026-03-10*
