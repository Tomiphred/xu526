#!/usr/bin/env bash
# One-step xu526 install for macOS 13+ (Apple Silicon or Intel).
#
#   ./scripts/install-macos.sh               install or update
#   ./scripts/uninstall.sh                   to remove it again (see --help)
#
# Doesn't use Homebrew: it downloads the official prebuilt Colima, Lima and
# Docker CLI (and Go when yours is missing or too old) into ~/.local, checks
# them against their published SHA-256 sums, installs xu526 (also as `xu`),
# starts Colima and the xu526 service at every login and adds ~/.local/bin to
# your PATH.
# Run it as yourself, not with sudo. Safe to run again to update.
#
# Settings (environment variables):
#   PREFIX=~/.local          where everything goes
#   XU_FLAGS="..."           extra flags for `xu526 daemon`, e.g. "-host-builds -domain apps.example.com"
#   COLIMA_CPU=2 COLIMA_MEMORY=4 COLIMA_DISK=60
#                            VM size (CPUs, GB, GB); used when the VM is first created
set -euo pipefail

COLIMA_VERSION=0.10.3
LIMA_VERSION=2.2.1
DOCKER_VERSION=29.9.3
GO_VERSION=1.24.7 # installed only when there's no Go 1.24+ already

PREFIX=${PREFIX:-$HOME/.local}
BIN="$PREFIX/bin"
XU_FLAGS=${XU_FLAGS:-}
COLIMA_CPU=${COLIMA_CPU:-2}
COLIMA_MEMORY=${COLIMA_MEMORY:-4}
COLIMA_DISK=${COLIMA_DISK:-60}
LABEL=dev.xu526.daemon
OLD_LABEL=dev.xu526.xud # before xu526 was one program
COLIMA_LABEL=dev.xu526.colima
AGENTS="$HOME/Library/LaunchAgents"
PLIST="$AGENTS/$LABEL.plist"
COLIMA_PLIST="$AGENTS/$COLIMA_LABEL.plist"
STATE="$HOME/Library/Application Support/xu526"
LOGS="$HOME/Library/Logs/xu526"
SOCK="$HOME/.colima/default/docker.sock"
GUI="gui/$(id -u)"
MARK="# added by xu526 install-macos.sh"

STEP="starting"
say() { STEP="$*"; printf '\n==> %s\n' "$*"; }
die() { printf '\nerror: %s\n' "$*" >&2; exit 1; }
trap 'rc=$?; [ $rc -ne 0 ] && printf "\nerror: the step \"%s\" failed (exit %d). Fix what the lines above say, then run this script again.\n" "$STEP" $rc >&2' EXIT

[ "$(uname -s)" = Darwin ] || die "this script is for macOS; on Linux use install-linux.sh"
[ "$(id -u)" != 0 ] || die "run this as yourself, not with sudo"
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"
ROOT=$(xu_root "$HERE") || die "unzip the whole download and run this script from inside it"
cd "$ROOT"
macos=$(sw_vers -productVersion)
[ "${macos%%.*}" -ge 13 ] || die "macOS 13 (Ventura) or newer is needed; this Mac has $macos"
case "$(uname -m)" in
  arm64) ARCH=arm64 GOARCH=arm64 DOCKER_ARCH=aarch64 ;;
  x86_64) ARCH=x86_64 GOARCH=amd64 DOCKER_ARCH=x86_64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac
export PATH="$BIN:$PREFIX/go/bin:$PATH"

xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
unload() { launchctl bootout "$GUI/$1" 2>/dev/null || true; }

# ---------- uninstall (kept for old instructions; see scripts/uninstall.sh) ----------
case "${1:-}" in
  --uninstall) trap - EXIT; exec bash scripts/uninstall.sh ;;
  --purge) trap - EXIT; exec bash scripts/uninstall.sh --purge ;;
esac

# ---------- downloads ----------
TMP=$(mktemp -d)
trap 'rc=$?; rm -rf "$TMP"; [ $rc -ne 0 ] && printf "\nerror: the step \"%s\" failed (exit %d). Fix what the lines above say, then run this script again.\n" "$STEP" $rc >&2' EXIT

fetch() { # url file
  curl -fL --retry 3 --progress-bar -o "$2" "$1" || die "download failed: $1"
}
checksum() { # file expected-sha256
  local got
  got=$(shasum -a 256 "$1" | awk '{print $1}')
  [ "$got" = "$2" ] || die "checksum mismatch for $(basename "$1") (got $got, expected $2); not installing it"
}
version_of() { "$@" 2>/dev/null | head -n1 || true; }
mkdir -p "$BIN" "$PREFIX/share" "$PREFIX/share/xu526"
# What this script installed besides xu/xud, so uninstall.sh --all removes
# exactly those files and nothing you installed yourself.
MANIFEST="$PREFIX/share/xu526/installed-tools"
touch "$MANIFEST"
record() { grep -qxF "$1" "$MANIFEST" || echo "$1" >>"$MANIFEST"; }

