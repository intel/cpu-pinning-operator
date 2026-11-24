# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0

IMG ?= cpu-pinning-operator:latest
CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
HELM_CHART ?= deploy/cpu-pinning-operator
HELM_RELEASE ?= cpu-pinning-operator
HELM_NAMESPACE ?= cpu-pinning-operator-system
# Extra helm flags. install/upgrade need the policy target, e.g.
#   make helm-install HELM_ARGS="-f my-values.yaml"
HELM_ARGS ?=
# lint and template render the worked example, as the chart has no default target.
HELM_EXAMPLE_VALUES ?= -f docs/examples/values/kserve.yaml

# Go build hardening flags.
# Reproducibility and security posture:
#   -trimpath        Strip filesystem paths from binary (reproducibility, no path leakage)
#   -buildvcs=auto   Embed VCS information for provenance when building from a git
#                    checkout; a source archive without .git still builds
#   -ldflags "-s -w" Strip debug info: -s omits symbol table, -w omits DWARF
#
# Note: -buildmode=pie omitted to keep the binary fully static. PIE needs an ELF loader
# at runtime, which would force a base image with glibc and OS packages.
# For CGO-free Go binaries, ASLR's marginal benefit does not justify the enlarged CVE surface.
GO_BUILD_FLAGS := -trimpath -buildvcs=auto -ldflags="-s -w"

.PHONY: build
build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/manager ./cmd

.PHONY: test
test:
	go test ./... -count=1

# Real-apiserver guards (test/crdinstall + internal/controller): fetches the
# envtest control-plane binaries, so they run instead of skipping. Why each needs
# a real apiserver is in those packages' doc comments.
# Apiserver generation the CEL cost estimate must match: k8s.io/* v0.37 in go.mod.
ENVTEST_K8S_VERSION ?= 1.37.0
ENVTEST_USE = $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path
.PHONY: test-crd
test-crd: envtest
	assets="$$($(ENVTEST_USE))"; \
	test -d "$$assets" || { echo "setup-envtest returned no usable $(ENVTEST_K8S_VERSION) binaries"; exit 1; }; \
	KUBEBUILDER_ASSETS="$$assets" \
		go test ./test/crdinstall/... ./internal/controller/... -count=1 -v

# The unit-tests workflow exports KUBEBUILDER_ASSETS from this.
.PHONY: envtest-path
envtest-path: envtest
	@$(ENVTEST_USE)

SETUP_ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest
# setup-envtest ships from the controller-runtime repo and tags with it, so take
# the pin from the module graph (@latest drifts, a hand-copied tag goes stale).
# Override with SETUP_ENVTEST_VERSION=... if needed.
SETUP_ENVTEST_VERSION ?= $(shell go list -m -f '{{.Version}}' sigs.k8s.io/controller-runtime)
.PHONY: envtest
envtest:
	@[ "$$($(SETUP_ENVTEST) version 2>/dev/null | awk '{print $$NF}')" = "$(SETUP_ENVTEST_VERSION)" ] || \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

# End-to-end tests against a REAL, NRI-enabled cluster with the operator already
# installed (envtest/kind cannot pin CPUs). Uses the ambient kubeconfig. Opt in
# to the heavier cases with env vars:
#   E2E_NAMESPACE       override the webhook-target namespace (default: read off
#                       the installed BalloonPinningPolicy, created if absent)
#   E2E_RUN_PINNING=1   also assert NRI actually pinned (needs operator image + DS)
#   E2E_DASHBOARDS=1    also assert per-node dashboard ConfigMaps
#
# The suite emits one Allure result JSON per test into $(ALLURE_RESULTS)/ (pass,
# fail, or skip). allure-go writes into $ALLURE_OUTPUT_PATH/allure-results, so
# ALLURE_OUTPUT_PATH is the single overridable knob and ALLURE_RESULTS is derived
# from it — this keeps the directory `test-e2e` writes and `test-e2e-report` reads
# in lockstep. It defaults to the repo root (next to this Makefile, regardless of
# the package under test); the dir is gitignored. Render the HTML report
# afterwards with `make test-e2e-report`.
ALLURE_OUTPUT_PATH ?= $(CURDIR)
ALLURE_RESULTS := $(ALLURE_OUTPUT_PATH)/allure-results
ALLURE_REPORT ?= allure-report

# E2E_STRICT is set here rather than left to the caller, because the default run is
# where a missing-configuration skip has to be loud: a test that could not run because
# the cluster was half set up fails instead of reporting a clean skip. It does NOT turn
# an opt-in gate or a capability the hardware lacks into a failure — a single-NUMA box
# still skips what it cannot do. Override with `E2E_STRICT= make test-e2e` to report
# missing-configuration skips as skips instead of failures.
E2E_STRICT ?= 1

