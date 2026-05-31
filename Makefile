SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c

LOCALBIN    := $(CURDIR)/bin
REPORT_DIR  := $(CURDIR)/reports
GOCACHE_DIR := /tmp/sori-gocache
GOTMPDIR    := /tmp/sori-gotmp

GOLANGCI_LINT         := $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION := v2.11.3

GOVULNCHECK         := $(LOCALBIN)/govulncheck
GOVULNCHECK_VERSION := v1.1.4

GOENV := GOCACHE="$(GOCACHE_DIR)" GOTMPDIR="$(GOTMPDIR)"

# All packages. Integration tests (those requiring a live registry) are
# excluded by the -short flag at test time, not by package scope.
PKGS_ALL      := ./...
# internal/bench requires -tags bench and has no regular test files;
# exclude it from coverage and lint so the combined total is meaningful.
PKGS_CORE     := $(shell go list ./... | grep -v 'internal/bench')
PKGS_SECURITY := $(shell go list ./... | grep -v 'internal/bench')

.PHONY: test coverage fmt vet lint lint-depguard lint-fix lint-security \
        vuln vuln-all golangci-lint govulncheck

# ── Tests ─────────────────────────────────────────────────────────────────────

# -short skips tests that require an external registry (Harbor etc.).
test:
	@mkdir -p "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go test -v -race -cover -short $(PKGS_ALL)

# ── Coverage ──────────────────────────────────────────────────────────────────

coverage:
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go test -short $(PKGS_CORE) -coverprofile="$(REPORT_DIR)/cover.out" -covermode=atomic
	go tool cover -func="$(REPORT_DIR)/cover.out" | tee "$(REPORT_DIR)/coverage.txt"

# ── Format / Vet ──────────────────────────────────────────────────────────────

fmt:
	go fmt $(PKGS_ALL)

vet:
	@mkdir -p "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go vet $(PKGS_ALL)

# ── Lint ──────────────────────────────────────────────────────────────────────

lint: golangci-lint lint-depguard
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOLANGCI_LINT) run --config=.golangci.yml $(PKGS_CORE) | tee "$(REPORT_DIR)/lint.txt"

lint-depguard: golangci-lint
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOLANGCI_LINT) run --enable-only depguard $(PKGS_CORE) | tee "$(REPORT_DIR)/lint-depguard.txt"

lint-fix: golangci-lint
	$(GOLANGCI_LINT) run --config=.golangci.yml --fix $(PKGS_CORE)

# Security-focused lint (gosec). Kept separate so findings do not block
# regular development CI — run via security-observe workflow or manually.
lint-security: golangci-lint
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@echo "[sori] security scan scope: $(PKGS_SECURITY)" | tee "$(REPORT_DIR)/lint-security-summary.txt"
	@set +e; \
	$(GOENV) $(GOLANGCI_LINT) run --enable-only gosec $(PKGS_SECURITY) \
	| tee "$(REPORT_DIR)/gosec.txt"; \
	echo "gosec_exit=$$?" | tee -a "$(REPORT_DIR)/lint-security-summary.txt"

# ── Vulnerability scan ────────────────────────────────────────────────────────

vuln: govulncheck
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@set +e; \
	$(GOENV) $(GOVULNCHECK) $(PKGS_SECURITY) 2>&1 | tee "$(REPORT_DIR)/govulncheck-core.txt"; \
	echo "govulncheck_exit=$$?" | tee "$(REPORT_DIR)/govulncheck-core.summary"

vuln-all: govulncheck
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@set +e; \
	$(GOENV) $(GOVULNCHECK) ./... 2>&1 | tee "$(REPORT_DIR)/govulncheck-all.txt"; \
	echo "govulncheck_all_exit=$$?" | tee "$(REPORT_DIR)/govulncheck-all.summary"

# ── Tool installation ─────────────────────────────────────────────────────────

golangci-lint:
	@mkdir -p "$(LOCALBIN)"
	@test -x "$(GOLANGCI_LINT)" || GOBIN="$(LOCALBIN)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

govulncheck:
	@mkdir -p "$(LOCALBIN)"
	@test -x "$(GOVULNCHECK)" || GOBIN="$(LOCALBIN)" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
