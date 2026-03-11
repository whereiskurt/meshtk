---
phase: 2
slug: authenticator-and-proxy-integration
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-10
---

# Phase 2 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) + testify v1.11.1 |
| **Config file** | none — Go convention |
| **Quick run command** | `go test ./internal/credcache/ ./internal/app/server/ -v -race -count=1` |
| **Full suite command** | `go test ./... -race -count=1` |
| **Estimated runtime** | ~15 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/credcache/ ./internal/app/server/ -v -race -count=1`
- **After every plan wave:** Run `go test ./... -race -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 15 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 02-01-01 | 01 | 1 | CRED-01 | unit | `go test ./internal/credcache/ -v -race -run TestAuthVerify` | ❌ W0 | ⬜ pending |
| 02-01-02 | 01 | 1 | CRED-04 | unit | `go test ./internal/credcache/ -v -race -run TestAuthCacheMiss` | ❌ W0 | ⬜ pending |
| 02-01-03 | 01 | 1 | CRED-05 | unit | `go test ./internal/credcache/ -v -race -run TestAuthCircuitBreaker` | ❌ W0 | ⬜ pending |
| 02-02-01 | 02 | 2 | AUTH-01 | unit | `go test ./internal/app/server/ -v -race -run TestInspectAuthSwap` | ❌ W0 | ⬜ pending |
| 02-02-02 | 02 | 2 | AUTH-03 | unit | `go test ./internal/app/server/ -v -race -run TestInspectAuthReject` | ❌ W0 | ⬜ pending |
| 02-02-03 | 02 | 2 | AUTH-04 | unit | `go test ./internal/app/server/ -v -race -run TestInspectPassthrough` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `internal/credcache/auth_test.go` — stubs for CRED-01, CRED-04, CRED-05 (singleflight, circuit breaker)
- [ ] `internal/app/server/inspect_test.go` — stubs for AUTH-01, AUTH-03, AUTH-04 (proxy integration with mock Authenticator)

*Existing test infrastructure (go test, testify) covers framework needs.*

---

## Manual-Only Verifications

*All phase behaviors have automated verification.*

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 15s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
