#!/usr/bin/env bash
# Tests for site/public/node.sh (the one-line install without an account) against a fake release: it
# checks the bundle against SHA256SUMS, runs the installer's checks first, asks at the terminal (here a
# file) and installs with --anonymous only on yes.
#
# SPDX-License-Identifier: Apache-2.0
set -uo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/site/public/node.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
fail() { echo "FAIL: $1" >&2; fails=$((fails + 1)); }

# A fake bundle: install.sh records how it was called.
rel="$tmp/release"; mkdir -p "$rel/b/aisathome-node-bundle-linux-amd64"
cat > "$rel/b/aisathome-node-bundle-linux-amd64/install.sh" <<'SH'
#!/bin/sh
echo "install.sh $*" >> "$CALLS"
case "$*" in *--dry-run*) [ -z "${DRY_FAIL:-}" ] || exit 1 ;; esac
exit 0
SH
chmod +x "$rel/b/aisathome-node-bundle-linux-amd64/install.sh"
(cd "$rel/b" && tar -cJf "$rel/aisathome-node-bundle-linux-amd64.tar.xz" aisathome-node-bundle-linux-amd64)
(cd "$rel" && sha256sum aisathome-node-bundle-linux-amd64.tar.xz > SHA256SUMS)

# run <answer>: node.sh piped into sh as the one-liner does, the answer on the "terminal".
run() {
  printf '%s\n' "$1" > "$tmp/tty"; : > "$tmp/calls"
  out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" \
    sh < "$script" 2>&1)"; code=$?
  calls="$(cat "$tmp/calls")"
}

run y
[[ $code -eq 0 ]] || fail "yes: exit $code: $out"
[[ "$calls" == $'install.sh --dry-run --anonymous\ninstall.sh --anonymous' ]] || fail "yes: calls were: $calls"
for want in "does science work only, never people's chats, and earns no credit" "You must be 18 or older" "https://aisathome.org/about/terms/" "Install? [y/N]" ": OK"; do
  [[ "$out" == *"$want"* ]] || fail "yes: the output lacks '$want'"
done

for no in n "" maybe; do
  run "$no"
  [[ $code -eq 0 && "$calls" == "install.sh --dry-run --anonymous" ]] || fail "answer '$no': installed anyway ($calls)"
  [[ "$out" == *"Nothing was installed"* ]] || fail "answer '$no': no message"
done

# A bundle that does not match SHA256SUMS is never unpacked or run.
cp "$rel/SHA256SUMS" "$tmp/sums.ok"
echo "0000000000000000000000000000000000000000000000000000000000000000  aisathome-node-bundle-linux-amd64.tar.xz" > "$rel/SHA256SUMS"
run y
[[ $code -ne 0 && -z "$calls" ]] || fail "a bad checksum: exit $code, calls $calls"
[[ "$out" == *"does not match SHA256SUMS"* ]] || fail "a bad checksum: no message: $out"
cp "$tmp/sums.ok" "$rel/SHA256SUMS"

# Failed checks: no install.
DRY_FAIL=1 run y
[[ $code -ne 0 && "$calls" == "install.sh --dry-run --anonymous" ]] || fail "failed checks: exit $code, calls $calls"

# No terminal to ask on: nothing is downloaded or installed.
printf '' > "$tmp/calls"
out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_SUDO="" AISATHOME_TTY="$tmp/none" TMPDIR="$tmp" sh < "$script" 2>&1)"; code=$?
[[ $code -ne 0 && ! -s "$tmp/calls" && "$out" == *"needs a terminal"* ]] || fail "no terminal: exit $code: $out"

# A test site's copy names its gateway: both installer runs get it.
printf 'y\n' > "$tmp/tty"; : > "$tmp/calls"
out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_GATEWAY_URL=wss://api.staging.example.org/v1/node/connect \
  AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" sh < "$script" 2>&1)"; code=$?
want=$'install.sh --dry-run --anonymous --gateway-url wss://api.staging.example.org/v1/node/connect\ninstall.sh --anonymous --gateway-url wss://api.staging.example.org/v1/node/connect'
[[ $code -eq 0 && "$(cat "$tmp/calls")" == "$want" ]] || fail "gateway: calls were: $(cat "$tmp/calls")"
# The two defaults are single lines a test site can replace.
grep -q '^base="${AISATHOME_RELEASE_BASE:-https://github.com/aisathome/aisathome/releases/latest/download}"$' "$script" || fail "no base line"
grep -q '^gateway="${AISATHOME_GATEWAY_URL:-}"$' "$script" || fail "no gateway line"

grep -q '^set -eu$' "$script" || fail "node.sh does not stop on errors"
sh -n "$script" || fail "node.sh is not valid sh"

if [[ $fails -gt 0 ]]; then echo "node.sh: $fails failed" >&2; exit 1; fi
echo "node.sh: all tests passed"