say "checking Colima $COLIMA_VERSION"
if version_of "$BIN/colima" version | grep -q "$COLIMA_VERSION"; then
  echo "    already installed"
else
  base="https://github.com/abiosoft/colima/releases/download/v$COLIMA_VERSION/colima-Darwin-$ARCH"
  fetch "$base" "$TMP/colima"
  fetch "$base.sha256sum" "$TMP/colima.sha256sum"
  checksum "$TMP/colima" "$(awk '{print $1}' "$TMP/colima.sha256sum")"
  install -m 755 "$TMP/colima" "$BIN/colima"
  record "bin/colima"
fi

say "checking Lima $LIMA_VERSION"
if version_of "$BIN/limactl" --version | grep -q "$LIMA_VERSION"; then
  echo "    already installed"
else
  name="lima-$LIMA_VERSION-Darwin-$ARCH.tar.gz"
  base="https://github.com/lima-vm/lima/releases/download/v$LIMA_VERSION"
  fetch "$base/$name" "$TMP/$name"
  fetch "$base/SHA256SUMS" "$TMP/SHA256SUMS"
  want=$(awk -v n="$name" '$2 == n || $2 == "*"n {print $1}' "$TMP/SHA256SUMS")
  [ -n "$want" ] || die "$name isn't listed in Lima's SHA256SUMS"
  checksum "$TMP/$name" "$want"
  mkdir "$TMP/lima"
  tar -xzf "$TMP/$name" -C "$TMP/lima" # bin/limactl, share/lima (+ libexec/lima on some versions)
  [ -x "$TMP/lima/bin/limactl" ] || die "unexpected layout in $name"
  rm -rf "$PREFIX/share/lima" "$PREFIX/libexec/lima"
  cp -R "$TMP/lima/." "$PREFIX/"
  (cd "$TMP/lima" && find . ! -type d | sed 's|^\./||') | while read -r f; do record "$f"; done
  record "share/lima/"
  [ -d "$TMP/lima/libexec/lima" ] && record "libexec/lima/"
fi

say "checking the Docker CLI $DOCKER_VERSION"
if version_of "$BIN/docker" --version | grep -q "$DOCKER_VERSION"; then
  echo "    already installed"
else
  # Docker publishes no checksum file for these; they come over HTTPS from Docker itself.
  fetch "https://download.docker.com/mac/static/stable/$DOCKER_ARCH/docker-$DOCKER_VERSION.tgz" "$TMP/docker.tgz"
  tar -xzf "$TMP/docker.tgz" -C "$TMP"
  [ -x "$TMP/docker/docker" ] || die "unexpected layout in docker-$DOCKER_VERSION.tgz"
  install -m 755 "$TMP/docker/docker" "$BIN/docker"
  record "bin/docker"
fi

say "checking Go"
if [ -d "$ROOT/bin" ]; then
  echo "    not needed: this download has xu526 ready-built"
elif go version 2>/dev/null | grep -qE 'go1\.(2[4-9]|[3-9][0-9])'; then
  echo "    $(go version)"
else
  name="go$GO_VERSION.darwin-$GOARCH.tar.gz"
  fetch "https://dl.google.com/go/$name" "$TMP/$name"
  fetch "https://dl.google.com/go/$name.sha256" "$TMP/go.sha256"
  checksum "$TMP/$name" "$(awk '{print $1}' "$TMP/go.sha256")"
  rm -rf "$PREFIX/go"
  tar -xzf "$TMP/$name" -C "$PREFIX"
  record "go/"
  echo "    $(go version)"
fi
command -v git >/dev/null || echo "    note: git is missing (needed only for deploys from git); run: xcode-select --install"

# ---------- Colima ----------
mkdir -p "$AGENTS" "$LOGS" "$STATE"
if colima status >/dev/null 2>&1; then
  say "Colima is already running"
else
  say "starting Colima (Docker in a small Linux VM; the first start downloads an image and takes a few minutes)"
  args=(--cpu "$COLIMA_CPU" --memory "$COLIMA_MEMORY" --disk "$COLIMA_DISK" --vm-type vz)
  [ "$ARCH" = arm64 ] && args+=(--vz-rosetta) # lets x86 images run too
  colima start "${args[@]}"
