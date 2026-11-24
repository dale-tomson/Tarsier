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
  * `cache_size=-512` (`config.db`, ~512 KiB) / `cache_size=-2000` (`metrics.db`, ~2 MiB)
* **Post-Cleanup Maintenance (`internal/cleanup/maintenance.go`):** After retention pruning, `DatabaseMaintainer.MaintainHandle` executes bounded `PRAGMA incremental_vacuum(pages)` passes, `PRAGMA optimize`, `PRAGMA wal_checkpoint(TRUNCATE)`, and `PRAGMA shrink_memory;` on both `RW` and `RO` handles to release unused SQLite page-cache heap allocations back to Go.

---

## 4. Architecture & Memory Budget Deep Dive

### Hard Operational Budgets

Tarsier is engineered to run unobtrusively on resource-constrained hosts without competing with primary applications:

| Dimension | Hard Target / Ceiling | Enforcement Mechanism |
|---|---|---|
| **Container Memory Ceiling** | `< 256 MB` cgroup limit | `mem_limit: 256m`, `GOMEMLIMIT=200MiB`, `GODEBUG=madvdontneed=1` |
| **Normal Operating RSS** | `60–120 MB` typical | Bounded channels, capped ring buffers, `sync.Pool` slice reuse, rightsized SQLite caches |
| **CPU Budget** | `< 5%` of one core idle/normal (`0.1–0.5 CPU`) | Zero-alloc `[]byte` log scanning (`< 10 µs`/line), `10s` polling, single-writer SQLite queues |
| **Concurrency Queues** | Fixed bounded channels | `ConfigWriterQueueSize = 256`, `MetricsWriterQueueSize = 2048`, `LogIngestor` queue `8192`, `QueryCache` capacity `256` |

### Rationale & Trade-Off Decisions (Why These Constraints?)

* **VPS / Edge Host Non-Starvation:** Single-server observability tools run alongside revenue-generating workloads on 1–2 GB VPS instances and edge nodes. A monitoring daemon that spikes above several hundred megabytes risks triggering the Linux OOM killer against the very databases or web servers it is supposed to observe. Maintaining a strict `< 256 MB` container ceiling guarantees Tarsier remains safe to co-locate.
* **Why Pure-Go SQLite (`modernc.org/sqlite`):** Eliminating CGO enables deterministic cross-compilation (`CGO_ENABLED=0`) for `linux/amd64`, `linux/arm64`, and `darwin` from any host without C cross-compilers, `glibc`/`musl` ABI mismatches, or external database containers to administer.
* **Why Raw Logs Are Kept Out of SQLite:** Writing high-volume log lines and FTS5 inverted indexes into SQLite produces unbounded WAL growth, heavy page-cache churn, and slow retention `DELETE` locks. Instead, Tarsier buffers per-source logs into `64 KiB` uncompressed blocks (`TRLB`), compresses them via `klauspost/compress/zstd` into append-only daily `.chunk` files, and maintains fixed 36-byte `.idx` sparse records. Binary-searching `.idx` files keeps seek latency sub-millisecond with constant memory, while retention cleanup is an $O(1)$ file unlink.
* **Why SQLite Cache Rightsizing (`-512` KiB / `-2000` KiB):** `config.db` holds small metadata tables totaling under `1 MB` on disk, so `-512` (~512 KiB) caches the active working set without reserving unnecessary pages. `metrics.db` stores `sample_raw`, `sample_1m`, `sample_5m`, and `sample_1d` as `WITHOUT ROWID` tables clustered on `(series_id, ts)`, allowing `-2000` (~2 MiB) to keep hot interior B-tree nodes resident. Right-sizing from the earlier `-2000` / `-4000` settings across `RW` and `RO` connections cuts SQLite page-cache memory by roughly `58%` (~10.5 MiB saved across pools) with negligible query latency impact.
* **Why `PRAGMA shrink_memory;`:** Even with negative `cache_size` caps, SQLite retains allocated cache pages after large maintenance operations (`incremental_vacuum`, `wal_checkpoint(TRUNCATE)`). Issuing `PRAGMA shrink_memory;` on both `RW` and `RO` handles immediately frees unused SQLite cache allocations back to the Go heap.
* **Why In-Memory Buffer Rightsizing (`50 × 150` Logs, `2000 × 60` Metrics):** Capping `TailCache` at `50` sources × `150` lines and `HotCache` at `2000` series × `60` points (`10` minutes of `10s` samples) prevents uncompressed live telemetry from hoarding `75–150 MB` of RAM on multi-container hosts. Older log lines and longer metric windows seamlessly fall back to `.idx`/`.chunk` disk scans and `metrics.db` queries.
* **Why `sync.Pool` Reuse:** `ChunkWriterManager`, `LogQueryEngine`, `ReportBuilder`, `file_tailer.go`, `logs_demux.go`, and `auth.WriteJSON` (`jsonBufPool`, capped at `64 KiB` per pooled buffer) reuse byte slices and buffers on hot paths. This eliminates repeated `64 KiB`–`70 KiB` heap allocations per Zstd block or API response and reduces Go GC CPU overhead.
* **Why `GODEBUG=madvdontneed=1`:** Go's runtime defaults on newer Linux kernels can use `MADV_FREE` for scavenged heap pages, leaving freed memory counted toward container cgroup RSS until the kernel experiences global memory pressure. Setting `GODEBUG=madvdontneed=1` forces immediate `MADV_DONTNEED` reclamation so container RSS mirrors actual live heap usage and prevents spurious cgroup OOM kills.

> [!IMPORTANT]
> Any new in-memory cache, ring buffer, or channel added to Tarsier must declare an explicit upper bound in configuration or constants and reuse buffers via `sync.Pool` whenever operating on per-line or per-block payloads.

---

## 5. Zero-Node Frontend (`tarsier/web/`)

* **Stack:** `petite-vue` (IIFE, ~6 KB), `Pico.css` v2, and `uPlot` served from `tarsier/web/static/vendor/` via Go `embed.FS` (`assets_test.go` verifies embedded asset size stays strictly under budget).
* **Strict XSS Policy:** `v-html` is banned across `tarsier/web/index.html`. Log lines and alert summaries render via `v-text` (plain escaped text). Pre-cleanup HTML reports render inside `<iframe sandbox="" :srcdoc="...">` with zero script execution privileges.

---

## 6. Testing Standards

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

## 7. Git Workflow & Pull Requests

Follow the mandatory repository rules in `AGENTS.md`:

1. **Branch from Updated `main`:**
   ```bash
   git checkout main && git pull && git checkout -b <type>/<short-description>
   ```
   Never commit directly to `main`.
2. **Atomic Commits:** Keep commits scoped to a single feature or fix with clear imperative subjects and zero emojis.
3. **Pull Request Template:** All PR descriptions must follow [`.github/pull_request_template.md`](https://github.com/dale-tomson/Tarsier/blob/main/.github/pull_request_template.md), including the **Architectural Constraints & PRAGMAs Checklist**, **Test Verification** (`go test -race -v ./...`, `gofmt -l -s .`, `go vet ./...`), and **Acceptance Checklist**.
