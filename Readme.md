<p align="center">
  <img src="icon.svg" alt="Tarsier Icon" width="160">
</p>

<h1 align="center">Tarsier</h1>

<p align="center">
  <strong>Ultra-lightweight, self-hosted, single-server observability engine and dashboard.</strong>
</p>

<p align="center">
  <a href="#beta-notice"><img src="https://img.shields.io/badge/release-v0.2.0--beta-orange.svg" alt="Release: v0.2.0-beta"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/go-1.27-blue.svg" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg" alt="License: AGPL-3.0"></a>
  <a href="#hard-operational-budgets"><img src="https://img.shields.io/badge/cgo-disabled-success.svg" alt="Zero CGO"></a>
  <a href="#hard-operational-budgets"><img src="https://img.shields.io/badge/node.js-none-success.svg" alt="Zero Node"></a>
</p>

**Tarsier** is a single-server observability engine and web dashboard engineered for VPS nodes, homelabs, and edge deployments where monitoring tooling must never starve primary workloads. Packaged as a single static Go binary (`CGO_ENABLED=0`) and container image targeting **`< 256 MB` RAM** (`GOMEMLIMIT=200MiB`), it combines host telemetry, Docker container monitoring, compressed log chunk storage with fast search, in-memory alerting, automated database compaction, and pre-cleanup digest reports into one self-contained service with zero external dependencies.

<p align="center">
  <img src="wiki/images/02-overview-dashboard.png" alt="Tarsier Overview Dashboard" width="100%">
</p>

---

<a name="beta-notice"></a>
> [!WARNING]
> ### BETA TEST SOFTWARE
> Tarsier is currently in active Beta testing (`v0.2.0-beta`). Core architectural systems and functional modules are implemented and tested, but breaking changes to configuration schemas, REST API contracts, or on-disk storage layouts may occur before a stable `v1.0.0` release.

---

## Hard Operational Budgets

| Resource Dimension | Budget / Target | Implementation |
|---|---|---|
| **Container Memory** | `< 256 MB` cgroup ceiling (`60–120 MB` RSS normal) | `GOMEMLIMIT=200MiB`, fixed ring buffers, bounded channels, `sync.Pool` |
| **CPU Overhead** | `< 5%` of one core idle/normal (`0.1–0.5 CPU`) | Zero-alloc `[]byte` log scanning (`< 10 µs`/line), single-writer SQLite queues |
| **Native Dependencies** | Zero CGO | Pure Go (`modernc.org/sqlite`, `klauspost/compress/zstd`), compiles with `CGO_ENABLED=0` |
| **Frontend Runtime** | Zero Node.js | Vendored `petite-vue` (~6 KB), `Pico.css` v2, and `uPlot` embedded via `embed.FS` |
| **Storage Architecture** | Two SQLite DBs only (`config.db`, `metrics.db`) | Append-only `64 KB` Zstd `.chunk` files for logs; no `logs.db` or FTS5 bloat |

---

## Quick Start Guides

### 1. One-Line Docker Run (30-Second Evaluation)

Run Tarsier instantly against your local Docker daemon:

```bash
docker run -d \
  --name tarsier \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v tarsier-data:/data \
  -e TARSIER_ADMIN_USER=admin \
  -e TARSIER_ADMIN_PASSWORD_HASH='$2a$12$ynZsf5yrFkCAE3V2kOMY6uPVgENIRXSBRxAVF9H8Jb0pKy4j.mztW' \
  ghcr.io/dale-tomson/tarsier:latest
```

Open `http://localhost:8080` and log in with username `admin` and password `admin`.

*(Note: On macOS / Windows Docker Desktop, `/proc` reflects the LinuxKit VM. For physical host telemetry, run on bare-metal or VPS Linux).*

### 2. Production Docker Compose

For persistent single-server production deployment with host telemetry:

```yaml
services:
  tarsier:
    image: ghcr.io/dale-tomson/tarsier:latest
    container_name: tarsier
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      - GOMEMLIMIT=200MiB
      - TARSIER_ADMIN_USER=admin
      - TARSIER_ADMIN_PASSWORD_HASH='$2a$12$ynZsf5yrFkCAE3V2kOMY6uPVgENIRXSBRxAVF9H8Jb0pKy4j.mztW'
      - TARSIER_SESSION_SECRET=change-me-to-a-secure-random-32-byte-hex-string
      - HOST_PROC=/host/proc
      - HOST_SYS=/host/sys
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - ./data:/data
      - ./tarsier.yaml:/config/tarsier.yaml:ro
    deploy:
      resources:
        limits:
          memory: 256M
          cpus: "0.50"
```

