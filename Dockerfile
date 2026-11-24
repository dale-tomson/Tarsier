# Stage 1: Build static multi-arch binary natively on host architecture
ARG GO_VERSION=alpine
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /build/tarsier

# Cache dependency downloads with BuildKit cache mount
COPY tarsier/go.mod tarsier/go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source tree
COPY tarsier/ .

# Minify web assets at image build time (preserves clean/formatted source in repo)
RUN --mount=type=cache,target=/go/pkg/mod \
    go install github.com/tdewolff/minify/v2/cmd/minify@v2.20.37 && \
    find web -type f \( -name "*.html" -o -name "*.css" -o -name "*.js" \) -exec /go/bin/minify -o {} {} \;

# Build arguments for version metadata
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Target architecture set automatically by Docker Buildx
ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -tags netgo \
    -ldflags="-s -w -extldflags '-static' \
      -X 'main.Version=${VERSION}' \
      -X 'main.Commit=${COMMIT}' \
      -X 'main.BuildDate=${BUILD_DATE}'" \
    -o /build/bin/tarsier ./cmd/tarsier

# Stage 2: Minimal distroless container runtime
FROM gcr.io/distroless/static-debian12:nonroot

ENV GOMEMLIMIT=200MiB
ENV GODEBUG=madvdontneed=1

# Copy timezone data & SSL certs from builder
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy static binary
COPY --from=builder /build/bin/tarsier /usr/local/bin/tarsier

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/tarsier"]
