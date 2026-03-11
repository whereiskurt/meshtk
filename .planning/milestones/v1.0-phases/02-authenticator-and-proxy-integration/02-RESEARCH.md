# Phase 2: Authenticator and Proxy Integration - Research

**Researched:** 2026-03-10
**Domain:** MQTT proxy authentication pipeline, Go singleflight, circuit breaker, CONNACK rejection
**Confidence:** HIGH

## Summary

Phase 2 wires the credential cache (built in Phase 1) into the MQTT proxy CONNECT path. The Authenticator struct orchestrates cache lookup, DynamoDB fallback (with singleflight deduplication), password verification (constant-time), and credential swap. Invalid clients receive a proper MQTT CONNACK 0x05 rejection written directly to their `net.Conn`. Passthrough usernames bypass all validation. A circuit breaker prevents piling up DynamoDB timeouts during outages.

The existing codebase provides clean integration points: `inspect.go` lines 79-93 contain a TODO marker for Phase 2 replacement, `proxy.go` lines 72-76 have the empty-username check that needs CONNACK replacement, and `cmd.go` has the `ServerCmd` struct where the Authenticator field gets added. All Phase 1 building blocks (Cache, DynamoDBStore, CredentialStore interface, Credential type, ErrNotFound) are ready for use.

**Primary recommendation:** Define an `Authenticator` interface in `internal/app/server/` (consumer-defined, matching the Decider pattern), implement the concrete struct in `internal/credcache/`, and thread `net.Conn` into the inspect path so CONNACK 0x05 can be written on auth failure before the connection is closed.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Replace the existing clobber logic in `inspect.go` (lines 79-93) with Authenticator.Verify() call
- Authenticator is a field on ServerCmd, called from the ConnectPacket case in inspectRawPacket
- Pass client `net.Conn` to inspectRawPacket so it can write CONNACK 0x05 directly on auth failure and return a signal for proxy.go to close the connection
- All CONNECT auth logic stays in inspect.go -- no split across files
- Interface defined in `internal/app/server/` (consumer defines the interface, Go convention)
- Implementation struct in `internal/credcache/` (near Cache and Store it wraps)
- Follows existing Decider interface pattern in the codebase
- Authenticator wraps Cache + CredentialStore + singleflight group
- Singleflight wired at Authenticator creation time (from STATE.md pre-phase decision)
- CONNACK 0x05 sent immediately on auth failure -- no intentional delay
- Log at WARN level: username, source IP, rejection reason (invalid/missing/timeout) -- no passwords logged
- Follows existing WriteDecisionLog pattern for blocked packets
- Empty username (no username in CONNECT) -> reject with CONNACK 0x05 (fail closed)
- Passthrough usernames bypass auth entirely and are forwarded as-is with their own credentials
- No credential swap for passthrough -- these are system accounts Mosquitto expects by name
- Passthrough list already wired from config (Phase 1)
- Constant-time comparison using `crypto/subtle.ConstantTimeCompare` for hex string comparison
- Authenticator.Verify() accepts raw bytes: `Verify(ctx context.Context, username string, password []byte) (bool, error)`
- Hex conversion (`fmt.Sprintf("%x", password)`) happens inside Verify(), encapsulating the encoding detail
- Compared against stored `mqttPassword` from DynamoDB/cache
- Circuit breaker pattern: Authenticator tracks healthy/degraded state internally
- After N consecutive DynamoDB failures, skip DynamoDB calls for a cooldown period (~10s)
- Auto-recovers by retrying after cooldown expires
- Logging: ERROR on first DynamoDB failure, then periodic summary every ~30s
- INFO log on recovery: "DynamoDB connectivity restored"
- Cache hits during outage are indistinguishable from normal cache hits
- On successful auth: swap `p.Username` to `n.Config.Server.ProxyUsername` and `p.Password` to `n.Config.Server.ProxyPassword`
- Mosquitto sees only generic creds -- never the client's real credentials
- Swap happens in inspect.go after Verify() returns true

