#!/usr/bin/env bash
# Removes xu526 (and, on a relay, xu526-server) from macOS or Linux,
# including installs from before xu526 was one program (xu + xud).
# It shows what it will remove and asks before doing it.
#
#   scripts/uninstall.sh            remove the programs and the service; keep apps and data
#   scripts/uninstall.sh --purge    also delete apps, data, logs, build cache, xu login,
#                                   and the containers, volumes, networks and xu/ images xu526 made
#   scripts/uninstall.sh --all      --purge, plus (macOS) the Colima VM and the Colima, Lima,
#                                   Docker CLI and Go files the installer downloaded
#
# Options: -y (don't ask), --dry-run (only show what would happen), -h (help).
# On Linux it uses sudo for the system files.
set -euo pipefail

level=keep yes= dry=
for a in "$@"; do
  case "$a" in
    --purge) [ "$level" = all ] || level=purge ;;
    --all) level=all ;;
    -y | --yes) yes=1 ;;
    -n | --dry-run) dry=1 ;;
    -h | --help) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $a (see --help)" >&2; exit 2 ;;
  esac
done

OS=$(uname -s)
case "$OS" in
  Darwin | Linux) ;;
  *) echo "this script is for macOS and Linux (on Windows, run it inside WSL)" >&2; exit 1 ;;
esac

# Linux system files need root; re-run through sudo, keeping the options.
if [ "$OS" = Linux ] && [ "$(id -u)" != 0 ] && [ -z "$dry" ]; then
  echo "using sudo to remove the system service and files"
  exec sudo bash "$0" "$@"
fi
if [ "$OS" = Darwin ] && [ "$(id -u)" = 0 ]; then
  echo "on macOS run this as yourself, not with sudo (xu526 was installed for your user)" >&2
  exit 1
fi

# The person who uses xu (their cache and login), even under sudo.
user=${SUDO_USER:-$(id -un)}
if [ "$OS" = Linux ] && command -v getent >/dev/null; then
  uhome=$(getent passwd "$user" | cut -d: -f6)
else
  uhome=$(eval echo "~$user")
fi
[ -n "$uhome" ] && [ -d "$uhome" ] || uhome=$HOME

PREFIX=${PREFIX:-$uhome/.local}
MARK="# added by xu526 install-macos.sh"
if [ "$OS" = Darwin ]; then
  GUI="gui/$(id -u)"
  AGENTS="$uhome/Library/LaunchAgents"
  PROGRAMS=("$PREFIX/bin/xu526" "$PREFIX/bin/xu" "$PREFIX/bin/xud")
  SERVICE_FILES=("$AGENTS/dev.xu526.daemon.plist" "$AGENTS/dev.xu526.xud.plist")
  DATA=("$uhome/Library/Application Support/xu526" "$uhome/Library/Logs/xu526"
        "$uhome/Library/Caches/xu" "$uhome/Library/Application Support/xu")
  MANIFEST="$PREFIX/share/xu526/installed-tools"
else
  PROGRAMS=(/usr/local/bin/xu526 /usr/local/bin/xu /usr/local/bin/xud /usr/local/bin/xu526-server)
  SERVICE_FILES=(/etc/systemd/system/xu526.service /etc/default/xu526 /etc/systemd/system/xu526-server.service /etc/default/xu526-server
    /etc/systemd/system/xud.service /etc/default/xud /etc/default/xud.before-xu526-server)
  cache=${XDG_CACHE_HOME:-$uhome/.cache}
  config=${XDG_CONFIG_HOME:-$uhome/.config}
  DATA=(/var/lib/xu526 "$cache/xu" "$config/xu")
  [ "$uhome" != /root ] && DATA+=(/root/.cache/xu /root/.config/xu) # xu run with sudo
  MANIFEST=
fi
RCS=("$uhome/.zshrc" "$uhome/.bash_profile")

