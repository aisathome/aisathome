#!/usr/bin/env bash
# Tests for install.sh that need no root, GPU or network: presets, overrides and input
# validation, all through --dry-run.
#
#   ./node/packaging/linux/install_test.sh
#
# SPDX-License-Identifier: Apache-2.0
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/install.sh"
fails=0 out="" code=0

# run VAR=value... -- args...: runs the installer in a clean environment, sets out and code.
run() {
  local env=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do env+=("$1"); shift; done
  shift
  out="$(env -i PATH="$PATH" "${env[@]}" bash "$installer" "$@" 2>&1)"; code=$?
}
fail() { echo "FAIL: $1"; echo "$out" | sed 's/^/    /'; fails=$((fails + 1)); }
has() { [[ "$out" == *"$2"* ]] || fail "$1: missing '$2'"; }
lacks() { [[ "$out" != *"$2"* ]] || fail "$1: unexpected '$2'"; }
ok() { [[ $code -eq 0 ]] || fail "$1: exit $code, want 0"; }
usage_err() { [[ $code -eq 2 ]] || fail "$1: exit $code, want 2"; }

# preset name, then the slots, context per slot and KV type it must produce.
check_preset() {
  local name="$1" np="$2" ctx="$3" kv="$4"
  run -- --dry-run --preset "$name"
  ok "$name"
  has "$name" "-np $np --kv-unified-per-slot $ctx "
  has "$name" "-ctk $kv -ctv $kv "
  has "$name" "--reasoning-budget 8192 "
  has "$name" "--cache-ram 2048 "
  has "$name" "--host 127.0.0.1 "
}

# Defaults: no preset given means balanced.
run -- --dry-run
ok default; has default "preset: balanced"; has default "-np 2 --kv-unified-per-slot 32768 "
has default "-ctk q8_0 -ctv q8_0 "

check_preset balanced 2 32768 q8_0
check_preset low-vram 2 32768 q4_0
check_preset long-context 1 131072 q4_0
check_preset long-context-dual 2 131072 q4_0

# PRESET from the environment, and --preset winning over it.
run PRESET=long-context -- --dry-run
ok "PRESET env"; has "PRESET env" "-np 1 --kv-unified-per-slot 131072 "
run PRESET=long-context -- --dry-run --preset low-vram
ok "--preset over PRESET"; has "--preset over PRESET" "preset: low-vram"

# Explicit variables override a preset, one at a time.
run SLOTS=3 -- --dry-run --preset long-context
ok "SLOTS override"; has "SLOTS override" "-np 3 --kv-unified-per-slot 131072 "
run CTX_PER_SLOT=65536 -- --dry-run --preset long-context-dual
ok "CTX override"; has "CTX override" "-np 2 --kv-unified-per-slot 65536 "
run KV_TYPE=q8_0 -- --dry-run --preset long-context
ok "KV override"; has "KV override" "-ctk q8_0 -ctv q8_0 "; lacks "KV override" "q4_0"
run REASONING_BUDGET=-1 CACHE_RAM_MIB=0 MODEL_ALIAS=my-model -- --dry-run
ok "other overrides"; has "other overrides" "--reasoning-budget -1 "
has "other overrides" "--cache-ram 0 "; has "other overrides" "--alias my-model "

# Invalid input is rejected with a usage error before anything is installed.
run -- --dry-run --preset turbo
usage_err "unknown preset"; has "unknown preset" "unknown preset 'turbo'"
run PRESET=Balanced -- --dry-run
usage_err "preset is case-sensitive"
run KV_TYPE=q5_1 -- --dry-run
usage_err "bad KV_TYPE"; has "bad KV_TYPE" "KV_TYPE must be"
for bad in SLOTS=0 SLOTS=two CTX_PER_SLOT=-5 REASONING_BUDGET=-2 CACHE_RAM_MIB=1.5 "MODEL_ALIAS=a b" "MODEL_ALIAS=x;y"; do
  run "$bad" -- --dry-run
  usage_err "$bad"
done
run -- --dry-run --frobnicate
usage_err "unknown flag"