### Claude's Discretion
- Circuit breaker thresholds (consecutive failure count, cooldown duration)
- Singleflight implementation details (key format, cleanup)
- Exact Authenticator constructor signature and initialization in ServerCmd
- Test strategy for proxy integration (mock Authenticator via interface)
- Error type design for distinguishing auth failure reasons (invalid, timeout, DynamoDB error)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CRED-01 | Proxy looks up MQTT username/password in DynamoDB using defcon.run schema | Authenticator.Verify() calls Cache.Get() then DynamoDBStore.Fetch() on miss; existing store.go handles DynamoDB scan |
| CRED-04 | Cache miss triggers transparent DynamoDB fetch and cache population | Singleflight-wrapped Fetch in Authenticator populates cache on miss; see Architecture Pattern 2 |
| CRED-05 | Proxy continues serving cached entries when DynamoDB is unreachable | Circuit breaker skips DynamoDB calls during outage; cache hits still succeed; cache misses reject (fail closed) |
| AUTH-01 | On valid credentials, proxy swaps username/password with generic Mosquitto creds before forwarding | inspect.go swaps p.Username/p.Password to Config.Server.ProxyUsername/ProxyPassword after Verify() returns true |
| AUTH-03 | On invalid or missing credentials, proxy sends CONNACK with return code 0x05 (not authorized) | ConnackPacket{ReturnCode: 0x05} written to client net.Conn; connection closed; see Code Example 1 |
| AUTH-04 | Configured passthrough usernames bypass credential validation entirely | Passthrough check in inspect.go before Verify() call; already wired from Phase 1 config |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| golang.org/x/sync/singleflight | v0.12.0 (in go.mod) | Deduplicate concurrent DynamoDB fetches for same username | Standard Go extended lib; already vendored via x/sync |
| crypto/subtle | stdlib | Constant-time password comparison | Go stdlib; prevents timing side-channel |
| github.com/eclipse/paho.mqtt.golang/packets | vendored | ConnackPacket construction for 0x05 rejection | Already used throughout proxy for packet parsing |
| internal/credcache (project) | Phase 1 | Cache, DynamoDBStore, Credential, CredentialStore, ErrNotFound | Built in Phase 1; direct dependency |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/sirupsen/logrus | v1.9.3 | WARN/ERROR/INFO logging for auth events | All auth logging -- matches existing codebase pattern |
| context (stdlib) | stdlib | Timeout for DynamoDB calls, cancellation propagation | Wrap DynamoDB Fetch with context.WithTimeout |
| sync/atomic | stdlib | Circuit breaker state (consecutive failure counter) | Lock-free counter for hot path performance |
| time (stdlib) | stdlib | Circuit breaker cooldown tracking | time.Now() comparisons for cooldown expiry |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Manual circuit breaker | sony/gobreaker | Adds dependency for ~50 lines of custom code; manual is simpler for single-use case |
| x/sync/singleflight | Manual sync.Mutex dedup | Singleflight handles caller notification and error propagation correctly; manual is error-prone |

**Installation:**
```bash
# No new dependencies needed -- all already in go.mod/vendor
# golang.org/x/sync is at v0.12.0
# crypto/subtle is stdlib
# paho.mqtt.golang/packets is vendored
```

## Architecture Patterns

### Recommended Project Structure
```
internal/
  app/
    server/
      authenticator.go   # Authenticator interface (consumer-defined)
      inspect.go          # Modified: calls Authenticator.Verify(), writes CONNACK 0x05
      proxy.go            # Modified: passes net.Conn to inspectRawPacket
      cmd.go              # Modified: Authenticator field on ServerCmd, init in NewServer
  credcache/
      auth.go             # CacheAuthenticator struct (implements Authenticator interface)
      cache.go            # (Phase 1 -- unchanged)
      store.go            # (Phase 1 -- unchanged)
      types.go            # (Phase 1 -- unchanged)
```

### Pattern 1: Consumer-Defined Interface (Authenticator)
**What:** Define the `Authenticator` interface in `internal/app/server/authenticator.go` where it is consumed, not in `internal/credcache/` where it is implemented.
**When to use:** Always -- this is the established Go convention and matches the existing `Decider` interface pattern.
**Example:**
```go
// internal/app/server/authenticator.go
package server

import "context"

// Authenticator validates MQTT CONNECT credentials.
// Implementations handle cache lookup, backend fetch, and password verification.
type Authenticator interface {
    // Verify checks if the given username/password combination is valid.
    // password is the raw bytes from the MQTT CONNECT packet.
    // Returns (true, nil) on valid credentials, (false, nil) on invalid,
    // or (false, err) on backend failure.
    Verify(ctx context.Context, username string, password []byte) (bool, error)
}
```

