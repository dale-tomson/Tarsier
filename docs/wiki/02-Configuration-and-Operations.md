# 2. Configuration & Operations

Tarsier reads its static infrastructure configuration from `tarsier.yaml`, while dynamic alert rules, live alert states, and file tailing offsets live in `config.db`.

---

## 1. Configuration Resolution Precedence

Tarsier locates its configuration file (`config.ResolveConfigPath`) in the following order:

1. Explicit CLI flag: `-config /path/to/tarsier.yaml`
2. Environment variable: `TARSIER_CONFIG`
3. Container default: `/config/tarsier.yaml` (if present and not a directory)
4. Local fallback: `./tarsier.yaml`

Validate any configuration file before deploying or reloading:

```bash
tarsier -test-config -config /path/to/tarsier.yaml
```

---

## 2. Environment Variable Expansion & Strict Schema Validation

### `${VAR}` and `${VAR:-default}` Substitution

`config.ExpandEnv` runs over the raw YAML bytes before parsing:

* `${VAR}` expands to the value of `VAR`, or empty string if unset.
* `${VAR:-default}` expands to `VAR` if set and non-empty, otherwise `default`.
* Raw `$` characters not followed by `{` (for example, inside bcrypt hashes) are left untouched.
* Unclosed `${` expressions or invalid variable names (`^[A-Za-z_][A-Za-z0-9_]*$`) fail fast at load time.

### Strict Schema Checking (`KnownFields(true)`)

Tarsier decodes YAML using `gopkg.in/yaml.v3` with `dec.KnownFields(true)`:

* Any misspelled or unrecognized YAML key immediately rejects startup or reload with a descriptive error.
* This prevents silent misconfigurations (for example, `retension:` instead of `retention:`).
* **Alert rules are never stored in `tarsier.yaml`** — declaring an `alerts:` block in YAML will be rejected by `KnownFields(true)`. Alert rules are managed via the Web UI or `/api/v1/alerts/rules` and stored in `config.db`.

---

## 3. Complete `tarsier.yaml` Schema Reference

### `server`

| Key | Type | Default | Description |
|---|---|---|---|
| `server.listen` | `string` | `":8080"` | Bind address (`":8080"`, `"127.0.0.1:8080"`). |
| `server.base_url` | `string` | `""` | External canonical URL used for notification links and `https://` cookie `Secure` flag detection. |
| `server.timezone` | `string` | `"UTC"` | Valid IANA timezone (e.g. `"UTC"`, `"America/New_York"`) or `"Local"`. |

