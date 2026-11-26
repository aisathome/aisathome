#!/usr/bin/env bash
# Tests for site/public/node.sh (the one-line install without an account, and the update of a node that
# is there already) against a fake release: it checks the bundle against SHA256SUMS, runs the installer's
# checks first, asks at the terminal (here a file) and, only on yes, installs a new node with --anonymous
# and start at boot (--enable-at-boot), or updates an installed one with no account option, as the
# installer updates a node at a terminal. What the real installer then does with each kind of installed
# token (linked, without an account, from before v0.6.0) is tested in node/packaging/linux/handoff_test.sh.
#
# SPDX-License-Identifier: Apache-2.0
set -uo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/site/public/node.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
fail() { echo "FAIL: $1" >&2; fails=$((fails + 1)); }

rel="$tmp/release"; mkdir -p "$rel/b/aisathome-node-bundle-linux-amd64"
# A fake bundle: install.sh records how it was called and, when it installs, the first line of its
# stdin (the terminal's answer file, so it reads the terminal); bin/aisathome-node is v0.7.0.
mkdir -p "$rel/b/aisathome-node-bundle-linux-amd64/bin"
cat > "$rel/b/aisathome-node-bundle-linux-amd64/install.sh" <<'SH'
#!/bin/sh
echo "install.sh${*:+ $*}" >> "$CALLS"
case "$*" in *--dry-run*) [ -z "${DRY_FAIL:-}" ] || exit 1; exit 0 ;; esac
read -r line || line=""
echo "stdin: $line" >> "$CALLS.stdin"
exit 0
SH
printf '#!/bin/sh\n[ "$1" = -version ] && echo "aisathome-node v0.7.0"\n' > "$rel/b/aisathome-node-bundle-linux-amd64/bin/aisathome-node"
chmod +x "$rel/b/aisathome-node-bundle-linux-amd64/install.sh" "$rel/b/aisathome-node-bundle-linux-amd64/bin/aisathome-node"
(cd "$rel/b" && tar -cJf "$rel/aisathome-node-bundle-linux-amd64.tar.xz" aisathome-node-bundle-linux-amd64)
(cd "$rel" && sha256sum aisathome-node-bundle-linux-amd64.tar.xz > SHA256SUMS)

# run <answer>: node.sh piped into sh as the one-liner does, the answer on the "terminal". The machine is
# $tmp/root (empty: a fresh machine).
mkdir -p "$tmp/root"
run() {
  printf '%s\n' "$1" > "$tmp/tty"; : > "$tmp/calls"; rm -f "$tmp/calls.stdin"
  out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" \
    AISATHOME_ROOT="$tmp/root" sh < "$script" 2>&1)"; code=$?
  calls="$(cat "$tmp/calls")" stdin="$(cat "$tmp/calls.stdin" 2>/dev/null)"
}

run y
[[ $code -eq 0 ]] || fail "yes: exit $code: $out"
[[ "$calls" == $'install.sh --dry-run --anonymous --enable-at-boot\ninstall.sh --anonymous --enable-at-boot' ]] || fail "yes: calls were: $calls"
for want in "does science work only, never people's chats, and earns no credit" "You must be 18 or older" "https://aisathome.org/about/terms/" "Install? [y/N]" ": OK" "MB downloaded" \
  "The node starts at boot" "sudo systemctl disable aisathome-llama aisathome-node"; do
  [[ "$out" == *"$want"* ]] || fail "yes: the output lacks '$want'"
done
[[ "$stdin" == "stdin: y" ]] || fail "yes: the installer did not read the terminal ($stdin)"
[[ "$out" != *"Existing node"* ]] || fail "yes: a fresh machine taken for an update"

for no in n "" maybe; do
  run "$no"
  [[ $code -eq 0 && "$calls" == "install.sh --dry-run --anonymous --enable-at-boot" ]] || fail "answer '$no': installed anyway ($calls)"
  [[ "$out" == *"Nothing was installed"* ]] || fail "answer '$no': no message"
done

