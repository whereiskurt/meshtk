# Codebase Concerns

**Analysis Date:** 2026-03-10

## Tech Debt

**Unfinished Refactoring - Inspector Packet Structure:**
- Issue: Inspector functions still coupled to ServerCmd instead of being methods on InspectorPacket
- Files: `internal/app/server/inspect.go:72`
- Impact: Code duplication, tight coupling reduces testability, makes packet inspection logic harder to extend
- Fix approach: Refactor `inspectRawPacket()` and related functions to be methods on `InspectorPacket` struct, removing ServerCmd dependency where possible

**Incomplete ALLOW_LIST Implementation:**
- Issue: Hardcoded allowlist check with placeholder logic that's disabled in production
- Files: `internal/app/server/proxy.go:78-82`
- Impact: Rate limiting and filtering logic is currently disabled; access control is not enforced
- Fix approach: Build out a proper configuration-driven allowlist system, enable the `shouldInspect` boolean flow

**Context Management Anti-pattern:**
- Issue: Using `context.TODO()` in config initialization instead of proper cancellable contexts
- Files: `pkg/config/config.go:178`
- Impact: No way to cancel long-running operations or enforce timeouts at application level
- Fix approach: Pass proper context through initialization chain; use context.WithCancel or context.WithTimeout for request-scoped operations

**Math/Rand Usage in Production Code:**
- Issue: Using `math/rand` for node coordinate randomization instead of `crypto/rand`
- Files: `internal/app/fleet/behaviours.go:8`, `internal/app/fleet/simulate.go:6`, `internal/app/fleet/nodes.go:9`
- Impact: Pseudo-random numbers are predictable; fleet simulation coordinates are not cryptographically secure
- Fix approach: Replace `math/rand` with `crypto/rand` for coordinate jitter generation

## Known Bugs

**Public Key Debugging Artifact:**
- Symptoms: Private keys and public keys printed to stdout in debug code
- Files: `internal/mqtt/mqtt.go:317`
- Trigger: Calling `GenerateKeyPair()` method
- Impact: Cryptographic keys exposed in logs/console output; potential security leak in production
- Workaround: Disable or comment out the debug print statements

**DEFCON API External Dependency - Hardcoded URL:**
- Symptoms: PKI decryption fails when DEFCON API is unavailable or returns 404
- Files: `internal/mqtt/crypto.go:47-139`
- Trigger: Receiving PKI-encrypted packets while DEFCON API is down
- Impact: Can't decrypt incoming PKI packets during API outages; no fallback mechanism
- Workaround: Cache fetched public keys locally; implement retry with exponential backoff

## Security Considerations

**Hardcoded Passthrough Usernames:**
- Risk: Specific MQTT usernames ("ghosts", "kph", "ax", "meshmap") bypass authentication checks
- Files: `internal/app/server/inspect.go:99`
- Current mitigation: Limited to known test usernames in current implementation
- Recommendations: Move to configuration file; use environment variable; remove before production deployment; add audit logging for passthrough accounts

**Password Generation Seed Leakage:**
- Risk: `USER_CREATION_SEED` environment variable used for password validation is read from environment
- Files: `internal/app/server/inspect.go:106`
- Current mitigation: Environment variable only (not in code)
- Recommendations: Rotate seed regularly; add rate limiting on failed auth attempts; hash stored password expectations

**API Token Exposure in HTTP Requests:**
- Risk: OpenAI API key passed in config struct without redaction in logging
- Files: `pkg/config/config.go` (Fleet struct: OpenAIKey field)
- Current mitigation: Marked with `json:"-"` tag
- Recommendations: Add explicit secret masking in logging; use dedicated secrets management; never log config dumps that contain API keys

**Public Key Fetching Without Verification:**
- Risk: DEFCON API response is trusted without signature verification
- Files: `internal/mqtt/crypto.go:88-139`
- Current mitigation: HTTPS connection (TLS verification)
- Recommendations: Implement pinning for DEFCON API endpoint; add response signature verification; cache immutable keys with TTL

**Plaintext Password in MQTT Configuration:**
- Risk: MQTT broker password in config file could be world-readable
- Files: `pkg/config/config.go:120` (Password field with default "larg4cats")
- Current mitigation: Marked with `json:"-"` tag; defaults are test credentials
- Recommendations: Never commit actual credentials; use environment variables only; implement secret rotation