.PHONY: test-e2e
test-e2e:
	E2E_STRICT="$(E2E_STRICT)" ALLURE_OUTPUT_PATH="$(ALLURE_OUTPUT_PATH)" \
		go test -tags e2e ./test/e2e/... -count=1 -v -timeout 15m

# Every automatable security e2e test, in four passes over the
# installed release: hardened (metrics.requireAuth=true), scrape (false),
# destructive, then the webhook NetworkPolicy test alone. Variables and per-pass output: docs/internal/e2e-internals.md.
# DRY_RUN=1 prints the helm/kubectl/go commands without running them.
.PHONY: test-validation
test-validation:
	E2E_STRICT="$(E2E_STRICT)" ALLURE_OUTPUT_PATH="$(ALLURE_OUTPUT_PATH)" hack/e2e-validation.sh

# Restore the shared objects a crashed run left changed (from its on-disk snapshot)
# and delete every object labelled cpu-pinning-e2e/run, from any run. Runs no test.
.PHONY: test-e2e-sweep
test-e2e-sweep:
	E2E_SWEEP_ONLY=1 ALLURE_OUTPUT_PATH="$(ALLURE_OUTPUT_PATH)" \
		go test -tags e2e ./test/e2e/ -run '^TestE2ESweepOnly$$' -count=1 -v -timeout 15m

# The cluster-free checks; E2E_CLUSTER=1 adds test-validation.
.PHONY: test-all
test-all: test test-crd test-e2e-guards
	@if [ -n "$(E2E_CLUSTER)" ]; then $(MAKE) test-validation; fi

# The e2e guards that need no cluster: the skip classification guards, the Allure
# status guards, the policy-resolution guards, the probe-stall diagnostic, the run-label
# guard and the sweeper check.
# Same checks the pre-commit hook runs,
# so they can be re-run on their own without staging anything — the hook takes the vet
# from the `vet` target rather than from here, which covers the same package.
.PHONY: test-e2e-guards
test-e2e-guards:
	go vet -tags e2e ./test/e2e/
	ALLURE_OUTPUT_PATH="$(ALLURE_OUTPUT_PATH)" \
		go test -tags e2e ./test/e2e/ -run 'TestNoBareSkips|TestMetricsSkipRouting|TestReserveAuthorityModeSkip|TestSourceScanSkipRouting|TestMetricAssertions|TestAllure|TestInstalledPolicies|TestSelectorTargetsNamespace|TestPolicyMatchRestore|TestProbeStall|TestNodeMetricsMissing|TestPodNRTReadsBindThePod|TestReservedFootprintModelsTheSpread|TestProbeWatch|TestSaturationCountsOnlyCapacityDenials|TestWatchRevisions|TestServingChainVerdict|TestRateLimitVerdict|TestReadinessInitLogVerdict|TestTLSProbe|TestCurlProbe|TestRestrictedGaps|TestPIIMatcher|TestLeaderPodOnce|TestRaxLedgerHeld|TestStaleNRTVerdict|TestStaleNRTFrozenCapacity|TestEveryCreateIsLabelled|TestSweepRestoresAndDeletesOnlyLabelled|TestPolicyIntentEditRetriesConflicts' -count=1 -v

# Render the HTML report from the Allure result JSON produced by `make test-e2e`.
# Needs the separate `allure` CLI (not a Go dependency): install via
# `npm install -g allure-commandline` or download a release from
# https://github.com/allure-framework/allure2/releases. See
# docs/internal/e2e-internals.md.
.PHONY: test-e2e-report
test-e2e-report:
	@command -v allure >/dev/null 2>&1 || { \
		echo "allure CLI not found. Install it: npm install -g allure-commandline"; \
		echo "or download a release from https://github.com/allure-framework/allure2/releases"; \
		exit 1; }
	@test -d "$(ALLURE_RESULTS)" || { \
		echo "no $(ALLURE_RESULTS)/ directory — run 'make test-e2e' first"; exit 1; }
	allure generate --clean "$(ALLURE_RESULTS)" -o "$(ALLURE_REPORT)"
	@echo "report written to $(ALLURE_REPORT)/ — open with 'allure open $(ALLURE_REPORT)'"

.PHONY: hooks
hooks:
	git config core.hooksPath .githooks
	@echo "git hooks enabled (core.hooksPath=.githooks). pre-commit now runs gofmt+vet+test."

.PHONY: fmt
fmt:
	go fmt ./...

# Two passes, because every file under test/e2e is behind `//go:build e2e`: the
# untagged pass resolves that package to "build constraints exclude all Go files" and
# vets nothing, silently and with a zero exit. Naming it with the tag set is what makes
# the e2e package reachable by an analyser at all — here, and in CI, which runs this.
.PHONY: vet
vet:
	go vet ./...
	go vet -tags e2e ./test/e2e/

