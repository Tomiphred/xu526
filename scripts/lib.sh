# Shared by the install scripts: find a ready-built program in a download
# package (bin/<os>-<arch>/NAME), or build it from a source checkout.
# Usage: xu_get_binary NAME DEST   (sets nothing; writes DEST)

xu_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) echo "unsupported CPU: $(uname -m)" >&2; return 1 ;;
  esac
}

xu_os() { uname -s | tr '[:upper:]' '[:lower:]'; }

# xu_root: the folder holding bin/ (a package) or go.mod (source).
xu_root() {
  local here="$1"
  for d in "$here" "$here/.."; do
    if [ -d "$d/bin" ] || [ -f "$d/go.mod" ]; then (cd "$d" && pwd); return 0; fi
  done
  echo "can't find the program files next to this script; unzip the whole download and run the script from inside it" >&2
  return 1
}

xu_get_binary() { # NAME DEST ROOT
  local name="$1" dest="$2" root="$3" arch os
  arch=$(xu_arch) || return 1
  os=$(xu_os)
  if [ -x "$root/bin/$os-$arch/$name" ]; then
    install -m 755 "$root/bin/$os-$arch/$name" "$dest"
    return 0
  fi
  if [ ! -f "$root/go.mod" ]; then
    echo "this download has no $name for $os/$arch" >&2
    return 1
  fi
  # From source: needs Go 1.24+.
  export PATH="/usr/local/go/bin:$HOME/.local/go/bin:$PATH"
  if ! go version 2>/dev/null | grep -qE 'go1\.(2[4-9]|[3-9][0-9])'; then
    echo "building $name from source needs Go 1.24 or newer (https://go.dev/dl/), or use the ready-built download" >&2
    return 1
  fi
  local ver
  ver=$(cat "$root/VERSION" 2>/dev/null || echo dev)
  (cd "$root" && go build -trimpath -ldflags "-s -w -X xu526/internal/version.Version=$ver ${XU_SHARED_CLIENT_ID:+-X xu526/internal/ghapp.SharedClientID=$XU_SHARED_CLIENT_ID}" -o "$dest" "./cmd/$name")
}

# xu_ensure_go installs Go into /usr/local/go on Linux when building from
# source and it's missing (packages don't need it).
xu_ensure_go() { # ROOT
  local root="$1" arch
  [ -d "$root/bin" ] && return 0
  export PATH="/usr/local/go/bin:$PATH"
  go version 2>/dev/null | grep -qE 'go1\.(2[4-9]|[3-9][0-9])' && return 0
  arch=$(xu_arch) || return 1
  echo "==> installing Go 1.24.7 (to build from source)"
  curl -fsSL "https://go.dev/dl/go1.24.7.linux-${arch}.tar.gz" -o /tmp/xu526-go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/xu526-go.tgz
  rm -f /tmp/xu526-go.tgz
}