### Pattern 2: Singleflight-Wrapped Cache-Aside
**What:** On cache miss, use `singleflight.Group.Do()` keyed by username to ensure exactly one DynamoDB fetch per username, even under concurrent CONNECT bursts.
**When to use:** Every cache miss path in the Authenticator.
**Example:**
```go
// internal/credcache/auth.go
package credcache

import (
    "context"
    "crypto/subtle"
    "fmt"
    "sync"
    "sync/atomic"
    "time"

    "golang.org/x/sync/singleflight"
)

type CacheAuthenticator struct {
    cache  *Cache
    store  CredentialStore
    sf     singleflight.Group

    // Circuit breaker state
    consecutiveFailures atomic.Int64
    lastFailure         atomic.Int64  // unix nanos
    failureThreshold    int64
    cooldownDuration    time.Duration

    // Logging (set at construction)
    mu              sync.Mutex
    lastLogTime     time.Time
    rejectedInWindow int64
}

func (a *CacheAuthenticator) Verify(ctx context.Context, username string, password []byte) (bool, error) {
    // 1. Cache lookup
    cred, ok := a.cache.Get(username)
    if !ok {
        // 2. Singleflight DynamoDB fetch
        fetched, err := a.fetchWithSingleflight(ctx, username)
        if err != nil {
            return false, err
        }
        if fetched == nil {
            return false, nil // user not found
        }
        cred = fetched
    }

    // 3. Constant-time password comparison
    hexPassword := fmt.Sprintf("%x", password)
    if subtle.ConstantTimeCompare([]byte(hexPassword), []byte(cred.Password)) != 1 {
        return false, nil
    }

    return true, nil
}
```

### Pattern 3: CONNACK 0x05 Rejection via net.Conn
**What:** On auth failure, construct a `ConnackPacket{ReturnCode: 0x05}` and write it directly to the client's `net.Conn` before returning a signal to close the connection.
**When to use:** Every auth failure in the ConnectPacket case of inspectRawPacket.
**Example:**
```go
// In inspect.go ConnectPacket case
connack := &packets.ConnackPacket{
    FixedHeader: packets.FixedHeader{MessageType: packets.Connack},
    ReturnCode:  packets.ErrRefusedNotAuthorised, // 0x05
}
connack.Write(clientConn) // clientConn is the net.Conn passed to inspectRawPacket
```

### Pattern 4: Circuit Breaker (Lightweight, No External Dep)
**What:** Track consecutive DynamoDB failures with atomic counter. After threshold, skip DynamoDB for cooldown period. Auto-recover on cooldown expiry.
**When to use:** Inside the singleflight fetch function, wrapping the actual DynamoDB call.
**Example:**
```go
func (a *CacheAuthenticator) isDegraded() bool {
    if a.consecutiveFailures.Load() < a.failureThreshold {
        return false
    }
    elapsed := time.Since(time.Unix(0, a.lastFailure.Load()))
    return elapsed < a.cooldownDuration
}

func (a *CacheAuthenticator) recordFailure() {
    a.consecutiveFailures.Add(1)
    a.lastFailure.Store(time.Now().UnixNano())
}

func (a *CacheAuthenticator) recordSuccess() {
    if a.consecutiveFailures.Swap(0) > 0 {
        // Log recovery: "DynamoDB connectivity restored"
    }
}
```

### Anti-Patterns to Avoid
- **Splitting auth logic across files:** All CONNECT auth decisions stay in inspect.go. The Authenticator is called from there but does not touch packet mutation.
- **Holding locks during DynamoDB calls:** Singleflight handles deduplication without holding cache locks. Cache.Set() is called only after fetch completes.
- **Logging passwords:** WARN logs include username, source IP, rejection reason. Never log password bytes.
- **Blocking on circuit breaker recovery probes:** Recovery happens passively -- when cooldown expires, the next cache miss attempts DynamoDB. No background probe goroutine needed.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Concurrent fetch deduplication | Custom mutex + waitgroup per key | `golang.org/x/sync/singleflight` | Handles error propagation, panic recovery, caller notification correctly |
| CONNACK packet construction | Raw byte buffer manipulation | `packets.ConnackPacket` from paho library | Already used in codebase; handles fixed header encoding |
| Constant-time string comparison | Custom comparison loop | `crypto/subtle.ConstantTimeCompare` | Audited stdlib; handles length differences safely |

