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

# Without --dry-run and without root, nothing is installed.
if [[ $EUID -ne 0 ]]; then
  run -- --preset balanced
  [[ $code -ne 0 ]] || fail "non-root install: exit 0"
  has "non-root install" "run with sudo"
fi

if [[ $fails -gt 0 ]]; then echo "$fails failure(s)"; exit 1; fi
echo "install.sh: all tests passed"
