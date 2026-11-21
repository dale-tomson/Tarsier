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
| `cache.max_sources` | `int` | `500` | Upper bound on distinct log/metric sources in memory (`10..5000`). |
| `cache.log_tail_lines` | `int` | `300` | Hot ring buffer lines per source (`50..1000`). |
| `cache.overview_ttl` | `duration` | `"3s"` | Overview endpoint cache TTL. |
| `cache.query_ttl` | `duration` | `"3s"` | Range query cache TTL (`QueryCache`, capacity `256`). |
| `heartbeat.enabled` | `bool` | `false` | Enable outbound HTTP GET uptime pings. |
| `heartbeat.url` | `string` | `""` | Target ping URL (e.g. Healthchecks.io / Uptime Kuma). |
| `heartbeat.interval` | `duration` | `"5m"` | Interval between heartbeat pings. |
| `heartbeat.timeout` | `duration` | `"10s"` | HTTP request timeout. |

---

## 4. Hot Config Reload

Reload `tarsier.yaml` without restarting the container or dropping in-memory ring buffers using either:

* **UI:** Settings > **Reload Configuration**
* **API:** `POST /api/v1/system/reload`
* **Signal:** `kill -HUP <pid>` (or `docker kill --signal=HUP tarsier`)

Tarsier re-runs `ExpandEnv`, `KnownFields(true)`, and `Validate` before swapping configuration. Invalid edits are rejected without interrupting the running config.

---

## 5. Operational FAQ & Troubleshooting

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
