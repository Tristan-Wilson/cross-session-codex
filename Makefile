GO ?= go
export GOTOOLCHAIN := go1.27.1
LINT_VERSION := v2.13.2
LINT := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)
RELEASE_TOOL := $(GO) run ./cmd/csc-release
RELEASE_DIR ?= dist/release
SNAPSHOT_DIR ?= dist/snapshot
export TAG RELEASE_DIR SNAPSHOT_DIR

.PHONY: build install fmt fmt-check vet lint test check cross-build release release-snapshot

build:
	$(RELEASE_TOOL) --build --out dist/cross-session-codex

install: build
	./dist/cross-session-codex install

fmt:
	$(LINT) fmt

fmt-check:
	$(LINT) fmt --diff

vet:
	$(GO) vet ./...

lint:
	$(LINT) run

test:
	$(GO) test -race ./...

check: fmt-check vet lint test

cross-build:
	$(RELEASE_TOOL) --cross-build --out dist

# Package only; neither target uploads, installs, or modifies Git tags.
release:
	$(RELEASE_TOOL) --tag "$$TAG" --out "$$RELEASE_DIR"

release-snapshot:
	$(RELEASE_TOOL) --snapshot --out "$$SNAPSHOT_DIR"