**Key insight:** The paho `packets` library already provides `ConnackPacket` with a `Write(io.Writer)` method and `ErrRefusedNotAuthorised` (0x05) constant. No need to manually encode MQTT bytes.

## Common Pitfalls

### Pitfall 1: Forgetting to pass net.Conn to inspectRawPacket
**What goes wrong:** Without `net.Conn`, inspect.go cannot write CONNACK 0x05 back to the client. The current signature `(ip *InspectorPacket) inspectRawPacket(n *ServerCmd)` does not include the connection.
**Why it happens:** The method was designed for read-only inspection, not writing responses.
**How to avoid:** Add `clientConn net.Conn` parameter to `inspectRawPacket`. The call site in `proxy.go` already has `conn` in scope (line 69: `ip.inspectRawPacket(n)`). Update to `ip.inspectRawPacket(n, conn)`.
**Warning signs:** Auth failures cause silent connection drops instead of CONNACK 0x05.

### Pitfall 2: inspectRawPacket return value not signaling auth failure
**What goes wrong:** Currently `inspectRawPacket` returns nothing (void). After writing CONNACK 0x05, `proxy.go` needs to know the connection should be closed -- not forwarded to the backend.
**Why it happens:** The current empty-username check (proxy.go:72-76) uses `return` to exit `handleProxy`. But the CONNACK write happens inside `inspectRawPacket`, and `handleProxy` does not know auth failed.
**How to avoid:** Have `inspectRawPacket` return a bool or error indicating whether the connection should be closed. Or have the ConnectPacket case set a field on InspectorPacket (e.g., `ip.AuthRejected = true`) that proxy.go checks.
**Warning signs:** Auth-rejected connections still attempt to forward packets to Mosquitto.

### Pitfall 3: Hex encoding mismatch between CONNECT password and DynamoDB stored password
**What goes wrong:** `ConnectPacket.Password` is `[]byte`. The current code converts it with `fmt.Sprintf("%x", p.Password)` (line 69 of inspect.go). The DynamoDB `mqttPassword` field stores the password as a string. If the stored format differs from the hex encoding (e.g., stored as plain text, or stored as uppercase hex), comparison always fails.
**Why it happens:** The encoding convention is implicit -- not documented at the DynamoDB schema level.
**How to avoid:** Verify the exact format of `mqttPassword` in the live DynamoDB table. The Verify() method encapsulates hex encoding, so if the format changes, only one place needs updating.
**Warning signs:** All credentials fail validation despite being correct in DynamoDB.

### Pitfall 4: Singleflight sharing errors across concurrent callers
**What goes wrong:** `singleflight.Group.Do()` shares the result (including errors) with all callers waiting for the same key. If the DynamoDB call fails, all concurrent callers for that username receive the same error.
**Why it happens:** This is by design in singleflight. But if the circuit breaker triggers on that shared error, all waiting callers are rejected simultaneously.
**How to avoid:** This is actually correct behavior -- if DynamoDB is down, all callers for a cache-miss username should fail. The circuit breaker prevents future callers from even attempting. Ensure error logging happens once (in the flight executor), not once per waiting caller.
**Warning signs:** Duplicate error log entries for the same failed DynamoDB call.

### Pitfall 5: Circuit breaker not resetting on cache hits
**What goes wrong:** The circuit breaker tracks DynamoDB failures. But if all connecting users are already cached (cache hits), the circuit breaker never gets a chance to probe recovery because no DynamoDB calls are made.
**Why it happens:** Cache hits bypass DynamoDB entirely, so the circuit breaker state is stale.
**How to avoid:** The cooldown timer handles this -- after cooldown expires, the next cache MISS will probe DynamoDB regardless of the failure counter. Cache hits during degraded mode are normal and desired.
**Warning signs:** Circuit breaker stays in degraded state for much longer than the cooldown duration.

## Code Examples

### Example 1: Writing CONNACK 0x05 Rejection
```go
// Source: vendor/github.com/eclipse/paho.mqtt.golang/packets/connack.go
// + vendor/github.com/eclipse/paho.mqtt.golang/packets/packets.go constants

func writeConnackRejection(conn net.Conn) error {
    connack := &packets.ConnackPacket{
        FixedHeader: packets.FixedHeader{MessageType: packets.Connack},
        ReturnCode:  packets.ErrRefusedNotAuthorised, // 0x05
    }
    return connack.Write(conn)
}
```

