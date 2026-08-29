# Llama Man build targets (DESIGN section 16.1).
#
# This skeleton implements the targets the code can actually run today: build,
# go-build, ui-build, test, vet, lint, fmt and clean. The rest of section 16.1 —
# dev, build-all, e2e, openapi, migrate-new, release-snapshot, install-local —
# lands with the features they drive.

SHELL := /bin/bash
.DEFAULT_GOAL := build

MODULE     := github.com/jlbyh2o/llamaman
BUILDINFO  := $(MODULE)/internal/buildinfo
BIN        := dist/llamaman

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u -d "@$${SOURCE_DATE_EPOCH:-$$(date +%s)}" +%Y-%m-%dT%H:%M:%SZ)
CHANNEL ?= stable

LDFLAGS := -s -w \
	-X '$(BUILDINFO).Version=$(VERSION)' \
	-X '$(BUILDINFO).Commit=$(COMMIT)' \
	-X '$(BUILDINFO).Date=$(DATE)' \
	-X '$(BUILDINFO).Channel=$(CHANNEL)'

.PHONY: build go-build ui-build ui test vet lint fmt clean

## build: the full artifact — UI first, then the binary that embeds it.
build: ui-build go-build

## go-build: static, trimmed binary into dist/. CGO_ENABLED=0 is a hard
## constraint: it is why the SQLite driver is modernc.org/sqlite (DESIGN
## section 2) and why cross-compiling to arm64 works.
go-build:
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/llamaman

## ui-build: build the SPA and sync it into the embed directory. This
## OVERWRITES the committed placeholder internal/web/dist/index.html, which
## exists only so `go build ./...` works on a clean checkout (DESIGN section
## 16.1).
ui-build:
	cd ui && npm ci && npm run build
	rm -rf internal/web/dist
	mkdir -p internal/web/dist
	cp -R ui/dist/. internal/web/dist/

## ui: DESIGN section 16.1 spells this target `ui`; it is the same thing.
ui: ui-build

## test: Go tests under the race detector, then the UI suite.
test:
	go test -race ./...
	cd ui && npm run test

## vet: the stdlib vet pass CI runs on every push.
vet:
	go vet ./...

## lint: the section-16.1 set, minus the tools this repo does not vendor yet
## (gofumpt, staticcheck, golangci-lint, govulncheck, shellcheck join as the
## code they check appears).
lint: vet
	gofmt -l . | tee /dev/stderr | (! read)
	cd ui && npm run lint
	cd ui && npx prettier --check .

## fmt: format both halves in place.
fmt:
	gofmt -w ./cmd ./internal
	cd ui && npm run format

## clean: remove build output. The committed placeholder is restored by git.
clean:
	rm -rf dist ui/dist internal/web/dist
	git checkout -- internal/web/dist/index.html 2>/dev/null || true
