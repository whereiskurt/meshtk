---
phase: 02-authenticator-and-proxy-integration
plan: 02
subsystem: auth
tags: [mqtt-connack, authenticator-interface, credential-swap, proxy-integration, connack-0x05]

# Dependency graph
requires:
  - phase: 02-authenticator-and-proxy-integration
    plan: 01
    provides: CacheAuthenticator with Verify(), singleflight, circuit breaker
  - phase: 01-foundation
    provides: CredCacheConfig, Passthrough wiring, ProxyUsername/ProxyPassword config
provides:
  - Authenticator interface in internal/app/server/ (consumer-defined)
  - writeConnackRejection helper for MQTT CONNACK 0x05 responses
  - AuthRejected field on InspectorPacket for proxy flow control
  - inspectRawPacket wired with credential validation and credential swap
  - ServerCmd initialized with CacheAuthenticator from config
affects: [03-admin-api, 04-integration-testing]

# Tech tracking
tech-stack:
  added: []
  patterns: [consumer-defined-interface, connack-rejection, credential-swap-on-valid-auth]

key-files:
  created: [internal/app/server/authenticator.go, internal/app/server/inspect_auth_test.go]
  modified: [internal/app/server/inspect.go, internal/app/server/proxy.go, internal/app/server/cmd.go]

key-decisions:
  - "Used NewDynamoDBStore(tableName, region, endpoint) instead of creating DynamoDB client manually in cmd.go -- store already encapsulates client creation"
  - "AuthRejected field on InspectorPacket instead of return value from inspectRawPacket -- less invasive change, consistent with struct-as-context pattern"
  - "CONNACK written inside inspectRawPacket, not proxy.go -- keeps all auth logic in one place per CONTEXT.md decision"

patterns-established:
  - "Consumer-defined interface: Authenticator matches Decider pattern in server package"
  - "Auth rejection flow: writeConnackRejection(clientConn) then ip.AuthRejected = true then proxy returns"
  - "Credential swap: valid auth swaps to ProxyUsername/ProxyPassword, passthrough forwards as-is"

requirements-completed: [AUTH-01, AUTH-03, AUTH-04]

# Metrics
duration: 5min
completed: 2026-03-11
---

# Phase 02 Plan 02: Authenticator Proxy Integration Summary

**Authenticator interface wired into MQTT proxy CONNECT path with CONNACK 0x05 rejection for invalid/missing credentials and credential swap to generic Mosquitto creds on valid auth**

## Performance

- **Duration:** 5 min
- **Started:** 2026-03-11T02:42:51Z
- **Completed:** 2026-03-11T02:47:26Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- Authenticator interface defined in server package following consumer-defined Decider pattern
- CONNACK 0x05 rejection helper writes proper MQTT protocol error to client connections
- inspect.go ConnectPacket case now validates credentials via Authenticator.Verify() with context timeout
- Valid credentials swapped to generic ProxyUsername/ProxyPassword before forwarding to Mosquitto
- Passthrough usernames bypass all validation and forward with original credentials
- proxy.go uses AuthRejected flag to cleanly close rejected connections after CONNACK is written
- ServerCmd initializes CacheAuthenticator with DynamoDB-backed credential store from config

## Task Commits

Each task was committed atomically:

1. **Task 1: Define Authenticator interface and CONNACK rejection helper** - `0918652` (feat)
2. **Task 2 (RED): Failing tests for auth integration** - `3c71925` (test)
3. **Task 2 (GREEN): Wire Authenticator into inspect/proxy/cmd** - `5bcee7d` (feat)

## Files Created/Modified
- `internal/app/server/authenticator.go` - Authenticator interface and writeConnackRejection helper
- `internal/app/server/inspect.go` - AuthRejected field, inspectRawPacket with clientConn param, auth flow in ConnectPacket case
- `internal/app/server/proxy.go` - Passes conn to inspectRawPacket, checks AuthRejected instead of empty username
- `internal/app/server/cmd.go` - Authenticator field on ServerCmd, initialized with credcache in NewServer
- `internal/app/server/inspect_auth_test.go` - 5 integration tests with mock Authenticator

## Decisions Made
- Used `NewDynamoDBStore(tableName, region, endpoint)` in cmd.go instead of manually creating a DynamoDB client -- the store constructor already encapsulates AWS client setup
- Config field names are `TTLSecs`, `MaxSizeMB`, `TableName`, `TableRegion` (not the longer names in the plan template) -- matched actual config.go struct
- `ErrRefusedNotAuthorised` is a `byte` type in the paho library -- tests use `byte()` cast for assertion compatibility with testify

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed config field name mismatch in cmd.go initialization**
- **Found during:** Task 2 (GREEN phase)
- **Issue:** Plan referenced `CacheTTLSecs`, `CacheMaxSizeMB`, `CredentialTableName`, `CredentialTableRegion` but actual config struct uses `TTLSecs`, `MaxSizeMB`, `TableName`, `TableRegion`
- **Fix:** Used actual field names from config.go
- **Files modified:** internal/app/server/cmd.go
- **Verification:** `go build ./...` passes
- **Committed in:** 5bcee7d

**2. [Rule 1 - Bug] Fixed DynamoDB client initialization approach**
- **Found during:** Task 2 (GREEN phase)
- **Issue:** Plan prescribed creating DynamoDB client manually with aws-sdk-go-v2 config/dynamodb imports, but `NewDynamoDBStore(tableName, region, endpoint)` already creates the client internally
- **Fix:** Used `NewDynamoDBStore` constructor directly, removed unused aws-sdk imports
- **Files modified:** internal/app/server/cmd.go
- **Verification:** `go build ./...` passes, no unused imports
- **Committed in:** 5bcee7d

---

**Total deviations:** 2 auto-fixed (2 bugs -- plan referenced wrong field names and unnecessary manual client creation)
**Impact on plan:** Both fixes were necessary for compilation. No scope creep.

## Issues Encountered
None beyond the auto-fixed deviations above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Complete authentication pipeline is wired: every MQTT CONNECT is validated against cached DynamoDB credentials
- Phase 2 is fully complete -- both plans (CacheAuthenticator + proxy wiring) are done
- Ready for Phase 3 (Admin API) and Phase 4 (integration testing)

---
*Phase: 02-authenticator-and-proxy-integration*
*Completed: 2026-03-11*
