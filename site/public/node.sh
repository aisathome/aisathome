#!/bin/sh
# AIs@home: start a node without an account (science only), or update the node on this machine.
#
#   curl -fsSL https://aisathome.org/node.sh | sh
#
# It downloads the latest prebuilt bundle and its SHA256SUMS from GitHub, checks the bundle, runs the
# installer's checks (--dry-run), says what it will do, asks before anything is installed (at the
# terminal, not through the pipe), then runs the bundle's install.sh with sudo:
# - A fresh machine: `install.sh --anonymous --enable-at-boot`, a node without an account that starts
#   again after a reboot, as the Windows node does.
# - A machine that has a node already (its systemd unit or its program is there; both are readable
#   without sudo): `install.sh` with no account option, at the terminal, which is how the installer
#   updates a node. It keeps the node's token when it is recorded for the node's gateway (a linked
#   node stays linked, one without an account stays so), and its node id, gateway, model, local
#   password, counters and whether it starts at boot. A token from before v0.6.0 has no record: the
#   installer moves it aside and asks "Link this node to your account? [Y/n]". Never --anonymous here
#   (that would register a node whose token was moved aside without an account), never --keep-token
#   (it refuses such a token instead of linking again), never --enable-at-boot (the node keeps its own
#   choice).
# The same steps by hand, to read before running: https://aisathome.org/node/install/ (a new node) and
# https://aisathome.org/node/update/ (an update).
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
root="${AISATHOME_ROOT:-}"       # tests: a stand-in for / (where an installed node is looked for)

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

# version <aisathome-node>: the release it says it is ("v0.7.0"), or "an unknown version".
version() {
  v="$("$1" -version 2>/dev/null | sed -n 's/^aisathome-node \(v[0-9][0-9.]*\)$/\1/p' | head -n 1)" || v=""
  echo "${v:-an unknown version}"
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

  # A node installed already (by any installer release): its unit, or its program.
  update=0 old=""
  if [ -e "$root/etc/systemd/system/aisathome-node.service" ] || [ -x "$root/opt/aisathome-node/bin/aisathome-node" ]; then
    update=1
    old="$(version "$root/opt/aisathome-node/bin/aisathome-node")"
    set --
  else
    set -- --anonymous --enable-at-boot
  fi
  [ -z "$gateway" ] || set -- "$@" --gateway-url "$gateway"

  echo
  if [ "$update" = 1 ]; then
    echo "Existing node found: updating it, keeping its settings ($old -> $(version ./bin/aisathome-node))."
  fi
  echo "Checking this machine (nothing is installed yet):"
  ./install.sh --dry-run "$@" || die "the checks failed (above): nothing was installed"

  if [ "$update" = 1 ]; then
    cat <<'NOTICE'

This updates the AIs@home node on this machine: the node agent, llama-server and their services, and
the model if the release pins another one. It needs sudo. The node keeps its token (linked to an
account, or without one), its node id, gateway, model, local password and counters, and whether it
starts at boot. A node installed before v0.6.0 is linked to your account again: the installer asks.
The node is offline for a few minutes while it restarts and runs its speed test.
NOTICE
    printf 'Update? [y/N] '
  else
    cat <<'NOTICE'

This installs an AIs@home node: the node agent and llama-server as system services, and the model
for your card (13-14 GB). It needs sudo. The node starts at boot
(sudo systemctl disable aisathome-llama aisathome-node turns that off).
Without an account this node does science work only, never people's chats, and earns no credit
until you link it to an account (aisathome-node link).
You must be 18 or older; running it means you accept the terms: https://aisathome.org/about/terms/
NOTICE
    printf 'Install? [y/N] '
  fi
  read -r answer <"$tty" || answer=""
  case "$answer" in
    y | Y | yes | Yes | YES) ;;
    *) echo "Nothing was installed. The bundle stays in $dir/$bundle"; exit 0 ;;
  esac
  # At the terminal: the installer's own questions (on an update, the account of a node whose token it
  # cannot keep) are answered there.
  $sudo ./install.sh "$@" <"$tty"
}
main
