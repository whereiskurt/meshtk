# Phase 2: Authenticator and Proxy Integration - Context

**Gathered:** 2026-03-10
**Status:** Ready for planning

<domain>
## Phase Boundary

Wire credential validation into the MQTT proxy CONNECT path. Valid clients are transparently forwarded with generic Mosquitto creds, invalid clients receive CONNACK 0x05 rejection, passthrough usernames bypass validation entirely. Includes singleflight for concurrent cache-miss deduplication and circuit breaker for DynamoDB degradation.

</domain>

<decisions>
## Implementation Decisions

### Auth Placement in Proxy Flow
- Replace the existing clobber logic in `inspect.go` (lines 79-93) with Authenticator.Verify() call
- Authenticator is a field on ServerCmd, called from the ConnectPacket case in inspectRawPacket
- Pass client `net.Conn` to inspectRawPacket so it can write CONNACK 0x05 directly on auth failure and return a signal for proxy.go to close the connection
- All CONNECT auth logic stays in inspect.go — no split across files

### Authenticator Architecture
- Interface defined in `internal/app/server/` (consumer defines the interface, Go convention)
- Implementation struct in `internal/credcache/` (near Cache and Store it wraps)
- Follows existing Decider interface pattern in the codebase
- Authenticator wraps Cache + CredentialStore + singleflight group
- Singleflight wired at Authenticator creation time (from STATE.md pre-phase decision)

### Rejection Experience
- CONNACK 0x05 sent immediately on auth failure — no intentional delay (existing rate limiter handles repeat offenders)
- Log at WARN level: username, source IP, rejection reason (invalid/missing/timeout) — no passwords logged
- Follows existing WriteDecisionLog pattern for blocked packets
- Empty username (no username in CONNECT) → reject with CONNACK 0x05 (fail closed)

### Passthrough Behavior
- Passthrough usernames bypass auth entirely and are forwarded as-is with their own credentials
- No credential swap for passthrough — these are system accounts Mosquitto expects by name
- Passthrough list already wired from config (Phase 1)

### Password Comparison
- Constant-time comparison using `crypto/subtle.ConstantTimeCompare` for hex string comparison
- Authenticator.Verify() accepts raw bytes: `Verify(ctx context.Context, username string, password []byte) (bool, error)`
- Hex conversion (`fmt.Sprintf("%x", password)`) happens inside Verify(), encapsulating the encoding detail
- Compared against stored `mqttPassword` from DynamoDB/cache

### DynamoDB Degradation Handling
- Circuit breaker pattern: Authenticator tracks healthy/degraded state internally
- After N consecutive DynamoDB failures, skip DynamoDB calls for a cooldown period (~10s) to avoid piling up 5s timeout requests
- Auto-recovers by retrying after cooldown expires
- Logging: ERROR on first DynamoDB failure, then periodic summary every ~30s ("DynamoDB unreachable: N clients rejected in last 30s")
- INFO log on recovery: "DynamoDB connectivity restored"
- Cache hits during outage are indistinguishable from normal cache hits — no special logging

### Credential Swap
- On successful auth: swap `p.Username` to `n.Config.Server.ProxyUsername` and `p.Password` to `n.Config.Server.ProxyPassword`
- Mosquitto sees only generic creds — never the client's real credentials
- Swap happens in inspect.go after Verify() returns true

### Claude's Discretion
- Circuit breaker thresholds (consecutive failure count, cooldown duration)
- Singleflight implementation details (key format, cleanup)
- Exact Authenticator constructor signature and initialization in ServerCmd
- Test strategy for proxy integration (mock Authenticator via interface)
- Error type design for distinguishing auth failure reasons (invalid, timeout, DynamoDB error)

</decisions>

<specifics>
## Specific Ideas

- The existing `proxy.go:72-76` already checks for empty username and returns — this should be replaced with proper CONNACK 0x05 rejection instead of silent close
- The `inspectRawPacket` function currently takes only `*ServerCmd` — will need `net.Conn` parameter added for writing CONNACK responses
- Singleflight key should be the username string (same as cache key)
- Circuit breaker logs should use the same logrus logger pattern as the rest of the codebase

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `internal/credcache/cache.go`: Cache.Get/Set/Delete/Stats — direct use in Authenticator
- `internal/credcache/store.go`: DynamoDBStore.Fetch — called on cache miss
- `internal/credcache/types.go`: Credential struct, CredentialStore interface, ErrNotFound
- `pkg/config/config.go`: Server.CredCache config with all settings, Server.ProxyUsername/ProxyPassword for credential swap
- `internal/app/server/decider.go`: Decider interface pattern — model for Authenticator interface

### Established Patterns
- Single-letter receiver names: `(n *ServerCmd)`, `(c *Config)`
- Constructor pattern: `New[Type]()` returns pointer
- Interface pattern: small interfaces defined by consumer (see Decider)
- Error handling: `if err != nil { log.Errorf(...); return }` pattern
- Logging: logrus with `n.Config.Log.Errorf()` and `n.InspectorLogger`

### Integration Points
- `internal/app/server/inspect.go:64-93`: ConnectPacket case — replace clobber with auth
- `internal/app/server/proxy.go:72-76`: Empty username check — replace with CONNACK 0x05
- `internal/app/server/cmd.go:26-36`: ServerCmd struct — add Authenticator field
- `internal/app/server/cmd.go:91+`: ProxyServer/StartProxyServer — initialize Authenticator with Cache + Store

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 02-authenticator-and-proxy-integration*
*Context gathered: 2026-03-10*
