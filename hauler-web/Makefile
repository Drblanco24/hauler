# Makefile for hauler-web
#
# Mirrors the hauler CLI's conventions so the two projects feel like one
# family: gofmt and go vet as gates, tests always with -race.

SHELL := /bin/bash

GO_FILES        := ./...
GO_COVERPROFILE := coverage.out
BIN_DIRECTORY   := bin
BINARY          := $(BIN_DIRECTORY)/hauler-web

# The hauler binary this build drives. Pinned so a deployment's behaviour is
# reproducible; every run records the version it actually used.
HAULER_VERSION ?= v2.0.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all
all: fmt vet test build

.PHONY: build
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/hauler-web

.PHONY: fmt
fmt:
	go fmt $(GO_FILES)

.PHONY: vet
vet:
	go vet $(GO_FILES)

.PHONY: lint
lint:
	golangci-lint run

# Unit tests. No database, no network, no hauler binary required.
.PHONY: test
test:
	go test $(GO_FILES) -cover -race -covermode=atomic -coverprofile=$(GO_COVERPROFILE)

# Tests that shell out to a real hauler binary. Point HAULERWEB_TEST_HAUL at a
# haul archive to also exercise store-info parsing over real content.
.PHONY: test-integration
test-integration:
	go test -tags=integration -race $(GO_FILES)

.PHONY: cover
cover: test
	go tool cover -func=$(GO_COVERPROFILE) | tail -1

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN_DIRECTORY) $(GO_COVERPROFILE)