fi
docker context use colima >/dev/null 2>&1 || true
[ -S "$SOCK" ] || die "Colima is running but $SOCK is missing; try: colima restart"
docker info >/dev/null 2>&1 || die "docker can't reach Colima; try: colima restart"
echo "    $(docker info --format '{{.OperatingSystem}} ({{.Architecture}}), Docker {{.ServerVersion}}')"

# Start Colima at login. `colima start` returns once the VM is up.
cat >"$COLIMA_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$COLIMA_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$(xml "$BIN/colima")</string>
    <string>start</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(xml "$BIN:/usr/bin:/bin:/usr/sbin:/sbin")</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$(xml "$LOGS/colima.log")</string>
  <key>StandardErrorPath</key><string>$(xml "$LOGS/colima.log")</string>
</dict>
</plist>
EOF
plutil -lint "$COLIMA_PLIST" >/dev/null
unload "$COLIMA_LABEL"
launchctl bootstrap "$GUI" "$COLIMA_PLIST" # Colima is already up, so this start is a no-op

# ---------- xu526 ----------
say "installing xu526"
xu_get_binary xu526 "$BIN/xu526.new" "$ROOT"
mv -f "$BIN/xu526.new" "$BIN/xu526"
# A browser download marks files as quarantined; macOS would then refuse to
# run the (unsigned) program from the login service.
xattr -d com.apple.quarantine "$BIN/xu526" 2>/dev/null || true
ln -sf xu526 "$BIN/xu" # the short name the commands use
rm -f "$BIN/xud"       # the old daemon program
echo "    $("$BIN/xu526" version)"

# ---------- the xu526 service ----------
say "installing xu526 as a login service"
unload "$OLD_LABEL"
rm -f "$AGENTS/$OLD_LABEL.plist"
# At login Colima may still be booting: the wrapper waits up to 5 minutes
# for its socket, then becomes xu526 ($0 is xu526, "$@" is daemon and its flags).
wait_cmd='for i in $(seq 1 150); do [ -S "$XU_DOCKER_SOCK" ] && break; sleep 2; done; exec "$0" "$@"'
args="    <string>/bin/sh</string>
    <string>-c</string>
    <string>$(xml "$wait_cmd")</string>
    <string>$(xml "$BIN/xu526")</string>
    <string>daemon</string>
    <string>-state</string>
    <string>$(xml "$STATE/apps.json")</string>
    <string>-docker-sock</string>
    <string>$(xml "$SOCK")</string>"
for f in $XU_FLAGS; do
  args="$args
    <string>$(xml "$f")</string>"
done
# launchd gives agents a bare PATH; keep this one so xu526 finds git (and
# node, go, python, zig when -host-builds is on).
cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
$args
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(xml "$PATH")</string>
    <key>XU_DOCKER_SOCK</key><string>$(xml "$SOCK")</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ExitTimeOut</key><integer>60</integer>
  <key>StandardOutPath</key><string>$(xml "$LOGS/xu526.log")</string>
  <key>StandardErrorPath</key><string>$(xml "$LOGS/xu526.log")</string>
</dict>
</plist>
EOF
plutil -lint "$PLIST" >/dev/null
unload "$LABEL"
launchctl bootstrap "$GUI" "$PLIST"
launchctl kickstart -k "$GUI/$LABEL"

# ---------- PATH ----------
case "${SHELL:-/bin/zsh}" in
  */bash) rc="$HOME/.bash_profile" ;;
  *) rc="$HOME/.zshrc" ;;
esac
touch "$rc"
if ! grep -q "$MARK" "$rc"; then
  printf 'export PATH="%s:%s/go/bin:$PATH" %s\n' "$BIN" "$PREFIX" "$MARK" >>"$rc"
  say "added $BIN to your PATH in $rc"
fi

# ---------- check ----------
say "waiting for xu526"
ok=
for _ in $(seq 1 20); do
  if curl -fsS http://127.0.0.1:7070/v1/node >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
if [ -z "$ok" ]; then
  tail -n 20 "$LOGS/xu526.log" 2>/dev/null || true
  die "xu526 didn't start; the log is $LOGS/xu526.log"
fi

cat <<EOF

xu526 is installed and running.

  Dashboard   http://127.0.0.1:7070
  Go public   Dashboard → Settings → Connect with xu526 tunnel (or: xu tunnel login)
  Your app    cd /path/to/app && xu build . --deploy myapp

  Logs        tail -f "$LOGS/xu526.log"      (Colima: $LOGS/colima.log)
  Restart     launchctl kickstart -k $GUI/$LABEL
  Update      run this script again from a newer copy
  Uninstall   $HERE/uninstall.sh   (--purge also deletes apps and data, --all also Colima)

Open a new terminal (or run: exec \$SHELL) so the xu command is found.
EOF
