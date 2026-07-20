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
    - [6.4 Reload Nginx](#64-reload-nginx)
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

### Determine your architecture

```bash
dpkg --print-architecture
# amd64  →  use litesync-linux-amd64
# arm64  →  use litesync-linux-arm64
```

### Download and verify

```bash
# Set the version you want to install (check the Releases page for the latest)
VERSION="v1.0.0"
ARCH="amd64"   # or arm64

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

### 6.4 Reload Nginx

```bash
sudo systemctl reload nginx
```

Certbot will automatically renew the certificate. Verify the renewal timer:

```bash
sudo systemctl status certbot.timer
```

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
ARCH="amd64"   # or arm64

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
