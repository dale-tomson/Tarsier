# Tarsier Standalone Docker Compose Deployment

This directory contains everything needed to deploy and run **Tarsier** in production on a single server or VPS using Docker Compose and pre-built multi-architecture images from GitHub Container Registry (`ghcr.io/dale-tomson/tarsier:latest`).

---

## Quick Start

### 1. One-Line Installation

Run the automated installer to download the deployment bundle and set up your initial configuration files:

```bash
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash
```

To install a specific release version or into a custom directory:

```bash
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash -s -- -v v0.1.0 -d /opt/tarsier
```

To automatically pull the image and launch the container immediately:

```bash
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash -s -- --start
```

---

### 2. Manual Setup

If you prefer to set up files manually without running the install script:

1. **Clone or download the deployment files:**
   Ensure you have `docker-compose.yml`, `.env.example`, and `tarsier.example.yml` in your deployment directory.

2. **Initialize configuration files:**
   ```bash
   cp .env.example .env
   cp tarsier.example.yml tarsier.yaml
   ```

3. **Start Tarsier:**
   ```bash
   docker compose up -d
   ```

4. **Open the Dashboard:**
   Navigate to [http://localhost:8080](http://localhost:8080) (or your configured `TARSIER_PORT`) and sign in:
   - **Username:** `admin`
   - **Password:** `admin`

---

## File Overview

| File | Purpose |
| :--- | :--- |
| `docker-compose.yml` | Container specification targeting `ghcr.io/dale-tomson/tarsier:latest` with memory constraints (`mem_limit: 256m`, `GOMEMLIMIT=200MiB`), read-only root filesystem, and persistent volume mounts. |
| `install.sh` | Portable POSIX installer script that downloads the bundle, provisions `.env` and `tarsier.yaml`, and generates a cryptographic session secret. |
| `.env.example` | Environment variable template for ports, bcrypt admin credentials, session secret, and notification channels. |
| `.env` | Active environment file (*created on install, do not commit secrets to git*). |
| `tarsier.example.yml` | Complete annotated specification and reference for all Tarsier configuration options. |
| `tarsier.yaml` | Active YAML configuration loaded into `/config/tarsier.yaml` inside the container. |
| `data/` | Persistent host directory storing SQLite databases (`config.db`, `metrics.db`) and compressed log chunks (`logs/`). |

---

## Production Hardening

### 1. Generate a Strong Admin Password
By default, the `.env` template is set to password `admin`. For production environments, generate a new bcrypt hash (cost factor 12) with `htpasswd`:

```bash
htpasswd -nbBC 12 "" "your-strong-password" | tr -d ':\n'
```

Copy the generated hash into `.env`:
```ini
TARSIER_ADMIN_PASSWORD_HASH='$2a$12$...'
```
*(Wrap in single quotes in `.env` to prevent Docker Compose variable interpolation of `$`).*

### 2. Session Secret
The installer automatically generates a random 32-byte hexadecimal secret for `TARSIER_SESSION_SECRET` in `.env`. If setting up manually, generate one with:

```bash
openssl rand -hex 32
```

### 3. Native Linux Host Telemetry Mounts
To monitor the physical Linux host's CPU, RAM, storage disks, and network interfaces:

1. Open `docker-compose.yml` and uncomment the bare-metal Linux telemetry volume mounts:
   ```yaml
   - /proc:/host/proc:ro
   - /sys:/host/sys:ro
   - /:/host/root:ro,rslave
   - /var/log:/host/var/log:ro
   ```
2. In `.env`, set:
   ```ini
   HOST_PROC=/host/proc
   HOST_SYS=/host/sys
   HOST_ROOT=/host/root
   ```

*(On macOS or Windows Docker Desktop / OrbStack / Colima, leave these commented out. Tarsier monitors container stats and VM metrics automatically without host path mounts).*

---

## Maintenance & Operations

### View Real-Time Logs
```bash
docker compose logs -f tarsier
```

### Stop or Restart
```bash
docker compose restart
docker compose down
```

### Upgrade to a New Version
To update to the latest release image:
```bash
docker compose pull
docker compose up -d
```

Or update `TARSIER_VERSION` in `.env` to pin a specific release version (e.g., `TARSIER_VERSION=v0.1.0`).

### Backup and Restore
All state is stored inside `./data`:
- `config.db`: Alert rules, offsets, admin web sessions, container lifecycle history.
- `metrics.db`: Metrics time series and downsampled rollups.
- `logs/`: Append-only 64 KB Zstandard-compressed chunk files and sparse indexes.

To back up your instance, simply archive the `./data` directory while the container is stopped or using SQLite online backup tools.