# Local node UI: port, drop-in for the agent, and validation.
run -- --dry-run
ok "UI default"; has "UI default" "local UI: 7381"; has "UI default" "Environment=AISATHOME_UI_PORT=7381"
has "UI default" "Environment=AISATHOME_UI_PASSWORD_FILE=/etc/aisathome-node/ui-password"
has "UI default" "Environment=AISATHOME_STATE_FILE=/var/lib/aisathome-node/node-state.json"
has "UI default" "Environment=AISATHOME_RESTART_TRIGGER=/var/lib/aisathome-node/restart-engine"
has "UI default" 'Environment="AISATHOME_PRESET=balanced: 2 x 32768 tokens, KV q8_0"'
has "UI default" "DeviceAllow=char-nvidia-frontend rw"
run UI_PORT=8123 SLOTS=1 -- --dry-run --preset long-context
ok "UI_PORT"; has "UI_PORT" "AISATHOME_UI_PORT=8123"; has "UI_PORT" "AISATHOME_PRESET=long-context: 1 x 131072 tokens, KV q4_0"
run UI_PORT=off -- --dry-run
ok "UI off"; has "UI off" "AISATHOME_UI_PORT=off"
for bad in UI_PORT=80 UI_PORT=8000 UI_PORT=70000 UI_PORT=abc "UI_PORT=7381;x" UI_PORT=07381; do
  run "$bad" -- --dry-run
  usage_err "$bad"
done

# --node-release: download and checksum check against a fake release served over file://.
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) arch="" ;; esac
if [[ -n "$arch" ]]; then
  rel="$(mktemp -d)"; trap 'rm -rf "$rel"' EXIT
  asset="aisathome-node-linux-$arch"
  # make_release <dir>: a fake binary with a matching SHA256SUMS (plus another asset's line).
  make_release() {
    mkdir -p "$1"
    printf '#!/bin/sh\necho fake node\n' > "$1/$asset"
    (cd "$1" && sha256sum "$asset" > SHA256SUMS && echo "$(printf '0%.0s' {1..64})  aisathome-gateway-linux-$arch" >> SHA256SUMS)
  }
  make_release "$rel/latest/download"
  make_release "$rel/download/v1.2.3"
  make_release "$rel/download/v1.2.4"; echo tampered >> "$rel/download/v1.2.4/$asset"
  make_release "$rel/download/v1.2.5"; sed -i "/ $asset\$/d" "$rel/download/v1.2.5/SHA256SUMS"
  mkdir "$rel/tmp"
  base="AISATHOME_RELEASE_URL=file://$rel"

  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release latest
  ok "release latest"; has "release latest" "$asset from file://$rel/latest/download"
  has "release latest" "(verified)"; has "release latest" "-np 2 --kv-unified-per-slot 32768 "
  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release v1.2.3 --preset long-context
  ok "release tag"; has "release tag" "file://$rel/download/v1.2.3"; has "release tag" "-np 1 "
  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release v1.2.4
  [[ $code -eq 1 ]] || fail "tampered binary: exit $code, want 1"; has "tampered binary" "checksum mismatch"
  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release v1.2.5
  [[ $code -eq 1 ]] || fail "missing checksum: exit $code, want 1"; has "missing checksum" "no checksum for $asset"
  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release v9.9.9
  [[ $code -eq 1 ]] || fail "missing release: exit $code, want 1"; has "missing release" "download failed"
  [[ -z "$(ls -A "$rel/tmp")" ]] || fail "download directory left behind: $(ls "$rel/tmp")"
else
  echo "skipping --node-release tests: no release binary for $(uname -m)"
fi
for bad in 1.2.3 v1.2 "v1.2.3;x" "../v1.2.3"; do
  run -- --dry-run --node-release "$bad"
  usage_err "--node-release $bad"
done
run -- --dry-run --node-release latest --node-bin /bin/true
usage_err "--node-bin and --node-release"
run AISATHOME_RELEASE_URL=http://example.invalid -- --dry-run --node-release latest
usage_err "plain http release URL"

# Without --dry-run and without root, nothing is installed.
if [[ $EUID -ne 0 ]]; then
  run -- --preset balanced
  [[ $code -ne 0 ]] || fail "non-root install: exit 0"
  has "non-root install" "run with sudo"
fi

if [[ $fails -gt 0 ]]; then echo "$fails failure(s)"; exit 1; fi
echo "install.sh: all tests passed"
