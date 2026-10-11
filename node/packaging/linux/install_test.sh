#!/usr/bin/env bash
# Tests for install.sh that need no root, GPU or network: presets, overrides, input
# validation, the prebuilt bundle's driver and GPU checks (fake nvidia-smi), free disk space
# (fake df) and the model download (file:// URLs), through --dry-run and --download-only.
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
# df: FAKE_DF_KB KiB free (default 100 GB). nvidia-smi: FAKE_GPUS lines (\n-separated), or
# fails like a broken driver with FAKE_SMI_FAIL=1.
mkdir "$tmp/fake"
cat > "$tmp/fake/df" <<'FAKE'
#!/bin/sh
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "/dev/fake 200000000 100000000 ${FAKE_DF_KB:-100000000} 50% /"
FAKE
cat > "$tmp/fake/nvidia-smi" <<'FAKE'
#!/bin/sh
if [ -n "$FAKE_SMI_FAIL" ]; then echo "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."; exit 9; fi
printf '%b\n' "${FAKE_GPUS-0, NVIDIA GeForce RTX 3090, 535.183.01, 8.6}"
FAKE
chmod +x "$tmp/fake/df" "$tmp/fake/nvidia-smi"
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

# The pinned model and a generated node.json, from the repository (no bundle): a dry run
# shows the download plan and checks the free disk space.
run -- --dry-run --node-id test-node
ok "model plan"; has "model plan" "model: https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502/Qwen3.8-27B-UD-Q4_K_M.gguf"
has "model plan" "sha256 322e194ff79741c7baa497c240f677f54b201b0efab44ca8e50f122b39123482"
has "model plan" "disk: 102.4 GB free"; lacks "model plan" "GPU 0"
has "model plan" '"node_id": "test-node"'; has "model plan" '"gateway_url": "wss://api.aisathome.org/v1/node/connect"'
has "model plan" '"name": "qwen3.8-27b", "roles": ["deep", "agent"], "slots": 2, "ctx": 32768'
# Sizes print with a decimal point in any locale (a comma locale gave "16,5 GB").
comma_locale="$(locale -a 2>/dev/null | grep -m1 -iE '^(nb_NO|de_DE|fr_FR)\.utf-?8$' || true)"
if [[ -n "$comma_locale" ]]; then
  run LC_ALL="$comma_locale" -- --dry-run
  ok "comma locale"; has "comma locale" "16.5 GB) to"; lacks "comma locale" "16,5"
fi
run FAKE_DF_KB=10000000 -- --dry-run
[[ $code -eq 1 ]] || fail "no disk space: exit $code, want 1"; has "no disk space" "not enough disk space"
run FAKE_DF_KB=10000000 -- --dry-run --model /etc/hostname
ok "--model skips the download"; lacks "--model skips the download" "huggingface"
run -- --dry-run --model /nonexistent.gguf
[[ $code -eq 1 ]] || fail "missing model file: exit $code, want 1"; has "missing model file" "no model file"
run -- --dry-run --preset long-context --gateway-url wss://gw.example.org:8443/v1/node/connect --node-id n1
ok "node.json follows the preset"; has "node.json follows the preset" '"slots": 1, "ctx": 131072'
has "node.json follows the preset" '"gateway_url": "wss://gw.example.org:8443/v1/node/connect"'
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
  run -- --dry-run --preset balanced --node-id test-node
  ok "bundle"; has "bundle" "llama-server: prebuilt, llama.cpp v0.5.0 (7fe450e19305), CUDA 12.8"
  has "bundle" "GPU 0: NVIDIA GeForce RTX 3090, driver 535.183.01, compute capability 8.6: supported"
  has "bundle" "model: https://huggingface.co/unsloth/"; has "bundle" '"node_id": "test-node"'
  has "bundle" "-np 2 --kv-unified-per-slot 32768 "
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
  run FAKE_SMI_FAIL=1 -- --dry-run --node-id t --llama-build "$b"
  ok "bundle dir as --llama-build"; lacks "bundle dir as --llama-build" "prebuilt"
  sed -i '/^min_driver=/d' "$b/BUILD-INFO"
  run -- --dry-run --node-id t
  [[ $code -eq 1 ]] || fail "broken BUILD-INFO: exit $code, want 1"; has "broken BUILD-INFO" "BUILD-INFO is incomplete"
else
  echo "skipping bundle tests: not x86_64 with AVX2"
fi
installer="$repo_installer"

# Without --dry-run and without root, nothing is installed.
if [[ $EUID -ne 0 ]]; then
  run -- --preset balanced
  [[ $code -ne 0 ]] || fail "non-root install: exit 0"
  has "non-root install" "run with sudo"
fi

if [[ $fails -gt 0 ]]; then echo "$fails failure(s)"; exit 1; fi
echo "install.sh: all tests passed"
