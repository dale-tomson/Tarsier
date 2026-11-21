# Future Features

This document outlines the selected capabilities planned for future integration into Tarsier. These features represent architectural extensions chosen to improve single-server observability, automated diagnostics, and operator workflow without violating Tarsier's core performance and footprint constraints.

---

## Architectural Invariants

Every future feature must strictly adhere to the engineering boundaries defined in `AGENTS.md`:

1. **Hard Operational Budgets:**
   * Container cgroup ceiling strictly `< 256 MB` (`GOMEMLIMIT=200MiB`), targeting `60–120 MB` RSS in normal operation.
   * CPU envelope strictly `< 5%` of a single CPU core idle/normal.
2. **Pure Go & Zero CGO:**
   * Strict `CGO_ENABLED=0` compilation across all supported architectures (`linux/amd64`, `linux/arm64`, `darwin`).
3. **Zero Node.js:**
   * No `package.json`, `node_modules`, or build-time bundlers. Vendored frontend assets (`petite-vue`, `Pico.css`, `uPlot`) are embedded directly via Go `embed.FS`.
4. **Zero Emojis:**
   * Zero emojis across all UI templates, frontend text, backend logs, error messages, code comments, commit messages, PR descriptions, and markdown documentation.

---

## Selected Capabilities

### 1. Accidental Secret & Credential Leak Shield

* **Problem & Context:** Applications frequently log sensitive credentials by accident, including OAuth/Bearer tokens, database connection URLs, API keys, and private keys. Once logged, these secrets become stored on disk and exposed in dashboard log viewers or notification channels.
* **Key Capabilities:**
  * Fast, zero-alloc streaming scanner operating on byte slices during log ingestion prior to block compression.
  * In-place masking of known token and secret signatures (replacing sensitive substrings with `[REDACTED]`).
  * Prevents credential exposure in compressed log chunks, incident alerts, and pre-cleanup digest reports.

---

### 2. Embedded Synthetic Probes & SSL Expiry Monitoring

* **Problem & Context:** Operators often deploy separate monitoring tools (such as Uptime Kuma) solely to track external HTTP/TCP endpoint availability and TLS certificate expiration dates.
* **Key Capabilities:**
  * Native, low-overhead synthetic probing engine for HTTP/HTTPS status, latency, and TCP port connectivity.
  * Automated TLS handshake inspection tracking remaining certificate lifetime (`ssl_cert_days_remaining`).
  * Seamless integration with Tarsier's time-series engine and alert rule builder for proactive expiration and downtime warnings.

---

### 3. Automatic Zero-Touch Deploy & Container Replacement Detection

* **Problem & Context:** Correlating performance spikes or errors with application updates currently requires manually checking CI/CD logs or configuring external webhooks.
* **Key Capabilities:**
  * Passive Docker socket event stream monitoring to detect container recreation cycles and image digest updates.
  * Automated generation of deployment lifecycle events stored in the configuration database.
  * Visual deployment markers on metrics graphs to instantly correlate rollouts with system behavior.

---

### 4. Post-Mortem Process Snapshots on Alert Breaches

* **Problem & Context:** Transient CPU or memory spikes that occur during off-hours frequently resolve before an operator can log in, leaving time-series graphs without attribution as to which operating system process caused the breach.
* **Key Capabilities:**
  * Point-in-time process scanner triggered on-demand when host CPU or memory alert thresholds are exceeded.
  * Captures the Top 5 processes by CPU and RSS from the host process table.
  * Attaches the process snapshot directly to the incident record for post-mortem analysis.

---

### 5. Zero-Config Database & Service Mini-Exporters

* **Problem & Context:** Running standalone Prometheus exporters (e.g., redis_exporter, postgres_exporter) for small workloads introduces unnecessary memory and configuration overhead.
* **Key Capabilities:**
  * Native, single-connection collectors for common infrastructure services including Redis, PostgreSQL, MySQL, and Systemd.
  * Gathers core health telemetry (e.g., Redis memory and hit/miss ratios, database connection counts, replication lag, and failed Systemd units).
  * Reuses standard collection loops with bounded timeouts to guarantee predictable resource consumption.

---

### 6. Zero-Alloc Log Anomaly & Novel Pattern Detection

* **Problem & Context:** Operators cannot anticipate every failure mode with explicit regex alert rules, leaving novel error spikes unnoticed until user impact occurs.
* **Key Capabilities:**
  * Rolling, bounded in-memory baseline of normalized log message signatures.
  * Proactive detection and alerting when an application service begins logging unclassified error patterns at anomalous frequencies.
  * Minimal memory footprint using fixed-capacity template tracking structures.

---

### 7. Per-Source Log Rate Limiting & Buffer Protection

* **Problem & Context:** Malfunctioning application containers emitting uncontrolled floods of log lines can overwhelm memory buffers and degrade ingest performance.
* **Key Capabilities:**
  * In-memory token-bucket rate limiter per log source to enforce sustainable ingestion limits.
  * Configurable burst allowance with dropped line accounting and rate warning markers.
  * Guarantees system stability and ensures misbehaving containers do not starve host telemetry.

---

### 8. Federated Fleet Observability (Tarsier Hub)

* **Problem & Context:** Managing multiple VPS nodes or servers currently requires visiting separate dashboard instances, while building a centralized telemetry lake introduces massive storage and maintenance complexity.
* **Key Capabilities:**
  * Decoupled, independent central gateway (`tarsier-hub`) designed to run on lightweight hosting with minimal memory footprint.
  * Centralized OIDC Single Sign-On (SSO) and role-based access for team environments.
  * Unified fleet overview with on-demand query federation while individual edge nodes retain 100% data ownership and local alert resilience.
