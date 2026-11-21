# ==============================================================================
# Tarsier Internal Module Build & Test Automation
# ==============================================================================

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS = -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.Commit=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'

.PHONY: all build test test-cover clean

all: test build

build:
	@mkdir -p ../bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o ../bin/tarsier ./cmd/tarsier

test:
	go test -race -v ./...

test-cover:
	go test -race -coverprofile=../coverage.out ./...
	go tool cover -func=../coverage.out

clean:
	rm -rf ../bin/ ../coverage.out