### Example 2: Modified inspectRawPacket ConnectPacket Case
```go
// inspect.go -- ConnectPacket case replacement
case *packets.ConnectPacket:
    connInfo := &ConnectionInfo{
        ClientID:      p.ClientIdentifier,
        Username:      p.Username,
        Password:      fmt.Sprintf("%x", p.Password),
        SocketAddress: ip.Track.SocketAddress,
        ConnectTime:   time.Now().Unix(),
    }
    ip.MQTT.Type = "CONNECT"
    ip.Track = connInfo
    n.ConnMutex.Lock()
    n.ConnTrack[ip.Track.SocketAddress] = connInfo
    n.ConnMutex.Unlock()

    // Passthrough check
    passthrough := false
    for _, pt := range n.Config.Server.CredCache.Passthrough {
        if p.Username == pt {
            passthrough = true
            break
        }
    }

    if passthrough {
        // Passthrough -- forward as-is with original credentials
    } else if p.Username == "" {
        // Empty username -- reject (fail closed)
        writeConnackRejection(clientConn)
        ip.AuthRejected = true
    } else {
        // Credential validation
        ctx, cancel := context.WithTimeout(context.Background(),
            time.Duration(n.Config.Server.CredCache.TimeoutSecs)*time.Second)
        defer cancel()

        valid, err := n.Authenticator.Verify(ctx, p.Username, p.Password)
        if err != nil {
            n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=error, err=%v",
                ip.Track.SocketAddress, p.Username, err)
            writeConnackRejection(clientConn)
            ip.AuthRejected = true
        } else if !valid {
            n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=invalid",
                ip.Track.SocketAddress, p.Username)
            writeConnackRejection(clientConn)
            ip.AuthRejected = true
        } else {
            // Valid -- swap to generic Mosquitto credentials
            p.Username = n.Config.Server.ProxyUsername
            p.Password = []byte(n.Config.Server.ProxyPassword)
        }
    }
```

### Example 3: Singleflight Fetch in Authenticator
```go
func (a *CacheAuthenticator) fetchWithSingleflight(ctx context.Context, username string) (*Credential, error) {
    // Circuit breaker check
    if a.isDegraded() {
        return nil, fmt.Errorf("dynamodb circuit breaker open")
    }

    val, err, _ := a.sf.Do(username, func() (interface{}, error) {
        cred, err := a.store.Fetch(ctx, username)
        if err != nil {
            if !errors.Is(err, ErrNotFound) {
                a.recordFailure()
            }
            return nil, err
        }
        a.recordSuccess()
        a.cache.Set(username, cred)
        return cred, nil
    })

    if err != nil {
        return nil, err
    }
    if val == nil {
        return nil, nil
    }
    return val.(*Credential), nil
}
```