### `auth`

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.session_ttl` | `duration` | `"24h"` | Cookie session validity (`s`, `m`, `h`, `d`). |
| `auth.max_login_attempts` | `int` | `5` | Consecutive failed logins per IP before lockout (`1..100`). |
| `auth.lockout` | `duration` | `"15m"` | Lockout duration returning HTTP `429` (`s`, `m`, `h`). |

### `storage`

| Key | Type | Default | Description |
|---|---|---|---|
| `storage.path` | `string` | `"/data"` | Root directory holding `config.db`, `metrics.db`, and `logs/`. |
| `storage.logs_max_size` | `bytes` | `"3GiB"` | Physical cap on `/data/logs/`. Exceeding triggers emergency eviction down to `90%` of this cap. Supports SI (`MB`, `GB`) and IEC (`MiB`, `GiB`) units. |
| `storage.metrics_max_size` | `bytes` | `"256MiB"` | Physical size budget for `metrics.db`. |

### `retention`

| Key | Type | Default | Description |
|---|---|---|---|
| `retention.logs` | `duration` | `"7d"` | Retention horizon for compressed log chunks (`h`, `d`). |
| `retention.metrics_raw` | `duration` | `"24h"` | Retention for raw 10-second metric samples (`sample_raw`). |
| `retention.metrics_1m` | `duration` | `"7d"` | Retention for 1-minute rollups (`sample_1m`). |
| `retention.metrics_5m` | `duration` | `"30d"` | Retention for 5-minute rollups (`sample_5m`). |
| `retention.metrics_1d` | `duration` | `"90d"` | Retention for daily rollups (`sample_1d`). |
| `retention.containers_exited` | `duration` | `"24h"` | Retention horizon for exited, dead, or destroyed container metadata and lifecycle events in `config.db` when not restarted. |

### `host`

| Key | Type | Default | Description |
|---|---|---|---|
| `host.enabled` | `bool` | `true` | Enable Linux `/proc`, `/sys`, and `statfs` host collection. |
| `host.interval` | `duration` | `"10s"` | Host telemetry polling interval. |
| `host.disks` | `[]string` | `[]` | Explicit mount points to monitor. Empty `[]` auto-discovers physical mounts via `/proc/mounts` or `/proc/self/mountinfo`. |
| `host.disk_exclude_fs` | `[]string` | `["tmpfs", "overlay", "squashfs", "devtmpfs"]` | Filesystem types excluded during mount auto-discovery. |

### `docker`

| Key | Type | Default | Description |
|---|---|---|---|
| `docker.enabled` | `bool` | `true` | Enable Docker Engine Unix socket monitoring. |
| `docker.socket` | `string` | `"/var/run/docker.sock"` | Path to Docker Engine Unix domain socket. |
| `docker.stats_interval` | `duration` | `"10s"` | Polling frequency for container CPU, memory, and network stats. |
| `docker.collect_logs` | `bool` | `true` | Stream and index container `stdout`/`stderr` logs. |
| `docker.collect_metrics` | `bool` | `true` | Collect container resource metrics. |
| `docker.include` | `[]string` | `[]` | Glob allowlist of container names (`[]` = include all). |
| `docker.exclude` | `[]string` | `["tarsier"]` | Glob blocklist of container names (applied after `include`). |

### `logs`

| Key | Type | Default | Description |
|---|---|---|---|
| `logs.default_timezone` | `string` | `"UTC"` | Fallback timezone for unstructured log timestamps lacking an explicit offset. |
| `logs.detect.max_scan_bytes` | `int` | `4096` | Max initial bytes scanned per line for structure/timestamp detection (`64..8192`). |
| `logs.detect.keyword_levels` | `bool` | `true` | Infer log severity (`DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL`) via zero-alloc keyword matching. |
| `logs.detect.hints` | `bool` | `true` | Emit confidence metadata (`~` for inferred timestamps, dashed badge for inferred levels). |
| `logs.files` | `[]object` | See example | List of `{ name, path }` entries for host log files to tail (supports shell globs, tracks inode rotation and truncation). |

### `notifiers`

| Key | Type | Default | Description |
|---|---|---|---|
| `notifiers.email.enabled` | `bool` | `false` | Enable SMTP email delivery. |
| `notifiers.email.smtp_host` | `string` | `""` | SMTP server hostname. |
| `notifiers.email.smtp_port` | `int` | `587` | SMTP port (`587` STARTTLS, `465` TLS, `25` plaintext). |
| `notifiers.email.security` | `string` | `"starttls"` | `"none"`, `"starttls"`, or `"tls"`. |
| `notifiers.email.username` | `string` | `""` | SMTP authentication username. |
| `notifiers.email.password` | `string` | `""` | SMTP password (supply via `${SMTP_PASSWORD}`). |
| `notifiers.email.from` | `string` | `""` | RFC 5322 sender address (e.g. `"Tarsier <alerts@example.com>"`). |
| `notifiers.email.to` | `[]string` | `[]` | Recipient email addresses. |
| `notifiers.discord.enabled` | `bool` | `false` | Enable Discord webhook notifications. |
| `notifiers.discord.webhook_url` | `string` | `""` | Discord webhook URL (supply via `${DISCORD_WEBHOOK_URL}`). |
| `notifiers.discord.mention` | `string` | `"@here"` | Mention prefix (`"@here"`, `"@everyone"`, `"<@&role_id>"`, or `""`). |

### `cleanup`

| Key | Type | Default | Description |
|---|---|---|---|
| `cleanup.schedule` | `string` | `"0 3 * * *"` | Standard 5-field cron expression (`Minute Hour DayOfMonth Month DayOfWeek`). |
| `cleanup.batch_size` | `int` | `5000` | Rows deleted per batch in `metrics.db` (`100..50000`). |
| `cleanup.batch_pause` | `duration` | `"50ms"` | Pause between `metrics.db` deletion batches to avoid write lock contention. |
| `cleanup.report.enabled` | `bool` | `true` | Generate and send pre-cleanup digest reports. |
| `cleanup.report.channels` | `[]string` | `["discord", "email"]` | Delivery channels for digest reports. |
| `cleanup.report.min_attempts` | `int` | `2` | Delivery attempts before applying `on_failure` policy. |
| `cleanup.report.retry_delay` | `duration` | `"10m"` | Delay between retry attempts. |
| `cleanup.report.on_failure` | `string` | `"proceed"` | `"proceed"` (delete expired data anyway) or `"block"` (skip deletion to preserve audit trail). |
| `cleanup.report.top_patterns` | `int` | `20` | Max normalized error patterns included in the digest. |
| `cleanup.report.sample_lines_per_pattern` | `int` | `2` | Raw exemplar log lines attached per error pattern. |
| `cleanup.report.format` | `string` | `"text"` | `"text"` or `"html"`. |
| `cleanup.report.template` | `string` | `""` | Path to a custom Go `text/template` file (`""` uses built-in template). |
| `cleanup.report.max_attachment` | `bytes` | `"8MiB"` | Maximum email report attachment size. |

### `cache` & `heartbeat`

| Key | Type | Default | Description |
|---|---|---|---|
| `cache.max_sources` | `int` | `50` | Upper bound on distinct log/metric sources in memory (recommended `10..2000`; validator bounds `1..10000`). |
| `cache.log_tail_lines` | `int` | `150` | Hot ring buffer lines per source (`50..2000`). |
| `cache.metric_max_series` | `int` | `2000` | Maximum distinct metric series retained in the in-memory `HotCache` (`100..20000`). |
| `cache.metric_ring_points` | `int` | `60` | Fixed ring buffer points per metric series (`30..360`; `60` points = 10 minutes at `10s` polling). |
| `cache.overview_ttl` | `duration` | `"3s"` | Overview endpoint cache TTL. |
| `cache.query_ttl` | `duration` | `"3s"` | Range query cache TTL (`QueryCache`, capacity `256`). |
| `heartbeat.enabled` | `bool` | `false` | Enable outbound HTTP GET uptime pings. |
| `heartbeat.url` | `string` | `""` | Target ping URL (e.g. Healthchecks.io / Uptime Kuma). |
| `heartbeat.interval` | `duration` | `"5m"` | Interval between heartbeat pings. |
| `heartbeat.timeout` | `duration` | `"10s"` | HTTP request timeout. |

---

## 4. `tarsier.yaml` Configuration Architecture & Native Fallbacks

Tarsier's configuration loader (`internal/config/loader.go` and `internal/config/config.go`) separates **file loading** from **schema defaulting** so that partial, minimal, or even empty YAML files produce a complete, memory-bounded runtime configuration.

### Loading & Defaulting Pipeline (`LoadBytes` + `ApplyDefaults`)

1. **Environment Expansion (`config.ExpandEnv`):** Raw YAML bytes are scanned for `${VAR}` and `${VAR:-default}` expressions before YAML parsing.
2. **Pre-Seeded Struct (`NewDefaultConfig`):** Before decoding YAML, `LoadBytes` allocates a `Config` pre-populated with safe production defaults across all 11 subsystems.
3. **Strict Decoding (`KnownFields(true)`):** `yaml.NewDecoder` decodes onto that pre-seeded struct. Unrecognized keys fail immediately. If the YAML document is completely empty (`io.EOF`), decoding succeeds using the `NewDefaultConfig()` baseline.
4. **Zero-Value Backfill (`ApplyDefaults`):** Even if an unmarshaled field is left empty (`""`), zero (`0`), or `nil` (for slices and pointer booleans), `ApplyDefaults()` restores the default value from `NewDefaultConfig()`.
5. **Semantic Validation (`config.Validate`):** Every duration, byte size, cron expression, URL, timezone, and bounded cache limit is validated before startup or hot reload completes.

> [!NOTE]
> `config.Load(path)` requires a readable file at the resolved configuration path (via `-config`, `TARSIER_CONFIG`, `/config/tarsier.yaml`, or `./tarsier.yaml`). However, the file itself can be minimal—or even empty (0 bytes)—because `NewDefaultConfig()` and `ApplyDefaults()` supply safe defaults for every omitted subsystem.

> [!IMPORTANT]
> Boolean flags that default to `true` (`host.enabled`, `docker.enabled`, `docker.collect_logs`, `docker.collect_metrics`, `logs.detect.keyword_levels`, `logs.detect.hints`, and `cleanup.report.enabled`) are modeled as `*bool` pointers in Go. This allows `ApplyDefaults()` to distinguish between an **omitted** key (`nil` -> falls back to `true`) and an **explicit** `false` (preserved as `*false`).

### Subsystem Native Fallback Summary

| Subsystem | Native Fallback Behavior (`NewDefaultConfig` / `ApplyDefaults`) | Notes on `tarsier.example.yaml` vs. Native Defaults |
|---|---|---|
| `server` | `listen: ":8080"`, `base_url: ""`, `timezone: "UTC"` | Empty `base_url` disables canonical link prefixes and relies on TLS/proxy headers for `Secure` cookies. |
| `auth` | `session_ttl: "24h"`, `max_login_attempts: 5`, `lockout: "15m"` | Credentials and session signing keys come exclusively from environment variables, never YAML. |
| `storage` | `path: "/data"`, `logs_max_size: "3GiB"`, `metrics_max_size: "256MiB"` | Physical caps enforce 90% hysteresis eviction on `/data/logs/` if disk usage exceeds `logs_max_size`. |
| `retention` | `logs: "7d"`, `metrics_raw: "24h"`, `metrics_1m: "7d"`, `metrics_5m: "30d"`, `metrics_1d: "90d"`, `containers_exited: "24h"` | Keeps high-resolution raw samples and dead ephemeral container metadata short-lived while preserving rolled-up history. |
| `host` | `enabled: true`, `interval: "10s"`, `disks: []`, `disk_exclude_fs: ["tmpfs", "overlay", "squashfs", "devtmpfs"]` | Empty `disks` auto-discovers physical mounts via `/proc/mounts` or `/proc/self/mountinfo`. |
| `docker` | `enabled: true`, `socket: "/var/run/docker.sock"`, `stats_interval: "10s"`, `collect_logs: true`, `collect_metrics: true`, `include: []`, `exclude: []` | Degrades gracefully to host-only monitoring if the Docker socket is unreachable. `tarsier.example.yaml` sets `exclude: ["tarsier"]`. |
| `logs` | `default_timezone: ""` (treated as `UTC`), `detect.max_scan_bytes: 4096`, `detect.keyword_levels: true`, `detect.hints: true`, `files: []` | Empty `files` disables host file tailing; Docker container log streaming still operates when Docker is enabled. |
| `notifiers` | `email.enabled: false`, `email.smtp_port: 587`, `email.security: "starttls"`, `email.to: []`, `discord.enabled: false`, `discord.mention: ""` | Both notifiers are disabled unless explicitly enabled. `tarsier.example.yaml` sets `discord.mention: "@here"`. |
| `cleanup` | `schedule: "0 3 * * *"`, `batch_size: 5000`, `batch_pause: "50ms"`, `report.enabled: true`, `report.channels: []`, `report.min_attempts: 2`, `report.retry_delay: "10m"`, `report.on_failure: "proceed"`, `report.top_patterns: 20`, `report.sample_lines_per_pattern: 2`, `report.format: "text"`, `report.template: ""`, `report.max_attachment: "8MiB"` | `tarsier.example.yaml` configures `report.channels: ["discord", "email"]`. With empty `channels`, reports are generated and stored in `config.db` without external delivery. |
| `cache` | `max_sources: 50`, `log_tail_lines: 150`, `metric_max_series: 2000`, `metric_ring_points: 60`, `overview_ttl: "3s"`, `query_ttl: "3s"` | Bounds live in-memory telemetry to keep total RSS inside the `60–120 MB` target envelope. |
| `heartbeat` | `enabled: false`, `url: ""`, `interval: "5m"`, `timeout: "10s"` | Disabled until `heartbeat.enabled: true` and a valid HTTP(S) `heartbeat.url` are supplied. |

---

## 5. Environment Variables & Native Fallbacks Reference

Tarsier reads runtime environment variables directly in Go packages (`config`, `auth`, `collector/host`, `api`) and supports `${VAR}` / `${VAR:-default}` substitution inside `tarsier.yaml` and Docker Compose.

### Runtime, Auth & System Environment Variables

| Variable | Consumer | Fallback / Default | Required | Description |
|---|---|---|---|---|
| `TARSIER_CONFIG` | Go binary (`config.ResolveConfigPath`) | `/config/tarsier.yaml` (if present), else `./tarsier.yaml` | No | Path to `tarsier.yaml` inside the host or container. Overridden by `-config` CLI flag. |
| `TARSIER_CONFIG_FILE` | Docker Compose | `./tarsier.yaml` | No | Host path bind-mounted read-only to `/config/tarsier.yaml` inside the container. |
| `TARSIER_DATA_DIR` | Docker Compose | `./data` (container mounts to `/data`; YAML `storage.path` defaults to `/data`) | No | Host directory bind-mounted to `/data` for `config.db`, `metrics.db`, and `/data/logs/`. |
| `TARSIER_PORT` | Docker Compose | `8080` | No | Host port mapped to container port `8080`. |
| `TARSIER_VERSION` | Docker Compose | `latest` | No | Container image tag pulled from `ghcr.io/dale-tomson/tarsier`. |
| `TARSIER_ADMIN_USER` | Go binary (`auth.NewAuthenticatorFromEnv`) | `admin` | No | Single-admin login username. |
| `TARSIER_ADMIN_PASSWORD_HASH` | Go binary (`auth.NewAuthenticatorFromEnv`) | None (fatal startup error if unset or invalid) | **Yes** | Bcrypt password hash (minimum cost `10`, recommended `12`). Wrap in single quotes in `.env`. |
| `TARSIER_SESSION_SECRET` | Go binary (`auth.NewAuthenticatorFromEnv`) | Ephemeral 32-byte random secret generated at startup | Recommended | HMAC-SHA256 cookie signing key (`>= 32` bytes). If unset, active sessions reset on every restart. |
| `GOMEMLIMIT` | Go runtime (set in `Dockerfile` & Compose) | `200MiB` (in container image & Compose) | Recommended | Soft Go GC memory ceiling keeping process memory safely below the `256 MB` cgroup limit. |
| `GODEBUG` | Go runtime (set in `Dockerfile` & Compose) | `madvdontneed=1` (in container image & Compose) | Recommended | Forces Linux `MADV_DONTNEED` so freed Go heap pages are immediately reclaimed from container RSS. |
| `HOST_PROC` | Go binary (`collector/host`, `api`) | `/proc` (set to `/host/proc` for Linux host mounts) | No | Path to host `procfs` for CPU, memory, load average, network, and mount metrics. |
| `HOST_SYS` | Go binary (`collector/host`) | `/sys` (set to `/host/sys` for Linux host mounts) | No | Path to host `sysfs` for block device and thermal telemetry. |
| `HOST_ROOT` | Go binary (`collector/host`) | `/` (set to `/host/root` for Linux host mounts) | No | Root filesystem prefix used for `statfs` disk usage calculations. |

> [!WARNING]
> Tarsier does **not** read a plaintext `TARSIER_ADMIN_PASSWORD` environment variable at runtime. Authentication requires a pre-hashed bcrypt string in `TARSIER_ADMIN_PASSWORD_HASH` (generate with `htpasswd -nbBC 12 "" "your-password" | tr -d ':\n'`).

### Notification & Integration Environment Variables (`tarsier.yaml` Expansion)

These variables are injected via `${VAR:-default}` expansion in `tarsier.example.yaml` and `docker-compose.yml`. If the corresponding notifier is `enabled: false`, missing values remain inactive.

| Variable | YAML Target | Example / Compose Fallback | Description |
|---|---|---|---|
| `DISCORD_WEBHOOK_URL` | `notifiers.discord.webhook_url` | `""` (disabled) | Incoming Discord webhook URL for alerts and pre-cleanup digest reports. |
| `SMTP_ENABLED` | `notifiers.email.enabled` | `false` | Enables or disables the SMTP email notifier in `tarsier.example.yaml`. |
| `SMTP_HOST` | `notifiers.email.smtp_host` | `""` (Go) / `smtp.example.com` (example YAML) | Outbound SMTP server hostname or IP address. Required if email is enabled. |
| `SMTP_PORT` | `notifiers.email.smtp_port` | `587` | Outbound SMTP port (`587` STARTTLS, `465` TLS, `25` plaintext). |
| `SMTP_SECURITY` | `notifiers.email.security` | `starttls` | SMTP transport security mode (`none`, `starttls`, or `tls`). |
| `SMTP_USER` | `notifiers.email.username` | `""` (Go) / `alerts@example.com` (example YAML) | SMTP authentication username (custom YAML may also reference `${SMTP_USERNAME}`). |
| `SMTP_PASSWORD` | `notifiers.email.password` | `""` | SMTP authentication password. Keep in `.env`, never commit to YAML. |
| `SMTP_FROM` | `notifiers.email.from` | `""` (Go) / `Tarsier <alerts@example.com>` (example YAML) | RFC 5322 sender address. Required if email is enabled. |
| `SMTP_TO` | `notifiers.email.to` | `[]` (Go) / `admin@example.com` (example YAML) | Recipient email address(es). At least one recipient is required if email is enabled. |
| `HEARTBEAT_URL` | `heartbeat.url` | `""` (Go) / disabled when `heartbeat.enabled: false` | External HTTP(S) uptime ping URL (e.g., Healthchecks.io or Uptime Kuma). |

---

## 6. Hot Config Reload

Reload `tarsier.yaml` without restarting the container or dropping in-memory ring buffers using either:

* **UI:** Settings > **Reload Configuration**
* **API:** `POST /api/v1/system/reload`
* **Signal:** `kill -HUP <pid>` (or `docker kill --signal=HUP tarsier`)

Tarsier re-runs `ExpandEnv`, `KnownFields(true)`, and `Validate` before swapping configuration. Invalid edits are rejected without interrupting the running config.

---

## 7. Operational FAQ & Troubleshooting

### Resolving Docker Socket Permissions (Exit Code 13 / `EACCES`)

The distroless container runtime image is built on `nonroot`, while `/var/run/docker.sock` on most Linux hosts is owned by `root:docker` (`0660`). If the container lacks permission to open the socket, Docker discovery fails with `permission denied` (`EACCES`, exit code `13`).

**Solutions:**

1. **Production Compose Default:** `deploy/docker-compose.yml` sets `user: "0:0"` alongside `read_only: true` and `security_opt: [no-new-privileges:true]` so Tarsier can read `/var/run/docker.sock` (`:ro`) and `/host/proc` without altering host permissions.
2. **Non-Root with Socket GID:** Alternatively, look up the host `docker` group ID (`stat -c '%g' /var/run/docker.sock`) and pass `group_add: ["<DOCKER_GID>"]` in Compose while keeping a non-root UID.

### Docker Desktop (macOS / Windows) vs. Bare-Metal Linux `procfs` Realities

* **Bare-Metal Linux VPS:** Mount `/proc:/host/proc:ro`, `/sys:/host/sys:ro`, `/:/host/root:ro,rslave`, and set `HOST_PROC=/host/proc`, `HOST_SYS=/host/sys`, `HOST_ROOT=/host/root`. Tarsier reads physical host CPU ticks, memory, physical disks, and host network interfaces directly from the mounted host filesystems.
* **macOS & Windows (Docker Desktop / OrbStack / Colima):** Darwin and Windows hosts do not expose `/proc` or `/sys`. Leave the `/host/*` mounts commented out and use `HOST_PROC=/proc`, `HOST_SYS=/sys`, `HOST_ROOT=/`. Tarsier runs cleanly without mount errors, monitors all Docker containers via `/var/run/docker.sock`, and reports the Linux VM's telemetry as host telemetry.

### Why Does `TARSIER_ADMIN_PASSWORD_HASH` Fail in Compose?

Docker Compose interprets `$var` inside `.env` and `docker-compose.yml`. Bcrypt hashes start with `$2a$12$...`. Always wrap the hash in single quotes inside `.env` (`TARSIER_ADMIN_PASSWORD_HASH='$2a$12$...'`) or escape every `$` as `$$` inside `docker-compose.yml`.
