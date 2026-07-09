# cganno build management.
#
# Pure-Go (modernc.org/sqlite + hts) ⇒ CGO_ENABLED=0 cross-compiles cleanly.
# GOWORK=off is required: the parent go.work references modules that don't exist.

BIN     := cganno
PKG     := ./cmd/cganno
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      := GOWORK=off CGO_ENABLED=0 go

# Release matrix. windows/amd64 is intentionally NOT in the default `cross`
# (see the separate `dist-windows` target).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.DEFAULT_GOAL := build
.PHONY: build test vet fmt tidy clean install cross cross-bin dist dist-windows help $(PLATFORMS)

## build: compile for the host platform into bin/
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## test: run the test suite with the race detector
test:
	GOWORK=off go test -race ./...

## vet: run go vet
vet:
	GOWORK=off go vet ./...

## fmt: format all Go files
fmt:
	gofmt -w .

## tidy: tidy go.mod
tidy:
	GOWORK=off go mod tidy

## install: install cganno into GOBIN
install:
	$(GO) install -ldflags '$(LDFLAGS)' $(PKG)

## cross: build release tarballs for linux,darwin x amd64,arm64
cross: $(PLATFORMS)
dist: cross

## cross-bin: build raw release binaries (no archive) for linux,darwin x amd64,arm64
# Emits dist/cganno-<version>-<os>-<arch> (the bare executable) — used by the
# release workflow, which attaches the raw binaries to the GitHub Release.
cross-bin:
	@mkdir -p dist; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BIN)-$(VERSION)-$$os-$$arch; \
		echo "building $$name"; \
		GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -ldflags '$(LDFLAGS)' -o dist/$$name $(PKG); \
	done

# One target per platform: build a static binary and tar it.
$(PLATFORMS):
	@os=$$(echo $@ | cut -d/ -f1); arch=$$(echo $@ | cut -d/ -f2); \
	name=$(BIN)-$(VERSION)-$$os-$$arch; \
	echo "building $$name"; \
	mkdir -p dist/$$name; \
	GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
		go build -ldflags '$(LDFLAGS)' -o dist/$$name/$(BIN) $(PKG); \
	tar -C dist -czf dist/$$name.tar.gz $$name; \
	rm -rf dist/$$name

## dist-windows: build a windows/amd64 zip (opt-in; not built by default)
dist-windows:
	@name=$(BIN)-$(VERSION)-windows-amd64; \
	mkdir -p dist/$$name; \
	GOWORK=off CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build -ldflags '$(LDFLAGS)' -o dist/$$name/$(BIN).exe $(PKG); \
	(cd dist && zip -qr $$name.zip $$name); \
	rm -rf dist/$$name

## clean: remove build artifacts
clean:
	rm -rf bin dist

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
