#!/usr/bin/env bash
# Installs xu526 on a Linux computer or server (Ubuntu/Debian, also WSL2):
# Docker, the xu526 program (also as `xu`), and its background service.
#
#   sudo ./install-linux.sh          install or update (keeps your apps)
#
# The relay and the xu526 tunnel sign-up site are a different program:
# xu526-server (its own download and installer).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

[ "$(id -u)" = 0 ] || { echo "please run with sudo" >&2; exit 1; }
ROOT=$(xu_root "$HERE")

# This machine running the old relay? That's xu526-server now.
if [ -f /etc/default/xud ] && grep -qE -- '(^|[" ])-relay([" ]|$)' /etc/default/xud; then
  echo "This machine runs the relay (xud -relay). Install xu526-server here instead:" >&2
  echo "  sudo ./install.sh   (from the xu526-server download)" >&2
  exit 1
fi

if ! command -v docker >/dev/null; then
  if command -v apt-get >/dev/null; then
    echo "==> installing Docker"
    apt-get update -q
    apt-get install -y -q docker.io git curl ca-certificates
  else
    echo "note: Docker isn't installed; install it to deploy images (apps from commands still need -allow-process)"
  fi
fi
command -v systemctl >/dev/null && systemctl enable --now docker 2>/dev/null || true

xu_ensure_go "$ROOT"
echo "==> installing xu526"
xu_get_binary xu526 /usr/local/bin/xu526.new "$ROOT"
mv -f /usr/local/bin/xu526.new /usr/local/bin/xu526
ln -sf xu526 /usr/local/bin/xu # the short name the commands use
echo "    $(/usr/local/bin/xu526 version)"

# Move over from the old two-program install (xud + xu).
if [ -f /etc/systemd/system/xud.service ]; then
  echo "==> replacing the old xud service (apps and settings are kept)"
  systemctl disable --now xud 2>/dev/null || true
  rm -f /etc/systemd/system/xud.service
  if [ -f /etc/default/xud ] && [ ! -f /etc/default/xu526 ]; then
    cp /etc/default/xud /etc/default/xu526
  fi
fi
rm -f /usr/local/bin/xud

echo "==> starting the xu526 service"
install -d /var/lib/xu526
cat > /etc/systemd/system/xu526.service <<'UNIT'
[Unit]
Description=xu526 (runs and shares your apps)
After=docker.service network-online.target
Wants=network-online.target docker.service

[Service]
# Override in /etc/default/xu526, e.g. XU_FLAGS="-http :80 -domain apps.example.com"
Environment="XU_FLAGS=-http :80 -domain auto"
EnvironmentFile=-/etc/default/xu526
ExecStart=/usr/local/bin/xu526 daemon -state /var/lib/xu526/apps.json $XU_FLAGS
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable xu526 >/dev/null 2>&1
systemctl restart xu526

for _ in $(seq 1 15); do
  curl -fsS --noproxy '*' http://127.0.0.1:7070/v1/node >/dev/null 2>&1 && break
  sleep 1
done
echo
if curl -fsS --noproxy '*' http://127.0.0.1:7070/v1/node >/dev/null 2>&1; then
  echo "xu526 is running."
else
  echo "xu526 didn't answer yet; see: journalctl -u xu526 -n 30"
fi
cat <<MSG

  Dashboard     http://127.0.0.1:7070
  Go public     Dashboard → Settings → Connect with xu526 tunnel (or: xu tunnel login)
  Deploy        xu build ./my-app --deploy my-app
  Logs          journalctl -u xu526 -f
  Uninstall     sudo $HERE/uninstall.sh   (--purge also deletes apps and data)
MSG
