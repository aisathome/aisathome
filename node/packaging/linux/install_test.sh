#!/usr/bin/env bash
# Tests for install.sh that need no root, GPU or network: tiers, overrides, input
# validation, the prebuilt bundle's driver and GPU checks (fake nvidia-smi), the tier's free
# GPU memory check, the speed test (fake llama-server), free disk space
# (fake df), the model download (file:// URLs), through --dry-run and --download-only, and
# the suspend hook (fake cgroups and systemctl, real processes).
#
#   ./node/packaging/linux/install_test.sh
#
# SPDX-License-Identifier: Apache-2.0
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
installer="$here/install.sh"
fails=0 out="" code=0
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

# Fakes first on PATH, so no test depends on this machine's GPU or disk.
# df: FAKE_DF_KB KiB free (default 100 GB). nvidia-smi: FAKE_GPUS lines (\n-separated) for the
# driver check, FAKE_MEM lines (index, uuid, name, total MiB, free MiB) for the tier, FAKE_APPS
# lines (gpu uuid, pid, MiB) for compute processes, or fails like a broken driver with
# FAKE_SMI_FAIL=1. systemctl: only "show -p MainPID" (FAKE_MAINPID, default 0: not running).
mkdir "$tmp/fake"
cat > "$tmp/fake/df" <<'FAKE'
#!/bin/sh
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "/dev/fake 200000000 100000000 ${FAKE_DF_KB:-100000000} 50% /"
FAKE
cat > "$tmp/fake/nvidia-smi" <<'FAKE'
#!/bin/sh
if [ -n "$FAKE_SMI_FAIL" ]; then echo "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."; exit 9; fi
case "$*" in
  *memory.total*) printf '%b\n' "${FAKE_MEM-0, GPU-fake-0, NVIDIA GeForce RTX 3090, 24576, 24126}" ;;
  *query-compute-apps*) [ -z "$FAKE_APPS" ] || printf '%b\n' "$FAKE_APPS" ;;
  *) printf '%b\n' "${FAKE_GPUS-0, NVIDIA GeForce RTX 3090, 535.183.01, 8.6}" ;;
esac
FAKE
cat > "$tmp/fake/systemctl" <<'FAKE'
#!/bin/sh
case "$*" in *MainPID*) echo "${FAKE_MAINPID:-0}" ;; *) exit 1 ;; esac
FAKE
chmod +x "$tmp/fake/df" "$tmp/fake/nvidia-smi" "$tmp/fake/systemctl"
PATH="$tmp/fake:$PATH"

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

# The tier follows the card's size: big (192k) on 24 GB and more, standard (80k) on 16 GB.
# card <total MiB> <free MiB> [name]: one fake GPU.
card() { echo "0, GPU-fake-0, ${3:-NVIDIA GeForce RTX 3090}, $1, $2"; }
check_tier() {  # check_tier <FAKE_MEM> <tier> <ctx>
  run FAKE_MEM="$1" -- --dry-run
  ok "tier $2 ($1)"
  has "tier $2 ($1)" "tier: $2: 1 x $3 tokens, KV q4_0"
  has "tier $2 ($1)" "-np 1 --kv-unified-per-slot $3 "
  has "tier $2 ($1)" "-ctk q4_0 -ctv q4_0 "
  has "tier $2 ($1)" '"slots": 1, "ctx": '"$3"
  has "tier $2 ($1)" "--reasoning-budget 8192 "; has "tier $2 ($1)" "--cache-ram 2048 "; has "tier $2 ($1)" "--host 127.0.0.1 "
  has "tier $2 ($1)" "at least 20 tokens/s after a 32000-token prompt"
}
run -- --dry-run
ok default; has default "GPU 0: NVIDIA GeForce RTX 3090, 24576 MiB: the big tier (192k context)"
has default "memory: 24126 MiB free, the big tier needs 18500: fits"
check_tier "$(card 24576 24126)" big 196608
check_tier "$(card 24564 21979 'NVIDIA GeForce RTX 4090')" big 196608
check_tier "$(card 32607 32100 'NVIDIA GeForce RTX 5090')" big 196608
check_tier "$(card 16303 15841 'NVIDIA GeForce RTX 5070 Ti')" standard 81920
check_tier "$(card 16376 15900 'NVIDIA GeForce RTX 4080')" standard 81920
check_tier "$(card 20475 20000 'NVIDIA RTX 4000 Ada Generation')" standard 81920
# Too small a card, or too little free memory for the tier: a clear stop, never a smaller tier.
run FAKE_MEM="$(card 12282 11900 'NVIDIA GeForce RTX 4070')" -- --dry-run
[[ $code -eq 1 ]] || fail "12 GB card: exit $code, want 1"; has "12 GB card" "has 12282 MiB of memory; the network needs a card with 16 GB or more"
run FAKE_MEM="$(card 16303 15199 'NVIDIA GeForce RTX 5070 Ti')" -- --dry-run
[[ $code -eq 1 ]] || fail "16 GB with a display: exit $code, want 1"
has "16 GB with a display" "has 15199 MiB free; the standard tier (80k context) needs 15200 MiB free"
has "16 GB with a display" "Connect the monitor to the motherboard's graphics"; lacks "16 GB with a display" "tier: big"
run FAKE_MEM="$(card 16303 15200 'NVIDIA GeForce RTX 5070 Ti')" -- --dry-run
ok "16 GB at the limit"; has "16 GB at the limit" "the standard tier needs 15200: fits"
run FAKE_MEM="$(card 24576 17000)" -- --dry-run
[[ $code -eq 1 ]] || fail "24 GB, busy desktop: exit $code, want 1"
has "24 GB, busy desktop" "has 17000 MiB free; the big tier (192k context) needs 18500 MiB free"; lacks "24 GB, busy desktop" "standard tier"
# An update: the memory this node's own running llama-server holds counts as free.
run FAKE_MEM="$(card 24576 5000)" FAKE_MAINPID=4242 FAKE_APPS="GPU-fake-0, 4242, 18300\nGPU-fake-0, 999, 700" -- --dry-run
ok "update frees own memory"; has "update frees own memory" "memory: 23300 MiB free (incl. 18300 MiB of this node's running llama-server)"
run FAKE_MEM="$(card 24576 5000)" FAKE_MAINPID=4242 FAKE_APPS="GPU-fake-1, 4242, 18300" -- --dry-run
[[ $code -eq 1 ]] || fail "own memory on another GPU: exit $code, want 1"
# Several GPUs: the largest card is used (on a tie, the freer one), and llama-server is pinned to it.
run FAKE_MEM="0, GPU-aaa, NVIDIA GeForce RTX 4080, 16376, 15000\n1, GPU-bbb, NVIDIA GeForce RTX 3090, 24576, 24000" -- --dry-run
ok "two GPUs"; has "two GPUs" "GPU 1: NVIDIA GeForce RTX 3090, 24576 MiB: the big tier (192k context); llama-server uses only this GPU"
has "two GPUs" "Environment=CUDA_VISIBLE_DEVICES=GPU-bbb"
run FAKE_MEM="0, GPU-aaa, NVIDIA GeForce RTX 3090, 24576, 20000\n1, GPU-bbb, NVIDIA GeForce RTX 3090, 24576, 24000" -- --dry-run
ok "two equal GPUs"; has "two equal GPUs" "CUDA_VISIBLE_DEVICES=GPU-bbb"
# An update on two equal cards: the node's own card has little free memory because its running
# llama-server holds it; the other card has a busy desktop. Counted before the cards are
# compared, the node's card is the freer one (23300 MiB against 17000) and is kept.
run FAKE_MEM="0, GPU-aaa, NVIDIA GeForce RTX 3090, 24576, 5000\n1, GPU-bbb, NVIDIA GeForce RTX 3090, 24576, 17000" FAKE_MAINPID=4242 FAKE_APPS="GPU-aaa, 4242, 18300\nGPU-bbb, 777, 7000" -- --dry-run
ok "update, two equal GPUs"; has "update, two equal GPUs" "CUDA_VISIBLE_DEVICES=GPU-aaa"
has "update, two equal GPUs" "memory: 23300 MiB free (incl. 18300 MiB of this node's running llama-server)"
run -- --dry-run
lacks "one GPU, no pin" "CUDA_VISIBLE_DEVICES"
run FAKE_MEM="Field \"uuid\" is not a valid field to query." -- --dry-run
[[ $code -eq 1 ]] || fail "unreadable memory: exit $code, want 1"; has "unreadable memory" "cannot read the GPU memory from nvidia-smi"

