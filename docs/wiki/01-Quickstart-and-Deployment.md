# 1. Quickstart & Deployment

Tarsier is packaged as a single static binary and a multi-architecture distroless container image (`ghcr.io/dale-tomson/tarsier:latest` for `linux/amd64` and `linux/arm64`). Choose the deployment model that matches your server environment.

---

## 1. 5-Minute Docker Quickstart

For immediate evaluation on a host with Docker installed, you can launch Tarsier directly with `docker run`:

```bash
mkdir -p ./tarsier-data

# Download example config
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/tarsier.example.yml -o tarsier.yaml

# Generate a bcrypt hash (cost 12) for password "admin" or your chosen password
# Example below uses password "admin"
docker run -d \
  --name tarsier \
  --restart unless-stopped \
  --pid host \
  --read-only \
  --memory 256m \
  --cpus 1.0 \
  --security-opt no-new-privileges:true \
  -p 127.0.0.1:8080:8080 \
  -e TARSIER_CONFIG=/config/tarsier.yaml \
  -e TARSIER_ADMIN_USER=admin \
  -e TARSIER_ADMIN_PASSWORD_HASH='$2a$12$ynZsf5yrFkCAE3V2kOMY6uPVgENIRXSBRxAVF9H8Jb0pKy4j.mztW' \
  -e TARSIER_SESSION_SECRET="$(openssl rand -hex 32)" \
  -e GOMEMLIMIT=200MiB \
  -e HOST_PROC=/host/proc \
  -e HOST_SYS=/host/sys \
  -e HOST_ROOT=/host/root \
  -v "$(pwd)/tarsier.yaml:/config/tarsier.yaml:ro" \
  -v "$(pwd)/tarsier-data:/data" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /proc:/host/proc:ro \
  -v /sys:/host/sys:ro \
  -v /:/host/root:ro,rslave \
  -v /var/log:/host/var/log:ro \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  ghcr.io/dale-tomson/tarsier:latest
```

