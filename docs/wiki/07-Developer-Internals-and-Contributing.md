# 7. Developer Internals & Contributing

This guide covers Tarsier's internal architecture, concurrency invariants, zero-Node frontend rules, testing standards, and contribution workflow.

---

## 1. The Three Zeroes

Every contribution must preserve Tarsier's core engineering constraints defined in `AGENTS.md`:

1. **Zero CGO:** Compiles cleanly with `CGO_ENABLED=0` across `linux/amd64`, `linux/arm64`, and `darwin`. Pure-Go `modernc.org/sqlite` replaces C-backed SQLite drivers; host telemetry reads `/proc` and `/sys` directly without C bindings.
2. **Zero Node.js:** No `package.json`, `node_modules`, Vite, Webpack, or TypeScript build steps anywhere in the application codebase. Static assets under `tarsier/web/` are embedded into the Go binary via `embed.FS` and minified only inside the multi-stage `Dockerfile` using `tdewolff/minify`.
3. **Zero Emojis:** Do not use emojis anywhere in UI templates, frontend JavaScript, backend logs, error messages, code comments, commit messages, PR descriptions, or documentation.

---

## 2. Repository & Package Layout

```text
tarsier/
  cmd/tarsier/       # Binary entrypoint, CLI flags (-config, -test-config, -version), SIGHUP reload
  internal/
    config/          # YAML loader, ${VAR:-default} expansion, KnownFields(true), validation
    db/              # SQLite factory, mandatory PRAGMAs, single-writer queues (config.db & metrics.db)
    auth/            # Constant-time bcrypt auth, HMAC-SHA256 sessions, IP rate limiter
    model/           # Shared domain types, Level/Stream enums, Clock abstraction
    metrics/         # HotCache ring buffers, sample ingestor, 1m/5m/1d rollup engine
    collector/       # Host (/proc, /sys, statfs) collector and inode-aware file tailer
    docker/          # Thin hand-rolled HTTP client over /var/run/docker.sock
    logs/            # Zero-alloc timestamp/level detector, Zstd chunk writer, .idx binary search, TailCache
    alert/           # RuleCache, metric/log/container evaluators, state machine, Discord & SMTP notifiers, heartbeat
    cleanup/         # Message normalizer, pre-cleanup report builder, delivery retries, retention executor, DB maintenance
    api/             # chi/v5 HTTP router, QueryCache, REST /api/v1 handlers, OpenAPI conformance tests
  web/               # index.html, static/js/*.js, static/css/app.css, vendored petite-vue, Pico.css, uPlot
```

---

## 3. SQLite Single-Writer Queues & PRAGMAs (`internal/db`)

`internal/db/factory.go` opens strictly two SQLite databases (`config.db` and `metrics.db`) with split connection handles per database:

* **Write Handle (`RW`):** `SetMaxOpenConns(1)` driven exclusively by a dedicated single-writer goroutine consuming a bounded job channel (`ConfigWriterQueueSize = 256`, `MetricsWriterQueueSize = 2048`). This eliminates `SQLITE_BUSY` write lock contention under concurrent ingestion.
* **Read Pool (`RO`):** `SetMaxOpenConns(2)` opened with `mode=ro` so dashboard queries never serialize behind writes.
* **Enforced PRAGMAs (`internal/db/pragmas.go`):**
  * `journal_mode=WAL`
  * `synchronous=NORMAL`
  * `busy_timeout=5000`
  * `foreign_keys=ON`
  * `temp_store=MEMORY`
  * `mmap_size=0` (prevents unbounded virtual/RSS memory inflation)
  * `auto_vacuum=INCREMENTAL`
  * `cache_size=-2000` (`config.db`, ~2 MiB) / `cache_size=-4000` (`metrics.db`, ~4 MiB)

---

## 4. Zero-Node Frontend (`tarsier/web/`)

* **Stack:** `petite-vue` (IIFE, ~6 KB), `Pico.css` v2, and `uPlot` served from `tarsier/web/static/vendor/` via Go `embed.FS` (`assets_test.go` verifies embedded asset size stays strictly under budget).
* **Strict XSS Policy:** `v-html` is banned across `tarsier/web/index.html`. Log lines and alert summaries render via `v-text` (plain escaped text). Pre-cleanup HTML reports render inside `<iframe sandbox="" :srcdoc="...">` with zero script execution privileges.

---

## 5. Testing Standards

All code changes must maintain bounded memory (`sync.Pool` reuse, capped maps/channels) and pass the full Go test suite with the race detector:

```bash
# Run full test suite with race detector
make test
# Or directly:
cd tarsier && CGO_ENABLED=0 go test -race -v ./...

# Check formatting and static analysis
cd tarsier && gofmt -l -s . && go vet ./...
```

Key testing invariants enforced across the codebase:

* **Table-Driven Parsers & Config Validation:** `internal/config/config_test.go`, `internal/collector/host/collector_test.go`.
* **Log Golden Corpus (`150+` lines):** `internal/logs/timestamp_test.go` and `level_test.go` validate sub-10 µs zero-alloc parsing against real-world formats and false-positive traps.
* **Deterministic Fake Clocks:** `internal/alert/statemachine_test.go` and `internal/metrics/rollup_test.go` inject `model.Clock` mocks so `for_sec`, cooldowns, and midnight UTC rollups run deterministically without `time.Sleep`.
* **OpenAPI Contract Conformance:** `internal/api/openapi_test.go` checks routes against `docs/openapi.yaml`.

---

## 6. Git Workflow & Pull Requests

Follow the mandatory repository rules in `AGENTS.md`:

1. **Branch from Updated `main`:**
   ```bash
   git checkout main && git pull && git checkout -b <type>/<short-description>
   ```
   Never commit directly to `main`.
2. **Atomic Commits:** Keep commits scoped to a single feature or fix with clear imperative subjects and zero emojis.
3. **Pull Request Template:** All PR descriptions must follow [`.github/pull_request_template.md`](https://github.com/dale-tomson/Tarsier/blob/main/.github/pull_request_template.md), including the **Architectural Constraints & PRAGMAs Checklist**, **Test Verification** (`go test -race -v ./...`, `gofmt -l -s .`, `go vet ./...`), and **Acceptance Checklist**.
