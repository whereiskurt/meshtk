# Architecture Patterns

**Domain:** MQTT proxy credential cache integration
**Researched:** 2026-03-10

## Current Proxy Pipeline (As-Is)

Understanding the existing flow is critical because the credential cache must slot into it without disrupting the packet-per-goroutine model.

```
Client TCP Connect
       |
       v
  proxyproto.Listener.Accept()    [cmd.go:StartProxyServer]
       |
       v
  handleProxy(conn)               [proxy.go] -- one goroutine per client
       |
       +---> net.DialTimeout() to backend (Mosquitto)
       |
       +---> handleBackend() goroutine (backend -> client relay)
       |
       v
  [per-packet loop]
       |
       v
  packets.ReadPacket()            -- Paho library parses raw MQTT
       |
       v
  InspectorPacket created
       |
       v
  ip.inspectRawPacket(n)          [inspect.go]
       |
       +---> ConnectPacket case:
       |       1. Extract username, password, clientID
       |       2. Store in ConnTrack map
       |       3. Hardcoded allowlist check (ghosts, kph, ax, meshmap)
       |       4. Format validation (12-char hex username + password)
       |       5. generateMQTTPassword() verification against USER_CREATION_SEED env var
       |       6. On success: SWAP creds to "public"/"31337"
       |       7. On failure: CLOBBER to ""/"" (causes hangup in handleProxy)
       |
       +---> PublishPacket/SubscribePacket/etc: ConnTrack lookup + Meshtastic inspection
       |
       v
  PacketDecider.Decide(ip)        [decider.go] -- rule-based allow/block/kill
       |
       v
  Serialize + Forward to backend
       |
       v
  Backend response relayed to client
```

**Key observation:** Credential validation currently lives INSIDE `inspectRawPacket()` in the `ConnectPacket` switch case. It is tightly coupled -- the validation, the credential swap, and the rejection all happen in the same block. The cache integration must decompose this.

## Recommended Architecture (To-Be)

### Component Boundaries

| Component | Package | Responsibility | Depends On | Depended On By |
|-----------|---------|---------------|------------|----------------|
| **CredentialCache** | `internal/credcache/cache.go` | In-memory map with TTL expiry, hit/miss counters | Nothing (pure Go, sync.RWMutex) | Authenticator, HTTP API |
| **DynamoDBStore** | `internal/credcache/dynamo.go` | Fetch credentials from DynamoDB on cache miss | AWS SDK v2 (dynamodb) | Authenticator |
| **Authenticator** | `internal/credcache/auth.go` | Orchestrates validate flow: cache lookup -> DynamoDB fallback -> cache populate | CredentialCache, DynamoDBStore | Proxy pipeline (inspect.go) |
| **CredCacheHTTP** | `internal/credcache/http.go` | REST endpoints for eviction, refresh, stats | CredentialCache, Authenticator | HTTP listener in cmd.go |
| **Config extensions** | `pkg/config/config.go` | DynamoDB table name, region, TTL duration, generic creds, HTTP listen addr | Viper | All credential components |

### Why This Boundary Split

**CredentialCache is a pure data structure.** It knows nothing about DynamoDB or MQTT. This makes it trivially testable and reusable. It is a `sync.RWMutex`-protected `map[string]CachedCredential` with TTL checking on read.

**DynamoDBStore is an adapter.** It implements a `CredentialStore` interface (`FetchCredential(username string) (*Credential, error)`). If the backing store ever changes, only this file changes. It uses the AWS SDK v2 default credential chain (already proven working in the S3Mover code in `pkg/network/`).

**Authenticator is the orchestration layer.** It ties cache + store together with the validate-swap-reject logic. The proxy pipeline calls `Authenticator.ValidateAndSwap(packet)` and gets back allow/reject. This replaces the inline logic currently in `inspectRawPacket()`.

**CredCacheHTTP is an admin surface.** It runs on a separate goroutine alongside the proxy listener. Endpoints: `DELETE /cache/{username}` (evict), `POST /cache/{username}/refresh` (force re-fetch), `GET /cache/stats` (hit/miss/size).

### Component Diagram

