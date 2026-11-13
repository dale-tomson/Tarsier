# AGENTS.md — AI & Developer Engineering Guidelines

## 1. Git Workflow & Branching Rules (Mandatory)
* **Branching Strategy:** Any changes **must** be committed and pushed to a **new branch created from an updated `main` branch** (`git checkout main && git pull && git checkout -b <type>/<short-description>`).
* **Exception:** This rule holds false **only** if the changes are directly related to the current active working branch you are already checked out on. Never commit directly to `main`.
* **Commit Granularity:** Keep commits atomic, scoped to one feature/task, with clear imperative messages.
* **Pull Request Messages:** Upon pushing any branch, always provide a complete, copyable GitHub PR title and description in Markdown format (including Summary, Architectural Constraints & PRAGMAs, Test Verification, and Acceptance Checklist).

## 2. Project Overview & Core Constraints
Tarsier is a lightweight, self-hosted, single-server observability tool distributed as a single static Go binary and multi-arch container image (`linux/amd64`, `linux/arm64`).

### Hard Operational Budgets
* **Memory:** Strict `< 256 MB` container cgroup ceiling (`GOMEMLIMIT=200MiB`), targeting `60–120 MB` RSS in normal operation.
* **CPU:** `< 5%` of one core idle/normal (`0.1–0.5 CPU` envelope).
* **Zero CGO & Zero Node:** Must compile with `CGO_ENABLED=0`. No Node.js, npm, or frontend bundlers anywhere in the repo.

## 3. Architecture & Tech Stack Rules
When writing or modifying code, adhere strictly to these finalized architectural choices:

* **Backend Runtime:** Go 1.27 (`net/http` + `chi/v5`).
* **Config (`tarsier.yaml`):** Parsed via `gopkg.in/yaml.v3` with strict `KnownFields(true)` and `${VAR}` / `${VAR:-default}` env substitution. Secrets stay in env vars; alert rules are **never** stored in YAML.
* **SQLite Stores (`modernc.org/sqlite`, pure Go):**
  * **Two databases only:** `config.db` (alert rules, state, offsets, sessions, container events) and `metrics.db` (`series`, `sample_raw`, `sample_1m`, `sample_5m`, `sample_1d` using `WITHOUT ROWID`).
  * **No `logs.db`:** Do not store raw logs or FTS5 tables in SQLite.
  * **Concurrency Model:** One dedicated single-writer goroutine (`SetMaxOpenConns(1)`) fed by a bounded channel per DB, paired with an isolated read pool (`SetMaxOpenConns(2)`). Enforce `WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `temp_store=MEMORY`, `auto_vacuum=INCREMENTAL`, and strict negative KiB `cache_size` pragmas.
* **Log Storage & Search (`/data/logs/<source_id>/`):**
  * Append-only `64 KB` blocks compressed via `klauspost/compress/zstd` (`.chunk` files) paired with binary-searched sparse index files (`.idx`).
  * All log searches are **scoped to a single source**. Prune blocks via `.idx` binary search, decompress into `sync.Pool` buffers, and scan with zero-alloc byte operations (`bytes.Contains` / Boyer-Moore).
* **Log Structure Detection:**
  * Must operate in-place on `[]byte` (avoiding `string` conversions on the hot path) in `< 10 µs` per 200B line. No per-line regex compilation.
* **Collectors:**
  * **Host:** Direct `/host/proc`, `/host/sys`, and `statfs` parsing.
  * **Docker:** Thin hand-rolled HTTP client over `/var/run/docker.sock` (do not import the official Docker SDK).
* **Frontend (`web/`):**
  * No build step. Vendored `petite-vue` (IIFE, ~6 KB), `Pico.css` (v2), and `uPlot` embedded directly into the binary via `embed.FS`.
  * Render all log lines as plain text (never `v-html` / raw HTML) to prevent XSS.

## 4. Coding & Testing Standards
* **Bounded Memory:** Every in-memory map, ring buffer, and channel must have an explicit configured upper bound. Reuse byte slices via `sync.Pool` in ingest and decompression paths.
* **Testing:**
  * Write table-driven unit tests for parsers, config validation, and alert state machines (using fake clocks).
  * Log detection changes must pass the 150+ line golden corpus test including false-positive traps.
  * Run `go test -race ./...` before pushing any branch.
* **Directory Layout:**
  ```text
  cmd/tarsier/       # CLI entrypoint, flags (-config, -test-config, -version)
  internal/
    config/          # YAML loader, \${VAR} expansion, validation
    db/              # SQLite factory, PRAGMAs, single-writer queues (config & metrics)
    collector/       # Host (/proc, /sys) and hand-rolled Docker socket collectors
    logs/            # Zero-alloc timestamp/level detector, Zstd chunk writer, .idx binary search
    alert/           # Rule cache, evaluators, state machine, SMTP & Discord notifiers
    cleanup/         # Retention worker, pre-cleanup report generator (Markdown/HTML)
    api/             # chi HTTP server, auth middleware, REST /api/v1 handlers
  web/               # index.html, ES modules, vendored petite-vue, Pico.css, uPlot