# The old presets: long-context (the v0.2.0 setting) is accepted and ignored; others are errors.
run -- --dry-run --preset long-context
ok "--preset long-context"; has "--preset long-context" "tier: big: 1 x 196608 tokens"
run PRESET=long-context -- --dry-run
ok "PRESET env"; has "PRESET env" "-np 1 --kv-unified-per-slot 196608 "
for gone in balanced low-vram long-context-dual turbo; do
  run -- --dry-run --preset "$gone"
  usage_err "removed preset $gone"; has "removed preset $gone" "unknown preset '$gone'; presets are gone"
done

# Explicit variables override the tier, one at a time (testing); the free-memory check is skipped.
run SLOTS=3 -- --dry-run
ok "SLOTS override"; has "SLOTS override" "-np 3 --kv-unified-per-slot 196608 "; has "SLOTS override" "check is skipped"
run CTX_PER_SLOT=65536 -- --dry-run
ok "CTX override"; has "CTX override" "-np 1 --kv-unified-per-slot 65536 "
run CTX_PER_SLOT=65536 FAKE_MEM="$(card 16303 14000)" -- --dry-run
ok "CTX override, busy card"; has "CTX override, busy card" "tier: test settings (card: standard): 1 x 65536 tokens"
run KV_TYPE=q8_0 -- --dry-run
ok "KV override"; has "KV override" "-ctk q8_0 -ctv q8_0 "; lacks "KV override" "q4_0"
run REASONING_BUDGET=-1 CACHE_RAM_MIB=0 MODEL_ALIAS=my-model MIN_DECODE_TPS=12.5 -- --dry-run
ok "other overrides"; has "other overrides" "--reasoning-budget -1 "
has "other overrides" "--cache-ram 0 "; has "other overrides" "--alias my-model "; has "other overrides" "at least 12.5 tokens/s"

# Invalid input is rejected with a usage error before anything is installed.
run PRESET=Balanced -- --dry-run
usage_err "preset is case-sensitive"
run KV_TYPE=q5_1 -- --dry-run
usage_err "bad KV_TYPE"; has "bad KV_TYPE" "KV_TYPE must be"
for bad in SLOTS=0 SLOTS=two CTX_PER_SLOT=-5 REASONING_BUDGET=-2 CACHE_RAM_MIB=1.5 "MODEL_ALIAS=a b" "MODEL_ALIAS=x;y" MIN_DECODE_TPS=-1 MIN_DECODE_TPS=fast; do
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
has "UI default" 'Environment="AISATHOME_PRESET=big: 1 x 196608 tokens, KV q4_0"'
# GPU access in the rendered units: major 195 is "nvidia-frontend" in /proc/devices up to driver
# 535 and "nvidia" from 545 on; with only the old name, driver 575 ran on the CPU (2026-10-18).
# device_rules <name> <unit text> <rule>...: each rule is a line of the unit.
device_rules() {
  local name="$1" text="$2" rule; shift 2
  for rule in "$@"; do grep -qxF "$rule" <<<"$text" || fail "$name: no line '$rule'"; done
}
dropin="$(sed -n '/^local UI: /,/^aisathome-llama.service:$/p' <<<"$out")"
llama="$(sed -n '/^aisathome-llama.service:$/,$p' <<<"$out")"
device_rules "UI drop-in" "$dropin" "DeviceAllow=char-nvidia-frontend rw" "DeviceAllow=char-nvidia rw"
device_rules "llama unit" "$llama" "DeviceAllow=char-nvidia-frontend rw" "DeviceAllow=char-nvidia rw" \
  "DeviceAllow=char-nvidia-uvm rw" "DeviceAllow=char-nvidia-caps r" "ProtectClock=yes" "User=aisathome" \
  "After=nvidia-resume.service" "Environment=GGML_NO_BACKTRACE=1"
grep -q '^ExecStart=/opt/aisathome-node/bin/llama-server -m /var/lib/aisathome-node/models/model.gguf ' <<<"$llama" ||
  fail "llama unit: no ExecStart"
run UI_PORT=8123 FAKE_MEM="$(card 16303 15841)" -- --dry-run
ok "UI_PORT"; has "UI_PORT" "AISATHOME_UI_PORT=8123"; has "UI_PORT" "AISATHOME_PRESET=standard: 1 x 81920 tokens, KV q4_0"
run UI_PORT=off -- --dry-run
ok "UI off"; has "UI off" "AISATHOME_UI_PORT=off"
for bad in UI_PORT=80 UI_PORT=8000 UI_PORT=70000 UI_PORT=abc "UI_PORT=7381;x" UI_PORT=07381; do
  run "$bad" -- --dry-run
  usage_err "$bad"
done

