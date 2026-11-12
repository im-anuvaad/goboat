#!/usr/bin/env bash
# goboat installer: the only command you ever run over SSH.
#
#   curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash
#
# It asks nothing and installs no system packages (docs/architecture.md §4):
#   1. checks the host (Ubuntu 24.04+, systemd, cgroup v2, x86_64/aarch64,
#      about 2 GB of RAM, 10 GB of free disk)
#   2. downloads goboat-linux-<arch>, verifies it against checksums.txt
#      (nothing is changed on a mismatch)
#   3. stops goboat if it runs, installs the binary, and runs `goboat setup-host`:
#      directories, master key, the build user, the Go toolchain goboat builds
#      apps with (/usr/local/lib/goboat/go, checked against go.dev's checksum),
#      a 2 GB swap file if the server has less swap, the systemd unit, a new
#      one-time password, start, and prints the address to open
#
# Running it again updates goboat and its Go toolchain to the latest release
# (keeping every app and setting) and prints a new one-time password (lost
# password, broken dashboard domain).
#
#   GOBOAT_VERSION=v1.0.0   install that release instead of the latest
set -euo pipefail

REPO="im-anuvaad/goboat"
INSTALL_PATH="/usr/local/bin/goboat"
MIN_UBUNTU="24.04"
MIN_RAM_MB=1800   # a "2 GB" VPS reports a little less
MIN_DISK_GB=10

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# version_ge A B: true if A >= B (dotted numeric versions)
version_ge() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]; }

check_host() {
  [ "$(id -u)" -eq 0 ] || fail "run as root (pipe to 'sudo bash')"
  [ "$(uname -s)" = "Linux" ] || fail "goboat only supports Linux"

  [ -r /etc/os-release ] || fail "cannot read /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = "ubuntu" ] || fail "unsupported OS '${ID:-unknown}': goboat requires Ubuntu ${MIN_UBUNTU} or newer"
  version_ge "${VERSION_ID:-0}" "$MIN_UBUNTU" || fail "Ubuntu ${VERSION_ID:-?} is too old: goboat requires ${MIN_UBUNTU} or newer"

  [ -d /run/systemd/system ] || fail "systemd is not running (required)"
  [ -f /sys/fs/cgroup/cgroup.controllers ] || fail "cgroup v2 (unified hierarchy) is required"

  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "unsupported CPU architecture '$(uname -m)' (supported: x86_64, aarch64)" ;;
  esac

  local ram_mb disk_gb
  ram_mb=$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)
  [ "${ram_mb:-0}" -ge "$MIN_RAM_MB" ] || fail "the server has ${ram_mb} MB of RAM; goboat builds apps on the server and needs about 2 GB (2 vCPU / 2 GB recommended)"
  mkdir -p /var/lib
  disk_gb=$(df -Pk /var/lib | awk 'NR==2 {print int($4/1048576)}')
  [ "${disk_gb:-0}" -ge "$MIN_DISK_GB" ] || fail "only ${disk_gb} GB free on /var/lib; goboat needs at least ${MIN_DISK_GB} GB (Go toolchain, build caches, swap, backups)"

  command -v curl >/dev/null      || fail "curl is required"
  command -v sha256sum >/dev/null || fail "sha256sum is required"
}

download() {
  if [ -n "${GOBOAT_VERSION:-}" ]; then
    BASE="https://github.com/${REPO}/releases/download/${GOBOAT_VERSION}"
  else
    BASE="https://github.com/${REPO}/releases/latest/download"
  fi

  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT

  local bin="goboat-linux-${ARCH}"
  say "Downloading ${bin} (${GOBOAT_VERSION:-latest})"
  curl -fsSL --retry 3 -o "${TMP}/${bin}" "${BASE}/${bin}" || fail "download failed: ${BASE}/${bin}"
  curl -fsSL --retry 3 -o "${TMP}/checksums.txt" "${BASE}/checksums.txt" || fail "download failed: ${BASE}/checksums.txt"

  say "Verifying SHA-256"
  local expected actual
  expected="$(awk -v f="$bin" '$2 == f || $2 == "*"f {print $1}' "${TMP}/checksums.txt")"
  [ -n "$expected" ] || fail "no checksum for ${bin} in checksums.txt"
  actual="$(sha256sum "${TMP}/${bin}" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    rm -f "${TMP}/${bin}"
    fail "checksum mismatch for ${bin} (expected ${expected}, got ${actual}); nothing was installed"
  fi

  install -m 0755 -o root -g root "${TMP}/${bin}" "${INSTALL_PATH}.new"
  "${INSTALL_PATH}.new" version >/dev/null || { rm -f "${INSTALL_PATH}.new"; fail "the downloaded binary does not run on this machine"; }
  NEW_VERSION="$("${INSTALL_PATH}.new" version)"
}

install_binary() {
  if systemctl is-active --quiet goboat 2>/dev/null; then
    say "Stopping goboat (apps restart with it; a few seconds of downtime)"
    systemctl stop goboat
  fi
  mv -f "${INSTALL_PATH}.new" "$INSTALL_PATH"
  say "Installed: ${NEW_VERSION}"
}

main() {
  check_host
  download
  install_binary
  "$INSTALL_PATH" setup-host
}

main "$@"