> **Note for macOS / Windows Docker Desktop:** Omit the `/proc`, `/sys`, `/`, and `/var/log` mounts and leave `HOST_PROC=/proc`, `HOST_SYS=/sys`, and `HOST_ROOT=/`. See [2. Configuration & Operations](02-Configuration-and-Operations#docker-desktop-vs-linux-vps-procfs) for details.

Open `http://127.0.0.1:8080` and sign in with your configured credentials.

---

## 2. Production Docker Compose Deployment

### Option A: Automated Standalone Installer

The repository provides a POSIX installer script at `deploy/install.sh` that downloads the deployment bundle, generates a 32-byte random `TARSIER_SESSION_SECRET`, and initializes `.env` and `tarsier.yaml`:

```bash
# Download bundle into ./tarsier
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash

# Or pin a release version, custom target directory, and auto-start
curl -fsSL https://raw.githubusercontent.com/dale-tomson/Tarsier/main/deploy/install.sh | bash -s -- -v v0.2.0-beta -d /opt/tarsier --start
```

### Option B: Manual `docker-compose.yml` with Resource Envelopes

Use the production Compose definition from [`deploy/docker-compose.yml`](https://github.com/dale-tomson/Tarsier/blob/main/deploy/docker-compose.yml):

```yaml
services:
  tarsier:
    image: ghcr.io/dale-tomson/tarsier:latest
    container_name: tarsier
    restart: unless-stopped
    pid: host
    user: "0:0"
    read_only: true
    mem_limit: 256m
    cpus: 1.0
    security_opt:
      - no-new-privileges:true
    env_file:
      - path: .env
        required: false
    environment:
      TARSIER_CONFIG: /config/tarsier.yaml
      TARSIER_ADMIN_USER: ${TARSIER_ADMIN_USER:-admin}
      TARSIER_ADMIN_PASSWORD_HASH: ${TARSIER_ADMIN_PASSWORD_HASH}
      TARSIER_SESSION_SECRET: ${TARSIER_SESSION_SECRET}
      HOST_PROC: /host/proc
      HOST_SYS: /host/sys
      HOST_ROOT: /host/root
      GOMEMLIMIT: 200MiB
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./tarsier.yaml:/config/tarsier.yaml:ro
      - ./data:/data
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /:/host/root:ro,rslave
      - /var/log:/host/var/log:ro
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=16m
```

#### Generating Production Credentials

1. **Admin Password Hash (`bcrypt`, minimum cost 10, recommended cost 12):**
   ```bash
   htpasswd -nbBC 12 "" "your-strong-password" | tr -d ':\n'
   ```
   Wrap the resulting hash in single quotes inside `.env` (`TARSIER_ADMIN_PASSWORD_HASH='$2a$12$...'`) or double every `$` to `$$` if embedded inline inside `docker-compose.yml`.
2. **Session Secret (`>= 32 bytes`):**
   ```bash
   openssl rand -hex 32
   ```
   If `TARSIER_SESSION_SECRET` is omitted, Tarsier generates an ephemeral 32-byte key on startup, which invalidates all active browser sessions on container restart.

---

## 3. Standalone Binary & Systemd Unit

Release archives for `linux/amd64` and `linux/arm64` are published via GoReleaser, or you can compile the static binary from source (`make build`).

### Installation

```bash
# Verify configuration before starting
/usr/local/bin/tarsier -test-config -config /etc/tarsier/tarsier.yaml

# Print version and build metadata
/usr/local/bin/tarsier -version
```

### Systemd Service Unit (`/etc/systemd/system/tarsier.service`)

```ini
[Unit]
Description=Tarsier Single-Server Observability Engine
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=/etc/tarsier/tarsier.env
Environment="GOMEMLIMIT=200MiB"
Environment="GODEBUG=madvdontneed=1"
ExecStart=/usr/local/bin/tarsier -config /etc/tarsier/tarsier.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s

# Resource & Security Hardening
MemoryMax=256M
CPUQuota=100%
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/tarsier

[Install]
WantedBy=multi-user.target
```

Create `/etc/tarsier/tarsier.env` with `chmod 600` permissions:

```ini
TARSIER_ADMIN_USER=admin
TARSIER_ADMIN_PASSWORD_HASH=$2a$12$replace_with_bcrypt_hash
TARSIER_SESSION_SECRET=replace_with_64_char_hex_secret
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now tarsier
sudo systemctl status tarsier
```

Notice `ExecReload=/bin/kill -HUP $MAINPID`: Tarsier handles `SIGHUP` to hot-reload `tarsier.yaml` without restarting or dropping active collections.

---

## 4. Reverse Proxy Setups

Tarsier intentionally does not terminate TLS natively. Bind Tarsier to `127.0.0.1:8080` and terminate HTTPS at a reverse proxy.

### Caddy

```caddyfile
tarsier.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

### Nginx

```nginx
server {
    listen 443 ssl http2;
    server_name tarsier.example.com;

    ssl_certificate     /etc/letsencrypt/live/tarsier.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tarsier.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

> **Rate Limiter Accuracy:** Tarsier uses `chi/middleware.RealIP` to track failed login attempts per client IP. Ensure your reverse proxy forwards `X-Real-IP` or `X-Forwarded-For` cleanly so brute-force lockouts apply to the actual client IP rather than `127.0.0.1`.

### Traefik (Docker Labels)

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.tarsier.rule=Host(`tarsier.example.com`)"
  - "traefik.http.routers.tarsier.entrypoints=websecure"
  - "traefik.http.routers.tarsier.tls.certresolver=letsencrypt"
  - "traefik.http.services.tarsier.loadbalancer.server.port=8080"
```

---

## 5. Security Hardening Checklist

* **Loopback Binding:** Bind the HTTP port to `127.0.0.1:8080:8080` so unencrypted traffic is never exposed on public network interfaces.
* **HTTPS `Secure` Cookies:** Set `server.base_url` in `tarsier.yaml` to your `https://` URL so session cookies automatically enforce the `Secure` attribute.
* **Read-Only Root & Mounts:** Keep `read_only: true` on the container filesystem, mount `/var/run/docker.sock`, `/proc`, `/sys`, `/`, and `/var/log` with `:ro`, and isolate `/tmp` on a `noexec,nosuid` tmpfs.
* **Secrets Out of YAML:** Keep `TARSIER_ADMIN_PASSWORD_HASH`, `TARSIER_SESSION_SECRET`, `SMTP_PASSWORD`, and `DISCORD_WEBHOOK_URL` in environment variables rather than committing them in `tarsier.yaml`.
* **Request Size & Brute-Force Protection:** Tarsier enforces a `1 MiB` HTTP request body limit (`maxBytesMiddleware`) and locks out IPs that exceed `auth.max_login_attempts` (default `5`) for `auth.lockout` (default `15m`), returning HTTP `429`.