```
                                     +-------------------+
                                     |   DynamoDB Table   |
                                     | (defcon.run schema)|
                                     +--------+----------+
                                              ^
                                              | GetItem (on miss)
                                              |
+----------+     +----------+     +-----------+-----------+
|  Client   | --> |  Proxy   | --> |     Authenticator     |
| (MQTT     |     | Pipeline |     |                       |
|  CONNECT) |     | inspect  |     | ValidateAndSwap()     |
+----------+     | RawPacket|     |   1. cache.Get(user)  |
                  +----------+     |   2. if miss: store   |
                       |           |      .Fetch(user)     |
                       |           |   3. cache.Set(user)  |
                       v           |   4. verify password  |
                  +----------+     |   5. swap creds       |
                  | Mosquitto|     +-----------+-----------+
                  | Backend  |                |
                  +----------+     +----------v----------+
                                   |   CredentialCache    |
                                   | map[string]CachedCred|
                                   | TTL, RWMutex, stats  |
                                   +---------------------+
                                              ^
                                              | evict/refresh/stats
                                   +----------+----------+
                                   |    CredCacheHTTP     |
                                   | :8080 (configurable) |
                                   +---------------------+
```

## Data Flow

### CONNECT Packet Flow (After Integration)

```
1. Client sends MQTT CONNECT with username + password
2. handleProxy() reads packet, creates InspectorPacket
3. inspectRawPacket() hits ConnectPacket case:
   a. Extract username, password, clientID (unchanged)
   b. Store in ConnTrack (unchanged)
   c. Call Authenticator.ValidateAndSwap(connectPacket) [NEW]
      i.   cache.Get(username) -> hit? use cached credential
      ii.  cache miss -> dynamoStore.Fetch(username) -> DynamoDB GetItem
      iii. DynamoDB returns credential -> cache.Set(username, credential, TTL)
      iv.  DynamoDB returns nothing -> reject (user not found)
      v.   Compare password against stored credential
      vi.  On match: swap packet creds to generic Mosquitto creds from config
      vii. On mismatch: clobber to ""/"" (existing hangup behavior)
4. handleProxy() continues with existing empty-username check (line 73-76)
5. If valid: packet forwarded to Mosquitto with swapped creds
6. If invalid: connection dropped (existing behavior)
```

### Cache Miss Flow

```
Authenticator.ValidateAndSwap(username, password)
  |
  +-> cache.Get(username)
  |     returns: nil (miss), increments miss counter
  |
  +-> dynamoStore.Fetch(username)
  |     AWS SDK: dynamodb.GetItem(TableName, Key: {username})
  |     returns: {username, passwordHash, createdAt, ...} or nil
  |
  +-> if credential found:
  |     cache.Set(username, credential, defaultTTL)
  |     verify password against credential
  |     return allow/reject
  |
  +-> if credential not found:
        return reject (unknown user)
```

### Cache Hit Flow

```
Authenticator.ValidateAndSwap(username, password)
  |
  +-> cache.Get(username)
  |     returns: CachedCredential{credential, expiresAt}
  |     checks: time.Now() < expiresAt? yes -> increment hit counter
  |
  +-> verify password against cached credential
  |
  +-> return allow/reject
```

### TTL Expiry Flow

```
cache.Get(username)
  |
  +-> entry exists but time.Now() >= expiresAt
  |     delete entry from map
  |     increment miss counter
  |     return nil (treated as miss)
```

### HTTP Admin Flow

```
DELETE /cache/{username}
  -> cache.Delete(username)
  -> 200 OK / 404 Not Found

POST /cache/{username}/refresh
  -> dynamoStore.Fetch(username)
  -> cache.Set(username, freshCredential, TTL)
  -> 200 OK / 404 (user not in DynamoDB)

GET /cache/stats
  -> return {size, hits, misses, hitRate, oldestEntry, newestEntry}
```

## Data Structures

```go
// CachedCredential wraps a credential with expiry metadata.
type CachedCredential struct {
    Username     string
    PasswordHash string    // The expected password (hashed or raw per defcon.run schema)
    ExpiresAt    time.Time
    FetchedAt    time.Time
}

// CredentialCache is the in-memory TTL cache.
type CredentialCache struct {
    mu      sync.RWMutex
    entries map[string]*CachedCredential
    ttl     time.Duration
    stats   CacheStats
}

// CacheStats tracks operational metrics.
type CacheStats struct {
    Hits       uint64
    Misses     uint64
    Evictions  uint64
    Size       int
}

// CredentialStore is the backend adapter interface.
type CredentialStore interface {
    Fetch(username string) (*CachedCredential, error)
}

// Authenticator orchestrates cache + store + validation.
type Authenticator struct {
    cache        *CredentialCache
    store        CredentialStore
    genericUser  string  // from config: what to swap TO
    genericPass  string  // from config: what to swap TO
}
```

## Patterns to Follow

### Pattern 1: Lazy-Load Cache (Read-Through)