**Encryption Key Format Vulnerability:**
- Risk: Primary channel key defaults to "AQ==" (single byte \x01) which is expanded to 16 bytes with zeros
- Files: `internal/mqtt/mqtt.go:68-71`, `internal/app/server/cmd.go:68-70`
- Current mitigation: Key expansion logic in place
- Recommendations: Use cryptographically strong keys; validate key strength; warn if default key is used in production

## Performance Bottlenecks

**External API Calls in Hot Path:**
- Problem: DEFCON API call made synchronously for every PKI-encrypted packet that needs decryption
- Files: `internal/mqtt/crypto.go:47` (FetchPublicKeyFromDefcon called inside decryptPKI)
- Cause: No caching mechanism; 10-second timeout per request blocks packet processing
- Current impact: One slow DEFCON response blocks the entire packet processing goroutine
- Improvement path: Implement LRU cache for fetched public keys with TTL; use background goroutines for cache refresh; add circuit breaker for failing API

**Unbounded Map Growth in Connection Tracking:**
- Problem: ConnTrack map accumulates entries indefinitely
- Files: `internal/app/server/cmd.go:26-33` (ConnTrack map), `internal/app/server/inspect.go:86-87` (entries added), `internal/app/server/proxy.go:27-30` (entries deleted)
- Cause: Manual cleanup in defer; connection cleanup may not always execute; long-lived connections
- Current impact: Memory leak over extended operation; connection info never pruned for dead sockets
- Improvement path: Add TTL-based eviction; periodic cleanup goroutine; bounded map with LRU eviction

**No Connection Pooling for HTTP Requests:**
- Problem: New http.Client created for each external API call
- Files: `internal/mqtt/crypto.go:91-93`, `internal/app/fleet/cmd.go:540`
- Cause: HTTP client instantiated inside request methods instead of reused
- Current impact: Connection setup/teardown overhead; no connection reuse; exhausts socket limits under load
- Improvement path: Create package-level or singleton http.Client; set reasonable timeouts; tune MaxIdleConns

**Synchronous File I/O in Server Hot Path:**
- Problem: Log file operations (rotate, write) done synchronously during packet inspection
- Files: `internal/app/server/cmd.go:254-320`
- Cause: LogFileMutex held during file operations
- Current impact: Blocks packet processing during log rotation; latency spikes
- Improvement path: Implement async logging channel; batch write operations; use os.WriteFile in separate goroutine

## Fragile Areas

**Protobuf Parsing with Silent Failures:**
- Files: `internal/app/server/inspect.go:124`, `internal/app/server/inspect.go:176`, etc.
- Why fragile: Uses `if err == nil` pattern for proto.Unmarshal; silently continues on parse failure instead of logging
- Safe modification: Add structured logging for unmarshal failures; validate payload structure before parsing; consider using defensive parsing with explicit error handling
- Test coverage: No test coverage visible for malformed protobuf payloads

**Cryptography Code Without Full Error Handling:**
- Files: `internal/mqtt/crypto.go` (entire file)
- Why fragile: Complex cryptographic operations with multiple error paths; some paths return errors but callers might not properly handle them
- Safe modification: Add unit tests for encryption/decryption round-trips; test with invalid keys, nonces, ciphertexts; validate all crypto preconditions
- Test coverage: Gaps in test coverage for edge cases like invalid key lengths, nonce corruption, MAC verification failures

**MQTT Packet Type Assertions Without Guards:**
- Files: `internal/app/server/inspect.go:73-147`
- Why fragile: Type assertions on MQTT packets could panic if raw packet is nil; default case at end assumes all types covered
- Safe modification: Add nil checks before type assertions; add explicit error returns instead of silent skipping; add test for all MQTT packet types
- Test coverage: Missing tests for edge cases (disconnected clients, malformed packets)

**Template Execution with Panic on Error:**
- Files: `internal/app/fleet/nodes.go:170`, `internal/app/server/cmd.go:308`
- Why fragile: Template parsing/execution uses panic() instead of returning errors
- Safe modification: Convert panics to error returns; validate templates at startup; add template syntax validation
- Test coverage: Template failure modes not tested

**Rate Limiter State Without Cleanup:**
- Files: `internal/app/server/proxy.go:15-20` (package-level limiter state)
- Why fragile: Single shared rate limiter instance across all connections; token state persists indefinitely
- Safe modification: Use per-connection limiters; implement background cleanup for inactive connections; add configurability
- Test coverage: No test coverage for rate limiter behavior under concurrent load

## Scaling Limits