# --node-release: download and checksum check against a fake release served over file://.
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) arch="" ;; esac
if [[ -n "$arch" ]]; then
  rel="$tmp/rel"; mkdir "$rel"
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
  has "release latest" "(verified)"; has "release latest" "-np 1 --kv-unified-per-slot 196608 "
  run "$base" TMPDIR="$rel/tmp" -- --dry-run --node-release v1.2.3
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

# The pinned model and a generated node.json, from the repository (no bundle): a dry run
# shows the download plan and checks the free disk space.
run -- --dry-run --node-id test-node
ok "model plan"; has "model plan" "model: https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502/Qwen3.8-27B-UD-IQ4_XS.gguf"
has "model plan" "sha256 40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199, 14.3 GB"
has "model plan" "disk: 102.4 GB free"; lacks "model plan" "compute capability"
has "model plan" '"node_id": "test-node"'; has "model plan" '"gateway_url": "wss://api.aisathome.org/v1/node/connect"'
has "model plan" '"name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 1, "ctx": 196608'
# Sizes print with a decimal point in any locale (a comma locale gave "16,5 GB").
comma_locale="$(locale -a 2>/dev/null | grep -m1 -iE '^(nb_NO|de_DE|fr_FR)\.utf-?8$' || true)"
if [[ -n "$comma_locale" ]]; then
  run LC_ALL="$comma_locale" -- --dry-run
  ok "comma locale"; has "comma locale" "14.3 GB) to"; lacks "comma locale" "14,3"
fi
run FAKE_DF_KB=10000000 -- --dry-run
[[ $code -eq 1 ]] || fail "no disk space: exit $code, want 1"; has "no disk space" "not enough disk space"
# --model: only the pinned file (checked by size in a dry run, by SHA-256 when installing).
head -c 4096 /dev/urandom > "$tmp/own-model.gguf"
osha="$(sha256sum "$tmp/own-model.gguf" | cut -d' ' -f1)"
run FAKE_DF_KB=10000000 MODEL_URL=https://example.org/m/own-model.gguf MODEL_SHA256="$osha" MODEL_SIZE=4096 -- --dry-run --model "$tmp/own-model.gguf"
ok "--model skips the download"; lacks "--model skips the download" "example.org"
run -- --dry-run --model /etc/hostname
[[ $code -eq 1 ]] || fail "--model, another file: exit $code, want 1"
has "--model, another file" "/etc/hostname is not the network's model"; has "--model, another file" "the pinned Qwen3.8-27B-UD-IQ4_XS.gguf has 14252845984"
eval "$(sed -n '/^check_model_file() {/,/^}/p' "$installer")"
die() { echo "$1"; exit 1; }
out="$(model_size=4096 model_sha="$osha" model_name=own-model.gguf check_model_file "$tmp/own-model.gguf" 1 2>&1)"; code=$?
ok "check_model_file, the pinned file"; has "check_model_file, the pinned file" "(verified)"
out="$(model_size=4096 model_sha="$(printf 'a%.0s' {1..64})" model_name=own-model.gguf check_model_file "$tmp/own-model.gguf" 1 2>&1)"; code=$?
[[ $code -ne 0 ]] || fail "check_model_file, same size, other hash: accepted"; has "check_model_file, other hash" "is not the network's model: sha256 $osha"
unset -f check_model_file die
run -- --dry-run --model /nonexistent.gguf
[[ $code -eq 1 ]] || fail "missing model file: exit $code, want 1"; has "missing model file" "no model file"
run FAKE_MEM="$(card 16303 15841)" -- --dry-run --gateway-url wss://gw.example.org:8443/v1/node/connect --node-id n1
ok "node.json follows the tier"; has "node.json follows the tier" '"slots": 1, "ctx": 81920'
has "node.json follows the tier" '"gateway_url": "wss://gw.example.org:8443/v1/node/connect"'
run -- --dry-run --node-config /etc/hostname
ok "--node-config"; lacks "--node-config" "node.json (generated)"
for bad in "--node-id a\ b" "--node-id -x" "--gateway-url http://gw.example.org" "--gateway-url wss://gw/x?y" "--node-config /etc/hostname --node-id n1"; do
  eval "run -- --dry-run $bad"
  usage_err "$bad"
done
for bad in "MODEL_URL=file:///x.gguf" "MODEL_URL=http://x/m.gguf MODEL_SHA256=$(printf 'a%.0s' {1..64}) MODEL_SIZE=1" \
           "MODEL_URL=file:///x.gguf MODEL_SHA256=abc MODEL_SIZE=1" "MODEL_URL=file:///x.gguf MODEL_SHA256=$(printf 'a%.0s' {1..64}) MODEL_SIZE=0"; do
  eval "run $bad -- --dry-run"
  usage_err "$bad"
done

# pick_node_id on its own: the real install calls it with the pasted token, which a dry run
# never reads. An account-page token (nt-<id>.<secret>) names the node; --node-id wins.
out=""
eval "$(sed -n '/^pick_node_id() {/,/^}/p' "$installer")"
die() { echo "$1"; return 1; }
check_id() { node_id="$1"; pick_node_id "$2"; [[ "$node_id" = "$3" ]] || fail "node id from '$2' (--node-id '$1'): got '$node_id', want '$3'"; }
check_id "" "nt-n-0123abcd.secret" n-0123abcd
check_id mine "nt-n-0123abcd.secret" mine
check_id "" "config-token" "$(hostname -s)"
check_id "" "nt-n-0123ABCD.secret" "$(hostname -s)"
check_id "" "nt-n-0123abcd" "$(hostname -s)"
unset -f die check_id pick_node_id

