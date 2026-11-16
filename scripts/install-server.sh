#!/usr/bin/env bash
# Installs xu526-server, the relay and the xu526 tunnel sign-up site, on a
# Linux server (Ubuntu/Debian with systemd). It runs no apps and needs no
# Docker.
#
#   sudo ./install.sh          install or update (keeps relay users and settings)
#
# Settings: /etc/default/xu526-server (created on first install).
# People's computers run xu526 (a separate download).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

[ "$(id -u)" = 0 ] || { echo "please run with sudo" >&2; exit 1; }
ROOT=$(xu_root "$HERE")
CONF=/etc/default/xu526-server

command -v curl >/dev/null || { apt-get update -q && apt-get install -y -q curl ca-certificates; }
xu_ensure_go "$ROOT"
echo "==> installing xu526-server"
xu_get_binary xu526-server /usr/local/bin/xu526-server.new "$ROOT"
mv -f /usr/local/bin/xu526-server.new /usr/local/bin/xu526-server
echo "    $(/usr/local/bin/xu526-server version)"

# Moving from the old relay (xud -relay): same data folder, same settings.
if [ -f /etc/default/xud ] && grep -qE -- '(^|[" ])-relay([" ]|$)' /etc/default/xud; then
  echo "==> replacing the old relay service (xud -relay); relay users, tokens and settings are kept"
  systemctl disable --now xud 2>/dev/null || true
  rm -f /etc/systemd/system/xud.service /usr/local/bin/xud
  [ -f "$CONF" ] || cp /etc/default/xud "$CONF"
  mv /etc/default/xud /etc/default/xud.before-xu526-server
fi

if [ ! -f "$CONF" ]; then
  cat > "$CONF" <<'CFG'
# xu526-server settings. After editing: sudo systemctl restart xu526-server
#
# The relay: shared apps get APP-USER.<domain>; computers connect to xu.<domain>.
# Needs DNS "*.<domain>" → this server, and a wildcard certificate.
XU_FLAGS="-http :80 -domain example.com"
# With HTTPS (recommended; the sign-up site requires it):
# XU_FLAGS="-domain example.com -http :80 -https :443 -tls-cert /etc/letsencrypt/live/example.com/fullchain.pem -tls-key /etc/letsencrypt/live/example.com/privkey.pem"

# The sign-up site (xu526 tunnel): uncomment and fill in. See the README.
# XU_PORTAL_DOMAIN=tunnel.example.com
# XU_PORTAL_NAME="xu526 tunnel"
# XU_PORTAL_DATABASE_FILE=/etc/xu526/portal-db
# XU_PORTAL_SMTP_HOST=smtp.hostinger.com
# XU_PORTAL_SMTP_PORT=465
# XU_PORTAL_SMTP_USER=admin@example.com
# XU_PORTAL_SMTP_PASSWORD_FILE=/etc/xu526/smtp-password
CFG
  chmod 644 "$CONF"
  FIRST=1
fi

if grep -q '^XU_PORTAL_DOMAIN=' "$CONF" && ! command -v psql >/dev/null; then
  echo "note: the sign-up site needs PostgreSQL: sudo apt install -y postgresql (then see the README)"
fi

echo "==> starting the xu526-server service"
install -d /var/lib/xu526
cat > /etc/systemd/system/xu526-server.service <<'UNIT'
[Unit]
Description=xu526-server (relay and xu526 tunnel sign-up site)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
# Settings live in /etc/default/xu526-server (XU_FLAGS and XU_PORTAL_*).
Environment="XU_FLAGS=-http :80"
EnvironmentFile=-/etc/default/xu526-server
ExecStart=/usr/local/bin/xu526-server serve -state /var/lib/xu526/apps.json $XU_FLAGS
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable xu526-server >/dev/null 2>&1
if [ -n "${FIRST:-}" ]; then
  echo
  echo "Installed. Edit $CONF (your domain, HTTPS, the sign-up site), then:"
  echo "  sudo systemctl start xu526-server"
  exit 0
fi
systemctl restart xu526-server
sleep 2
echo
journalctl -u xu526-server -n 12 --no-pager | sed 's/^/  /'
cat <<MSG

  Status        sudo systemctl status xu526-server
  Logs          journalctl -u xu526-server -f
  Users         xu526-server users
  Settings      $CONF   (then: sudo systemctl restart xu526-server)
  Uninstall     sudo $HERE/uninstall.sh
MSG
