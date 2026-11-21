<p align="center">
  <img src="icon.svg" alt="Tarsier Icon" width="160">
</p>

<h1 align="center">Tarsier</h1>

<p align="center">
  <strong>Ultra-lightweight, self-hosted, single-server observability engine and dashboard.</strong>
</p>

<p align="center">
  <a href="#beta-notice"><img src="https://img.shields.io/badge/status-beta-orange.svg" alt="Status: Beta"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/go-1.27-blue.svg" alt="Go Version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg" alt="License: AGPL-3.0"></a>
  <a href="#architectural-principles"><img src="https://img.shields.io/badge/cgo-disabled-success.svg" alt="Zero CGO"></a>
  <a href="#embedded-dashboard"><img src="https://img.shields.io/badge/node.js-none-success.svg" alt="Zero Node"></a>
</p>

**Tarsier** is an ultra-lightweight, self-hosted, single-server observability engine and dashboard. Packaged as a single static Go binary (`CGO_ENABLED=0`) and container image targeting **`< 256 MB` RAM** (`GOMEMLIMIT=200MiB`), it combines host telemetry, Docker container monitoring, compressed log chunk storage with fast search, in-memory alerting, automated database compaction, and pre-cleanup digest reports into one cohesive service with zero external dependencies.

---

<a name="beta-notice"></a>
> [!WARNING]
> ### BETA SOFTWARE — NOT READY FOR PRODUCTION USE
> **Tarsier is currently in active Beta status.** Core architectural systems and functional modules are implemented and tested, but breaking changes to configuration schemas, REST API contracts, and on-disk storage layouts may occur before a stable `v1.0.0` release.
> 
> **Do not deploy Tarsier as your sole or critical production monitoring system.** Experimental deployments and feedback on VPS/homelab single-node environments are welcome.

---

## Architectural Principles & Hard Budgets

Tarsier is engineered from the ground up for strict resource efficiency on single servers and budget VPS instances:

* **Strict Memory Envelope:** Container cgroup ceiling of `< 256 MB` RAM (`GOMEMLIMIT=200MiB`), maintaining **60–120 MB RSS** during normal continuous operation.
* **Low CPU Envelope:** `< 5%` of one CPU core idle/normal envelope (`0.1–0.5 CPU`).
* **Zero CGO & Zero External Runtimes:** Pure Go standard library + `modernc.org/sqlite`. Compiles with `CGO_ENABLED=0`. Zero C libraries, zero Node.js, zero npm, and zero frontend bundlers.
* **Two Databases Only:** No `logs.db`. Storage is strictly separated into:
  * `config.db`: Alert rules, live states, offsets, admin sessions, and container lifecycle events.
  * `metrics.db`: Metrics series, raw samples (24h), and multi-tier rollups (`sample_1m`, `sample_5m`, `sample_1d` using `WITHOUT ROWID`).
