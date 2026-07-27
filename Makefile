SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c

LOCALBIN    := $(CURDIR)/bin
DIST_DIR    := $(CURDIR)/dist
REPORT_DIR  := $(CURDIR)/reports
GOCACHE_DIR := /tmp/sori-gocache
GOTMPDIR    := /tmp/sori-gotmp

GOLANGCI_LINT         := $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION := v2.11.3

GOVULNCHECK         := $(LOCALBIN)/govulncheck
GOVULNCHECK_VERSION := v1.1.4

GOENV := GOCACHE="$(GOCACHE_DIR)" GOTMPDIR="$(GOTMPDIR)"
SORICTL_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# All packages. Integration tests (those requiring a live registry) are
# excluded by the -short flag at test time, not by package scope.
PKGS_ALL      := ./...
# Lint and security targets use ./... patterns (golangci-lint requires relative patterns).
PKGS_CORE     := ./...
PKGS_SECURITY := ./...
# Coverage excludes internal/bench: it requires -tags bench and has no
# regular test files, so its 0% coverage would drag down the combined total.
PKGS_COVERAGE := $(shell go list ./... | grep -v 'internal/bench')

.PHONY: test coverage test-registry-integration smoke-cli test-real-dataset \
        fmt vet lint lint-depguard lint-fix lint-security lint-security-observe vuln vuln-observe vuln-all vuln-all-observe \
        build-sorictl release-dist golangci-lint govulncheck

# ── Tests ─────────────────────────────────────────────────────────────────────

# -short skips tests that require an external registry (Harbor etc.).
test:
	@mkdir -p "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go test -v -race -cover -short -shuffle=on -count=1 $(PKGS_ALL)

test-registry-integration:
	@mkdir -p "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go test -v -run TestRegistryIntegration ./...

smoke-cli: build-sorictl
	SORICTL_BIN="$(LOCALBIN)/sorictl" ./scripts/sorictl-registry-smoke.sh small

test-real-dataset: build-sorictl
	SORICTL_BIN="$(LOCALBIN)/sorictl" ./scripts/sorictl-registry-smoke.sh real

# ── Coverage ──────────────────────────────────────────────────────────────────

coverage:
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go test -short -shuffle=on -count=1 $(PKGS_COVERAGE) -coverprofile="$(REPORT_DIR)/cover.out" -covermode=atomic
	go tool cover -func="$(REPORT_DIR)/cover.out" | tee "$(REPORT_DIR)/coverage.txt"

# ── Format / Vet ──────────────────────────────────────────────────────────────

fmt:
	go fmt $(PKGS_ALL)

vet:
	@mkdir -p "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go vet $(PKGS_ALL)

# ── Builds ───────────────────────────────────────────────────────────────────

build-sorictl:
	@mkdir -p "$(LOCALBIN)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) go build -trimpath -o "$(LOCALBIN)/sorictl" ./cmd/sorictl

release-dist:
	@rm -rf "$(DIST_DIR)"
	@mkdir -p "$(DIST_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@set -e; \
	for target in $(SORICTL_TARGETS); do \
		os="$${target%/*}"; \
		arch="$${target#*/}"; \
		ext=""; \
		if [[ "$$os" == "windows" ]]; then ext=".exe"; fi; \
		out="$(DIST_DIR)/sorictl_$${os}_$${arch}$${ext}"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GOENV) go build -trimpath -o "$$out" ./cmd/sorictl; \
	done
	@(cd "$(DIST_DIR)" && sha256sum * > checksums.txt)

# ── Lint ──────────────────────────────────────────────────────────────────────

lint: golangci-lint lint-depguard
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOLANGCI_LINT) run --config=.golangci.yml $(PKGS_CORE) | tee "$(REPORT_DIR)/lint.txt"

lint-depguard: golangci-lint
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOLANGCI_LINT) run --enable-only depguard $(PKGS_CORE) | tee "$(REPORT_DIR)/lint-depguard.txt"

lint-fix: golangci-lint
	$(GOLANGCI_LINT) run --config=.golangci.yml --fix $(PKGS_CORE)

# Security-focused lint (gosec). The default target enforces findings; use
# lint-security-observe when a report artifact is needed without failing CI.
lint-security: golangci-lint
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@echo "[sori] security scan scope: $(PKGS_SECURITY)" | tee "$(REPORT_DIR)/lint-security-summary.txt"
	$(GOENV) $(GOLANGCI_LINT) run --enable-only gosec $(PKGS_SECURITY) \
	| tee "$(REPORT_DIR)/gosec.txt"

lint-security-observe: golangci-lint
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@echo "[sori] security scan scope: $(PKGS_SECURITY)" | tee "$(REPORT_DIR)/lint-security-summary.txt"
	@set +e; \
	$(GOENV) $(GOLANGCI_LINT) run --enable-only gosec $(PKGS_SECURITY) \
	| tee "$(REPORT_DIR)/gosec.txt"; \
	echo "gosec_exit=$$?" | tee -a "$(REPORT_DIR)/lint-security-summary.txt"

# ── Vulnerability scan ────────────────────────────────────────────────────────

vuln: govulncheck
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOVULNCHECK) $(PKGS_SECURITY) 2>&1 | tee "$(REPORT_DIR)/govulncheck-core.txt"

vuln-observe: govulncheck
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	@set +e; \
	$(GOENV) $(GOVULNCHECK) $(PKGS_SECURITY) 2>&1 | tee "$(REPORT_DIR)/govulncheck-core.txt"; \
	echo "govulncheck_exit=$$?" | tee "$(REPORT_DIR)/govulncheck-core.summary"

vuln-all: govulncheck
	@mkdir -p "$(REPORT_DIR)" "$(GOCACHE_DIR)" "$(GOTMPDIR)"
	$(GOENV) $(GOVULNCHECK) ./... 2>&1 | tee "$(REPORT_DIR)/govulncheck-all.txt"

vuln-all-observe: govulncheck
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
