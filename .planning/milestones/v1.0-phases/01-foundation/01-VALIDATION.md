---
phase: 1
slug: foundation
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-10
---

# Phase 1 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go stdlib `testing` |
| **Config file** | None — Go tests work out of the box |
| **Quick run command** | `go test ./internal/credcache/ -v -count=1` |
| **Full suite command** | `go test ./... -v -count=1` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/credcache/ -v -count=1`
- **After every plan wave:** Run `go test ./... -v -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 10 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 1-01-01 | 01 | 0 | CRED-02, CRED-03 | unit | `go test ./internal/credcache/ -run TestCache -v` | ❌ W0 | ⬜ pending |
| 1-01-02 | 01 | 0 | CONF-01..04, AUTH-02, AUTH-05 | unit | `go test ./pkg/config/ -run TestCredCache -v` | ❌ W0 | ⬜ pending |
| 1-01-03 | 01 | 0 | — | unit | `go test ./internal/credcache/ -run TestStore -v` | ❌ W0 | ⬜ pending |
| 1-02-01 | 02 | 1 | CONF-01, CONF-02, CONF-03, CONF-04 | unit | `go test ./pkg/config/ -run TestCredCacheConfig -v` | ❌ W0 | ⬜ pending |
| 1-02-02 | 02 | 1 | AUTH-02 | unit | `go test ./pkg/config/ -run TestProxyCreds -v` | ❌ W0 | ⬜ pending |
| 1-02-03 | 02 | 1 | AUTH-05 | unit | `go test ./pkg/config/ -run TestPassthrough -v` | ❌ W0 | ⬜ pending |
| 1-03-01 | 03 | 1 | CRED-02, CRED-03 | unit | `go test ./internal/credcache/ -run TestCache -v` | ❌ W0 | ⬜ pending |
| 1-04-01 | 04 | 1 | — | unit | `go test ./internal/credcache/ -run TestStore -v` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `internal/credcache/cache_test.go` — stubs for CRED-02, CRED-03
- [ ] `internal/credcache/store_test.go` — stubs for DynamoDB adapter
- [ ] `pkg/config/config_test.go` — stubs for CONF-01..04, AUTH-02, AUTH-05

*Existing infrastructure covers test framework (Go stdlib `testing`).*

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| Binary starts and operates identically | Success Criteria 4 | Integration behavior | Build and run `go build ./...`, verify no compile errors and existing startup behavior unchanged |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 10s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
