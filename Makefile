# ==============================================================================
# Tarsier Build & Release Automation
# ==============================================================================

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS = -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.Commit=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'

IMAGE_NAME ?= ghcr.io/tarsier-obs/tarsier:$(VERSION)

.PHONY: all build build-all test test-cover clean docker-build

all: test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/tarsier ./cmd/tarsier

build-all:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/tarsier-linux-amd64 ./cmd/tarsier
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/tarsier-linux-arm64 ./cmd/tarsier

test:
	go test -race -v ./...

test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

docker-build:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE_NAME) .

clean:
	rm -rf bin/ coverage.out