# A machine with a node already: an update, with no account option and no --enable-at-boot (the node keeps
# its token, settings and boot choice; a token from before v0.6.0 gets the installer's link question at
# the terminal), saying so and naming both versions. Found by its unit or by its program.
mkdir -p "$tmp/root/etc/systemd/system" "$tmp/root/opt/aisathome-node/bin"
: > "$tmp/root/etc/systemd/system/aisathome-node.service"
printf '#!/bin/sh\n[ "$1" = -version ] && echo "aisathome-node v0.6.0"\n' > "$tmp/root/opt/aisathome-node/bin/aisathome-node"
chmod +x "$tmp/root/opt/aisathome-node/bin/aisathome-node"
run y
[[ $code -eq 0 ]] || fail "update: exit $code: $out"
[[ "$calls" == $'install.sh --dry-run\ninstall.sh' ]] || fail "update: calls were: $calls"
[[ "$stdin" == "stdin: y" ]] || fail "update: the installer did not read the terminal ($stdin)"
for want in "Existing node found: updating it, keeping its settings (v0.6.0 -> v0.7.0)" "Update? [y/N]" \
  "keeps its token (linked to an" "whether it
starts at boot" "installed before v0.6.0 is linked to your account again"; do
  [[ "$out" == *"$want"* ]] || fail "update: the output lacks '$want'"
done
for not in "Install? [y/N]" "earns no credit" "--anonymous" "--enable-at-boot"; do
  [[ "$out" != *"$not"* ]] || fail "update: the output has '$not'"
done
run n
[[ $code -eq 0 && "$calls" == "install.sh --dry-run" && "$out" == *"Nothing was installed"* ]] || fail "update, n: installed anyway ($calls)"
rm "$tmp/root/etc/systemd/system/aisathome-node.service"
run y
[[ "$calls" == $'install.sh --dry-run\ninstall.sh' && "$out" == *"(v0.6.0 -> v0.7.0)"* ]] || fail "update, program only: calls $calls"
: > "$tmp/root/etc/systemd/system/aisathome-node.service"; rm "$tmp/root/opt/aisathome-node/bin/aisathome-node"
run y
[[ "$calls" == $'install.sh --dry-run\ninstall.sh' && "$out" == *"(an unknown version -> v0.7.0)"* ]] || fail "update, unit only: calls $calls: $out"
printf 'y\n' > "$tmp/tty"; : > "$tmp/calls"
out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_GATEWAY_URL=wss://api.staging.example.org/v1/node/connect \
  AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" AISATHOME_ROOT="$tmp/root" sh < "$script" 2>&1)"; code=$?
want=$'install.sh --dry-run --gateway-url wss://api.staging.example.org/v1/node/connect\ninstall.sh --gateway-url wss://api.staging.example.org/v1/node/connect'
[[ $code -eq 0 && "$(cat "$tmp/calls")" == "$want" ]] || fail "update, gateway: calls were: $(cat "$tmp/calls")"
# A test site's copy has its gateway as the built-in default: that is for new nodes; an installed node keeps its
# own gateway (no --gateway-url), while a fresh machine gets the test site's.
sed 's|^gateway="${AISATHOME_GATEWAY_URL:-}"$|gateway="${AISATHOME_GATEWAY_URL:-wss://api.staging.example.org/v1/node/connect}"|' "$script" > "$tmp/site-node.sh"
grep -q 'api.staging.example.org' "$tmp/site-node.sh" || fail "the test site's copy was not made"
site_run() {
  printf 'y\n' > "$tmp/tty"; : > "$tmp/calls"
  out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" \
    AISATHOME_ROOT="$tmp/root" sh < "$tmp/site-node.sh" 2>&1)"; code=$?
}
site_run
[[ $code -eq 0 && "$(cat "$tmp/calls")" == $'install.sh --dry-run\ninstall.sh' ]] || fail "update, a test site's default gateway: calls were: $(cat "$tmp/calls")"
# A release candidate names itself in full.
printf '#!/bin/sh\n[ "$1" = -version ] && echo "aisathome-node v0.8.0-rc1"\n' > "$tmp/root/opt/aisathome-node/bin/aisathome-node"
chmod +x "$tmp/root/opt/aisathome-node/bin/aisathome-node"
run y
[[ "$out" == *"(v0.8.0-rc1 -> v0.7.0)"* ]] || fail "update, release candidate: $out"
rm -rf "$tmp/root"; mkdir -p "$tmp/root"
site_run
want=$'install.sh --dry-run --anonymous --enable-at-boot --gateway-url wss://api.staging.example.org/v1/node/connect\ninstall.sh --anonymous --enable-at-boot --gateway-url wss://api.staging.example.org/v1/node/connect'
[[ $code -eq 0 && "$(cat "$tmp/calls")" == "$want" ]] || fail "fresh, a test site's default gateway: calls were: $(cat "$tmp/calls")"

