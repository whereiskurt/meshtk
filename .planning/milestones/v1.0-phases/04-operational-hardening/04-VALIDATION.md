---
phase: 4
slug: operational-hardening
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-11
---

# Phase 4 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) |
| **Config file** | None — Go's built-in test runner |
| **Quick run command** | `go test ./internal/credcache/ ./internal/admin/ -count=1 -v` |
| **Full suite command** | `go test ./... -count=1` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/credcache/ ./internal/admin/ -count=1 -v`
- **After every plan wave:** Run `go test ./... -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 5 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 04-01-01 | 01 | 1 | ADMIN-04 | unit | `go test ./internal/admin/ -run TestListCredentials -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-01-02 | 01 | 1 | ADMIN-04 | unit | `go test ./internal/credcache/ -run TestEntries_SortedByTTL -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-01-03 | 01 | 1 | ADMIN-04 | unit | `go test ./internal/admin/ -run TestListCredentials_WithNegative -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-01-04 | 01 | 1 | ADMIN-05 | unit | `go test ./internal/admin/ -run TestFlushCredentials -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-01-05 | 01 | 1 | ADMIN-05 | unit | `go test ./internal/credcache/ -run TestDeleteAll -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-02-01 | 02 | 1 | ADMIN-06 | unit | `go test ./internal/admin/ -run TestHealth_Healthy -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-02-02 | 02 | 1 | ADMIN-06 | unit | `go test ./internal/admin/ -run TestHealth_Degraded -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-03-01 | 03 | 1 | NEG-01 | unit | `go test ./internal/credcache/ -run TestVerify_NegativeCache -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-03-02 | 03 | 1 | NEG-02 | unit | `go test ./internal/credcache/ -run TestNegativeCache_PreventsRepeatedDynamoCalls -count=1 -v` | ❌ W0 | ⬜ pending |
| 04-03-03 | 03 | 1 | NEG-03 | unit | `go test ./internal/credcache/ -run TestNegativeCache_Expiry -count=1 -v` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `internal/credcache/cache_test.go` — add tests for SetWithTTL, DeleteAll, Entries methods
- [ ] `internal/credcache/auth_test.go` — add tests for negative caching in Verify, IsDegraded public method
- [ ] `internal/admin/server_test.go` — add tests for handleListCredentials, handleFlushCredentials, handleHealth

---

## Manual-Only Verifications

*All phase behaviors have automated verification.*

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 5s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