* **Dedicated Single-Writer Concurrency:** Each SQLite database is managed by a single dedicated writer goroutine fed by a bounded channel (`SetMaxOpenConns(1)`), paired with an isolated read-only pool (`SetMaxOpenConns(2)`). Pragmas enforced: `WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `temp_store=MEMORY`, `auto_vacuum=INCREMENTAL`, and negative KiB `cache_size`.
* **Append-Only Compressed Log Chunks:** Logs are never stored in SQLite or FTS5 tables. Logs are written to immutable **64 KB Zstandard-compressed blocks** (`.chunk` files) paired with binary-searched sparse index files (`.idx`), enabling instant $O(1)$ unlinks upon retention expiry.
* **Strict XSS Protection:** Zero `v-html` directives across the entire web application. All log payloads render as plain escaped text; rendered HTML pre-cleanup digest reports are displayed strictly inside sandboxed `<iframe>` tags (`sandbox="" :srcdoc="..."`).

---

## Feature Overview

### 1. Host Telemetry Collection
* Direct `/host/proc`, `/host/sys`, and `statfs` parsing without shell subcommands or external exporters.
* Real-time metrics:
  * **CPU:** Overall utilization %, per-core breakdown, and system load averages (`1m`, `5m`, `15m`).
  * **Memory:** RAM total, used, available, cached, free, and swap space.
  * **Storage:** Physical filesystem auto-discovery, used/free space, and disk I/O read/write throughput.
  * **Network:** Interface throughput (bytes in/out, packets, drops, errors).
* Raw `10s` metric ingestion into fixed-capacity ring buffers for instant sub-millisecond API queries.

### 2. Multi-Tier Metric Rollups
* Automated background downsampler computing:
  * `sample_1m`: 1-minute aggregates (average, min, max).
  * `sample_5m`: 5-minute aggregates downsampled from 1m.
  * `sample_1d`: Daily rollups calculated at midnight UTC preserving 14-day host Average vs. Peak (Max) metrics for long-term capacity planning.

### 3. Docker Container Discovery & Lifecycle Monitoring
* Hand-rolled HTTP client communicating directly over `/var/run/docker.sock` (no heavy official Docker SDK dependencies).
* Container resource stats streaming: CPU %, memory usage, and network I/O.
* Container lifecycle event tracking: `start`, `stop`, `die`, `oom`, `restart`, `pause`, `unpause` recorded directly into `config.db`.
* Flexible container filtering using glob patterns (`include` and `exclude`).

### 4. High-Performance Log Ingestion & Chunk Search
* Ingests Docker container logs (`stdout`/`stderr`) and host log files (with glob support, inode rotation tracking, and file truncation recovery).
* Sub-10 µs in-place zero-allocation timestamp and severity level detection (`DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL`) across JSON, logfmt, syslog, CLF, and unstructured text.
* Transparent confidence indicators: inferred timestamps (`~`) and inferred levels (dashed badge).
* Single-source log query engine: searches both hot in-memory tail ring buffers and compressed disk chunks.
* In-memory zero-allocation scanning with Boyer-Moore byte matching, regex filtering, and surrounding `±20` line context modal inspection.

### 5. In-Memory Alert Engine & Notification Dispatcher
* In-memory state machine with hysteresis, sustained duration (`for`), and cooldown timers.
* Three rule kinds:
  * **Metric Alerts:** Numerical comparison operators (`>`, `>=`, `<`, `<=`, `==`) over host and container metrics.
  * **Log Pattern Alerts:** Substring or regular expression matching with occurrence thresholds over sliding time windows.
  * **Container Lifecycle Alerts:** Automated alerts triggered on container events (`oom`, `die`, `restart`, `stop`, `down`).
* Multi-channel delivery:
  * **Discord Webhooks:** Rich JSON embeds with severity color-coding and target metadata.
  * **SMTP Email:** Clean HTML and plaintext emails over TLS or STARTTLS.
* Built-in template library, live preview against live telemetry, rule enable/disable toggling, and JSON import/export.
* Outgoing HTTP heartbeat worker (e.g. for Healthchecks.io or Uptime Kuma) with version tagging.

### 6. Retention, Pre-Cleanup Digest Reports & Compaction
* **Message Normalizer:** Replaces dynamic values (`{uuid}`, `{hex}`, `{ip}`, `{ts}`, `{str}`, `{path}`, `{num}`) and aggregates recurring error patterns into a memory-bounded counter.
* **Pre-Cleanup Reports:** Generates plain-text or HTML summaries before data deletion (purged volumes, top error patterns, container events, and metric digests).
* **Delivery Gating:** Sends reports via Discord/SMTP with retry backoff and configurable `on_failure` policies (`proceed` vs. `block`).
* **Hybrid Retention Executor:**
  * Instant $O(1)$ filesystem unlinks of expired `.chunk` and `.idx` log files.
  * Emergency disk-cap enforcement against `storage.logs_max_size` with 90% hysteresis.
  * Chunked batch deletions for `metrics.db` (5,000 rows per batch with 50ms pauses) preventing write lock contention.
* **Automated SQLite Maintenance:** Bounded `PRAGMA incremental_vacuum`, query planner optimization via `PRAGMA optimize`, and WAL truncation via `PRAGMA wal_checkpoint(TRUNCATE)`.
* **Dry-Run Mode:** Non-destructive execution simulating data pruning and report generation without deleting any data.

### 7. Embedded Web UI & Settings Dashboard
* Single-binary delivery via Go 1.27 `embed.FS`.
* Built with `petite-vue` (IIFE, ~6 KB), `Pico.css` (v2), and `uPlot`.
* Total embedded gzipped UI assets strictly under **`< 100 KB`** (`~66 KB` typical).
* Auto light/dark theme synchronization with OS preferences.
* Interactive views:
  * **Overview:** High-level system vitals, active firing alerts, real-time CPU & RAM charts, active container grid with instant health indicators, and cleanup failure warnings.
  * **Host:** Compact 2-row metric card layout showing detected CPU core count and physical filesystem capacity, paired with synchronized uPlot panels (CPU, Load, RAM/Swap, Network, Disk I/O) and a 14-day history rollup table.
  * **Containers:** Streamlined 4-column layout (`Container Name` with green/red/grey hollow health indicator circle and image subtitle, `Stats` with CPU % / 100%, RAM used / limit, and yellow `Restarts: N` badge, `NET I/O` throughput rates, and `Actions`), running/exited state tabs, container name filter, and a dedicated `#container-detail?container=[name]` page with live telemetry, synchronized uPlot charts, 14-day history, and lifecycle event audit log.
  * **Logs:** Live tail streaming, regex & substring search, level filter pills (`DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL`), stream selection (`stdout` / `stderr`), and `±20` line surrounding context viewer.
  * **Alerts:** Active firing alerts, rule manager, rule builder dialog with live telemetry preview against real targets, and notification history.
  * **Settings:** Physical storage breakdown progress bars, retention policies, cleanup history, manual dry-run trigger, custom template live preview, and hot config reload without restart.