**What:** Cache is empty at startup. Entries are populated on first access per username. No pre-warming.

**When:** Always -- this matches the existing behavior where credentials are validated per-CONNECT.

**Why not pre-warm:** The DynamoDB table may have thousands of entries. Most will never connect. Lazy loading keeps memory proportional to active users.

```go
func (a *Authenticator) ValidateAndSwap(p *packets.ConnectPacket) (valid bool) {
    cred, err := a.cache.Get(p.Username)
    if err != nil || cred == nil {
        // Cache miss -- fetch from DynamoDB
        cred, err = a.store.Fetch(p.Username)
        if err != nil || cred == nil {
            return false // Unknown user or DynamoDB error
        }
        a.cache.Set(p.Username, cred)
    }

    if !a.verifyPassword(p.Password, cred.PasswordHash) {
        return false
    }

    // Swap credentials for forwarding to Mosquitto
    p.Username = a.genericUser
    p.Password = []byte(a.genericPass)
    return true
}
```

### Pattern 2: Interface-Based Store Adapter

**What:** DynamoDB access hidden behind `CredentialStore` interface.

**Why:** Enables test doubles. Unit tests use an in-memory mock store. Integration tests hit a real (or local) DynamoDB. The cache and authenticator never know the difference.

```go
type CredentialStore interface {
    Fetch(username string) (*CachedCredential, error)
}

// Production
type DynamoDBStore struct {
    client    *dynamodb.Client
    tableName string
}

// Test
type MockStore struct {
    credentials map[string]*CachedCredential
}
```

### Pattern 3: Passive TTL Expiry

**What:** No background goroutine scanning for expired entries. Expiry is checked on `Get()`. Expired entries are deleted lazily.

**Why:** Simpler, no goroutine overhead, no timer management. The cache is small (active MQTT users). For the admin "how big is the cache" stat, an occasional sweep can be triggered by the stats endpoint.

**Caveat:** If memory pressure becomes a concern (unlikely for credential data), add a periodic sweep. But start simple.

### Pattern 4: ServerCmd Composition (Not Inheritance)

**What:** Add `Authenticator` as a field on `ServerCmd`, initialized in `NewServer()`.

**Why:** Matches existing patterns -- `ServerCmd` already holds `PacketDecider`, `ConnTrack`, `InspectorLogger`, `Ciphers` as composed fields. The Authenticator is one more.

```go
type ServerCmd struct {
    Config        *config.Config
    // ... existing fields ...
    Authenticator *credcache.Authenticator  // NEW
}

func NewServer(c *config.Config) (n *ServerCmd) {
    n = new(ServerCmd)
    n.Config = c
    n.SetupTracker()
    n.LoadCiphers(c)
    n.LoadInspectorRules()
    n.Authenticator = credcache.NewAuthenticator(c)  // NEW
    return n
}
```

## Anti-Patterns to Avoid

### Anti-Pattern 1: Cache Inside inspectRawPacket

**What:** Putting cache logic directly in the `inspectRawPacket` switch case.

**Why bad:** The current inline credential logic is already hard to maintain (hardcoded usernames, env var dependency, mixed validation + mutation). Adding DynamoDB and cache logic there would make it worse. Extract to a dedicated component.

**Instead:** Call `n.Authenticator.ValidateAndSwap(p)` from the ConnectPacket case. The case body shrinks to ~5 lines.

### Anti-Pattern 2: Global Cache Variable

**What:** `var globalCache *CredentialCache` at package level.

**Why bad:** Breaks testability, creates hidden state, makes concurrent test runs impossible. The existing codebase already avoids globals (except `rateLimiter`, which is a minor smell).

**Instead:** Cache owned by `Authenticator`, owned by `ServerCmd`, initialized in `NewServer()`.

### Anti-Pattern 3: Synchronous DynamoDB on Every CONNECT

**What:** Skipping the cache and hitting DynamoDB for every connection.

**Why bad:** DynamoDB GetItem is 5-10ms. At 100 concurrent CONNECTs, that is 500-1000ms of blocked goroutine time per burst. The Meshtastic mobile clients reconnect frequently (network switches, sleep/wake). Cache is not optional.

**Instead:** Cache with TTL. DynamoDB only on miss.

### Anti-Pattern 4: Pre-Warming with Full Table Scan

**What:** `Scan()` the entire DynamoDB table at startup to populate the cache.

**Why bad:** Scan is expensive (reads every item), slow for large tables, and costs DynamoDB read capacity. Most entries will never be needed.

