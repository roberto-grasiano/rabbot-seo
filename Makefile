BINARY := rabbot
PKG := ./cmd/rabbot
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Tools installed via `go install` live in $(go env GOPATH)/bin, which may not be
# on PATH. Self-locate them so `make lint`/`make snapshot` work either way.
GOPATH_BIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(GOPATH_BIN)/golangci-lint)
GORELEASER ?= $(shell command -v goreleaser 2>/dev/null || echo $(GOPATH_BIN)/goreleaser)
GOVULNCHECK ?= $(shell command -v govulncheck 2>/dev/null || echo $(GOPATH_BIN)/govulncheck)
BENCHSTAT ?= $(shell command -v benchstat 2>/dev/null || echo $(GOPATH_BIN)/benchstat)

FUZZTIME ?= 20s
BENCHCOUNT ?= 10

.PHONY: build vet lint test cover vulncheck fuzz-smoke bench tidy snapshot all

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

vet:
	go vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

test:
	CGO_ENABLED=1 go test -race ./...

# Identical command to CI's ubuntu coverage leg (lint-parity pattern): atomic
# mode is required under -race. Writes coverage.out (gitignored, uploaded to
# Codecov in CI). Informational only — there is no coverage threshold gate.
cover:
	CGO_ENABLED=1 go test -race -covermode=atomic -coverprofile=coverage.out ./...

# Identical command to CI's vulncheck job + audit.yaml (lint-parity pattern).
# govulncheck is an analysis tool installed on demand, NOT a build dependency
# (no go.mod change). Deliberately UNPINNED (@latest) so it tracks the live
# vuln DB schema. On Go < 1.26.4 this reports the two reachable stdlib
# advisories (GO-2026-5039 / GO-2026-5037) that CI's setup-go 1.26.4 pin clears.
vulncheck:
	[ -x "$(GOVULNCHECK)" ] || go install golang.org/x/vuln/cmd/govulncheck@latest
	$(GOVULNCHECK) ./...

fuzz-smoke:   # -fuzz must match exactly one target => one invocation each
	CGO_ENABLED=0 go test -run='^$$' -fuzz='^FuzzExtract$$'      -fuzztime=$(FUZZTIME) ./internal/extract
	CGO_ENABLED=0 go test -run='^$$' -fuzz='^FuzzRobots$$'       -fuzztime=$(FUZZTIME) ./internal/fetcher
	CGO_ENABLED=0 go test -run='^$$' -fuzz='^FuzzSitemap$$'      -fuzztime=$(FUZZTIME) ./internal/scheduler
	CGO_ENABLED=0 go test -run='^$$' -fuzz='^FuzzNormalizeURL$$' -fuzztime=$(FUZZTIME) ./internal/urlx
	CGO_ENABLED=0 go test -run='^$$' -fuzz='^FuzzValidate$$'     -fuzztime=$(FUZZTIME) ./internal/richresult

# Benchmarks reflect the SHIPPED static binary, so they run with CGO_ENABLED=0
# (modernc.org/sqlite is pure Go — `make test`'s CGO_ENABLED=1 is ONLY because
# -race needs cgo; that is not the bench path). -benchmem reports allocs (every
# bench also calls b.ReportAllocs()); -count=10 gives benchstat enough samples to
# estimate variance. Output is teed to bench.txt (gitignored). benchstat is a
# developer analysis tool installed on demand via
# `go install golang.org/x/perf/cmd/benchstat@latest` and self-located above like
# golangci-lint/govulncheck — it is NEVER a module dependency (go.mod is untouched).
bench:
	[ -x "$(BENCHSTAT)" ] || go install golang.org/x/perf/cmd/benchstat@latest
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchmem -count=$(BENCHCOUNT) ./... | tee bench.txt
	@echo "compare runs with: $(BENCHSTAT) old.txt bench.txt"

tidy:
	go mod tidy

snapshot:
	# Local snapshot of the archives/checksums + SBOMs; skips docker (no daemon
	# needed) and skips signing (keyless cosign needs an interactive OIDC flow —
	# the v0.1.0-rc1 tag rehearses signing in CI). Needs syft on PATH for SBOMs.
	$(GORELEASER) release --snapshot --clean --skip=publish,docker,sign

all: tidy vet lint test build