---

## Quick Start (Docker Compose)

### Local Testing & Development

The repository includes a ready-to-run `docker-compose.yml` and `.env.example` pre-configured for instant local testing against containers already running on your machine:

1. **Set up local environment file:**
   ```bash
   cp .env.example .env
   ```
   *(Pre-configured with local dev credentials: username `admin`, password `admin`, and port `8080`)*

2. **(Optional) Customize exposed port or settings:**
   Edit `.env` to change the host port (e.g. `TARSIER_PORT=2090` or leave as `8080`).

3. **Build and start Tarsier:**
   ```bash
   docker compose up --build -d
   ```
   *Note: A dedicated [`.dockerignore`](.dockerignore) is included to keep build contexts minimal (< 1 MB) and ensure local secrets (`.env*`), persistent data (`data/`), and build binaries are never transferred to the Docker daemon or baked into the image.*

4. **Access Dashboard:**
   Open [http://localhost:8080](http://localhost:8080) (or your configured `TARSIER_PORT`) and log in with:
   - **Username:** `admin`
   - **Password:** `admin`

   Tarsier will automatically discover and monitor containers running on your local Docker daemon.

> [!NOTE]
> **macOS & Windows (Docker Desktop / OrbStack / Colima):** Container metrics and logs work out of the box via `/var/run/docker.sock`. Bare-metal Linux host telemetry (`/proc` and `/sys`) is commented out by default in `docker-compose.yml` so non-Linux machines can run seamlessly without mounting missing host paths. For native Linux VPS / bare-metal deployments, simply uncomment the host mounts in `docker-compose.yml` to monitor physical CPU, memory, and disk health.

---

### Production Deployment

For production single-server deployments, pre-built multi-architecture container images are published to GitHub Container Registry (`ghcr.io/dale-tomson/tarsier:latest`).

#### Option A: Quick One-Line Install (Recommended)
Download and initialize the standalone deployment bundle automatically:
```bash
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash
```
*(To pin a specific version, pass `-s -- -v v0.1.0`. See [deploy/README.md](deploy/README.md) for full installer options).*

#### Option B: Manual Compose Setup
1. **Generate Admin Password Hash:**
   Generate a strong bcrypt hash (cost factor 12) for the `admin` user:
   ```bash
   htpasswd -nbBC 12 "" "your-strong-password" | tr -d ':\n'
   ```
   *(Note: Remember to escape `$` as `$$` when placing in `docker-compose.yml` or `.env` for Compose interpolation).*

2. **Configure `.env` and `tarsier.yaml`:**
   Copy the example templates from [`deploy/`](deploy/) or the repository root:
   ```bash
   cp deploy/.env.example .env
   cp deploy/tarsier.example.yml tarsier.yaml
   ```
   Or use the standalone Compose definition from [`deploy/docker-compose.yml`](deploy/docker-compose.yml):
```yaml
services:
  tarsier:
    image: ghcr.io/dale-tomson/tarsier:latest
    container_name: tarsier
    restart: unless-stopped
    pid: host
    user: "0:0"
    read_only: true
    mem_limit: 256m
    cpus: 1.0
    security_opt:
      - no-new-privileges:true
    env_file:
      - path: .env
        required: false
    environment:
      TARSIER_CONFIG: /config/tarsier.yaml
      TARSIER_ADMIN_USER: admin
      TARSIER_ADMIN_PASSWORD_HASH: "$$2a$$12$$replace_with_your_bcrypt_hash"
      TARSIER_SESSION_SECRET: "change-this-to-a-random-32-byte-secret"
      HOST_PROC: /host/proc
      HOST_SYS: /host/sys
      GOMEMLIMIT: 200MiB
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./tarsier.yaml:/config/tarsier.yaml:ro
      - ./data:/data
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /:/host/root:ro,rslave
      - /var/log:/host/var/log:ro
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=16m
```

3. **Start Tarsier:**
   ```bash
   docker compose up -d
   ```
   Access the dashboard at `http://127.0.0.1:8080`.

---

## Configuration (`tarsier.yaml`)

Infrastructure, telemetry targets, log files, retention periods, and notifiers are defined in `tarsier.yaml`. Secrets can be injected using `${VAR}` or `${VAR:-default}` syntax:

```yaml
server:
  listen: ":8080"
  base_url: "http://127.0.0.1:8080"
  timezone: "UTC"

auth:
  session_ttl: "24h"
  max_login_attempts: 5
  lockout: "15m"

storage:
  path: "/data"
  logs_max_size: "3GiB"
  metrics_max_size: "256MiB"

retention:
  logs: "7d"
  metrics_raw: "24h"
  metrics_1m: "7d"
  metrics_5m: "30d"
  metrics_1d: "90d"

cache:
  max_sources: 128
  log_tail_lines: 500

host:
  enabled: true
  interval: "10s"
  disks: [] # Empty = auto-discover physical mounts

docker:
  enabled: true
  socket: "/var/run/docker.sock"
  stats_interval: "10s"
  collect_metrics: true
  include: [] # If set, ONLY matching container name globs are monitored
  exclude: ["tarsier"]

logs:
  files:
    - name: "host-syslog"
      path: "/host/var/log/syslog"
    - name: "nginx-access"
      path: "/host/var/log/nginx/access.log"

notifiers:
  email:
    enabled: true
    smtp_host: "smtp.example.com"
    smtp_port: 587
    security: "starttls"
    username: "alerts@example.com"
    password: "${SMTP_PASSWORD}"
    from: "Tarsier Alerts <alerts@example.com>"
    to: ["admin@example.com"]
  discord:
    enabled: true
    webhook_url: "${DISCORD_WEBHOOK_URL}"

heartbeat:
  enabled: false
  url: "https://hc-ping.com/your-uuid-here"
  interval: "1m"
  timeout: "5s"

cleanup:
  schedule: "0 3 * * *"
  batch_size: 5000
  batch_pause: "50ms"
  report:
    enabled: true
    channels: ["discord", "email"]
    min_attempts: 2
    retry_delay: "5s"
    on_failure: "proceed" # "proceed" | "block"
    top_patterns: 10
    sample_lines_per_pattern: 3
    format: "text"         # "text" | "html"
    max_attachment: "2MB"
```

*Note: Alert rules are never stored in `tarsier.yaml`. Alert rules are created, edited, and toggled live in the web dashboard and stored inside `config.db`.*

---

## REST API Overview

All API endpoints except `/healthz` and `/api/v1/auth/login` require an authenticated session cookie:

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/healthz` | Public liveness probe returning `{ status: "ok", version: "..." }` |
| `POST` | `/api/v1/auth/login` | Session login with IP rate limiting and lockout protection |
| `POST` | `/api/v1/auth/logout` | Terminates active session and invalidates cookie |
| `GET` | `/api/v1/auth/me` | Current authenticated user metadata |
| `GET` | `/api/v1/system/config` | Sanitized server configuration (secrets redacted) |
| `GET` | `/api/v1/system/health` | Go runtime memory, SQLite DB status, storage breakdown, and warnings |
| `POST` | `/api/v1/system/reload` | Hot reload of `tarsier.yaml` without process restart |
| `GET` | `/api/v1/host/metrics` | Time-series query across host CPU, load, memory, disk, network |
| `GET` | `/api/v1/metrics/daily/summary` | Today's running average and peak comparison for host and containers |
| `GET` | `/api/v1/metrics/daily` | Historical 14-day daily rollups (`sample_1d`) |
| `GET` | `/api/v1/containers` | Tracked container list with health states and resource snapshots |
| `GET` | `/api/v1/containers/{name}` | Container detail with historical charts and lifecycle events |
| `GET` | `/api/v1/sources` | Discovered log sources (Docker containers & host files) |
| `GET` | `/api/v1/logs` | Log query with level, stream, regex/substring search, and pagination |
| `GET` | `/api/v1/logs/context` | Surrounding `±20` line log context around a specific line |
| `GET` | `/api/v1/alerts/active` | Current firing alert states across all targets |
| `GET` | `/api/v1/alerts/rules` | Configured alert rules with live evaluation states |
| `POST` | `/api/v1/alerts/rules` | Creates a new alert rule |
| `PUT` | `/api/v1/alerts/rules/{id}` | Updates an existing alert rule |
| `DELETE` | `/api/v1/alerts/rules/{id}` | Deletes an alert rule |
| `POST` | `/api/v1/alerts/rules/{id}/enable` | Enables a rule |
| `POST` | `/api/v1/alerts/rules/{id}/disable` | Disables a rule |
| `POST` | `/api/v1/alerts/rules/preview` | Live evaluation of candidate rule condition against real telemetry |
| `GET` | `/api/v1/alerts/rules/export` | Exports all configured rules as a JSON file |
| `POST` | `/api/v1/alerts/rules/import` | Imports rules from a JSON payload |
| `GET` | `/api/v1/alerts/history` | Historical audit log of alert firing and resolution events |
| `POST` | `/api/v1/alerts/test` | Dispatches test notification to email or Discord |
| `GET` | `/api/v1/system/cleanup/history` | Historical audit log of past cleanup runs and dry runs |
| `GET` | `/api/v1/system/cleanup/{id}/report` | Stored pre-cleanup report body for a specific run |
| `POST` | `/api/v1/system/cleanup/run` | Triggers a cleanup run (`?dry_run=true` for non-destructive preview) |
| `GET` | `/api/v1/system/cleanup/template` | Downloads the default plain-text or HTML report template |
| `POST` | `/api/v1/system/cleanup/preview` | Dry-run template preview evaluating custom Go template syntax |

---

## Building from Source

### Prerequisites
* **Go 1.27+**
* Pure Go build: no C compiler (gcc/clang), no Node.js, and no external build tools required.

```bash
# Clone the repository
git clone https://github.com/dale-tomson/Tarsier.git
cd Tarsier

# Run test suite with race detector
make test
# Or directly inside the tarsier module:
cd tarsier && CGO_ENABLED=0 go test -race ./... && cd ..

# Build single static binary
make build
# Output binary is generated at bin/tarsier

# Validate configuration file
./bin/tarsier -test-config -config ./tarsier.yaml

# Run binary directly
./bin/tarsier -config ./tarsier.yaml
```

### Building the Docker Container Image

You can build the minimal distroless image locally using Docker or Docker Buildx:

```bash
# Build local multi-arch capable distroless image
docker build -t tarsier:local .

# Or build with Buildx for a specific target architecture
docker buildx build --platform linux/amd64,linux/arm64 -t tarsier:local .
```

The build uses a two-stage `Dockerfile`:
1. **Builder Stage:** Compiles the static binary (`CGO_ENABLED=0`, `-tags netgo`) inside `golang:alpine`.
2. **Runtime Stage:** Produces a minimal image based on `gcr.io/distroless/static-debian12:nonroot` with only the static binary, SSL root certificates, and timezone data. Memory limit is pre-set via `GOMEMLIMIT=200MiB`.
3. **Build Context Filtering:** The [`.dockerignore`](.dockerignore) file excludes version control history (`.git`), local databases (`data/`), `.env` secrets, and temp files, keeping the build context under 1 MB.

---

## API & OpenAPI Documentation

Tarsier exposes an authenticated REST API under `/api/v1` along with public `/healthz` endpoints.
A complete, self-contained OpenAPI 3.1.0 specification is available at [`docs/openapi.yaml`](docs/openapi.yaml).

* **View in Browser / Editor:** Open `docs/openapi.yaml` in Swagger Editor, VS Code OpenAPI Viewer, or ReDoc.
* **API Testing Tools:** Import `docs/openapi.yaml` directly into Postman, Bruno, or Insomnia to execute tests against your Tarsier instance.
* For more information and usage instructions, see [`docs/README.md`](docs/README.md).

---

## Security Recommendations

1. **Reverse Proxy & HTTPS:** Tarsier does not terminate TLS natively; always place it behind Caddy, Nginx, or Traefik with automatic HTTPS certificates enabled.
2. **Docker Socket Protection:** Access to `/var/run/docker.sock` allows inspecting container telemetry. Mount the socket read-only (`:ro`), run Tarsier inside an isolated management network, and enforce strong admin credentials.
3. **Restricted Host Mounts:** Always mount `/host/proc`, `/host/sys`, and host log paths with the read-only (`:ro`) flag.
4. **Environment Variables:** Store notifier passwords and webhook URLs in environment variables rather than plaintext configuration files.

---

## License

This project is licensed under the [GNU Affero General Public License v3](LICENSE).