exec 3>&1 # dry-run notes go here, even from commands whose output is silenced
run() {
  if [ -n "$dry" ]; then printf '    would run: %s\n' "$*" >&3; else "$@"; fi
}
exists() { [ -e "$1" ] || [ -L "$1" ]; }

# ---------- Docker ----------
DOCKER=
docker_ok() {
  local d
  for d in docker "$PREFIX/bin/docker"; do
    if command -v "$d" >/dev/null 2>&1 && "$d" info >/dev/null 2>&1; then DOCKER=$d; return 0; fi
  done
  return 1
}
xu_containers() { "$DOCKER" ps -aq --filter label=xu.managed=true; }
xu_volumes() { "$DOCKER" volume ls -q --filter label=xu.managed=true; }
xu_networks() { "$DOCKER" network ls -q --filter label=xu.managed=true; }
xu_images() { "$DOCKER" images --format '{{.Repository}}:{{.Tag}}' | grep '^xu/' || true; }

# ---------- what will happen ----------
echo "xu526 uninstall on $([ "$OS" = Darwin ] && echo macOS || echo Linux) ($level)"
echo
echo "Stop and remove:"
if [ "$OS" = Darwin ]; then
  echo "  - the xu526 login service (launchd agent dev.xu526.daemon)"
else
  echo "  - the xu526 and xu526-server services (systemd)"
fi
for f in "${PROGRAMS[@]}" "${SERVICE_FILES[@]}"; do exists "$f" && echo "  - $f"; done
for rc in "${RCS[@]}"; do [ -f "$rc" ] && grep -qF "$MARK" "$rc" && echo "  - the PATH line xu526 added to $rc"; done

docker_reachable=
if [ "$level" != keep ]; then
  echo
  echo "Delete (--purge):"
  for f in "${DATA[@]}"; do exists "$f" && echo "  - $f"; done
  if docker_ok; then
    docker_reachable=1
    c=$(xu_containers | wc -l | tr -d ' '); v=$(xu_volumes | wc -l | tr -d ' ')
    n=$(xu_networks | wc -l | tr -d ' '); i=$(xu_images | wc -l | tr -d ' ')
    echo "  - Docker: $c xu526 containers, $v volumes (app data such as databases), $n networks, $i xu/ image tags"
  else
    echo "  - (Docker isn't running, so xu526's containers, volumes and images can't be removed now;"
    echo "     start Docker and run this again with --purge to remove them)"
  fi
fi