# Rerunning on an installed node (an update): the installed token, node id and gateway URL
# are kept unless the operator gives new ones. The functions on their own, with a fake
# /etc/aisathome-node, then a dry run of a copy of the installer that reads it.
etc="$tmp/etc"; mkdir "$etc"
eval "$(sed -n '/^installed_value() {/,/^}/p;/^pick_gateway_url() {/,/^}/p;/^pick_node_id() {/,/^}/p;/^read_token() {/,/^}/p' "$installer")"
die() { echo "$1" >&2; exit "${2:-1}"; }
printf '{"node_id": "box-1", "gateway_url": "wss://staging.example.org/v1/node/connect", "slots": 2}\n' > "$etc/node.json"
secret="nt-n-0123abcd.$(printf 'S%.0s' {1..43})"
printf 'AISATHOME_NODE_TOKEN=%s\n' "$secret" > "$etc/token.env"; chmod 600 "$etc/token.env"
[[ "$(installed_value "$etc/node.json" node_id)" = box-1 ]] || fail "installed node_id"
[[ "$(installed_value "$etc/node.json" gateway_url)" = wss://staging.example.org/v1/node/connect ]] || fail "installed gateway_url"
[[ -z "$(installed_value "$etc/node.json" slots)" && -z "$(installed_value "$etc/none.json" node_id)" ]] || fail "installed_value: not a string, or no file"
echo '{broken' > "$etc/bad.json"
[[ -z "$(installed_value "$etc/bad.json" node_id 2>&1)" ]] || fail "installed_value: invalid JSON"
# gw <flag> <installed>: the gateway URL picked, or the error.
gw() { (gateway_url="$1"; pick_gateway_url "$2"; echo "$gateway_url") 2>&1; }
[[ "$(gw "" wss://staging.example.org/v1/node/connect)" = wss://staging.example.org/v1/node/connect ]] || fail "gateway: installed kept"
[[ "$(gw wss://gw.example.org/v1/node/connect wss://staging.example.org/v1/node/connect)" = wss://gw.example.org/v1/node/connect ]] || fail "gateway: flag wins"
[[ "$(gw "" "")" = wss://api.aisathome.org/v1/node/connect ]] || fail "gateway: default"
[[ "$(gw "" 'http://x;y')" = *"installed gateway_url"* ]] || fail "gateway: an invalid installed URL is refused"
check_id() { local got; got="$( (node_id="$1"; pick_node_id "$2" "$3"; echo "$node_id") 2>&1)"
  [[ "$got" = "$4" ]] || fail "node id (--node-id '$1', token '$2', installed '$3'): got '$got', want '$4'"; }
check_id "" config-token box-1 box-1
check_id mine config-token box-1 mine
check_id "" "nt-n-0123abcd.x" box-1 n-0123abcd
check_id "" config-token "" "$(hostname -s)"
check_id "" config-token 'a b' "the installed node id 'a b' is not valid; pass --node-id"
# tok <tty 0|1> <keep 0|1> <token.env> <stdin>: sets out (stderr, then token=...) and code.
tok() { out="$( (fake_tty=$1; interactive() { [[ $fake_tty = 1 ]]; }; keep_token=$2; read_token "$3"; echo "token=$token") <<<"$4" 2>&1)"; code=$?; }
tok 1 0 "$etc/token.env" ""
ok "Enter keeps the token"; has "Enter keeps the token" "token=$secret"; has "Enter keeps the token" "or press Enter to keep the installed one"
tok 1 0 "$etc/token.env" "nt-new"
ok "a pasted token replaces it"; has "a pasted token replaces it" "token=nt-new"
tok 0 1 "$etc/token.env" "nt-ignored"
ok "--keep-token"; has "--keep-token" "token=$secret"; lacks "--keep-token" "nt-ignored"
tok 0 0 "$etc/token.env" "nt-piped"
ok "piped token"; has "piped token" "token=nt-piped"
tok 0 0 "$etc/token.env" ""
[[ $code -ne 0 ]] || fail "empty piped input kept the token"; has "empty piped input" "--keep-token"
tok 1 0 "$etc/none.env" ""
[[ $code -ne 0 ]] || fail "Enter without an installed token"; has "Enter without an installed token" "no token on stdin"
tok 0 1 "$etc/none.env" ""
usage_err "--keep-token without an installed token"
# The secret appears only where the test prints it: never in a message.
for t in "1 0" "0 1"; do
  tok $t "$etc/token.env" ""; [[ "${out%token=*}" != *"$secret"* ]] || fail "the token was printed"
done
unset -f die gw check_id tok installed_value pick_gateway_url pick_node_id read_token interactive
# A dry run reads the installed node.json (a copy of the installer pointed at the fake one).
sed "s|^conf=/etc/aisathome-node\$|conf=$etc|" "$installer" > "$tmp/install-conf.sh"
grep -q "^conf=$etc\$" "$tmp/install-conf.sh" || fail "no conf= line to point at the fake install"
out="$(env -i PATH="$PATH" bash "$tmp/install-conf.sh" --dry-run 2>&1)"; code=$?
ok "rerun keeps node.json"; has "rerun keeps node.json" '"node_id": "box-1"'
has "rerun keeps node.json" '"gateway_url": "wss://staging.example.org/v1/node/connect"'
out="$(env -i PATH="$PATH" bash "$tmp/install-conf.sh" --dry-run --node-id n2 --gateway-url wss://gw.example.org/v1/node/connect 2>&1)"; code=$?
ok "flags over installed"; has "flags over installed" '"node_id": "n2"'; has "flags over installed" '"gateway_url": "wss://gw.example.org/v1/node/connect"'

# install_libs on its own: a bundle unpacked under umask 077 (owner-only files) must still give
# libraries the aisathome service user can read, and keep the version symlinks.
eval "$(sed -n '/^install_libs() {/,/^}/p' "$installer")"
mkdir -p "$tmp/libsrc/lib" "$tmp/libdst"
(umask 077; : > "$tmp/libsrc/lib/libllama.so.0.5.0"; : > "$tmp/libsrc/lib/libllama-server-impl.so"
 chmod 0700 "$tmp/libsrc/lib/libllama.so.0.5.0"; ln -s libllama.so.0.5.0 "$tmp/libsrc/lib/libllama.so.0")
install_libs "$tmp/libsrc" "$tmp/libdst"
for f in libllama.so.0.5.0 libllama-server-impl.so; do
  [[ "$(stat -c %A "$tmp/libdst/$f" 2>/dev/null)" =~ ^-r..r..r.. ]] || fail "install_libs: $f is $(stat -c %A "$tmp/libdst/$f" 2>/dev/null), want readable by all"
done
[[ -L "$tmp/libdst/libllama.so.0" ]] || fail "install_libs: the libllama.so.0 symlink was not kept"
unset -f install_libs

# The device rules against /proc/devices as two drivers list it. systemd allows, for each
# DeviceAllow=char-<pattern>, the majors whose name under "Character devices:" matches the
# glob <pattern>, and skips a pattern that matches nothing (src/core/bpf-devices.c, systemd
# v249 and main). majors <unit text> <proc devices file>: the majors the unit allows.
majors() {
  local pat line maj name in
  while read -r pat; do
    in=0
    while IFS= read -r line; do
      if [[ "$line" = "Character devices:" ]]; then in=1; continue; fi
      if [[ -z "$line" ]]; then in=0; continue; fi
      [[ $in = 1 ]] || continue
      read -r maj name <<<"$line"
      # a glob, like systemd's fnmatch
      if [[ "$name" == $pat ]]; then echo "$maj"; fi
    done < "$2"
  done < <(sed -n 's/^DeviceAllow=char-\([^ ]*\) .*/\1/p' <<<"$1") | sort -un | paste -sd' ' -
}
# Driver 530 (RTX 4090 desktop) and 575.51.03 (RTX 3090 VM), both read on 2026-10-18.
printf 'Character devices:\n  1 mem\n195 nvidia-frontend\n234 nvidia-caps\n509 nvidia-uvm\n510 nvidia-nvswitch\n511 nvidia-nvlink\n\nBlock devices:\n195 nvidia\n259 blkext\n' > "$tmp/devices-530"
printf 'Character devices:\n  1 mem\n195 nvidia\n195 nvidia-modeset\n195 nvidiactl\n236 nvidia-uvm\n239 nvidia-caps\n\nBlock devices:\n259 blkext\n' > "$tmp/devices-575"
for drv in "530 509" "575 236"; do
  read -r v uvm <<<"$drv"
  got="$(majors "$llama" "$tmp/devices-$v")"
  [[ " $got " == *" 195 "* && " $got " == *" $uvm "* ]] || fail "llama unit, driver $v: allows majors '$got', want 195 (GPU) and $uvm (nvidia-uvm)"
  got="$(majors "$dropin" "$tmp/devices-$v")"
  [[ " $got " == *" 195 "* ]] || fail "UI drop-in, driver $v: allows majors '$got', want 195 (GPU, for nvidia-smi)"
done

# engine_on_gpu on its own (fake journalctl and systemctl): llama-server on the CPU stops
# the install, and the engine is stopped and left disabled rather than running on the CPU.
eval "$(sed -n '/^engine_on_gpu() {/,/^}/p' "$installer")"
die() { echo "$1"; return 1; }
e="$tmp/engine"; mkdir -p "$e"
printf '#!/bin/sh\nprintf "%%s\\n" "$FAKE_JOURNAL"\nseq "${FAKE_JOURNAL_REPEAT:-0}" | sed "s/^/restarting, line /"\n' > "$e/journalctl"
printf '#!/bin/sh\necho "systemctl $*" >> "%s/systemctl.log"\n' "$e" > "$e/systemctl"
chmod +x "$e/journalctl" "$e/systemctl"
check_engine() {
  : > "$e/systemctl.log"
  out="$(export PATH="$e:$PATH" FAKE_JOURNAL="$1"; engine_on_gpu "2026-10-18 17:40:00")"; code=$?
  out+=$'\n'"$(cat "$e/systemctl.log")"
}
check_engine $'load_backend: loaded CUDA backend\nggml_cuda_init: found 1 CUDA devices:'
ok "engine on the GPU"; lacks "engine on the GPU" "systemctl"
check_engine $'E ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\nwarning: no usable GPU found, --gpu-layers option will be ignored'
[[ $code -eq 1 ]] || fail "engine on the CPU: exit $code, want 1"
has "engine on the CPU" "ran on the CPU, so the installer stopped it"
has "engine on the CPU" "systemctl disable --now aisathome-node aisathome-llama"
unset -f engine_on_gpu die check_engine

# engine_fits on its own: llama-server out of GPU memory at start stops the install with the
# tier's advice and leaves the engine disabled (systemd would restart it every 5 s).
eval "$(sed -n '/^engine_fits() {/,/^}/p;/^fit_advice() {/,/^}/p;/^engine_on_gpu() {/,/^}/p' "$installer")"
die() { echo "$1"; return 1; }
: > "$e/systemctl.log"
out="$(export PATH="$e:$PATH" FAKE_JOURNAL=$'llama_kv_cache: CUDA0 KV buffer size = 1440.00 MiB\nggml_backend_cuda_buffer_type_alloc_buffer: allocating 480.00 MiB on device 0: cudaMalloc failed: out of memory'; tier=standard; engine_fits "2026-10-23 10:00:00")"; code=$?
out+=$'\n'"$(cat "$e/systemctl.log")"
[[ $code -eq 1 ]] || fail "engine out of memory: exit $code, want 1"
has "engine out of memory" "ran out of GPU memory while loading the standard tier"; has "engine out of memory" "Connect the monitor to the motherboard"
has "engine out of memory" "systemctl disable --now aisathome-node aisathome-llama"
# A long log after the match (systemd restarting the engine for minutes): a "grep -q" would end
# early, journalctl would get SIGPIPE, and under pipefail the check would pass silently
# (2026-10-23, RTX 4080 SUPER VM). FAKE_JOURNAL_REPEAT adds that many lines after the log.
out="$(export PATH="$e:$PATH" FAKE_JOURNAL="cudaMalloc failed: out of memory" FAKE_JOURNAL_REPEAT=50000; tier=standard; engine_fits "2026-10-23 10:00:00")"; code=$?
[[ $code -eq 1 ]] || fail "engine out of memory, long log: exit $code, want 1"
out="$(export PATH="$e:$PATH" FAKE_JOURNAL="warning: no usable GPU found, --gpu-layers option will be ignored" FAKE_JOURNAL_REPEAT=50000; engine_on_gpu "2026-10-23 10:00:00" 2>&1)"; code=$?
[[ $code -eq 1 ]] || fail "engine on the CPU, long log: exit $code, want 1"
: > "$e/systemctl.log"
out="$(export PATH="$e:$PATH" FAKE_JOURNAL="main: model loaded"; tier=big; engine_fits "2026-10-23 10:00:00")"; code=$?
ok "engine not out of memory"; [[ ! -s "$e/systemctl.log" ]] || fail "engine not out of memory: systemctl called"
unset -f engine_fits fit_advice engine_on_gpu die

# The speed test against a fake llama-server: /tokenize, and /completion with llama-server's
# timings. FAKE_TPS sets the decode speed it reports; it records the request it got.
fs="$tmp/fakeserver"; mkdir -p "$fs"
cat > "$fs/server.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path == "/tokenize":
            out = {"tokens": list(range(len(body["content"].split())))}
        else:
            open(os.path.join(sys.argv[2], "req.json"), "w").write(json.dumps({k: v for k, v in body.items() if k != "prompt"} | {"prompt_len": len(body["prompt"])}))
            tps = float(open(os.path.join(sys.argv[2], "tps")).read())
            if tps < 0: self.send_response(500); self.end_headers(); return
            out = {"timings": {"prompt_n": len(body["prompt"]), "prompt_ms": 23456.0, "predicted_per_second": tps}}
        data = json.dumps(out).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(data)
s = HTTPServer(("127.0.0.1", 0), H); open(sys.argv[1], "w").write(str(s.server_port)); s.serve_forever()
PY
echo 30 > "$fs/tps"
python3 "$fs/server.py" "$fs/port" "$fs" & fspid=$!
for _ in $(seq 50); do [[ -s "$fs/port" ]] && break; sleep 0.1; done
eval "$(sed -n '/^speed_probe() {/,/^}/p;/^speed_check() {/,/^}/p' "$installer")"
die() { echo "$1"; return 1; }
speed() {  # speed <tokens/s the fake reports> [min]: runs speed_check, sets out and code
  echo "$1" > "$fs/tps"; : > "$e/systemctl.log"
  out="$(export PATH="$e:$PATH"; engine_url="http://127.0.0.1:$(cat "$fs/port")" speed_ctx=32000 min_tps="${2:-20}" \
    gpu_idx=0 gpu_name="NVIDIA GeForce RTX 5060 Ti" speed_check 2>&1)"; code=$?
  out+=$'\n'"$(cat "$e/systemctl.log")"
}
speed 28.9
ok "speed fast enough"; has "speed fast enough" "speed: 28.9 tokens/s after a 32000-token prompt (read in 23 s); the network needs at least 20"
lacks "speed fast enough" "systemctl disable"
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); sys.exit(0 if r["prompt_len"] == 32000 and r["n_predict"] == 256 and r["ignore_eos"] and r["cache_prompt"] is False else 1)' "$fs/req.json" ||
  fail "speed test request: $(cat "$fs/req.json")"
speed 13.8
[[ $code -eq 1 ]] || fail "speed too slow: exit $code, want 1"
has "speed too slow" "GPU 0 (NVIDIA GeForce RTX 5060 Ti) writes 13.8 tokens/s after a 32000-token prompt; the network needs at least 20"
has "speed too slow" "systemctl disable --now aisathome-node aisathome-llama"
speed 20.0
ok "speed at the floor"
speed 13.8 0
ok "MIN_DECODE_TPS=0 never refuses"
speed -1
[[ $code -eq 1 ]] || fail "speed test error: exit $code, want 1"; has "speed test error" "the speed test failed (HTTPError"
has "speed test error" "systemctl disable --now"
kill "$fspid" 2>/dev/null; wait "$fspid" 2>/dev/null
unset -f speed_probe speed_check speed die

# uninstall.sh in a fake root (fake systemctl and userdel): removes the units, their drop-ins
# (also one made with "systemctl edit"), binaries and config; keeps the model unless --purge.
u="$tmp/uninstall"; mkdir -p "$u/bin"
for c in systemctl userdel; do printf '#!/bin/sh\necho "%s $*" >> "%s/log"\n' "$c" "$u" > "$u/bin/$c"; chmod +x "$u/bin/$c"; done
uninstall() {
  : > "$u/log"
  out="$(env -i PATH="$u/bin:$PATH" AISATHOME_UNINSTALL_ROOT="$u/root" bash "$here/uninstall.sh" "$@" 2>&1)"; code=$?
  out+=$'\n'"$(cat "$u/log")"
}
fake_install() {
  local sd="$u/root/etc/systemd/system"
  mkdir -p "$sd/aisathome-node.service.d" "$sd/aisathome-llama.service.d" "$sd/other.service.d" \
    "$u/root/opt/aisathome-node/bin" "$u/root/etc/aisathome-node" "$u/root/var/lib/aisathome-node/models" \
    "$u/root/usr/local/bin" "$u/root/usr/local/libexec"
  for f in aisathome-node.service aisathome-llama.service aisathome-gpu-init.service aisathome-node-sleep.service \
           aisathome-node-watchdog.service aisathome-node-watchdog.timer aisathome-node-restart.path \
           aisathome-node-restart.service aisathome-node.service.d/local-ui.conf \
           aisathome-llama.service.d/override.conf other.service.d/keep.conf; do : > "$sd/$f"; done
  : > "$u/root/usr/local/bin/aisathome-node"; : > "$u/root/var/lib/aisathome-node/models/model.gguf"
}
fake_install
uninstall
ok "uninstall"; has "uninstall" "kept /var/lib/aisathome-node"; lacks "uninstall" "userdel"
has "uninstall" "systemctl disable --now aisathome-node-watchdog.timer aisathome-node aisathome-llama"
left="$(cd "$u/root" && find etc opt usr -name '*aisathome*')"
[[ -z "$left" ]] || fail "uninstall: left behind: $left"
[[ -e "$u/root/etc/systemd/system/other.service.d/keep.conf" ]] || fail "uninstall: removed another unit's drop-in"
[[ -e "$u/root/var/lib/aisathome-node/models/model.gguf" ]] || fail "uninstall: removed the model without --purge"
fake_install
uninstall --purge
ok "uninstall --purge"; has "uninstall --purge" "userdel aisathome"
[[ ! -e "$u/root/var/lib/aisathome-node" ]] || fail "uninstall --purge: kept /var/lib/aisathome-node"
[[ -z "$(cd "$u/root" && find etc opt usr -name '*aisathome*')" ]] || fail "uninstall --purge: left files behind"

# --download-only: a fake model served over file://, checked against its SHA-256.
mkdir -p "$tmp/hf" "$tmp/dl1" "$tmp/dl2" "$tmp/dl3" "$tmp/dl4"
head -c 300000 /dev/urandom > "$tmp/hf/test-model.gguf"
msha="$(sha256sum "$tmp/hf/test-model.gguf" | cut -d' ' -f1)"
m=(MODEL_URL="file://$tmp/hf/test-model.gguf" MODEL_SHA256="$msha" MODEL_SIZE=300000)
run "${m[@]}" -- --download-only "$tmp/dl1"
ok "download"; has "download" "(verified)"; has "download" "Install with --model $tmp/dl1/test-model.gguf"
cmp -s "$tmp/hf/test-model.gguf" "$tmp/dl1/test-model.gguf" || fail "download: file differs"
# A complete file is checked, not downloaded again (the URL now points nowhere).
run MODEL_URL="file://$tmp/gone/test-model.gguf" MODEL_SHA256="$msha" MODEL_SIZE=300000 -- --download-only "$tmp/dl1"
ok "existing model"; has "existing model" "checking the existing"; lacks "existing model" "downloading"
# Resume: an interrupted download continues from the .part file.
head -c 120000 "$tmp/hf/test-model.gguf" > "$tmp/dl2/test-model.gguf.part"
run "${m[@]}" -- --download-only "$tmp/dl2"
ok "resume"; has "resume" "resuming at 0.0 of 0.0 GB"; has "resume" "(verified)"
cmp -s "$tmp/hf/test-model.gguf" "$tmp/dl2/test-model.gguf" || fail "resume: file differs"
[[ ! -e "$tmp/dl2/test-model.gguf.part" ]] || fail "resume: .part left behind"
# Verification failure: wrong hash, and a corrupt .part (resumed into a bad file). Both
# delete the download, so the next run starts clean.
run MODEL_URL="file://$tmp/hf/test-model.gguf" MODEL_SHA256="$(printf '0%.0s' {1..64})" MODEL_SIZE=300000 -- --download-only "$tmp/dl3"
[[ $code -eq 1 ]] || fail "bad checksum: exit $code, want 1"; has "bad checksum" "model checksum mismatch: got $msha"
[[ -z "$(ls -A "$tmp/dl3")" ]] || fail "bad checksum: files left behind: $(ls "$tmp/dl3")"
head -c 120000 /dev/zero > "$tmp/dl4/test-model.gguf.part"
run "${m[@]}" -- --download-only "$tmp/dl4"
[[ $code -eq 1 ]] || fail "corrupt part: exit $code, want 1"; has "corrupt part" "model checksum mismatch"
[[ -z "$(ls -A "$tmp/dl4")" ]] || fail "corrupt part: files left behind: $(ls "$tmp/dl4")"
run "${m[@]}" -- --download-only "$tmp/dl4"
ok "after corrupt part"; has "after corrupt part" "(verified)"
# A complete file with the wrong content is replaced.
head -c 300000 /dev/zero > "$tmp/dl4/test-model.gguf"
run "${m[@]}" -- --download-only "$tmp/dl4"
ok "wrong existing model"; has "wrong existing model" "is not the pinned model"
cmp -s "$tmp/hf/test-model.gguf" "$tmp/dl4/test-model.gguf" || fail "wrong existing model: not replaced"
run MODEL_URL="file://$tmp/hf/gone.gguf" MODEL_SHA256="$msha" MODEL_SIZE=300000 -- --download-only "$tmp/dl3"
[[ $code -eq 1 ]] || fail "missing download: exit $code, want 1"; has "missing download" "model download failed"
run "${m[@]}" FAKE_DF_KB=1000 -- --download-only "$tmp/dl3"
[[ $code -eq 1 ]] || fail "download, no disk space: exit $code, want 1"; has "download, no disk space" "not enough disk space on $tmp/dl3"
run "${m[@]}" -- --download-only "$tmp/nonexistent"
usage_err "--download-only without a directory"

# The prebuilt bundle: install.sh next to bin/llama-server, lib/ and BUILD-INFO (fakes).
b="$tmp/bundle"
mkdir -p "$b/bin" "$b/lib"
cp "$installer" "$b/install.sh"
printf '#!/bin/sh\necho fake llama-server\n' > "$b/bin/llama-server"
printf '#!/bin/sh\necho fake node\n' > "$b/bin/aisathome-node"
chmod +x "$b/bin/"*
: > "$b/lib/libggml-cuda.so"
cat > "$b/BUILD-INFO" <<'INFO'
llama_cpp_tag=v0.5.0
llama_cpp_commit=7fe450e19305b828c199d602c23a8337aaa1f03b
cuda_version=12.8
cuda_archs=86-real;89-real;120a-real
compute_caps=8.6 8.9 12.0
min_driver=525.60.13
min_driver_cc12=570.26
INFO
repo_installer="$installer" installer="$b/install.sh"
if [[ "$(uname -m)" = x86_64 ]] && grep -qw avx2 /proc/cpuinfo; then
  run -- --dry-run --node-id test-node
  ok "bundle"; has "bundle" "llama-server: prebuilt, llama.cpp v0.5.0 (7fe450e19305), CUDA 12.8"
  has "bundle" "GPU 0: NVIDIA GeForce RTX 3090, driver 535.183.01, compute capability 8.6: supported"
  has "bundle" "model: https://huggingface.co/unsloth/"; has "bundle" '"node_id": "test-node"'
  has "bundle" "-np 1 --kv-unified-per-slot 196608 "
  # gpu "lines" expect: ok or the error text.
  check_gpus() {
    run FAKE_GPUS="$1" -- --dry-run --node-id t
    if [[ "$2" = ok ]]; then ok "GPUs '$1'"; else [[ $code -eq 1 ]] || fail "GPUs '$1': exit $code, want 1"; has "GPUs '$1'" "$2"; fi
  }
  check_gpus "0, NVIDIA GeForce RTX 3090, 525.60.13, 8.6" ok
  check_gpus "0, NVIDIA GeForce RTX 3090, 520.61.05, 8.6" "NVIDIA driver 520.61.05 is too old for the prebuilt llama-server (CUDA 12.8): it needs 525.60.13 or newer"
  check_gpus "0, NVIDIA GeForce RTX 3090, 470.256.02, 8.6" "is too old"
  check_gpus "0, NVIDIA GeForce RTX 4090, 580.95.05, 8.9" ok
  check_gpus "0, NVIDIA GeForce RTX 5090, 570.26, 12.0" ok
  check_gpus "0, NVIDIA GeForce RTX 5090, 565.57.01, 12.0" "needs NVIDIA driver 570.26 or newer; this machine has 565.57.01"
  check_gpus "0, NVIDIA GeForce RTX 2080 Ti, 550.54.15, 7.5" "has compute capability 7.5; the prebuilt llama-server supports 8.6 8.9 12.0"
  check_gpus "0, NVIDIA H100 80GB HBM3, 550.54.15, 9.0" "compute capability 9.0"
  check_gpus "0, NVIDIA GeForce RTX 3090, 550.54.15, 8.6\n1, NVIDIA GeForce GTX 1080, 550.54.15, 6.1" "GPU 1 (NVIDIA GeForce GTX 1080) has compute capability 6.1"
  check_gpus "" "nvidia-smi lists no GPU"
  check_gpus "Field \"compute_cap\" is not a valid field to query." "cannot read nvidia-smi's output"
  run FAKE_SMI_FAIL=1 -- --dry-run --node-id t
  [[ $code -eq 1 ]] || fail "broken driver: exit $code, want 1"; has "broken driver" "nvidia-smi failed"
  run FAKE_DF_KB=1000 -- --dry-run --node-id t
  [[ $code -eq 1 ]] || fail "bundle, no disk space: exit $code, want 1"; has "bundle, no disk space" "not enough disk space"
  # An explicit --llama-build skips the bundle's checks (the manual path is unchanged).
  # (A GPU the bundle was not built for; the tier still needs nvidia-smi.)
  run FAKE_GPUS="0, NVIDIA GeForce RTX 2080 Ti, 550.54.15, 7.5" -- --dry-run --node-id t --llama-build "$b"
  ok "bundle dir as --llama-build"; lacks "bundle dir as --llama-build" "prebuilt"
  sed -i '/^min_driver=/d' "$b/BUILD-INFO"
  run -- --dry-run --node-id t
  [[ $code -eq 1 ]] || fail "broken BUILD-INFO: exit $code, want 1"; has "broken BUILD-INFO" "BUILD-INFO is incomplete"
else
  echo "skipping bundle tests: not x86_64 with AVX2"
fi
installer="$repo_installer"

# The suspend hook (aisathome-node-sleep), taken from install.sh and run with sh against fake
# cgroups, a fake systemctl and real processes: before suspend it must return only once no
# node process is left (SIGKILL after a while, give up after a bound).
s="$tmp/sleep"; mkdir -p "$s/bin" "$s/run"
sed -n "/^cat > \/usr\/local\/libexec\/aisathome-node-sleep <<'SL'\$/,/^SL\$/p" "$installer" | sed '1d;$d' > "$s/hook"
grep -q '^#!/bin/sh' "$s/hook" || fail "sleep hook: not found in install.sh"
procs="$s/sys/fs/cgroup/system.slice/aisathome-llama.service/cgroup.procs"
mkdir -p "${procs%/*}"
# systemctl: logs its arguments; is-active from FAKE_ACTIVE; stop sends SIGTERM to the PIDs
# in the fake cgroup, as systemd would. kill does nothing, so the hook's own kill is tested.
cat > "$s/bin/systemctl" <<'FAKE'
#!/bin/sh
echo "systemctl $*" >> "$AISATHOME_SLEEP_ROOT/systemctl.log"
case "$1" in
  is-active) [ "${FAKE_ACTIVE:-1}" = 1 ]; exit ;;
  stop) for p in $(cat "$AISATHOME_SLEEP_ROOT"/sys/fs/cgroup/system.slice/*/cgroup.procs); do kill -TERM "$p" 2>/dev/null; done ;;
esac
exit 0
FAKE
# nvidia-smi that hangs: the hook must not depend on it (a stuck driver hangs it too).
printf '#!/bin/sh\nsleep 1000\n' > "$s/bin/nvidia-smi"
chmod +x "$s/bin/"*
# spawn <script>: starts a detached bash process (reaped by init, as systemd reaps a
# service's processes) and prints its PID.
spawned=()
spawn() { ( bash -c "$1" </dev/null >/dev/null 2>&1 & echo $! ); }
alive() { [[ -d "/proc/$1" ]]; }
# hook <pre|post> [VAR=value...]: runs the hook; sets out, code and secs (elapsed seconds).
hook() {
  local arg="$1" t0; shift
  : > "$s/systemctl.log"; t0="$(date +%s)"
  out="$(env -i PATH="$s/bin:$PATH" AISATHOME_SLEEP_ROOT="$s" "$@" sh "$s/hook" "$arg" 2>&1)"; code=$?
  secs=$(( $(date +%s) - t0 ))
  out+=$'\n'"$(cat "$s/systemctl.log")"
}
# A process that needs 2 s to exit after SIGTERM (like llama-server freeing GPU memory),
# and one that exits at once.
slow="$(spawn 'trap "sleep 2; exit 0" TERM; while :; do sleep 0.1; done')"
quick="$(spawn 'exec sleep 1000')"
spawned+=("$slow" "$quick")
sleep 0.3
printf '%s\n%s\n' "$slow" "$quick" > "$procs"
hook pre
ok "sleep pre"; has "sleep pre" "systemctl stop --no-block aisathome-node.service aisathome-llama.service"
alive "$slow" && fail "sleep pre: returned while a node process (slow to exit) still ran"
alive "$quick" && fail "sleep pre: returned while a node process still ran"
[[ $secs -ge 1 ]] || fail "sleep pre: returned after $secs s, before the slow process could exit"
lacks "sleep pre" "SIGKILL"; lacks "sleep pre" "systemctl kill"
[[ -e "$s/run/aisathome-node-was-running" ]] || fail "sleep pre: running node not marked for resume"
# Resume restarts what the hook stopped, once.
hook post
ok "sleep post"; has "sleep post" "systemctl start aisathome-llama aisathome-node"
[[ ! -e "$s/run/aisathome-node-was-running" ]] || fail "sleep post: mark left behind"
hook post
lacks "sleep post, nothing stopped" "systemctl start"
# A node stopped by hand stays stopped after resume; the hook still stops and checks.
: > "$procs"
hook pre FAKE_ACTIVE=0
ok "sleep pre, node stopped"; has "sleep pre, node stopped" "systemctl stop --no-block"
[[ $secs -le 2 ]] || fail "sleep pre, node stopped: took $secs s"
[[ ! -e "$s/run/aisathome-node-was-running" ]] || fail "sleep pre, node stopped: marked for resume"
hook post
lacks "sleep post, node stopped by hand" "systemctl start"
# A process that ignores SIGTERM is killed after AISATHOME_SLEEP_TERM_WAIT seconds.
stubborn="$(spawn "trap '' TERM; exec sleep 1000")"
spawned+=("$stubborn")
sleep 0.3
echo "$stubborn" > "$procs"
hook pre AISATHOME_SLEEP_TERM_WAIT=1
ok "sleep pre, SIGTERM ignored"; has "sleep pre, SIGTERM ignored" "sending SIGKILL: $stubborn"
has "sleep pre, SIGTERM ignored" "systemctl kill --signal=KILL aisathome-node.service aisathome-llama.service"
alive "$stubborn" && fail "sleep pre: returned while a process that ignores SIGTERM still ran"
[[ $secs -le 4 ]] || fail "sleep pre, SIGTERM ignored: took $secs s"
# A process that survives SIGKILL (stuck in the kernel, like the nvidia_uvm teardown): the
# hook gives up after the bound, says so, and exits 0 so that resume still works. PID 1
# stands in for it (a kill from a non-root user cannot touch it).
if [[ $EUID -ne 0 ]]; then
  echo 1 > "$procs"
  hook pre AISATHOME_SLEEP_TERM_WAIT=1 AISATHOME_SLEEP_KILL_WAIT=1
  ok "sleep pre, unkillable"; has "sleep pre, unkillable" "still running after SIGKILL; suspend will probably fail"
  [[ $secs -le 5 ]] || fail "sleep pre, unkillable: took $secs s"
fi
: > "$procs"
kill -KILL "${spawned[@]}" 2>/dev/null
# The units: the hook runs before sleep and before the NVIDIA driver suspends the GPU, and
# llama-server starts only after the driver's resume.
unit="$(sed -n '/^cat > \/etc\/systemd\/system\/aisathome-node-sleep.service <<UNIT$/,/^UNIT$/p' "$installer")"
for want in "Before=sleep.target" "WantedBy=sleep.target" \
            "Before=nvidia-suspend.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service"; do
  [[ "$unit" == *"$want"* ]] || fail "sleep unit: missing $want"
done

# Without --dry-run and without root, nothing is installed.
if [[ $EUID -ne 0 ]]; then
  run --
  [[ $code -ne 0 ]] || fail "non-root install: exit 0"
  has "non-root install" "run with sudo"
fi

if [[ $fails -gt 0 ]]; then echo "$fails failure(s)"; exit 1; fi
echo "install.sh: all tests passed"
