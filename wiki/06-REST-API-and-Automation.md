# 6. REST API & Automation

Tarsier exposes a JSON REST API under `/api/v1` alongside a public `/healthz` liveness endpoint, documented by a complete OpenAPI 3.1.0 specification at [`docs/openapi.yaml`](https://github.com/dale-tomson/Tarsier/blob/main/docs/openapi.yaml).

---

## 1. Authentication & Security Model

All `/api/v1/*` endpoints (except `POST /api/v1/auth/login`) require an authenticated `tarsier_session` `HttpOnly` cookie signed with HMAC-SHA256 (`TARSIER_SESSION_SECRET`).

* **Brute-Force Protection:** `POST /api/v1/auth/login` enforces constant-time credential verification (`crypto/subtle` + dummy bcrypt cost-matching to prevent username enumeration) and per-IP rate limiting (`auth.max_login_attempts` / `auth.lockout`), returning HTTP `429` during lockouts.
* **Request Body Limit:** Incoming API payloads are capped at `1 MiB` (`maxBytesMiddleware`).

### Authenticating with `curl`

```bash
# 1. Authenticate and store session cookie
curl -fsS -c cookies.txt -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"your-password"}'

# 2. Query authenticated endpoints
curl -fsS -b cookies.txt http://127.0.0.1:8080/api/v1/system/health
```

---

## 2. Endpoints by Domain

### Health & Authentication

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/healthz` | Public | Liveness probe returning `{ "status": "ok", "version": "..." }`. |
| `POST` | `/api/v1/auth/login` | Public (rate-limited) | Authenticates admin credentials and sets `tarsier_session` cookie. |
| `POST` | `/api/v1/auth/logout` | Cookie | Invalidates the active session in `config.db` and clears cookie. |
| `GET` | `/api/v1/auth/me` | Cookie | Returns the currently authenticated username. |

### System, Configuration & Cleanup

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/system/config` | Returns sanitized active `tarsier.yaml` configuration (secrets redacted). |
| `GET` | `/api/v1/system/health` | Returns Go runtime memory stats, SQLite DB status, storage sizes, heartbeat status, and cleanup warnings. |
| `POST` | `/api/v1/system/reload` | Hot-reloads `tarsier.yaml` from disk with full validation. |
| `GET` | `/api/v1/system/cleanup/history` | Lists past scheduled/manual cleanup runs and dry runs. |
| `GET` | `/api/v1/system/cleanup/{id}/report` | Fetches stored pre-cleanup digest report text/HTML for run `{id}`. |
| `POST` | `/api/v1/system/cleanup/run` | Triggers cleanup (`?dry_run=true` for non-destructive simulation). |
| `GET` | `/api/v1/system/cleanup/template` | Downloads the default plain-text or HTML report template (`?format=text\|html`). |
| `POST` | `/api/v1/system/cleanup/preview` | Renders a candidate custom Go `text/template` against live/sample telemetry. |

### Host & Container Telemetry

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/host/metrics` | Queries host time series (CPU, load, RAM, disk, network). |
| `GET` | `/api/v1/metrics/daily/summary` | Returns today's running average and peak metrics for host and containers. |
| `GET` | `/api/v1/metrics/daily` | Returns 14-day daily rollup history (`sample_1d`). |
| `GET` | `/api/v1/containers` | Lists discovered containers with state, health, restart count, and latest metrics. |
| `GET` | `/api/v1/containers/{name}` | Returns container telemetry history and lifecycle events (`container_event`). |

### Logs & Sources

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/sources` | Lists discovered container and file log sources with buffered line counts. |
| `GET` | `/api/v1/logs` | Queries single-source logs (`source`, `q`, `regex`, `level`, `stream`, `from`, `to`, `cursor`, `limit`). |
| `GET` | `/api/v1/logs/context` | Returns `±20` surrounding log lines around a specific record ID. |

### Alerting

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/alerts/active` | Lists currently firing alerts across all targets. |
| `GET` | `/api/v1/alerts/rules` | Lists all alert rules with live evaluation state. |
| `POST` | `/api/v1/alerts/rules` | Creates a new alert rule in `config.db`. |
| `GET` | `/api/v1/alerts/rules/{id}` | Retrieves a single rule by ID. |
| `PUT` | `/api/v1/alerts/rules/{id}` | Updates an existing rule and reloads `RuleCache`. |
| `DELETE` | `/api/v1/alerts/rules/{id}` | Deletes a rule and clears its live state. |
| `POST` | `/api/v1/alerts/rules/{id}/enable` | Enables a rule. |
| `POST` | `/api/v1/alerts/rules/{id}/disable` | Disables a rule. |
| `GET` | `/api/v1/alerts/templates` | Returns built-in starter alert rule templates. |
| `POST` | `/api/v1/alerts/rules/preview` | Dry-runs a candidate rule condition against live telemetry. |
| `GET` | `/api/v1/alerts/rules/export` | Exports all rules as JSON. |
| `POST` | `/api/v1/alerts/rules/import` | Imports rules from a JSON payload. |
| `GET` | `/api/v1/alerts/history` | Queries historical firing and resolved alert events. |
| `POST` | `/api/v1/alerts/test` | Dispatches a test notification (`email` or `discord`). |

---

## 3. Importing OpenAPI 3.1.0 into API Clients

The repository maintains a verified OpenAPI 3.1.0 schema at `docs/openapi.yaml` (validated by `tarsier/internal/api/openapi_test.go`):

* **Postman:** **Import** > select `docs/openapi.yaml` > set collection variable `baseUrl` to your Tarsier URL > run `POST /api/v1/auth/login` first to populate the cookie jar.
* **Bruno:** **Import Collection** > **OpenAPI V3** > select `docs/openapi.yaml` > configure environment `baseUrl`.
* **Insomnia:** **Import** > **File** > select `docs/openapi.yaml`.
* **Local Swagger UI:**
  ```bash
  docker run --rm -p 8081:8080 -e SWAGGER_JSON=/spec/openapi.yaml -v "$(pwd)/docs:/spec" swaggerapi/swagger-ui
  ```

---

## 4. Automation Snippets

### Shell: Backup Alert Rules & Trigger a Dry-Run Cleanup

```bash
#!/usr/bin/env bash
set -euo pipefail

BASE_URL="http://127.0.0.1:8080"
COOKIE_JAR="$(mktemp)"
trap 'rm -f "$COOKIE_JAR"' EXIT

curl -fsS -c "$COOKIE_JAR" -X POST "${BASE_URL}/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"${TARSIER_USER:-admin}\",\"password\":\"${TARSIER_PASSWORD}\"}" >/dev/null

# Export alert rules to a dated JSON backup
curl -fsS -b "$COOKIE_JAR" "${BASE_URL}/api/v1/alerts/rules/export" > "tarsier-rules-$(date +%F).json"

# Run non-destructive cleanup simulation
curl -fsS -b "$COOKIE_JAR" -X POST "${BASE_URL}/api/v1/system/cleanup/run?dry_run=true"
```

### Python: Query Recent `ERROR` Logs for a Container

```python
import os
import requests

BASE_URL = os.environ.get("TARSIER_URL", "http://127.0.0.1:8080")
session = requests.Session()

resp = session.post(
    f"{BASE_URL}/api/v1/auth/login",
    json={"username": os.environ.get("TARSIER_USER", "admin"), "password": os.environ["TARSIER_PASSWORD"]},
    timeout=10,
)
resp.raise_for_status()

sources = session.get(f"{BASE_URL}/api/v1/sources", timeout=10).json()["sources"]
target = next(s for s in sources if s["name"] == "api-server")

logs = session.get(
    f"{BASE_URL}/api/v1/logs",
    params={"source": target["id"], "level": "ERROR", "limit": 50},
    timeout=10,
).json()

for entry in logs.get("entries", []):
    print(f"[{entry['ts']}] ({entry['level']}) {entry['line']}")
```
