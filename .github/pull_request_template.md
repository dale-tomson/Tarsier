## Summary
<!-- Provide a clear, concise summary of the changes and the problem they solve. -->

## Type of Change
- [ ] 🚀 New feature (non-breaking change adding functionality)
- [ ] 🐛 Bug fix (non-breaking change fixing an issue)
- [ ] ⚡ Performance improvement
- [ ] 🧹 Refactoring / Code cleanup
- [ ] 📝 Documentation update
- [ ] ⚙️ CI / Build / Infrastructure

## Architectural Constraints & PRAGMAs Checklist
*Strict adherence to `AGENTS.md` rules is mandatory:*
- [ ] **Memory Envelope**: Operates under `< 256 MB` container cgroup ceiling (`GOMEMLIMIT=200MiB`).
- [ ] **CPU Envelope**: Consumes `< 5%` of one core idle/normal (`0.1–0.5 CPU` envelope).
- [ ] **Zero CGO**: Pure Go, compiles with `CGO_ENABLED=0` without C dependencies.
- [ ] **Zero Node / Bundler**: No npm, node, or frontend bundlers. Plain text log rendering (no `v-html`).
- [ ] **SQLite Concurrency & Stores**:
  - [ ] Two databases only (`config.db` and `metrics.db`).
  - [ ] Dedicated single-writer goroutines fed by bounded channels (`SetMaxOpenConns(1)`).
  - [ ] Enforces PRAGMAs (`WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `temp_store=MEMORY`, negative `cache_size`).
  - [ ] No `logs.db` or SQLite FTS5 tables.
- [ ] **Log Storage & Zero-Alloc Ingest**:
  - [ ] Log detection / search operates in-place on `[]byte` with bounded buffers & `sync.Pool`.
  - [ ] Binary-searched sparse index (`.idx`) and compressed chunk files (`.chunk`).

## Test Verification
<!-- Detail the test steps, execution commands, and results. -->
- [ ] Unit tests pass with race condition detector: `go test -race -v ./...`
- [ ] Code formatting verified: `gofmt -l -s .`
- [ ] Static analysis clean: `go vet ./...`

## Acceptance Checklist
- [ ] Branch created from updated `main` branch.
- [ ] Atomic commit(s) with clear imperative messages.
- [ ] Pull request description includes copyable markdown matching this template.
- [ ] Documentation updated if applicable (`Readme.md`, `tarsier.example.yaml`).
