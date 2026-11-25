#!/bin/sh
# AIs@home: start a node without an account (science only).
#
#   curl -fsSL https://aisathome.org/node.sh | sh
#
# It downloads the latest prebuilt bundle and its SHA256SUMS from GitHub, checks the bundle, runs the
# installer's checks (--dry-run), shows what a node without an account means, asks before anything is
# installed (at the terminal, not through the pipe), then runs `sudo ./install.sh --anonymous`. The
# same steps by hand, to read before running: https://aisathome.org/node/install/
#
# AISATHOME_RELEASE_BASE: where the bundle comes from (default: the latest GitHub release);
# AISATHOME_GATEWAY_URL: the gateway the node connects to (default: the installer's, aisathome.org's).
# A test site serves this same script with these two defaults changed, nothing else.
# SPDX-License-Identifier: Apache-2.0
set -eu

base="${AISATHOME_RELEASE_BASE:-https://github.com/aisathome/aisathome/releases/latest/download}"
gateway="${AISATHOME_GATEWAY_URL:-}"
bundle=aisathome-node-bundle-linux-amd64
sudo="${AISATHOME_SUDO-sudo}"  # tests: a stand-in
tty="${AISATHOME_TTY:-/dev/tty}" # where the answer comes from: the terminal, never the pipe

die() { echo "node.sh: $1" >&2; exit 1; }

# fetch <url> <file>: a large download with progress: curl's bar at a terminal; piped or logged, a plain
# line every 15 seconds with how much has arrived, then the total.
fetch() {
  if [ -t 2 ]; then curl -fL# -o "$2" "$1"; return; fi
  curl -fsSL -o "$2" "$1" &
  pid=$! n=0
  while kill -0 "$pid" 2>/dev/null; do
    sleep 1; n=$((n + 1))
    if [ $((n % 15)) -eq 0 ] && [ -f "$2" ] && kill -0 "$pid" 2>/dev/null; then
      echo "  $(($(wc -c <"$2") / 1000000)) MB so far" >&2
    fi
  done
  wait "$pid" || return
  echo "  $(($(wc -c <"$2") / 1000000)) MB downloaded" >&2
}

# All in a function, called on the last line: a download cut short runs nothing.
main() {
  [ "$(uname -s)" = Linux ] || die "this is for Linux; on Windows see https://aisathome.org/node/windows/"
  [ "$(uname -m)" = x86_64 ] || die "the bundle is for x86_64 Linux"
  for c in curl sha256sum tar xz; do command -v "$c" >/dev/null 2>&1 || die "needs $c"; done
  [ -r "$tty" ] || die "needs a terminal to ask before installing; use the steps at https://aisathome.org/node/install/"

  dir="$(mktemp -d "${TMPDIR:-/tmp}/aisathome-node.XXXXXX")"
  cd "$dir"
  echo "Downloading the node bundle (about 0.5 GB) into $dir"
  fetch "$base/$bundle.tar.xz" "$bundle.tar.xz" || die "download failed: $base/$bundle.tar.xz"
  curl -fsSL -o SHA256SUMS "$base/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
  grep " $bundle.tar.xz\$" SHA256SUMS | sha256sum -c - || die "the bundle does not match SHA256SUMS: nothing was installed"
  tar -xJf "$bundle.tar.xz"
  cd "$bundle"

  echo
  echo "Checking this machine (nothing is installed yet):"
  set -- --anonymous
  [ -z "$gateway" ] || set -- "$@" --gateway-url "$gateway"
  ./install.sh --dry-run "$@" || die "the checks failed (above): nothing was installed"

  cat <<'NOTICE'

This installs an AIs@home node: the node agent and llama-server as system services, and the model
for your card (13-14 GB). It needs sudo.
Without an account this node does science work only, never people's chats, and earns no credit
until you link it to an account (aisathome-node link).
You must be 18 or older; running it means you accept the terms: https://aisathome.org/about/terms/
NOTICE
  printf 'Install? [y/N] '
  read -r answer <"$tty" || answer=""
  case "$answer" in
    y | Y | yes | Yes | YES) ;;
    *) echo "Nothing was installed. The bundle stays in $dir/$bundle"; exit 0 ;;
  esac
  $sudo ./install.sh "$@" <"$tty"
}
main