tools=()
if [ "$level" = all ]; then
  echo
  echo "Also remove (--all):"
  if [ "$OS" = Darwin ]; then
    echo "  - the Colima VM, everything in it, and its login service (dev.xu526.colima)"
    if [ -f "$MANIFEST" ]; then
      while IFS= read -r f; do
        case "$f" in "" | /* | *..*) continue ;; esac # only plain paths under PREFIX
        tools+=("$f")
        exists "$PREFIX/$f" && echo "  - $PREFIX/$f"
      done <"$MANIFEST"
    else
      echo "  - (no record of downloaded tools; Colima/Lima/Docker you installed yourself are left alone)"
    fi
  else
    echo "  - nothing more on Linux: Docker, Go and git came from your package manager or /usr/local/go"
    echo "    and other software may use them (remove them yourself if you're sure:"
    echo "    sudo apt-get remove docker.io; sudo rm -rf /usr/local/go)"
  fi
fi
echo

if [ -n "$dry" ]; then
  echo "dry run: nothing was changed"
  echo
elif [ -z "$yes" ]; then
  if [ ! -t 0 ]; then
    echo "not running in a terminal; pass -y to confirm" >&2
    exit 1
  fi
  printf 'Continue? [y/N] '
  read -r answer
  case "$answer" in y | Y | yes | YES) ;; *) echo "nothing removed"; exit 0 ;; esac
fi

# ---------- stop the service first, so it doesn't restart apps ----------
if [ "$OS" = Darwin ]; then
  run launchctl bootout "$GUI/dev.xu526.daemon" 2>/dev/null || true
  run launchctl bootout "$GUI/dev.xu526.xud" 2>/dev/null || true
else
  if command -v systemctl >/dev/null; then
    for unit in xu526 xu526-server xud; do
      if systemctl list-unit-files "$unit.service" >/dev/null 2>&1; then
        run systemctl disable --now "$unit" 2>/dev/null || true
      fi
    done
  fi
fi

# ---------- Docker objects ----------
if [ "$level" != keep ] && [ -n "$docker_reachable" ]; then
  echo "removing xu526's containers, networks, volumes and images"
  for id in $(xu_containers); do run "$DOCKER" rm -f "$id" >/dev/null; done
  for id in $(xu_networks); do run "$DOCKER" network rm "$id" >/dev/null || true; done
  for id in $(xu_volumes); do run "$DOCKER" volume rm -f "$id" >/dev/null || true; done
  for ref in $(xu_images); do run "$DOCKER" rmi "$ref" >/dev/null || true; done # untags; layers go with the last tag
fi

# ---------- files ----------
for f in "${PROGRAMS[@]}" "${SERVICE_FILES[@]}"; do exists "$f" && run rm -f "$f"; done
[ "$OS" = Linux ] && command -v systemctl >/dev/null && run systemctl daemon-reload 2>/dev/null || true
for rc in "${RCS[@]}"; do
  if [ -f "$rc" ] && grep -qF "$MARK" "$rc"; then
    if [ -n "$dry" ]; then
      echo "    would remove the xu526 PATH line from $rc"
    else
      grep -vF "$MARK" "$rc" >"$rc.xu526.tmp" || true
      cat "$rc.xu526.tmp" >"$rc" && rm -f "$rc.xu526.tmp" # keeps the file's owner and mode
    fi
  fi
done
if [ "$level" != keep ]; then
  for f in "${DATA[@]}"; do exists "$f" && run rm -rf "$f"; done
fi

# ---------- macOS tools (--all) ----------
if [ "$level" = all ] && [ "$OS" = Darwin ]; then
  run launchctl bootout "$GUI/dev.xu526.colima" 2>/dev/null || true
  exists "$AGENTS/dev.xu526.colima.plist" && run rm -f "$AGENTS/dev.xu526.colima.plist"
  colima=$(command -v colima 2>/dev/null || echo "$PREFIX/bin/colima")
  if [ -x "$colima" ]; then
    echo "deleting the Colima VM"
    PATH="$PREFIX/bin:$PATH" run "$colima" delete --force >/dev/null 2>&1 || true
  fi
  if [ -n "$DOCKER" ] || command -v docker >/dev/null 2>&1; then
    docker context inspect colima >/dev/null 2>&1 && { run docker context use default >/dev/null 2>&1 || true; run docker context rm colima >/dev/null 2>&1 || true; }
  fi
  for f in ${tools[@]+"${tools[@]}"}; do
    case "$f" in
      */) exists "$PREFIX/$f" && run rm -rf "${PREFIX:?}/$f" ;;
      *) exists "$PREFIX/$f" && run rm -f "${PREFIX:?}/$f" ;;
    esac
  done
  [ -n "$MANIFEST" ] && exists "$MANIFEST" && run rm -rf "$(dirname "$MANIFEST")"
fi

echo
if [ -n "$dry" ]; then
  echo "dry run finished; run without --dry-run to do it"
  exit 0
fi
case "$level" in
  keep) echo "xu526 is removed. Your apps' data is kept; run with --purge to delete it too." ;;
  purge) echo "xu526 and its data are removed." ;;
  all) echo "xu526, its data and the tools it installed are removed." ;;
esac
[ "$level" != keep ] && echo "If you used a custom -state or -github-secrets-dir, delete those folders yourself."
if [ "$level" != keep ] && command -v psql >/dev/null 2>&1; then
  echo "The sign-up site's PostgreSQL database is kept; to delete it: sudo -u postgres dropdb xu_portal"
fi
echo "Open a new terminal so your PATH no longer lists xu526."