# Piped (no terminal), the download shows plain lines, not curl's bar.
[[ "$out" != *"#"* && "$out" != *$'\r'* ]] || fail "yes: a progress bar in piped output"
# A download that fails says so (and nothing is installed).
mv "$rel/aisathome-node-bundle-linux-amd64.tar.xz" "$tmp/bundle.keep"
run y
[[ $code -ne 0 && -z "$calls" && "$out" == *"download failed"* ]] || fail "a failed download: exit $code: $out"
mv "$tmp/bundle.keep" "$rel/aisathome-node-bundle-linux-amd64.tar.xz"

# A bundle that does not match SHA256SUMS is never unpacked or run.
cp "$rel/SHA256SUMS" "$tmp/sums.ok"
echo "0000000000000000000000000000000000000000000000000000000000000000  aisathome-node-bundle-linux-amd64.tar.xz" > "$rel/SHA256SUMS"
run y
[[ $code -ne 0 && -z "$calls" ]] || fail "a bad checksum: exit $code, calls $calls"
[[ "$out" == *"does not match SHA256SUMS"* ]] || fail "a bad checksum: no message: $out"
cp "$tmp/sums.ok" "$rel/SHA256SUMS"

# Failed checks: no install.
DRY_FAIL=1 run y
[[ $code -ne 0 && "$calls" == "install.sh --dry-run --anonymous --enable-at-boot" ]] || fail "failed checks: exit $code, calls $calls"

# No terminal to ask on: nothing is downloaded or installed.
printf '' > "$tmp/calls"
out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_SUDO="" AISATHOME_TTY="$tmp/none" TMPDIR="$tmp" AISATHOME_ROOT="$tmp/root" sh < "$script" 2>&1)"; code=$?
[[ $code -ne 0 && ! -s "$tmp/calls" && "$out" == *"needs a terminal"* ]] || fail "no terminal: exit $code: $out"

# A test site's copy names its gateway: both installer runs get it.
printf 'y\n' > "$tmp/tty"; : > "$tmp/calls"
out="$(CALLS="$tmp/calls" AISATHOME_RELEASE_BASE="file://$rel" AISATHOME_GATEWAY_URL=wss://api.staging.example.org/v1/node/connect \
  AISATHOME_SUDO="" AISATHOME_TTY="$tmp/tty" TMPDIR="$tmp" AISATHOME_ROOT="$tmp/root" sh < "$script" 2>&1)"; code=$?
want=$'install.sh --dry-run --anonymous --enable-at-boot --gateway-url wss://api.staging.example.org/v1/node/connect\ninstall.sh --anonymous --enable-at-boot --gateway-url wss://api.staging.example.org/v1/node/connect'
[[ $code -eq 0 && "$(cat "$tmp/calls")" == "$want" ]] || fail "gateway: calls were: $(cat "$tmp/calls")"
# The two defaults are single lines a test site can replace.
grep -q '^base="${AISATHOME_RELEASE_BASE:-https://github.com/aisathome/aisathome/releases/latest/download}"$' "$script" || fail "no base line"
grep -q '^gateway="${AISATHOME_GATEWAY_URL:-}"$' "$script" || fail "no gateway line"

grep -q '^set -eu$' "$script" || fail "node.sh does not stop on errors"
sh -n "$script" || fail "node.sh is not valid sh"

if [[ $fails -gt 0 ]]; then echo "node.sh: $fails failed" >&2; exit 1; fi
echo "node.sh: all tests passed"