**Instead:** Lazy-load on first CONNECT per username.

## Integration Point: inspect.go Refactor

The core change is in `inspect.go`, `inspectRawPacket()`, `ConnectPacket` case. Current code (lines 99-115):

```go
// CURRENT: Hardcoded allowlist + seed-based password + inline swap
if p.Username == "ghosts" || p.Username == "kph" || ... {
    // passthrough
} else if !(len(p.Username) == 12 && ...) {
    p.Username = ""  // clobber -> hangup
} else {
    expectedPassword := generateMQTTPassword(p.Username, os.Getenv("USER_CREATION_SEED"))
    if string(p.Password) == expectedPassword {
        p.Username = "public"
        p.Password = []byte("31337")
    } else {
        p.Username = ""  // clobber -> hangup
    }
}
```

**Becomes:**

```go
// NEW: Authenticator handles everything
if !n.Authenticator.ValidateAndSwap(p) {
    p.Username = ""
    p.Password = []byte("")
}
```

The Authenticator internally handles:
- Admin/passthrough usernames (configurable allowlist replaces hardcoded names)
- Format validation (if still needed, or dropped in favor of DynamoDB-only)
- DynamoDB credential lookup with caching
- Password verification
- Credential swap to generic creds

## Suggested Build Order

Dependencies flow downward. Each step is usable and testable before the next.

```
Step 1: CredentialCache (pure data structure)
   |     No external dependencies. Unit testable in isolation.
   |     Deliverable: cache.go with Get/Set/Delete/Stats, full test coverage.
   |
   v
Step 2: Config extensions
   |     Add DynamoDB table name, region, TTL, generic creds, HTTP addr to Server struct.
   |     Deliverable: config.go changes + meshtk.yaml defaults.
   |
   v
Step 3: DynamoDBStore (adapter)
   |     Depends on: Config (table name, region), AWS SDK v2.
   |     Deliverable: dynamo.go with Fetch(), integration test with local DynamoDB.
   |
   v
Step 4: Authenticator (orchestrator)
   |     Depends on: CredentialCache, CredentialStore interface.
   |     Deliverable: auth.go with ValidateAndSwap(), unit tests with MockStore.
   |
   v
Step 5: inspect.go integration
   |     Depends on: Authenticator wired into ServerCmd.
   |     Deliverable: Refactored ConnectPacket case, existing proxy behavior preserved.
   |
   v
Step 6: CONNACK rejection
   |     Depends on: Working auth pipeline from Step 5.
   |     Deliverable: Send proper MQTT CONNACK with auth failure code instead of silent drop.
   |
   v
Step 7: CredCacheHTTP (admin API)
         Depends on: CredentialCache, Authenticator.
         Deliverable: HTTP server with evict/refresh/stats endpoints.
```

**Rationale for this order:**
- Steps 1-4 can be built and tested without touching any existing proxy code.
- Step 5 is the single integration point -- one function call replacement in inspect.go.
- Step 6 is a refinement (better client UX) that depends on Step 5 working.
- Step 7 is independent of the proxy pipeline; it talks only to the cache. Can be built in parallel with Steps 5-6 if desired.

## Concurrency Model

The proxy runs one goroutine per client connection. Multiple goroutines will call `Authenticator.ValidateAndSwap()` concurrently. The cache must be safe for concurrent access.

**CredentialCache uses `sync.RWMutex`:**
- `Get()` takes a read lock (multiple concurrent readers OK)
- `Set()` takes a write lock (exclusive)
- `Delete()` takes a write lock (exclusive)

**DynamoDB calls are naturally concurrent-safe** -- the AWS SDK v2 client is designed for concurrent use from multiple goroutines.

**No cache stampede concern:** If 10 connections arrive simultaneously for the same uncached username, multiple DynamoDB fetches may fire. This is acceptable because: (a) DynamoDB GetItem is idempotent, (b) the cost is negligible for credential lookups, (c) adding singleflight complexity is not worth it for this use case. If it becomes a concern later, `golang.org/x/sync/singleflight` can be added to the Authenticator without changing any interfaces.

## Sources

- Codebase analysis: `internal/app/server/proxy.go`, `inspect.go`, `decider.go`, `rules.go`, `cmd.go`
- Codebase analysis: `pkg/config/config.go`
- Existing patterns: `pkg/network/` (S3Mover AWS SDK usage)
- Go standard library: `sync.RWMutex` for concurrent map access
- AWS SDK v2 Go documentation (HIGH confidence -- well-established patterns)

---

*Architecture analysis: 2026-03-10*
