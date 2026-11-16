#!/usr/bin/env bash
# Builds the downloads into dist/:
#   xu526-<version>.zip          for computers: xu526 for Linux and macOS
#                                (amd64 and arm64), install scripts
#   xu526-<version>-windows.zip  for Windows 10/11 (amd64 and arm64)
#   xu526-server-<version>.zip   for the relay: xu526-server for Linux, install script
# Each holds the ready-built programs, the install scripts and the license;
# the source is the repository itself.
set -euo pipefail
cd "$(dirname "$0")/.."
VER=$(cat VERSION)
# The shared xu526 GitHub App's client ID (public, not a secret) goes into
# the program so Connect GitHub works with no setup:
#   XU_SHARED_CLIENT_ID=Iv23li... scripts/package.sh
GH=""
[ -n "${XU_SHARED_CLIENT_ID:-}" ] && GH="-X xu526/internal/ghapp.SharedClientID=$XU_SHARED_CLIENT_ID"
OUT=dist
rm -rf "$OUT/stage"
mkdir -p "$OUT/stage"
build() { # NAME OS ARCH DIR [OUTPUT-NAME EXTRA-LDFLAGS]
  CGO_ENABLED=0 GOOS=$2 GOARCH=$3 go build -trimpath -ldflags "-s -w -X xu526/internal/version.Version=$VER $GH ${6:-}" -o "$4/bin/$2-$3/${5:-$1}" "./cmd/$1"
}

C="$OUT/stage/xu526-$VER"
mkdir -p "$C"
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do build xu526 "${t%/*}" "${t#*/}" "$C"; done
cp scripts/lib.sh scripts/install-linux.sh scripts/install-macos.sh scripts/uninstall.sh "$C/"
cp VERSION "$C/"
cat > "$C/README.txt" <<TXT
xu526 $VER: run and share apps from your own computer or server.

Linux / WSL2:   sudo ./install-linux.sh
macOS:          ./install-macos.sh        (as yourself, not sudo)

Then open the dashboard at http://127.0.0.1:7070 and choose
Settings → Connect with xu526 tunnel to give your apps public addresses.

The program is xu526 (also installed as xu). Remove it with ./uninstall.sh.
Guide and source: https://github.com/Tomiphred/xu526
TXT
cp LICENSE "$C/"

W="$OUT/stage/xu526-$VER-windows"
mkdir -p "$W"
for a in amd64 arm64; do
  build xu526 windows "$a" "$W" xu526.exe
  # The same program without a console window, for the background service.
  build xu526 windows "$a" "$W" xu526-service.exe "-H=windowsgui"
done
cp scripts/install-windows.ps1 scripts/uninstall-windows.ps1 scripts/install.cmd scripts/uninstall.cmd VERSION LICENSE "$W/"
printf 'xu526 %s for Windows 10/11\r\n\r\nInstall:    double-click install.cmd  (it also installs Podman, the container engine)\r\nNeeds:      WSL2. If it is off, the installer says how to turn it on (one command, then restart)\r\nEngine:     install.cmd -Engine docker uses Docker Desktop instead\r\nDashboard:  http://127.0.0.1:7070  (Start menu: xu526 dashboard)\r\nGo public:  Dashboard -> Settings -> Connect with xu526 tunnel\r\nCommands:   xu526 (also xu) in a new terminal, e.g. xu build . --deploy myapp\r\nRemove:     double-click uninstall.cmd\r\n' "$VER" > "$W/README.txt"

S="$OUT/stage/xu526-server-$VER"
mkdir -p "$S"
for t in linux/amd64 linux/arm64; do build xu526-server "${t%/*}" "${t#*/}" "$S"; done
cp scripts/lib.sh scripts/uninstall.sh "$S/"
cp scripts/install-server.sh "$S/install.sh"
cp VERSION "$S/"
cat > "$S/README.txt" <<TXT
xu526-server $VER: the relay and the xu526 tunnel sign-up site.
Runs on a Linux server with a public IP (Ubuntu/Debian, systemd). No Docker needed.

Install or update:   sudo ./install.sh
Settings:            /etc/default/xu526-server   (then: sudo systemctl restart xu526-server)
People with tokens:  xu526-server users

Setup (domain, certificate, PostgreSQL, email): see "Running your own relay"
in the guide at https://github.com/Tomiphred/xu526. Remove it with ./uninstall.sh.
TXT
cp LICENSE "$S/"

(cd "$OUT/stage" && rm -f "../xu526-$VER.zip" "../xu526-$VER-windows.zip" "../xu526-server-$VER.zip" &&
  zip -qr "../xu526-$VER.zip" "xu526-$VER" && zip -qr "../xu526-$VER-windows.zip" "xu526-$VER-windows" && zip -qr "../xu526-server-$VER.zip" "xu526-server-$VER")
rm -rf "$OUT/stage"
ls -la "$OUT"/*.zip