### Example 4: CacheAuthenticator Constructor
```go
func NewCacheAuthenticator(cache *Cache, store CredentialStore, opts ...AuthOption) *CacheAuthenticator {
    a := &CacheAuthenticator{
        cache:            cache,
        store:            store,
        failureThreshold: 3,                // default: 3 consecutive failures
        cooldownDuration: 10 * time.Second,  // default: 10s cooldown
    }
    for _, opt := range opts {
        opt(a)
    }
    return a
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Clobber creds to empty string (silent drop) | CONNACK 0x05 rejection | Phase 2 | Proper MQTT protocol compliance; clients get clear error |
| Hardcoded passthrough list | Config-driven passthrough (Phase 1) | Phase 1 | Already done; Phase 2 uses it directly |
| No DynamoDB fallback | Cache + singleflight + circuit breaker | Phase 2 | Resilient auth with graceful degradation |
| `==` password comparison | `crypto/subtle.ConstantTimeCompare` | Phase 2 | Prevents timing side-channel attacks |

## Open Questions

1. **DynamoDB mqttPassword format verification**
   - What we know: Credential struct has `Password string` tagged `dynamodbav:"mqttPassword"`. The CONNECT password is hex-encoded via `fmt.Sprintf("%x", p.Password)`.
   - What's unclear: Whether stored mqttPassword is also hex-encoded, plaintext, or hashed. STATE.md flags this as a blocker/concern.
   - Recommendation: The Verify() method encapsulates encoding, so this is a single-point fix. Assume hex string comparison for now (per CONTEXT.md decision). If bcrypt/argon2, the comparison logic changes but the interface does not.

2. **Circuit breaker threshold tuning**
   - What we know: Claude's discretion per CONTEXT.md.
   - Recommendation: Start with 3 consecutive failures / 10s cooldown. These are reasonable defaults for a 5s DynamoDB timeout -- 3 failures = 15s of accumulated timeout before breaker trips, then 10s cooldown prevents hammering.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) + testify v1.11.1 (in go.mod) |
| Config file | none -- Go convention, `go test ./...` |
| Quick run command | `go test ./internal/credcache/ -v -race -run TestAuth` |
| Full suite command | `go test ./... -race -count=1` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| CRED-01 | DynamoDB lookup via Authenticator | unit | `go test ./internal/credcache/ -v -race -run TestAuthVerify` | Wave 0 |
| CRED-04 | Cache miss triggers fetch + populate | unit | `go test ./internal/credcache/ -v -race -run TestAuthCacheMiss` | Wave 0 |
| CRED-05 | Cached entries served during DynamoDB outage | unit | `go test ./internal/credcache/ -v -race -run TestAuthCircuitBreaker` | Wave 0 |
| AUTH-01 | Credential swap to generic creds on success | unit | `go test ./internal/app/server/ -v -race -run TestInspectAuthSwap` | Wave 0 |
| AUTH-03 | CONNACK 0x05 on invalid credentials | unit | `go test ./internal/app/server/ -v -race -run TestInspectAuthReject` | Wave 0 |
| AUTH-04 | Passthrough bypasses validation | unit | `go test ./internal/app/server/ -v -race -run TestInspectPassthrough` | Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/credcache/ ./internal/app/server/ -v -race -count=1`
- **Per wave merge:** `go test ./... -race -count=1`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/credcache/auth.go` -- CacheAuthenticator implementation (new file)
- [ ] `internal/credcache/auth_test.go` -- covers CRED-01, CRED-04, CRED-05 (singleflight, circuit breaker)
- [ ] `internal/app/server/authenticator.go` -- Authenticator interface definition (new file)
- [ ] `internal/app/server/inspect_test.go` -- covers AUTH-01, AUTH-03, AUTH-04 (proxy integration tests with mock Authenticator)

## Sources

### Primary (HIGH confidence)
- Codebase analysis: `internal/app/server/inspect.go` -- current ConnectPacket handling, TODO marker at line 79
- Codebase analysis: `internal/app/server/proxy.go` -- handleProxy flow, empty username check at line 72
- Codebase analysis: `internal/app/server/cmd.go` -- ServerCmd struct, NewServer constructor
- Codebase analysis: `internal/app/server/decider.go` -- Decider interface pattern (model for Authenticator)
- Codebase analysis: `internal/credcache/` -- Cache, DynamoDBStore, Credential, CredentialStore, ErrNotFound
- Codebase analysis: `vendor/github.com/eclipse/paho.mqtt.golang/packets/connack.go` -- ConnackPacket struct and Write method
- Codebase analysis: `vendor/github.com/eclipse/paho.mqtt.golang/packets/packets.go` -- ErrRefusedNotAuthorised = 0x05
- Go stdlib docs: `crypto/subtle.ConstantTimeCompare` -- constant-time byte comparison
- Go stdlib docs: `golang.org/x/sync/singleflight` -- concurrent call deduplication

### Secondary (MEDIUM confidence)
- `.planning/research/ARCHITECTURE.md` -- pre-phase architecture research
- `.planning/research/PITFALLS.md` -- pre-phase pitfall analysis
- `.planning/STATE.md` -- accumulated decisions including singleflight wiring requirement

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in go.mod/vendor, no new dependencies
- Architecture: HIGH -- clear integration points identified in codebase with TODO markers
- Pitfalls: HIGH -- pre-phase research validated against actual code; ConnackPacket API verified in vendor source
- Circuit breaker: MEDIUM -- design is straightforward but thresholds are heuristic

**Research date:** 2026-03-10
**Valid until:** 2026-04-10 (stable domain; Go stdlib and paho library are mature)
