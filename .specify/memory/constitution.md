# sori Constitution

<!--
  ②-form (D-12), authority revision AR-2026-08-17.1: this file does NOT own
  cross-repo invariants. It consumes the task Authority Snapshot and indexes
  only THIS repo's own enforced constraints. SoT for those is the rules
  themselves (Makefile gates / CI), not this prose.
-->

## Cross-repo authority — verified revision-pinned repository mirror

Cross-repo platform meaning is selected by the external Authority Router. For
`AR-2026-08-17.1` the scoped authority chain is:

- platform invariants: `Platform Spec Wiki — CURRENT / 1. constitution`
- platform structure / responsibility / call direction:
  `Platform Spec Wiki — CURRENT / 2. architecture`
- repository-portable invariant mirror: `HeaInSeo/NodeVault` —
  `docs/PLATFORM_MASTER_DESIGN.md §4.1–§4.10`
- mirror verification record: `HeaInSeo/NodeVault` —
  `docs/AUTHORITY_MIRROR_VERIFICATION.md`

sori does **not** treat NodeVault §4 as an independent platform canonical. A
task may consume the repository mirror for cross-repo invariant meaning only
when **all** of the following are true:

1. the task `Authority Snapshot` declares `AR-2026-08-17.1`;
2. the NodeVault verification record says `SYNC STATUS: VERIFIED`;
3. the mirror blob SHA matches the blob SHA recorded by that verification record;
4. every scoped/domain/component authority required by the sori task is
   explicitly present in the task `Authority Snapshot`;
5. no semantic conflict with the current Authority Router/upstream authority has
   been detected.

If any condition is missing, `STALE`, `UNKNOWN`, mismatched, or conflicting, stop
with `AUTHORITY_CONFLICT`; do not choose a source by timestamp, filename, or
search rank. **Revision equality alone is not sufficient.**

The current repository verification record covers platform invariants only. sori
work that depends on platform structure/ownership/call-direction or a specific
OCI/referrer/registry contract must carry the exact CURRENT architecture and
relevant scoped/component contract directly in the task `Authority Snapshot`.

Note: sori is the OCI-protocol library that the platform's **sori boundary**
invariant is written about. That boundary is cross-repo and is selected by the
current platform authority chain; it is therefore **not** restated or scoped
here — this constitution indexes only sori's own repo-local enforcement.

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

**Authority: CURRENT platform invariant under `AR-2026-08-17.1`. Enforcement in
this repo: PROPOSED where no deterministic local gate exists.** sori has no
deterministic rule that generally enforces this invariant today. The platform
invariant's authority status and this repo's local enforcement status are
separate axes.

## Governance

Cross-repo semantics cannot be amended by editing this constitution, a
repository mirror, or its verification record alone. They follow the task's
current Authority Snapshot; a new platform authority revision must be accepted
before repository mirrors are synchronized and independently re-verified.

**Version**: 2.1.0 | **Ratified**: 2026-08-02 | **Last Amended**: 2026-08-17