Start the service:

```bash
docker compose up -d
```

### 3. One-Line Automated Installer

Run the automated installer on any systemd-compatible Linux server with Docker:

```bash
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash
```

This interactive script verifies prerequisites, sets up `/opt/tarsier`, configures environment secrets, and starts the container bundle.

---

## Core Capabilities

* **Host Telemetry Collection:** Direct parsing of `/host/proc`, `/host/sys`, and `statfs` for CPU utilization, load averages, memory distribution, disk space, and network interface throughput.
* **Container Lifecycle Tracking:** Discovery and real-time event streaming (`start`, `stop`, `die`, `oom`, `restart`) directly over `/var/run/docker.sock` without the Docker SDK.
* **Multi-Tier Metric Rollups:** Ingestion of 10-second raw samples downsampled automatically to `sample_1m`, `sample_5m`, and `sample_1d` rollups for long-term capacity planning.
* **Zero-Alloc Log Search:** In-place sub-10 µs timestamp and level detection across JSON, logfmt, and syslog formats, stored in 64 KB Zstandard chunks with Boyer-Moore byte matching and surrounding line context inspection.
* **In-Memory Alerting Engine:** Metric thresholds, log pattern frequencies, and container lifecycle events evaluated through a state machine with hysteresis, sustained duration (`for`), and dispatch via Discord and SMTP.
* **Pre-Cleanup Reports & Maintenance:** Automatic pattern clustering replacing dynamic variables (`{uuid}`, `{ip}`) to deliver error digests before instant $O(1)$ chunk file pruning and bounded SQLite incremental vacuuming.

---

## Documentation

Full operational and developer documentation is maintained in the [GitHub Wiki](https://github.com/dale-tomson/Tarsier/wiki):

| Guide | Description |
|---|---|
| [Quickstart & Deployment](wiki/01-Quickstart-and-Deployment.md) | Docker Compose, standalone binary, systemd service, and reverse proxy setups (Nginx, Caddy, Traefik). |
| [Configuration & Operations](wiki/02-Configuration-and-Operations.md) | Complete `tarsier.yaml` reference, environment expansion, and operational troubleshooting playbooks. |
| [Host & Container Monitoring](wiki/03-Host-and-Container-Monitoring.md) | Telemetry collection mechanics, container metrics streaming, and multi-tier downsampler architecture. |
| [Log Search & Retention](wiki/04-Log-Search-and-Retention.md) | Format detection heuristics, 64 KB Zstd chunk format, Boyer-Moore search, and compaction mechanics. |
| [Alerting & Notifications](wiki/05-Alerting-and-Notifications.md) | Rule persistence in `config.db`, state machine hysteresis, Discord webhook, and SMTP configuration. |
| [REST API & Automation](wiki/06-REST-API-and-Automation.md) | OpenAPI 3.1.0 specification, session authentication, and integration with Postman/Bruno/curl. |
| [Developer Internals](wiki/07-Developer-Internals-and-Contributing.md) | Architecture deep dive, SQLite WAL single-writer concurrency, testing standards, and PR workflows. |

---

## Building from Source

### Prerequisites
* Go 1.27+
* Pure Go build: zero C compiler (gcc/clang) and zero Node.js/npm dependencies.

```bash
# Clone the repository
git clone https://github.com/dale-tomson/Tarsier.git
cd Tarsier

# Run test suite with race detector
make test

# Build single static binary (output at bin/tarsier)
make build

# Validate configuration file
./bin/tarsier -test-config -config ./tarsier.yaml
```

To build the minimal distroless Docker container image locally:

```bash
docker build -t tarsier:local .
```

---

## Security Best Practices

1. **Reverse Proxy & HTTPS:** Always terminate TLS in production using Caddy, Nginx, or Traefik with automatic certificates.
2. **Docker Socket Hardening:** Mount `/var/run/docker.sock` strictly read-only (`:ro`) and isolate Tarsier to a management network.
3. **Restricted Host Mounts:** Always mount `/host/proc` and `/host/sys` read-only (`:ro`).
4. **Secrets Management:** Keep passwords, webhook URLs, and session secrets in environment variables rather than plaintext YAML files.

---

## License

This project is licensed under the [GNU Affero General Public License v3](LICENSE).
