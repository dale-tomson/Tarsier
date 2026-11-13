# Tarsier

**Tarsier** is a lightweight, self-hosted observability tool designed for a **single server or VPS**. Packaged as a single static Go binary and distroless container image (`< 256 MB` RAM), it combines host telemetry, Docker container monitoring, searchable logs, alerting, and pre-cleanup digest reports with a built-in zero-build dashboard.

---

## Key Features

* **Ultra-Low Footprint:** Runs comfortably in **60–120 MB RAM** (`256 MB` hard container cap) and `< 5%` idle CPU with zero external dependencies (no Prometheus, Loki, Elasticsearch, or Node.js).
* **Host & Container Monitoring:**
  * Direct `/proc` & `/sys` telemetry: CPU (total/per-core, load), RAM/swap, auto-discovered filesystem usage, disk I/O, and network I/O.
  * Docker daemon integration via read-only Unix socket: container lifecycle events (`start`, `stop`, `die`, `oom`, `restart`), resource stats, and glob-based `include`/`exclude` filtering.
  * Automatic `10s` raw sampling with `1m`, `5m`, and daily (`1d` Average vs. Peak) rollups stored in pure-Go SQLite.
* **Fast Compressed Log Storage & Search:**
  * Collects Docker container logs (`stdout`/`stderr`) and host log files (with glob support, inode rotation, and truncation handling).
  * Stores logs in immutable, **Zstandard-compressed `64 KB` chunks** with binary-searched sparse indexes (`.idx`)—eliminating database bloat and enabling instant $O(1)$ file deletion on retention expiry.
  * Automatic, zero-allocation timestamp and log-level detection (JSON, logfmt, syslog, CLF, and free-form text) with transparent confidence indicators.
* **UI-Driven Alerting & Heartbeats:**
  * Build, preview, enable/disable, and export/import alert rules directly in the web UI (Metric thresholds, Log patterns, Container events) with hot-reload—no restarts required.
  * Supports **Email (SMTP)** and **Discord** webhooks with cooldowns, recovery notifications, and outbound HTTP heartbeats (e.g., Healthchecks.io).
* **Pre-Cleanup Digest Reports:**
  * Before purging expired data, Tarsier generates a summary report (log volume, top normalized error patterns, container restarts, metric min/avg/p95/max) and delivers it via Discord or Email using customizable Go templates.
* **Zero-Build Embedded Dashboard:**
  * Clean, responsive UI built with `petite-vue`, `Pico.css`, and `uPlot` (with automatic light/dark themes), embedded directly inside the Go binary.

---

## Quick Start (Docker Compose)

1. Generate a bcrypt password hash for your admin user:
```bash
   htpasswd -nbBC 12 "" "your-strong-password" | tr -d ':\n'

```

*(Remember to escape `$` as `$$` inside `docker-compose.yml`.)*

2. Create a `docker-compose.yml`:
```yaml
services:
  tarsier:
    image: ghcr.io/tarsier-obs/tarsier:latest
    container_name: tarsier
    restart: unless-stopped
    pid: host
    user: "0:0" # Or a user in the docker group for socket/log access
    read_only: true
    mem_limit: 256m
    environment:
      TARSIER_CONFIG: /config/tarsier.yaml
      TARSIER_ADMIN_USER: admin
      TARSIER_ADMIN_PASSWORD_HASH: "\$2a$$12$$replace_with_your_bcrypt_hash"
      TARSIER_SESSION_SECRET: "change-me-to-a-random-32-char-string"
      HOST_PROC: /host/proc
      HOST_SYS: /host/sys
      GOMEMLIMIT: 200MiB
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./config:/config:ro
      - tarsier-data:/data
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /:/host/root:ro,rslave
      - /var/log:/host/var/log:ro

volumes:
  tarsier-data:

```


3. Place your configuration at `./config/tarsier.yaml` (see Configuration below) and start the service:
```bash
docker compose up -d

```



---

## Configuration (`tarsier.yaml`)

Infrastructure settings, log paths, retention horizons, and notifier credentials are defined in `tarsier.yaml` with `${ENV_VAR}` interpolation. Alert rules are configured in the Web UI.

```yaml
server:
  listen: ":8080"
  base_url: "[https://tarsier.example.com](https://tarsier.example.com)"
  timezone: "UTC"

storage:
  path: /data
  logs_max_size: 3GiB
  metrics_max_size: 256MiB

retention:
  logs: 7d
  metrics_raw: 24h
  metrics_1m: 7d
  metrics_5m: 30d

host:
  enabled: true
  interval: 10s
  disks: [] # Empty = auto-discover physical mounts

docker:
  enabled: true
  socket: /var/run/docker.sock
  stats_interval: 10s
  include: [] # If set, ONLY matching container globs are monitored
  exclude: ["tarsier"]

logs:
  files:
    - name: nginx-access
      path: /host/var/log/nginx/access.log
    - name: app-logs
      path: /host/var/log/myapp/*.log

notifiers:
  discord:
    enabled: false
    webhook_url: \${DISCORD_WEBHOOK_URL}
  email:
    enabled: false
    smtp_host: smtp.example.com
    smtp_port: 587
    security: starttls
    username: alerts@example.com
    password: \${SMTP_PASSWORD}
    from: "Tarsier <alerts@example.com>"
    to: ["admin@example.com"]

cleanup:
  schedule: "0 3 * * *"
  report:
    enabled: true
    channels: [discord, email]
    format: markdown # markdown | html

```

Reload configuration without restarting via `SIGHUP` or the **Reload** button in the Settings UI.

---

## Building from Source

Requires **Go 1.27+**. No Node.js or CGO toolchain is needed.

```bash
# Build static binary for current platform
CGO_ENABLED=0 go build -ldflags="-s -w" -o tarsier ./cmd/tarsier

# Validate a configuration file
./tarsier -test-config -config ./tarsier.yaml

# Run tests
go test -race ./...

```

---

## Security Recommendations

* **Reverse Proxy:** Tarsier does not terminate TLS in v1; run it behind Caddy, Nginx, or Traefik with HTTPS enabled.
* **Read-Only Mounts:** Always mount `/var/run/docker.sock`, `/proc`, `/sys`, and host log directories as `:ro`. Note that read access to the Docker socket allows inspecting container metadata, so keep Tarsier behind strong authentication and firewall rules.

---

## License

MIT