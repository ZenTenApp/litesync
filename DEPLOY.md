# Deploying litesync on Ubuntu Server

A self-hosted Brave sync server backed by SQLite3 and an in-memory cache.

---

## Table of Contents

- [Deploying litesync on Ubuntu Server](#deploying-litesync-on-ubuntu-server)
  - [Table of Contents](#table-of-contents)
  - [1. Prerequisites](#1-prerequisites)
  - [2. Download the Release Binary](#2-download-the-release-binary)
    - [Determine your architecture](#determine-your-architecture)
    - [Download and verify](#download-and-verify)
  - [3. Create a Dedicated System User](#3-create-a-dedicated-system-user)
  - [4. Install the Binary and Data Directory](#4-install-the-binary-and-data-directory)
  - [5. Configure systemd Service](#5-configure-systemd-service)
  - [6. Reverse Proxy with Nginx + TLS](#6-reverse-proxy-with-nginx--tls)
    - [6.1 Install Nginx and Certbot](#61-install-nginx-and-certbot)
    - [6.2 Create the Nginx site](#62-create-the-nginx-site)
    - [6.3 Obtain a TLS certificate](#63-obtain-a-tls-certificate)
    - [6.4 Configure CORS for a Web App](#64-configure-cors-for-a-web-app)
    - [6.5 Reload Nginx](#65-reload-nginx)
    - [6.6 Rate limiting](#66-rate-limiting)
    - [6.7 SQLite durability & concurrency](#67-sqlite-durability--concurrency)
    - [6.8 Passwords-only entity policy](#68-passwords-only-entity-policy)
  - [7. Point Brave Browser at the Server](#7-point-brave-browser-at-the-server)
  - [8. Maintenance](#8-maintenance)
    - [View logs](#view-logs)
    - [Update the binary](#update-the-binary)
    - [Backup the database](#backup-the-database)
    - [Uninstall](#uninstall)

---

## 1. Prerequisites

| Requirement               | Notes                               |
| ------------------------- | ----------------------------------- |
| Ubuntu 22.04 LTS or later | 20.04 works too                     |
| `curl`                    | Pre-installed on most Ubuntu images |
| `systemd`                 | Already present                     |
| `nginx`                   | Required for TLS termination        |
| `certbot`                 | Required for TLS certificate        |

No Go installation or compilation is needed on the server.

---

## 2. Download the Release Binary

Releases are built automatically by the GitHub Actions workflow
(`.github/workflows/release.yml`) and published to the
[Releases page](https://github.com/ZenTenApp/litesync/releases).

### Architecture

The release binary is currently provided for `amd64` servers only. Verify that your
server uses that architecture:

```bash
dpkg --print-architecture
# amd64
```

### Download and verify

```bash
# Set the version you want to install (check the Releases page for the latest)
VERSION="v1.0.0"
ARCH="amd64"

# Download binary and checksum file
curl -fsSL \
  "https://github.com/ZenTenApp/litesync/releases/download/${VERSION}/litesync-linux-${ARCH}" \
  -o /tmp/litesync

curl -fsSL \
  "https://github.com/ZenTenApp/litesync/releases/download/${VERSION}/litesync-linux-${ARCH}.sha256" \
  -o /tmp/litesync.sha256

# Verify the checksum
cd /tmp
sha256sum --check litesync.sha256
# litesync-linux-amd64: OK
```

---

## 3. Create a Dedicated System User

Running the service as a non-root, no-login user limits the blast radius of any vulnerability.

```bash
sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  litesync
```

---

## 4. Install the Binary and Data Directory

```bash
# Install binary
sudo install -o root -g root -m 0755 /tmp/litesync /usr/local/bin/litesync

# Create data directory owned by the service user
sudo mkdir -p /var/lib/litesync
sudo chown litesync:litesync /var/lib/litesync
sudo chmod 0750 /var/lib/litesync
```

---

## 5. Configure systemd Service

Create the unit file:

```bash
sudo tee /etc/systemd/system/litesync.service > /dev/null << 'EOF'
[Unit]
Description=litesync – self-hosted Brave sync server
Documentation=https://github.com/ZenTenApp/litesync
After=network.target
Wants=network.target

[Service]
Type=simple
User=litesync
Group=litesync

# Binary and flags – bind to localhost only; Nginx handles external traffic
ExecStart=/usr/local/bin/litesync \
    -bind 127.0.0.1:8295 \
    -db /var/lib/litesync/litesync.sqlite

# Restart policy
Restart=on-failure
RestartSec=5s

# Logging – journal captures stdout/stderr automatically
StandardOutput=journal
StandardError=journal
SyslogIdentifier=litesync

# Hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/litesync
CapabilityBoundingSet=
AmbientCapabilities=

# Environment (optional – sets log level)
Environment=ENV=production

[Install]
WantedBy=multi-user.target
EOF
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now litesync
```

Check that it is running:

```bash
sudo systemctl status litesync
# ● litesync.service - litesync – self-hosted Brave sync server
#      Loaded: loaded (/etc/systemd/system/litesync.service; enabled; ...)
#      Active: active (running) since ...

# Tail live logs
sudo journalctl -u litesync -f
```

Verify the health endpoint (localhost only):

```bash
curl -s http://127.0.0.1:8295/
# OK
```

---

## 6. Reverse Proxy with Nginx + TLS

Nginx terminates TLS and forwards requests to litesync on `127.0.0.1:8295`.

### 6.1 Install Nginx and Certbot

```bash
sudo apt update
sudo apt install -y nginx certbot python3-certbot-nginx
```

### 6.2 Create the Nginx site

```bash
sudo tee /etc/nginx/sites-available/default > /dev/null << NGINX_EOF
server {
    server_name sync.example.com;   # <-- replace with your domain

    location / {
        proxy_pass http://127.0.0.1:8295;

        proxy_http_version 1.1;
        proxy_read_timeout 70s;

        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";

        proxy_set_header X-Real-IP \$remote_addr;
    }
}
NGINX_EOF
```

Test the Nginx config:

```bash
sudo nginx -t
```

### 6.3 Obtain a TLS certificate

```bash
sudo certbot --nginx -d "$Replace_with_your_domain" --non-interactive --agree-tos -m "$Replace_with_your_email"
```

### 6.4 Configure CORS for a Web App

If a browser-based web app needs to call litesync from a different origin, configure
an explicit allowlist in Nginx. This handles both CORS response headers and the
browser's `OPTIONS` preflight request (commonly triggered by the `Authorization`
header).

Create `/etc/nginx/conf.d/litesync-cors.conf`:

```bash
sudo tee /etc/nginx/conf.d/litesync-cors.conf > /dev/null << 'NGINX_EOF'
# Only origins listed here receive CORS response headers.
map $http_origin $cors_allow_origin {
    default "";
    "https://app.example.com" $http_origin;
    # "https://staging.app.example.com" $http_origin;
}
NGINX_EOF
```

Replace `https://app.example.com` with the exact origin of the calling web app.
An origin includes the scheme, hostname, and port when applicable; do not include
a trailing slash.

Then add the following directives inside the `server { ... }` block created above:

```nginx
# Returned only when the request Origin is in the allowlist above.
add_header Access-Control-Allow-Origin $cors_allow_origin always;
add_header Access-Control-Allow-Methods "POST, OPTIONS" always;
add_header Access-Control-Allow-Headers "Authorization, Content-Type" always;
add_header Vary "Origin" always;
```

Add this preflight handler at the beginning of the existing `location / { ... }`
block, before `proxy_pass`:

```nginx
# Respond to CORS preflight requests without sending them to litesync.
if ($request_method = OPTIONS) {
    return 204;
}
```

For example, the relevant parts of the site configuration should look like this:

```nginx
server {
    server_name sync.example.com;

    # These headers are returned for both OPTIONS preflight and actual POST responses.
    add_header Access-Control-Allow-Origin $cors_allow_origin always;
    add_header Access-Control-Allow-Methods "POST, OPTIONS" always;
    add_header Access-Control-Allow-Headers "Authorization, Content-Type, BraveServiceKey, Cache-Control, Pragma" always;
    add_header Access-Control-Max-Age 86400 always;
    add_header Vary "Origin" always;

    location / {
        if ($request_method = OPTIONS) {
            return 204;
        }

        proxy_pass http://127.0.0.1:8295;
        # ...retain the existing proxy_* directives...
    }
}
```

Do not use `Access-Control-Allow-Origin: *` for a sync service. If the sync
endpoint uses HTTPS, the calling web app must also use HTTPS to avoid mixed-content
blocking.

You can verify the preflight response after reloading Nginx:

```bash
curl -i -X OPTIONS 'https://sync.example.com/litesync/command/' \
  -H 'Origin: https://app.example.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: authorization,content-type'
```

The response should include `Access-Control-Allow-Origin` with the allowed origin.

### 6.5 Reload Nginx

```bash
sudo nginx -t
sudo systemctl reload nginx
```

Certbot will automatically renew the certificate. Verify the renewal timer:

```bash
sudo systemctl status certbot.timer
```

---

## 6.6 Rate limiting

_Applied automatically by `deploy.sh` (it writes `/etc/nginx/conf.d/litesync-limit.conf`
and adds `limit_req`/`limit_conn` to the site). Two layers: nginx first, then the
application._

### 6.6.1 Layer 1 — Nginx (external IP + connection flood shield)

`deploy.sh` installs two zones (http context) and applies them to the site:

```nginx
# /etc/nginx/conf.d/litesync-limit.conf
limit_req_zone $binary_remote_addr zone=brave_req:10m rate=20r/s;
limit_conn_zone $binary_remote_addr zone=brave_conn:10m;
```

```nginx
# within the litesync server block
limit_conn brave_conn 300;
location / {
    limit_req zone=brave_req burst=60 nodelay;
    # ...proxy directives...
}
```

- `limit_req` caps each source IP at ~20 requests/sec baseline, absorbing a burst
  of 60 instantly (`nodelay`) before excess requests get HTTP `503` + a
  `Retry-After` header. A generous allowance for one Brave client (whose initial
  sync is chatty) while bounding floods.
- `limit_conn` caps each source IP at 300 concurrent connections, stopping one
  host from exhausting nginx worker connections with hundreds of parallel syncs.
- Because litesync binds `127.0.0.1` behind nginx, `$binary_remote_addr` is the
  true client IP; do **not** key limits on `X-Forwarded-For` without a
  trusted-proxy allowlist (it is spoofable).

**Why this matters:** stress tests showed litesync's single SQLite file fails
with `database is locked` under ~90 concurrent write clients, and older binaries
have no app-level rate limiting. nginx shedding work before it reaches the DB is
the first line of defense.

**Caveat:** nginx limiting is coarse (per-IP). An attacker who can rotate BIP39
seeds / client identities defeats per-IP limits — which is why the app-layer
limiter below exists.

### 6.6.2 Layer 2 — litesync app (per client_id + per IP token bucket)

The server now ships a built-in token-bucket rate limiter, enabled by default.
It runs **after** Brave sync authentication, so it enforces a real per-identity
(Brave `client_id`, i.e. the sync seed) limit in addition to a source-IP limit —
the layer that stops seed-rotation abuse.

Defaults (overridable by env vars on the `systemd` unit):

| Env var | Default | Meaning |
|---|---|---|---|
| `LITESYNC_IP_RATE` | `30` | req/sec per source IP |
| `LITESYNC_IP_BURST` | `90` | burst per source IP |
| `LITESYNC_CLIENT_RATE` | `5` | req/sec per Brave `client_id` (seed) |
| `LITESYNC_CLIENT_BURST` | `20` | burst per Brave `client_id` |

Excess requests get `HTTP 429 Too Many Requests` + `Retry-After: 1` and are
logged (`rate limited by IP` / `by client_id`). A `rate <= 0` disables that
dimension (useful for local development). Note: memory is bounded because idle
keys are swept after 10 minutes, so endlessly rotating seeds cannot exhaust
memory.

To tune them, edit the unit and add an override:

```bash
sudo systemctl edit litesync
```

```ini
[Service]
Environment=LITESYNC_CLIENT_RATE=8
Environment=LITESYNC_CLIENT_BURST=30
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl restart litesync
```

---

## 6.7 SQLite durability & concurrency

`internal/sqlite_datastore.go` now opens the database with WAL journal mode and a
busy timeout, applied to **every** pooled connection via the DSN (not one-off
PRAGMA calls, which would only affect a single connection):

- `journal_mode=WAL` — readers don't block writers and vice-versa.
- `busy_timeout=5000` — concurrent commits **wait** up to 5s for the lock instead
  of failing instantly with `SQLITE_BUSY` (`database is locked`). This is the
  root-cause fix for the write-contention errors seen under load.
- `synchronous=NORMAL` — safe under WAL, reduces fsync frequency for commits.

**Measured impact** (local, rate limiting disabled): concurrent write throughput
jumped from ~45–50 ops/s with ~13% `database is locked` failures at ~90 clients
to **~476 ops/s with 0 errors at 120 concurrent writers**; write p99 latency
dropped from ~3.7s to ~293ms. WAL mode requires read/write on the data directory;
already satisfied by the systemd unit (`ReadWritePaths=/var/lib/litesync`).

A new deployment gets these for free. For an **existing** deployment, restarting
litesync after this change will migrate the journal to WAL automatically (the
`-wal`/`-shm` sidecar files appear next to `litesync.sqlite`).

---

## 6.8 Passwords-only entity policy

This server is intentionally a **passwords-only** Brave sync store. Every COMMIT
is validated before it touches the database and rejected if it violates the
policy. Two data types are allowed:

- **PASSWORD** (data type `45873`) — the product surface we store.
- **NIGORI** (data type `47745`) — always allowed because every client creates
  this encryption-settings entity on `connect()`; blocking it would stop clients
  from initialising their chain at all.

Everything else — bookmarks, history, autofill, 2FA/authenticator, preferences,
etc. — is **rejected on commit** with `HTTP 400`. Read requests (GetUpdates) for
other types are not blocked; they simply return whatever exists (no unrelated
entities are ever stored, so that is nothing).

An optional **size cap** bounds each stored password entity's marshalled
`Specifics` blob at **1KB by default**, configurable via env on the systemd unit:

| Env var | Default | Meaning |
|---|---|---|---|
| `LITESYNC_MAX_PASSWORD_SIZE` | `1024` | max bytes per password entity's Specifics; `-1` disables |

Oversize entities are rejected with `HTTP 400` (e.g.
`rejected: password entity too large (6133 bytes > 1024 byte limit)`). Note: a
password synced with a large note field can exceed the cap; raise it if you need
notes. To tune:

```bash
sudo systemctl edit litesync
```

```ini
[Service]
Environment=LITESYNC_MAX_PASSWORD_SIZE=2048
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart litesync
```

The policy lives in `internal/entity_policy.go` and is wired into the router
before the sync controller, so rejected data never reaches the database.

---

## 7. Point Brave Browser at the Server

```bash
brave-browser --sync-url=https://sync.example.com/litesync
```

> The `--sync-url` flag must be passed **every time** Brave is launched, or set it
> in a desktop launcher / shell alias.

---

## 8. Maintenance

### View logs

```bash
# Last 100 lines
sudo journalctl -u litesync -n 100

# Follow live
sudo journalctl -u litesync -f

# Since last boot
sudo journalctl -u litesync -b
```

### Update the binary

```bash
# Set the new version
VERSION="v1.1.0"
ARCH="amd64"

# Download and verify
curl -fsSL \
  "https://github.com/ZenTenApp/litesync/releases/download/${VERSION}/litesync-linux-${ARCH}" \
  -o /tmp/litesync

curl -fsSL \
  "https://github.com/ZenTenApp/litesync/releases/download/${VERSION}/litesync-linux-${ARCH}.sha256" \
  -o /tmp/litesync.sha256

cd /tmp && sha256sum --check litesync.sha256

# Install
sudo systemctl stop litesync
sudo install -o root -g root -m 0755 /tmp/litesync /usr/local/bin/litesync
sudo systemctl start litesync
sudo systemctl status litesync
```

### Backup the database

The entire state is a single SQLite file:

```bash
# Safe online backup using SQLite's backup API via the sqlite3 CLI
sudo -u litesync sqlite3 /var/lib/litesync/litesync.sqlite \
    ".backup /var/lib/litesync/litesync.sqlite.bak"

# Or simply stop the service, copy, restart
sudo systemctl stop litesync
sudo cp /var/lib/litesync/litesync.sqlite /var/backups/litesync-$(date +%F).sqlite
sudo systemctl start litesync
```

### Uninstall

```bash
sudo systemctl disable --now litesync
sudo rm /etc/systemd/system/litesync.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/litesync
sudo rm -rf /var/lib/litesync
sudo userdel litesync
```