# Toolchain the dead-code tools must build under: this project's go.mod `go`
# directive. `go run <tool>@latest` otherwise downgrades to the tool module's own
# (lower) minimum, which then can't load this project's newer packages — so pin it.
GO_TOOLCHAIN ?= go$(shell awk '/^go /{print $$2; exit}' go.mod)

# Dead-code scan: unreachable functions (x/tools deadcode) plus unused unexported
# symbols (staticcheck U1000). A reporting target, not a gate — it prints candidates
# and always exits 0; judge them against the diff under review. Uses `go run` so there's no separate install step; prints a hint and
# skips cleanly when a tool can't be fetched (offline, no module cache).
# Both tools exit non-zero on findings AND on error, so a bare `|| echo` can't tell
# them apart. Detect "tool didn't run" by probing `go run ... -h` (exits 0 if the
# tool built) and only print the unavailable hint then; otherwise any lines above
# are real findings (or clean if none). Always exits 0 — the reviewer judges scope.
.PHONY: deadcode
deadcode:
	@echo ">> deadcode: unreachable functions (nothing below = clean)"
	@GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/tools/cmd/deadcode@latest ./... 2>/dev/null \
		|| GOTOOLCHAIN=$(GO_TOOLCHAIN) go run golang.org/x/tools/cmd/deadcode@latest -h >/dev/null 2>&1 \
		|| echo "   (deadcode unavailable — go install golang.org/x/tools/cmd/deadcode@latest, or run online once)"
	@echo ">> staticcheck U1000: unused unexported symbols (nothing below = clean)"
	@GOTOOLCHAIN=$(GO_TOOLCHAIN) go run honnef.co/go/tools/cmd/staticcheck@latest -checks U1000 ./... 2>/dev/null \
		|| GOTOOLCHAIN=$(GO_TOOLCHAIN) go run honnef.co/go/tools/cmd/staticcheck@latest -h >/dev/null 2>&1 \
		|| echo "   (staticcheck unavailable — go install honnef.co/go/tools/cmd/staticcheck@latest, or run online once)"

# golangci-lint run of the analyzers configured in .golangci.yml. This target is a
# REPORTING one — it surfaces candidates, always exits 0, and is NOT part of the
# pre-commit gate (.githooks/pre-commit stays gofmt+vet+test). The CI job running the
# same config does gate, so a finding printed here turns a pull request red even
# though this target shrugs at it.
# Run via `go run @latest` under the pinned GOTOOLCHAIN so there's
# no separate install step (same pattern as `deadcode`). Prints a hint and skips
# cleanly when the tool can't be fetched (offline, no module cache). golangci-lint
# exits non-zero on findings AND on error, so probe `--version` to distinguish
# "tool didn't build" from real findings; always exits 0 — the reviewer judges scope.
.PHONY: lint
lint:
	@echo ">> golangci-lint (see .golangci.yml for the enabled set; nothing below = clean)"
	@if GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 --version >/dev/null 2>&1; then \
		GOTOOLCHAIN=$(GO_TOOLCHAIN) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run ./... || true; \
	else \
		echo "   (golangci-lint unavailable — go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2, or run online once)"; \
	fi

# Where the marker-generated RBAC/webhook baselines land. These are the CANONICAL
# permission set + webhook shape derived from the +kubebuilder:rbac / :webhook
# markers in internal/controller and internal/webhook. They are NOT applied
# directly (the chart ships hand-templated, release-scoped rbac.yaml /
# mutatingwebhook.yaml with Helm expressions controller-gen can't emit); instead
# a drift-guard test (test/generated) checks the chart templates against these
# baselines, so a marker change that isn't mirrored into the chart fails the gate.
GENERATED_DIR ?= deploy/generated

.PHONY: generate
generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=$(HELM_CHART)/crds
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./internal/..." output:rbac:artifacts:config=$(GENERATED_DIR)
	$(CONTROLLER_GEN) webhook paths="./internal/..." output:webhook:artifacts:config=$(GENERATED_DIR)

.PHONY: controller-gen
controller-gen:
	@test -f $(CONTROLLER_GEN) || GOBIN=$$(go env GOPATH)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0

.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push:
	docker push ${IMG}

.PHONY: helm-lint
helm-lint:
	helm lint $(HELM_CHART) $(HELM_EXAMPLE_VALUES) $(HELM_ARGS)

.PHONY: helm-template
helm-template:
	helm template $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) $(HELM_EXAMPLE_VALUES) $(HELM_ARGS)

.PHONY: helm-install
helm-install:
	helm install $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) --create-namespace $(HELM_ARGS)

.PHONY: helm-upgrade
helm-upgrade:
	helm upgrade $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) $(HELM_ARGS)

.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall $(HELM_RELEASE) -n $(HELM_NAMESPACE)
