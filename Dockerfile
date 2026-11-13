# Stage 1: Build static multi-arch binary
ARG GO_VERSION=alpine
FROM golang:${GO_VERSION} AS builder

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /build

# Cache dependency downloads
COPY go.mod go.sum* ./
RUN go mod download

# Copy source tree
COPY . .

# Build arguments for version metadata
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Target architecture set automatically by Docker Buildx
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -tags netgo \
    -ldflags="-s -w -extldflags '-static' \
      -X 'main.Version=${VERSION}' \
      -X 'main.Commit=${COMMIT}' \
      -X 'main.BuildDate=${BUILD_DATE}'" \
    -o /build/bin/tarsier ./cmd/tarsier

# Stage 2: Minimal distroless container runtime
FROM gcr.io/distroless/static-debian12:nonroot

ENV GOMEMLIMIT=200MiB

# Copy timezone data & SSL certs from builder
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy static binary
COPY --from=builder /build/bin/tarsier /usr/local/bin/tarsier

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/tarsier"]
