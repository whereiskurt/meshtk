---
phase: 01-foundation
verified: 2026-03-10T00:00:00Z
status: gaps_found
score: 14/15 must-haves verified
gaps:
  - truth: "Passthrough allowlist is sourced from YAML config at runtime (AUTH-05)"
    status: partial
    reason: "The config field Server.CredCache.Passthrough exists and is tested, but inspect.go line 80 still hardcodes the passthrough list as string literals rather than reading from config. The requirement as stated in REQUIREMENTS.md (passthrough 'sourced from YAML config, not hardcoded') is not fully satisfied. The plan deliberately deferred wiring to Phase 2 but did not flag this as a partial AUTH-05 delivery."
    artifacts:
      - path: "internal/app/server/inspect.go"
        issue: "Line 80 hardcodes 'ghosts', 'kph', 'ax', 'meshmap' literals instead of reading from cfg.Server.CredCache.Passthrough"
    missing:
      - "Wire inspect.go ConnectPacket handler to read passthrough list from config (Server.CredCache.Passthrough) instead of hardcoded literals — or explicitly reclassify AUTH-05 as Phase 2 in REQUIREMENTS.md traceability"
---

# Phase 1: Foundation Verification Report

**Phase Goal:** The credential cache infrastructure exists and is tested in isolation — config schema covers all new settings, the in-memory cache manages TTL and stats correctly, and the DynamoDB adapter can fetch a credential from the live table.
**Verified:** 2026-03-10
**Status:** gaps_found (1 partial gap)
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths

| #  | Truth | Status | Evidence |
|----|-------|--------|----------|
| 1  | Config struct exposes CredCacheConfig with all 7 fields | VERIFIED | `pkg/config/config.go` lines 51-59: struct with TTLSecs, MaxSizeMB, TableName, TableRegion, DynamoDBEndpoint, Passthrough, TimeoutSecs |
| 2  | Config struct exposes ProxyUsername and ProxyPassword fields | VERIFIED | `pkg/config/config.go` lines 76-77: both fields present in Server struct |
| 3  | Config struct exposes AdminListenAddress field | VERIFIED | `pkg/config/config.go` line 78: `AdminListenAddress string` present |
| 4  | Embedded YAML provides sensible defaults for all new fields | VERIFIED | `pkg/config/meshtk.yaml` lines 72-86: all fields including CredCache block with Passthrough slice |
| 5  | Env var override works for nested CredCache fields via MESHTK_ prefix | VERIFIED | `pkg/config/config.go` lines 212-213: `viper.SetEnvPrefix("meshtk")` + `AutomaticEnv()` wired; Viper nested key mapping confirmed by project pattern |
| 6  | generateMQTTPassword function and USER_CREATION_SEED dependency removed | VERIFIED | `grep -rn "generateMQTTPassword\|USER_CREATION_SEED"` returns zero results; inspect.go has no crypto/sha256, encoding/hex, or os imports for seed auth |
| 7  | Credential struct holds username, password, and usertype with DynamoDB tags | VERIFIED | `internal/credcache/types.go` lines 12-16: all three fields with correct `dynamodbav` tags |
| 8  | CredentialStore interface defines Fetch(ctx, username) signature | VERIFIED | `internal/credcache/types.go` lines 19-21: interface present and correct |
| 9  | Cache stores a credential and returns it on lookup by username | VERIFIED | TestCacheSetAndGet passes; cache.go implements Get/Set via Otter v2 |
| 10 | Cache returns miss for unknown usernames | VERIFIED | TestCacheGetMiss passes; Get returns nil, false |
| 11 | Cached entries expire after configured TTL | VERIFIED | TestCacheTTLExpiry passes: 1-second TTL, sleep 2s, entry gone |
| 12 | Cache reports accurate hit and miss counters via Stats() | VERIFIED | TestCacheStatsHits and TestCacheStatsMisses pass; Stats() reads from otter stats counter |
| 13 | DynamoDB adapter scans table with FilterExpression on mqttUsername | VERIFIED | TestStoreFetchFilterExpression passes; store.go lines 64-70 build expression with mqttUsername filter |
| 14 | DynamoDB adapter returns ErrNotFound for unknown usernames | VERIFIED | TestStoreFetchUnknownUser passes; ErrNotFound sentinel returned when Items is empty |
| 15 | Passthrough allowlist is sourced from YAML config at runtime (AUTH-05) | PARTIAL | Config field exists and tested (VERIFIED). But inspect.go line 80 still hardcodes `"ghosts"\|\|"kph"\|\|"ax"\|\|"meshmap"` — runtime behavior does not read from config. Requirement as stated is not fully met. |

**Score:** 14/15 truths verified (1 partial)

---

## Required Artifacts

### Plan 01-01 Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/config/config.go` | CredCacheConfig struct, extended Server struct | VERIFIED | CredCacheConfig at line 51; Server extended at lines 75-78 |
| `pkg/config/meshtk.yaml` | Default values for CredCache, ProxyUsername, ProxyPassword, AdminListenAddress | VERIFIED | All defaults present at lines 72-86 |
| `pkg/config/config_test.go` | Unit tests for config defaults and env var override | VERIFIED | TestCredCacheConfigDefaults, TestProxyCredsDefaults, TestAdminListenAddressDefault — all pass |