**Single MQTT Connection per Fleet:**
- Current capacity: One MqttClient per Fleet instance; limited by single broker connection throughput
- Limit: Broker connection bandwidth and message queue depth
- Scaling path: Implement connection pooling/multiplexing; add partition scheme for topics; consider MQTT clustering

**In-Memory Node Database:**
- Current capacity: Entire mesh network stored in memory as map
- Limit: Available system RAM; scales linearly with network size
- Scaling path: Use persistent database (SQLite, Postgres); implement LRU cache for hot nodes; pagination for large networks

**Synchronous Log File Writes:**
- Current capacity: Limited by disk I/O throughput and log rotation overhead
- Limit: Disk write latency directly affects packet processing latency
- Scaling path: Implement async/buffered logging; use external log aggregation; separate logs by severity level

**Rate Limiter per Socket Address:**
- Current capacity: Unbounded number of tracked socket addresses in rateLimiter.clients
- Limit: Memory exhaustion under connection flooding attacks
- Scaling path: Add connection limits; implement exponential backoff; use session-based rate limiting instead of per-socket

## Dependencies at Risk

**Eclipse Paho MQTT Client - Maintenance Status:**
- Risk: MQTT client library version 1.5.0 may lag behind latest upstream; lack of security patches
- Impact: Potential MQTT protocol bugs; security vulnerabilities in TLS/SSL handling
- Current usage: Core MQTT connectivity
- Migration plan: Monitor upstream releases; consider mqtt.go as alternative if paho lags

**Protobuf Version Alignment:**
- Risk: Meshtastic protobufs may diverge from upstream definitions
- Impact: Message compatibility issues if Meshtastic protocol evolves
- Current mitigation: Vendored protos in `protos/meshtastic/generated`
- Recommendation: Establish proto update cadence; version proto definitions; test protocol compatibility

**AWS SDK Dependency for S3:**
- Risk: Large dependency for single feature (S3 uploads); adds build complexity
- Impact: Larger binary; potential licensing concerns; additional surface area
- Current usage: Optional block list uploads to S3
- Migration plan: Make S3 optional feature; consider lightweight S3 client

## Missing Critical Features

**Connection Authentication Without OTP/MFA:**
- Problem: Only basic password validation; no additional authentication factors for access control
- Blocks: Can't implement device verification; vulnerable to credential compromise
- Impact: Single point of failure for network access

**No Request Timeout for Long Operations:**
- Problem: Some operations (packet inspection, decryption) have no timeout bounds
- Blocks: Can't protect against slow-client attacks or resource exhaustion
- Impact: Individual slow clients can degrade service for all others

**No Metrics/Observability:**
- Problem: No Prometheus metrics, OpenTelemetry, or structured logging for monitoring
- Blocks: Can't detect performance degradation; hard to investigate production issues
- Impact: Blind to system health; difficult to capacity plan or troubleshoot

**No Database Persistence Option:**
- Problem: All state (node database, connection tracking) is in-memory only
- Blocks: Can't survive restarts; can't distribute across multiple instances
- Impact: No high availability or disaster recovery capability

## Test Coverage Gaps

**Cryptography Edge Cases:**
- What's not tested: Invalid cipher states, corrupted nonces, tampered ciphertexts, key length validation
- Files: `internal/mqtt/crypto.go`
- Risk: Crypto bugs could silently produce incorrect results or panic
- Priority: High

**MQTT Packet Parsing:**
- What's not tested: Malformed packets, missing fields, truncated payloads, invalid message types
- Files: `internal/app/server/inspect.go`, `internal/app/server/proxy.go`
- Risk: Crashes or silent data loss when parsing untrusted input
- Priority: High

**Configuration Loading:**
- What's not tested: Missing config files, invalid YAML, missing required fields, type conversion errors
- Files: `pkg/config/config.go`
- Risk: Unclear error messages at startup; hard to diagnose configuration problems
- Priority: Medium

**Rate Limiting Behavior:**
- What's not tested: Concurrent requests, token refill timing, penalty application, cleanup
- Files: `internal/app/server/proxy.go:15-20`, `pkg/network/limiter.go` (implied)
- Risk: Rate limiter could malfunction under load or cause connection drops
- Priority: Medium

**Error Paths in Server Proxy:**
- What's not tested: Backend connection failures, packet write errors, context cancellation, protocol errors
- Files: `internal/app/server/proxy.go`
- Risk: Silent failures, connection leaks, incomplete data transmission
- Priority: Medium

---

*Concerns audit: 2026-03-10*
