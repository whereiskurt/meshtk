---
phase: 3
slug: admin-api
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-10
---

# Phase 3 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) + net/http/httptest |
| **Config file** | None — `go test ./...` |
| **Quick run command** | `go test ./internal/admin/ -v -count=1` |
| **Full suite command** | `go test ./... -count=1` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/admin/ -v -count=1`
- **After every plan wave:** Run `go test ./... -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 10 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 03-01-01 | 01 | 1 | ADMIN-01 | unit | `go test ./internal/admin/ -run TestEvict -v -count=1` | ❌ W0 | ⬜ pending |
| 03-01-02 | 01 | 1 | ADMIN-02 | unit | `go test ./internal/admin/ -run TestRefresh -v -count=1` | ❌ W0 | ⬜ pending |
| 03-01-03 | 01 | 1 | ADMIN-03 | unit | `go test ./internal/admin/ -run TestStats -v -count=1` | ❌ W0 | ⬜ pending |
| 03-01-04 | 01 | 1 | ADMIN-07 | integration | `go test ./internal/admin/ -run TestHandler -v -count=1` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `internal/admin/server_test.go` — stubs for ADMIN-01, ADMIN-02, ADMIN-03, ADMIN-07
- [ ] `Cache.Size()` method — wraps `inner.EstimatedSize()` for stats entry count
- [ ] `CacheAuthenticator.ResetCircuitBreaker()` — public method to reset failure counter for refresh

*These are prerequisite additions to existing code before admin handler tests can verify behavior.*

---

## Manual-Only Verifications

*All phase behaviors have automated verification.*

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 10s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