### Plan 01-02 Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/credcache/types.go` | Credential, CredentialStore, ErrNotFound | VERIFIED | All three present; Credential has all dynamodbav tags |
| `internal/credcache/cache.go` | Otter v2 wrapper with Get/Set/Delete/Stats/Close | VERIFIED | All 5 methods implemented; uses Otter v2 Options API |
| `internal/credcache/store.go` | DynamoDBStore implementing CredentialStore | VERIFIED | Implements Fetch with FilterExpression, ProjectionExpression, pagination |
| `internal/credcache/cache_test.go` | Cache hit/miss/TTL/stats tests | VERIFIED | 8 tests, all pass including TTL expiry (2s sleep) |
| `internal/credcache/store_test.go` | Store tests with mock DynamoDB client | VERIFIED | 5 tests, all pass; mock captures ScanInput for expression assertion |

---

## Key Link Verification

### Plan 01-01 Key Links

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `pkg/config/config.go` | `pkg/config/meshtk.yaml` | go:embed + Viper MergeConfig | VERIFIED | Line 17: `//go:embed meshtk.yaml`; line 215: `viper.MergeConfig(strings.NewReader(DefaultConfig))` |

### Plan 01-02 Key Links

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `internal/credcache/store.go` | `internal/credcache/types.go` | DynamoDBStore implements CredentialStore | VERIFIED | `func (s *DynamoDBStore) Fetch(ctx context.Context, username string) (*Credential, error)` satisfies the interface |
| `internal/credcache/cache.go` | `internal/credcache/types.go` | Cache stores *Credential values | VERIFIED | `otter.Cache[string, *Credential]` at line 21; type-safe throughout |

---

## Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|-------------|-------------|--------|----------|
| CONF-01 | 01-01 | Cache TTL and max memory size configurable | SATISFIED | `Server.CredCache.TTLSecs` and `Server.CredCache.MaxSizeMB` in config with tested defaults (900, 64) |
| CONF-02 | 01-01 | Admin API listen address configurable | SATISFIED | `Server.AdminListenAddress` present with default "localhost:9090", tested |
| CONF-03 | 01-01 | DynamoDB table name configurable | SATISFIED | `Server.CredCache.TableName` with default "run-human-electro", tested |
| CONF-04 | 01-01 | DynamoDB region configurable | SATISFIED | `Server.CredCache.TableRegion` with default "us-east-1", tested |
| AUTH-02 | 01-01 | Generic Mosquitto creds from YAML/env, not hardcoded | SATISFIED | `Server.ProxyUsername` ("public") and `Server.ProxyPassword` ("31337") in config, tested, Viper env override wired |
| AUTH-05 | 01-01 | Passthrough allowlist from YAML config, not hardcoded | PARTIAL | Config field `Server.CredCache.Passthrough` exists with correct defaults and is tested. However, `inspect.go` line 80 still hardcodes the list as literals and does not read from config at runtime. The requirement "sourced from YAML config (not hardcoded)" is not fully met until wiring occurs. Plan explicitly deferred wiring to Phase 2 without updating REQUIREMENTS.md traceability. |
| CRED-02 | 01-02 | In-memory cache with configurable max memory size | SATISFIED | NewCache(ttlSecs, maxSizeMB) creates Otter cache with MaximumSize = maxSizeMB * 10000; all cache tests pass |
| CRED-03 | 01-02 | Cache entries expire after configurable TTL | SATISFIED | ExpiryWriting calculator configured with TTL; TestCacheTTLExpiry proves expiry after 1s TTL + 2s sleep |

### Orphaned Requirements Check

No requirements mapped to Phase 1 in REQUIREMENTS.md traceability table that are absent from plans. All 8 requirement IDs declared in plans (CONF-01 through CONF-04, AUTH-02, AUTH-05, CRED-02, CRED-03) appear in REQUIREMENTS.md and are accounted for above.

---

## Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| `internal/app/server/inspect.go` | 79 | `// TODO(phase2): Replace with credential cache lookup` | Info | Expected marker; Phase 2 will replace this stub |
| `internal/app/server/inspect.go` | 80 | Hardcoded passthrough literals instead of config read | Warning | AUTH-05 not fully satisfied; passthrough list is not config-driven at runtime. Low operational risk since the values match config defaults, but changes to config will not be reflected. |

No stub return values, placeholder implementations, or empty handlers found in any credcache source files. All implementations are substantive and production-quality.

---

## Human Verification Required

None. All automated checks (build, vet, unit tests) pass. The phase goal is infrastructure-and-isolation-level, with no UI, real-time behavior, or external service integration required for verification.

---

## Gaps Summary

One partial gap blocks full AUTH-05 requirement satisfaction:

**AUTH-05 runtime wiring gap.** The YAML config field `Server.CredCache.Passthrough` exists, has correct defaults, and passes tests. However, `inspect.go` line 80 retains hardcoded string literals for the passthrough check. The requirement states the allowlist must be "sourced from YAML config (not hardcoded)" — which is not true at runtime. The plan intentionally deferred this wiring to Phase 2 but did not update REQUIREMENTS.md traceability or flag AUTH-05 as partially complete.

Resolution options (choose one before marking Phase 1 complete):
1. Wire `inspect.go` to read from `cfg.Server.CredCache.Passthrough` now — a small, low-risk change that fully satisfies AUTH-05.
2. Update REQUIREMENTS.md to move AUTH-05 to Phase 2 traceability with a note that config field creation is Phase 1 and wiring is Phase 2.

All other 7 requirement IDs are fully satisfied. The 13/13 unit tests pass. The project builds cleanly with no vet warnings. The core goal — credential cache infrastructure exists and is tested in isolation — is achieved.

---

_Verified: 2026-03-10_
_Verifier: Claude (gsd-verifier)_
