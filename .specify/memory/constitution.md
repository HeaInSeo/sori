# sori Constitution

<!--
  ②-form (D-12): this file does NOT own cross-repo invariants. It references the
  platform canonical constitution and indexes only THIS repo's own enforced
  constraints. SoT for those is the rules themselves (Makefile gates / CI), not
  this prose.
-->

## Cross-repo invariants live in the Platform Spec Wiki (canonical)

Cross-repo invariants — reproducibility, `casHash`, `stableRef`, the artifact
dual-axis (`lifecycle_phase` / `integrity_health`), the sori boundary, and
"do not record what you did not observe" (§1.10) — are owned solely by the
**Platform Spec Wiki `1. constitution`**. This document does not restate or fork
them; on any conflict, the wiki §1 wins.

Note: sori is the OCI-protocol library that the platform's **sori boundary**
invariant is written about. That boundary is cross-repo and wiki-owned; it is
therefore **not** restated or scoped here — this constitution indexes only
sori's own repo-local enforcement.

## Process discipline (repo-operational — owned by this repo)

- **Deterministic gates are the guarantee.** Merge is decided by deterministic
  checks (tests with `-race`, gosec, govulncheck, golangci-lint, registry smoke,
  module hygiene, actionlint, CodeQL). LLM/agent review is **advisory**: a
  passing review never merges alone, a failing gate is never overridden.
- **Spec-anchored change**; **test-first** (behavioral changes ship with tests
  that fail before / pass after; CI runs the `-race` variant); **Builder/Critic
  separation** (read-only Critic pass before merge).
- **Local verify (before a PR):** `make test lint lint-security vuln`.
- **Branch protection**: `master` lands via PR with required checks; no direct
  pushes.

## Repo-local enforced constraints (derived index — NOT canonical)

> Derived index of THIS repo's own gates. Not canonical — SoT is the gate itself.

- **race tests** (IMPLEMENTED — `make test`, CI `test.yml`): `go test -race
  -short -shuffle=on -count=1 ./...`; concurrency safety, blocking.
- **golangci-lint** (IMPLEMENTED — `make lint`, CI `lint.yml`): lint gate
  (`.golangci.yml`) plus a dedicated `depguard` pass (`make lint-depguard`).
- **gosec / security lint** (IMPLEMENTED — `make lint-security`, CI `test.yml`):
  gosec static security analysis, blocking (the enforcing variant, not the
  `lint-security-observe` report-only variant).
- **govulncheck** (IMPLEMENTED — `make vuln`, CI `test.yml`): vulnerability scan,
  blocking (the enforcing variant, not the `vuln-observe` report-only variant).
- **registry smoke** (IMPLEMENTED — `make smoke-cli`, CI `test.yml`
  `registry-smoke` job): full push → inspect → fetch → overwrite/skip cycle
  against a real `registry:2` service, blocking.
- **module hygiene** (IMPLEMENTED — CI `test.yml`): `go mod tidy` followed by
  `git diff --exit-code` on `go.mod`/`go.sum`, blocking.
- **actionlint** (IMPLEMENTED — CI `lint.yml`): GitHub Actions workflow
  validation, blocking.
- **CodeQL** (IMPLEMENTED — CI `codeql.yml`, on PR to `master`): SAST for `go`
  and `actions`.
- **coverage report** (NOT a threshold gate): `make coverage` runs in CI
  (`test.yml`) and uploads `reports/`, but enforces no minimum-coverage
  threshold (no `coverage-check` target exists); it fails only if the tests
  themselves fail. Do not treat as a coverage gate.
- **security-observe** (NOT a gate): `security-observe.yml` is
  `workflow_dispatch`-only and runs the `-observe` (report-only) targets; it is
  observational, never blocking.

## §1.10 — "do not record what you did not observe"

**Status: PROPOSED (not enforced in this repo).** §1.10 is a cross-repo
invariant owned by the wiki; sori has **no deterministic rule** enforcing it
today. Marked PROPOSED, not IMPLEMENTED, until such a gate exists.

**Version**: 1.0.0 | **Ratified**: 2026-08-02 | **Last Amended**: 2026-08-02
