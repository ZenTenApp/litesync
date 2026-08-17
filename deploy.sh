#!/usr/bin/env bash
# Interactive installer for litesync on Ubuntu/Debian.
# Copy this file to the server, then run: bash deploy.sh
set -Eeuo pipefail

REPOSITORY="ZenTenApp/litesync"
SERVICE="litesync"
SERVICE_USER="litesync"
INSTALL_PATH="/usr/local/bin/litesync"
DATA_DIR="/var/lib/litesync"
DB_PATH="$DATA_DIR/litesync.sqlite"
UNIT_PATH="/etc/systemd/system/$SERVICE.service"
NGINX_SITE="/etc/nginx/sites-available/$SERVICE"
NGINX_ENABLED="/etc/nginx/sites-enabled/$SERVICE"
CORS_CONFIG="/etc/nginx/conf.d/$SERVICE-cors.conf"

red() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
info() { printf '\n==> %s\n' "$*"; }
die() { red "Error: $*"; exit 1; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1"; }

cleanup() {
  rm -rf "${DOWNLOAD_DIR:-}" 
}
trap cleanup EXIT

[[ "${EUID:-$(id -u)}" -eq 0 ]] || die "Run this script as root (for example: sudo bash deploy.sh)."
[[ -r /etc/os-release ]] || die "This script requires a Debian/Ubuntu-style system."
. /etc/os-release
[[ "${ID:-}" == "ubuntu" || "${ID:-}" == "debian" ]] || die "This script supports Ubuntu or Debian."
require_command apt-get
require_command curl
require_command sha256sum
require_command systemctl

printf '%s\n' "litesync interactive deployment"
printf '%s\n' "This installs litesync, systemd, Nginx, and a Let's Encrypt TLS certificate."
printf '%s\n\n' "Before continuing, point the domain's DNS A/AAAA record to this server and allow inbound ports 80 and 443."

read -r -p "Release version to install (for example v1.0.0): " VERSION
[[ "$VERSION" =~ ^v[0-9][0-9A-Za-z._-]*$ ]] || die "Version must begin with 'v' (for example v1.0.0)."

[[ "$(dpkg --print-architecture)" == "amd64" ]] || die "This installer supports amd64 servers only."
ARCH="amd64"
printf 'Using architecture: %s\n' "$ARCH"

read -r -p "Public sync domain (for example sync.example.com): " DOMAIN
[[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] && [[ "$DOMAIN" != *".."* ]] || die "Enter a valid hostname, without http:// or a path."
DOMAIN="${DOMAIN,,}"

read -r -p "Email address for Let's Encrypt renewal notices: " EMAIL
[[ "$EMAIL" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || die "Enter a valid email address."

DEFAULT_CORS_ORIGIN="https://litesync.nostr.box"
read -r -p "Allow a web app to use this service via CORS? [Y/n]: " ENABLE_CORS
ENABLE_CORS="${ENABLE_CORS,,}"
CORS_ORIGIN=""
if [[ -z "$ENABLE_CORS" || "$ENABLE_CORS" == "y" || "$ENABLE_CORS" == "yes" ]]; then
  read -r -p "Allowed web-app origin [$DEFAULT_CORS_ORIGIN]: " CORS_ORIGIN
  CORS_ORIGIN="${CORS_ORIGIN:-$DEFAULT_CORS_ORIGIN}"
  [[ "$CORS_ORIGIN" =~ ^https?://[^/[:space:]]+(:[0-9]+)?$ ]] || die "Origin must include http(s)://, hostname, optional port, and no trailing slash."
  ENABLE_CORS="yes"
else
  ENABLE_CORS="no"
fi

printf '\nInstallation summary:\n  Version: %s\n  Architecture: %s\n  Domain: https://%s\n  CORS origin: %s\n' \
  "$VERSION" "$ARCH" "$DOMAIN" "${CORS_ORIGIN:-disabled}"
read -r -p "Continue? [y/N]: " CONFIRM
[[ "${CONFIRM,,}" == "y" || "${CONFIRM,,}" == "yes" ]] || { echo "Cancelled."; exit 0; }

info "Installing Nginx and Certbot"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y nginx certbot python3-certbot-nginx

info "Downloading and verifying litesync $VERSION"
DOWNLOAD_DIR="$(mktemp -d)"
ASSET="litesync-linux-$ARCH"
BASE_URL="https://github.com/$REPOSITORY/releases/download/$VERSION"
curl -fL --retry 3 --proto '=https' --tlsv1.2 "$BASE_URL/$ASSET" -o "$DOWNLOAD_DIR/$ASSET"
curl -fL --retry 3 --proto '=https' --tlsv1.2 "$BASE_URL/$ASSET.sha256" -o "$DOWNLOAD_DIR/$ASSET.sha256"
(
  cd "$DOWNLOAD_DIR"
  sha256sum --check "$ASSET.sha256"
)

info "Creating service account and data directory"
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DATA_DIR"
install -o root -g root -m 0755 "$DOWNLOAD_DIR/$ASSET" "$INSTALL_PATH"

info "Writing systemd service"
cat > "$UNIT_PATH" <<EOF
[Unit]
Description=litesync – self-hosted Brave sync server
Documentation=https://github.com/$REPOSITORY
After=network.target
Wants=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$INSTALL_PATH -bind 127.0.0.1:8295 -db $DB_PATH
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR
CapabilityBoundingSet=
AmbientCapabilities=
Environment=ENV=production

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now "$SERVICE"

info "Checking litesync health endpoint"
for _ in {1..10}; do
  if curl -fsS http://127.0.0.1:8295/ >/dev/null; then break; fi
  sleep 1
done
curl -fsS http://127.0.0.1:8295/ >/dev/null || { journalctl -u "$SERVICE" -n 50 --no-pager; die "litesync did not become healthy."; }

info "Writing Nginx configuration"
if [[ "$ENABLE_CORS" == "yes" ]]; then
  cat > "$CORS_CONFIG" <<EOF
# Only this exact browser origin receives CORS headers.
map \$http_origin \$cors_allow_origin {
    default "";
    "$CORS_ORIGIN" \$http_origin;
}
EOF
  CORS_HEADERS='    add_header Access-Control-Allow-Origin $cors_allow_origin always;
    add_header Access-Control-Allow-Methods "POST, OPTIONS" always;
    add_header Access-Control-Allow-Headers "Authorization, Content-Type, BraveServiceKey, Cache-Control, Pragma" always;
    add_header Access-Control-Max-Age 86400 always;
    add_header Vary "Origin" always;'
  PREFLIGHT='        if ($request_method = OPTIONS) {
            return 204;
        }
'
else
  rm -f "$CORS_CONFIG"
  CORS_HEADERS=""
  PREFLIGHT=""
fi

cat > "$NGINX_SITE" <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;
$CORS_HEADERS

    location / {
$PREFLIGHT        proxy_pass http://127.0.0.1:8295;
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
EOF
ln -sfn "$NGINX_SITE" "$NGINX_ENABLED"
rm -f /etc/nginx/sites-enabled/default
nginx -t
systemctl reload nginx

info "Requesting TLS certificate"
certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos -m "$EMAIL" --redirect
nginx -t
systemctl reload nginx

printf '\n'
green "Deployment complete."
printf 'Sync URL: https://%s/litesync\n' "$DOMAIN"
printf 'Brave launch command: brave-browser --sync-url=https://%s/litesync\n' "$DOMAIN"
printf 'Service status: systemctl status %s\nLogs: journalctl -u %s -f\n' "$SERVICE" "$SERVICE"
