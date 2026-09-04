GO ?= go
export GOTOOLCHAIN := go1.27.1
LINT_VERSION := v2.13.2
LINT := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)

.PHONY: build install fmt fmt-check vet lint test check cross-build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o dist/cross-session-codex ./cmd/cross-session-codex

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
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o dist/cross-session-codex-darwin-arm64 ./cmd/cross-session-codex
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -o dist/cross-session-codex-darwin-amd64 ./cmd/cross-session-codex
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o dist/cross-session-codex-linux-arm64 ./cmd/cross-session-codex
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o dist/cross-session-codex-linux-amd64 ./cmd/cross-session-codex
