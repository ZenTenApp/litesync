#!/usr/bin/env bash
# Run on the TARGET host. Assumes the new binary is already at /tmp/litesync-new.
# Backs up the current install for rollback, installs the new binary, restarts,
# and verifies. Idempotent; safe to re-run.
set -Eeuo pipefail

SRC="/tmp/litesync-new"
BIN="/usr/local/bin/litesync"
STAMP="$(date +%Y%m%d-%H%M%S)"

echo "==> Verifying source binary exists: $SRC"
[ -f "$SRC" ] || { echo "ERROR: $SRC not found (scp it first)"; exit 1; }
[ -x "$SRC" ] || chmod +x "$SRC"

echo "==> Snapshotting current binary for rollback"
if [ -f "$BIN" ]; then
  cp -a "$BIN" "${BIN}.bak-${STAMP}"
  echo "    backed up to ${BIN}.bak-${STAMP}"
fi

echo "==> Stopping service"
systemctl stop litesync 2>/dev/null || true

echo "==> Installing new binary"
install -o root -g root -m 0755 "$SRC" "$BIN"

echo "==> Starting service"
systemctl start litesync
systemctl daemon-reload 2>/dev/null || true

echo "==> Verifying (up to 15s)"
for i in $(seq 1 15); do
  health="$(curl -fsS -m2 http://127.0.0.1:8295/ 2>/dev/null || echo FAIL)"
  if [ "$health" != "FAIL" ]; then break; fi
  sleep 1
done
echo "    health: $(curl -fsS -m2 http://127.0.0.1:8295/ 2>/dev/null || echo DOWN)"

echo "==> Service state"
systemctl is-active litesync
systemctl status litesync --no-pager -n 3 2>/dev/null | tail -6

echo "==> Recent logs"
journalctl -u litesync -n 6 --no-pager 2>/dev/null | grep -iE "started|sync_url|running|version" | tail -4

echo "==> DONE"