#!/usr/bin/env bash
# Install an AIs@home node on Linux (systemd): llama-server plus aisathome-node, both
# running as the unprivileged system user "aisathome", sandboxed by systemd.
#
# From the prebuilt bundle (this script next to bin/llama-server, lib/ and aisathome-node):
#   sudo ./install.sh [--preset <name>] [--node-id <id>] [--enable-at-boot] [--dry-run]
# With your own llama.cpp build:
#   sudo ./install.sh --llama-build <llama.cpp/build> \
#        (--node-bin <aisathome-node> | --node-release <latest|vX.Y.Z>) [options]
# Options: --model <file.gguf>, --node-config <node.json>, --gateway-url <wss://...>,
#   --preset <name>, --enable-at-boot, --keep-token, --dry-run, --download-only <dir>.
#
# Without --model, the pinned model is downloaded from Hugging Face, resumed if interrupted
# and checked against its SHA-256. --download-only <dir> only does that (no root needed).
# Without --node-config, node.json is written from --node-id (default: the host name),
# --gateway-url and the preset. The bundle checks the NVIDIA driver and GPU first.
# --node-release downloads aisathome-node for this machine's architecture from a GitHub
# release of aisathome/aisathome and checks it against that release's SHA256SUMS.
# Preset: long-context, the network's one setting (see README.md).
# --dry-run runs the checks, prints the plan and the llama-server command, and exits
# without installing (no root needed). The node token is read from stdin (never from argv):
# paste it when asked, or: cat token.txt | sudo ./install.sh ...
# Updating: run the new bundle's install.sh. At a terminal, Enter on an empty line keeps the
# installed token (--keep-token does so without asking), and a generated node.json keeps the
# installed node id and gateway URL unless --node-id or --gateway-url is given.
#
# What it touches:
#   /opt/aisathome-node/            binaries and llama.cpp shared libraries (root-owned)
#   /var/lib/aisathome-node/models  the model (moved or downloaded; owned by aisathome)
#   /etc/aisathome-node/            node.json (root:aisathome 0640), token.env (root 0600)
#   /etc/systemd/system/aisathome-{llama,node}.service
#   local UI: see the "Local node UI" section below (UI_PORT=<port>|off, default 7381)
# Nothing listens on a public port: llama-server and the node's local UI bind 127.0.0.1, and
# the node only dials out.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "$1" >&2; exit "${2:-1}"; }
here="$(cd "$(dirname "$0")" && pwd)"
llama_build="" model="" node_bin="" node_release="" node_config="" at_boot=0 dry_run=0
node_id="" gateway_url="" download_only="" keep_token=0
conf=/etc/aisathome-node
preset="${PRESET:-long-context}" alias="${MODEL_ALIAS:-qwen3.8-27b}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --llama-build) llama_build="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --node-bin) node_bin="$2"; shift 2 ;;
    --node-release) node_release="$2"; shift 2 ;;
    --node-config) node_config="$2"; shift 2 ;;
    --node-id) node_id="$2"; shift 2 ;;
    --gateway-url) gateway_url="$2"; shift 2 ;;
    --download-only) download_only="$2"; shift 2 ;;
    --preset) preset="$2"; shift 2 ;;
    --enable-at-boot) at_boot=1; shift ;;
    --keep-token) keep_token=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    *) usage ;;
  esac
done

# One setting for the whole network (owner, 2026-10-11): 1 slot x 128k context, q4_0 KV cache,
# so every node takes the same requests and the model check has one reference. Explicit
# environment variables (SLOTS, CTX_PER_SLOT, KV_TYPE) override it, for testing only. Measured on an RTX 4090
# with Qwen3.8 27B UD-Q4_K_M, 2026-10-09 (the site's node/presets page). Per-slot context means one
# long request cannot starve the others (a single shared pool can: "Context size has been
# exceeded" on 2026-10-08).
case "$preset" in
  # 1 x 128k q4: 18,306 MiB, hard needles at 120k 118/120 (q8: 119/120, 20,354 MiB).
  long-context) p_slots=1 p_ctx=131072 p_kv=q4_0 ;;
  *) die "unknown preset '$preset'; the network uses one setting: long-context (1 x 128k, KV q4_0)" 2 ;;
esac
slots="${SLOTS:-$p_slots}" ctx_per_slot="${CTX_PER_SLOT:-$p_ctx}" kv_type="${KV_TYPE:-$p_kv}"
# Thinking budget in tokens (-1 = unlimited). Unlimited thinking ran for 8 minutes and filled
# the context; after the budget the model is told to answer.
reasoning_budget="${REASONING_BUDGET:-8192}"
# Host-RAM prompt cache in MiB (llama-server --cache-ram; default there 8192, 0 = off). It
# bounds how much conversation data stays warm in RAM; llama-server has no idle-time limit.
cache_ram="${CACHE_RAM_MIB:-2048}"
case "$kv_type" in q8_0|q4_0|f16) ;; *) die "KV_TYPE must be q8_0, q4_0 or f16" 2 ;; esac
[[ "$slots" =~ ^[1-9][0-9]*$ ]] || die "SLOTS must be a positive integer" 2
[[ "$ctx_per_slot" =~ ^[1-9][0-9]*$ ]] || die "CTX_PER_SLOT must be a positive integer" 2
[[ "$reasoning_budget" =~ ^(-1|[0-9]+)$ ]] || die "REASONING_BUDGET must be -1 or a non-negative integer" 2
[[ "$cache_ram" =~ ^[0-9]+$ ]] || die "CACHE_RAM_MIB must be a non-negative integer" 2
[[ -z "$node_bin" || -z "$node_release" ]] || die "use --node-bin or --node-release, not both" 2
[[ -z "$node_release" || "$node_release" = latest || "$node_release" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] ||
  die "--node-release must be 'latest' or a tag like v1.2.3" 2
# Releases come from GitHub; AISATHOME_RELEASE_URL points elsewhere (a mirror, or file:// in tests).
release_base="${AISATHOME_RELEASE_URL:-https://github.com/aisathome/aisathome/releases}"
[[ "$release_base" =~ ^(https|file):// ]] || die "AISATHOME_RELEASE_URL must start with https:// or file://" 2
[[ "$alias" =~ ^[A-Za-z0-9._-]+$ ]] || die "MODEL_ALIAS may only contain letters, digits, '.', '_' and '-'" 2

# The model the presets were measured with, pinned to a commit of the Hugging Face
# repository and checked by SHA-256 (the file the presets were measured
# with, 2026-10-09). MODEL_URL, MODEL_SHA256 and MODEL_SIZE (bytes) replace it, all
# three together.
pin_url=https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502/Qwen3.8-27B-UD-Q4_K_M.gguf
pin_sha=322e194ff79741c7baa497c240f677f54b201b0efab44ca8e50f122b39123482
pin_size=16464440224
if [[ -n "${MODEL_URL:-}${MODEL_SHA256:-}${MODEL_SIZE:-}" ]] &&
   [[ -z "${MODEL_URL:-}" || -z "${MODEL_SHA256:-}" || -z "${MODEL_SIZE:-}" ]]; then
  die "set MODEL_URL, MODEL_SHA256 and MODEL_SIZE together" 2
fi
model_url="${MODEL_URL:-$pin_url}" model_sha="${MODEL_SHA256:-$pin_sha}" model_size="${MODEL_SIZE:-$pin_size}"
[[ "$model_url" =~ ^(https|file)://[^[:space:]]+$ ]] || die "MODEL_URL must start with https:// or file://" 2
[[ "$model_sha" =~ ^[0-9a-f]{64}$ ]] || die "MODEL_SHA256 must be 64 lowercase hex digits" 2
[[ "$model_size" =~ ^[1-9][0-9]{0,14}$ ]] || die "MODEL_SIZE must be a positive number of bytes" 2
model_name="${model_url##*/}"; model_name="${model_name%%\?*}"
[[ "$model_name" =~ ^[A-Za-z0-9._-]+\.gguf$ ]] || model_name=model.gguf
# Disk space kept free beyond the model: the binaries (about 1.5 GB) and some slack.
reserve=$((2 * 1024 * 1024 * 1024))

# installed_value <node.json> <key>: a string field of an earlier install's node.json, or ""
# (no file, not readable, or no such field).
installed_value() {
  [[ -r "$1" ]] || return 0
  python3 -c 'import json,sys
try: v=json.load(open(sys.argv[1])).get(sys.argv[2], "")
except Exception: v=""
print(v if isinstance(v, str) else "")' "$1" "$2"
}

# pick_gateway_url <installed>: --gateway-url, else the installed node.json's, else the
# network's gateway.
pick_gateway_url() {
  local from="--gateway-url"
  if [[ -z "$gateway_url" && -n "$1" ]]; then gateway_url="$1" from="the installed gateway_url"; fi
  gateway_url="${gateway_url:-wss://api.aisathome.org/v1/node/connect}"
  [[ "$gateway_url" =~ ^wss?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$ ]] ||
    die "$from '$gateway_url' must look like wss://host/v1/node/connect (pass --gateway-url)" 2
}

# node.json is written by the installer unless --node-config is given. A rerun keeps the
# installed node id and gateway URL (not readable without root: a dry run may not see them).
old_id="" old_gw=""
if [[ -n "$node_config" ]]; then
  [[ -z "$node_id$gateway_url" ]] || die "--node-id and --gateway-url are for a generated node.json; edit your --node-config instead" 2
else
  [[ -z "$node_id" || "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
    die "--node-id '$node_id': use letters, digits, '.', '_' and '-' (up to 64)" 2
  old_id="$(installed_value "$conf/node.json" node_id)"
  old_gw="$(installed_value "$conf/node.json" gateway_url)"
  pick_gateway_url "$old_gw"
fi

# The prebuilt bundle: this script next to bin/llama-server, lib/ and BUILD-INFO. An explicit
# --llama-build, --node-bin or --node-release wins over the bundle's copy.
bundle=0
if [[ -z "$llama_build" && -x "$here/bin/llama-server" && -f "$here/BUILD-INFO" ]]; then
  bundle=1 llama_build="$here"
  [[ -n "$node_bin" || -n "$node_release" ]] || node_bin="$here/bin/aisathome-node"
fi

target=/var/lib/aisathome-node/models/model.gguf
llama_exec="/opt/aisathome-node/bin/llama-server -m $target --alias $alias \\
  --host 127.0.0.1 --port 8000 -ngl all -fa on -np $slots --kv-unified-per-slot $ctx_per_slot \\
  --reasoning-budget $reasoning_budget \\
  --reasoning-budget-message \"(Thinking budget reached; answering now.)\" --no-reasoning-preserve \\
  -ctk $kv_type -ctv $kv_type --cache-prompt --cache-ram $cache_ram --metrics"

# GPU access for the sandboxed services. Their device allowlist (implied by ProtectClock=)
# would otherwise hide the GPU, and CUDA would silently fall back to the CPU. systemd matches
# char-<name> against the names in /proc/devices (a glob) and skips a name that is not listed
# there, with a debug message only (Checked: src/core/bpf-devices.c and cgroup.c in systemd
# v249 and main). The GPU's major 195 (/dev/nvidia0, /dev/nvidiactl) is "nvidia-frontend" up
# to driver 535 and "nvidia" (plus "nvidiactl") from 545 on (Checked: NVIDIA's open kernel
# modules, nv-frontend.c in 535.183.01, nv.c in 545.23.06 and 575.51.03; on machines, 530
# lists nvidia-frontend and 575 lists nvidia, 2026-10-18). With only the old name, driver 575
# ran llama-server on the CPU. nvidia-uvm and nvidia-caps kept their names.
gpu_devices='DeviceAllow=char-nvidia-frontend rw
DeviceAllow=char-nvidia rw'

# Shared sandbox: no access to /home, read-only system, no privileges. Prompts must not
# reach disk: no core dumps (a dump holds prompts from RAM) and no swap (MemorySwapMax needs
# cgroup v2; systemd ignores it on v1).
sandbox='LimitCORE=0
MemorySwapMax=0
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
LockPersonality=yes
CapabilityBoundingSet=
ReadWritePaths=/var/lib/aisathome-node'

llama_unit() {
  cat <<UNIT
[Unit]
Description=AIs@home node: local llama-server (127.0.0.1 only)
After=network.target aisathome-gpu-init.service
Requires=aisathome-gpu-init.service
# On resume, start only after the driver has woken the GPU (nvidia-resume.service runs in
# parallel with the sleep hook's restart otherwise). No effect when no resume is pending.
After=nvidia-resume.service

[Service]
User=aisathome
Group=aisathome
Environment=LD_LIBRARY_PATH=/opt/aisathome-node/lib
# On a crash, ggml forks a debugger to print a backtrace; inside this sandbox that can hang
# the process instead of ending it (CUDA MMU fault, 2026-10-09). Exit at once so systemd
# restarts it.
Environment=GGML_NO_BACKTRACE=1
# GPU access: the GPU under its old and new driver names, then CUDA's memory manager.
$gpu_devices
DeviceAllow=char-nvidia-uvm rw
DeviceAllow=char-nvidia-caps r
ExecStart=$llama_exec
Restart=on-failure
RestartSec=5
# A hung CUDA process may ignore SIGTERM: kill it quickly instead of waiting 90 s.
TimeoutStopSec=20
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
$sandbox

[Install]
WantedBy=multi-user.target
UNIT
}

# engine_on_gpu <since>: llama-server, started at <since>, must run on the GPU. When CUDA sees
# no GPU (in the sandbox, most likely device rules that do not match the driver), llama-server
# carries on on the CPU: stop it then and leave it disabled, so nothing runs on the CPU.
engine_on_gpu() {
  journalctl -u aisathome-llama --since "$1" -o cat | grep -q "no usable GPU found" || return 0
  systemctl disable --now aisathome-node aisathome-llama >/dev/null 2>&1 || true
  local names
  names="$(grep -i nvidia /proc/devices 2>/dev/null | paste -sd, - || true)"
  die "llama-server found no GPU and ran on the CPU, so the installer stopped it. If nvidia-smi works, the service's device rules probably do not match this driver: get the newest installer and run it again, or tell us (NVIDIA devices in /proc/devices: ${names:-none}). Details: journalctl -u aisathome-llama"
}

# A temporary directory, removed on exit; created on first use (call workdir, then use $work).
work=""
workdir() { [[ -n "$work" ]] || { work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT; }; }

# Download the node binary for this architecture from a release and verify it against the
# release's SHA256SUMS. This catches corrupt downloads; it does not protect against a
# compromised release, since the checksums come from the same place.
fetch_node_release() {
  local arch url want got
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "no release binary for $(uname -m); build aisathome-node and use --node-bin" ;;
  esac
  local asset="aisathome-node-linux-$arch"
  if [[ "$node_release" = latest ]]; then url="$release_base/latest/download"; else url="$release_base/download/$node_release"; fi
  workdir
  for f in "$asset" SHA256SUMS; do
    curl -fsSL --proto '=https,file' --proto-redir '=https' --retry 3 -o "$work/$f" "$url/$f" ||
      die "download failed: $url/$f"
  done
  want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$work/SHA256SUMS")"
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "no checksum for $asset in $url/SHA256SUMS"
  got="$(sha256sum "$work/$asset" | cut -d' ' -f1)"
  [[ "$got" = "$want" ]] || die "checksum mismatch for $asset: got $got, SHA256SUMS says $want"
  chmod 0755 "$work/$asset"
  node_bin="$work/$asset"
  echo "aisathome-node: $asset from $url, sha256 $got (verified)" >&2
}

# ---- Local node UI ---------------------------------------------------------------------
# aisathome-node serves a status page with pause/resume and engine restart on
# http://127.0.0.1:$UI_PORT (README.md, "Local UI"). Kept in these functions, apart from the
# rest of the installer. UI_PORT=off installs no UI.
ui_port="${UI_PORT:-7381}"
ui_password=/etc/aisathome-node/ui-password
ui_restart_trigger=/var/lib/aisathome-node/restart-engine

node_ui_check() {
  [[ "$ui_port" = off || ( "$ui_port" =~ ^[1-9][0-9]{3,4}$ && "$ui_port" -le 65535 && "$ui_port" != 8000 ) ]] ||
    die "UI_PORT must be a port from 1000 to 65535 other than 8000 (llama-server), or off" 2
}

# The agent's systemd drop-in: UI settings, state file and the GPU access nvidia-smi needs.
node_ui_dropin() {
  cat <<UNIT
[Service]
Environment=AISATHOME_UI_PORT=$ui_port
Environment=AISATHOME_UI_PASSWORD_FILE=$ui_password
Environment=AISATHOME_STATE_FILE=/var/lib/aisathome-node/node-state.json
Environment=AISATHOME_RESTART_TRIGGER=$ui_restart_trigger
Environment="AISATHOME_PRESET=$preset: $slots x $ctx_per_slot tokens, KV $kv_type"
# Read-only GPU facts for the UI (name, memory, temperature) through nvidia-smi.
$gpu_devices
UNIT
}

node_ui_install() {
  local d=/etc/systemd/system/aisathome-node.service.d
  install -d -m 0755 "$d"
  node_ui_dropin > "$d/local-ui.conf"
  [[ "$ui_port" != off ]] || return 0
  # Password: random, kept across re-installs; readable by root and the agent only.
  if [[ ! -s "$ui_password" ]]; then
    (umask 077; head -c 18 /dev/urandom | base64 | tr '+/' '-_' > "$ui_password")
  fi
  chown root:aisathome "$ui_password"; chmod 0640 "$ui_password"
  # Engine restart from the UI: the unprivileged agent writes a file; this root path unit
  # restarts llama-server (and with it the agent). No sudo, polkit or capability needed.
  cat > /etc/systemd/system/aisathome-node-restart.path <<UNIT
[Unit]
Description=AIs@home node: restart llama-server when the local UI asks

[Path]
PathChanged=$ui_restart_trigger

[Install]
WantedBy=multi-user.target
UNIT
  cat > /etc/systemd/system/aisathome-node-restart.service <<UNIT
[Unit]
Description=AIs@home node: restart llama-server (asked by the local UI)

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart aisathome-llama.service
UNIT
  systemctl daemon-reload
  systemctl enable --now aisathome-node-restart.path
  # "aisathome-node status" on PATH, with this UI port.
  printf '#!/bin/sh\nexport AISATHOME_UI_PORT="${AISATHOME_UI_PORT:-%s}"\nexec /opt/aisathome-node/bin/aisathome-node "$@"\n' \
    "$ui_port" > /usr/local/bin/aisathome-node
  chmod 0755 /usr/local/bin/aisathome-node
  # A menu entry when a desktop is installed.
  if compgen -G '/usr/share/xsessions/*.desktop' >/dev/null || compgen -G '/usr/share/wayland-sessions/*.desktop' >/dev/null; then
    install -d -m 0755 /usr/local/share/applications /usr/local/share/icons/hicolor/scalable/apps
    cat > /usr/local/share/icons/hicolor/scalable/apps/aisathome-node.svg <<'SVG'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><rect width="32" height="32" rx="7" fill="#c2410c"/><text x="16" y="22" text-anchor="middle" font-family="monospace" font-size="17" font-weight="700" fill="#fff8f0">@</text></svg>
SVG
    cat > /usr/local/share/applications/aisathome-node.desktop <<DESKTOP
[Desktop Entry]
Type=Application
Name=AIs@home node
Comment=Status, pause and resume of this computer's AIs@home node
Exec=xdg-open http://127.0.0.1:$ui_port/
Icon=aisathome-node
Terminal=false
Categories=Network;System;Monitor;
DESKTOP
  fi
}

node_ui_message() {
  [[ "$ui_port" != off ]] || return 0
  cat <<MSG
Local UI (this computer only):               http://127.0.0.1:$ui_port/   or: aisathome-node status
UI password:                                 sudo cat $ui_password
From another computer:                       ssh -N -L $ui_port:127.0.0.1:$ui_port <user>@<this machine>
MSG
}
node_ui_check
# ---- end of local node UI ----------------------------------------------------------------

gb() { LC_ALL=C awk -v b="$1" 'BEGIN { printf "%.1f", b / 1e9 }'; }
trim() { local s="$1"; s="${s#"${s%%[![:space:]]*}"}"; printf '%s' "${s%"${s##*[![:space:]]}"}"; }
# ver_ge A B: version A >= version B.
ver_ge() { [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]]; }
# info KEY: a value from the bundle's BUILD-INFO (key=value lines; read, never sourced).
info() { sed -n "s/^$1=//p" "$here/BUILD-INFO" | head -n1; }

# The prebuilt llama-server needs x86_64 with AVX2, glibc 2.35 or newer, and an NVIDIA
# driver new enough for its CUDA version, on a GPU it was compiled for.
check_bundle() {
  local caps min min12 cuda out idx name drv cap n=0 missing
  caps="$(info compute_caps)" min="$(info min_driver)" min12="$(info min_driver_cc12)" cuda="$(info cuda_version)"
  [[ "$caps" =~ ^[0-9]+\.[0-9]+(\ [0-9]+\.[0-9]+)*$ && "$min" =~ ^[0-9]+(\.[0-9]+)+$ && "$min12" =~ ^[0-9]+(\.[0-9]+)+$ ]] ||
    die "$here/BUILD-INFO is incomplete; download the bundle again"
  echo "llama-server: prebuilt, llama.cpp $(info llama_cpp_tag) ($(info llama_cpp_commit | cut -c1-12)), CUDA $cuda, compute capability $caps" >&2
  [[ "$(uname -m)" = x86_64 ]] || die "the prebuilt bundle is for x86_64; this machine is $(uname -m). Build llama.cpp and use --llama-build."
  grep -qw avx2 /proc/cpuinfo || die "this CPU has no AVX2, which the prebuilt llama-server needs. Build llama.cpp and use --llama-build."
  command -v nvidia-smi >/dev/null ||
    die "nvidia-smi not found: install the NVIDIA driver ($min or newer; $min12 or newer for an RTX 50) and reboot"
  out="$(nvidia-smi --query-gpu=index,name,driver_version,compute_cap --format=csv,noheader 2>&1)" ||
    die "nvidia-smi failed, so the NVIDIA driver is not working: $out"
  while IFS=, read -r idx name drv cap; do
    idx="$(trim "$idx")" name="$(trim "$name")" drv="$(trim "$drv")" cap="$(trim "${cap:-}")"
    [[ -n "$idx" ]] || continue
    [[ "$drv" =~ ^[0-9]+(\.[0-9]+)+$ && "$cap" =~ ^[0-9]+\.[0-9]+$ ]] || die "cannot read nvidia-smi's output: $out"
    ver_ge "$drv" "$min" || die "NVIDIA driver $drv is too old for the prebuilt llama-server (CUDA $cuda): it needs $min or newer. Update the driver (Ubuntu: sudo ubuntu-drivers install, then reboot), or build llama.cpp yourself and use --llama-build."
    case " $caps " in
      *" $cap "*) ;;
      *) die "GPU $idx ($name) has compute capability $cap; the prebuilt llama-server supports $caps (RTX 30, 40 and 50 series). Build llama.cpp for this GPU and use --llama-build." ;;
    esac
    if [[ "${cap%%.*}" -ge 12 ]] && ! ver_ge "$drv" "$min12"; then
      die "GPU $idx ($name) needs NVIDIA driver $min12 or newer; this machine has $drv. Update the driver, then reboot."
    fi
    echo "GPU $idx: $name, driver $drv, compute capability $cap: supported" >&2
    n=$((n + 1))
  done <<<"$out"
  [[ $n -gt 0 ]] || die "nvidia-smi lists no GPU"
  missing="$(LD_LIBRARY_PATH="$here/lib" ldd "$here/bin/llama-server" 2>&1 | grep 'not found' || true)"
  [[ -z "$missing" ]] || die "the prebuilt llama-server cannot run here (it needs glibc 2.35 or newer, e.g. Ubuntu 22.04 or Debian 12):
$missing"
}

# model_missing <file>: bytes still to download for <file> (a complete file counts as 0;
# fetch_model checks its hash).
model_missing() {
  local have=0
  if [[ -f "$1" && "$(stat -c %s "$1")" = "$model_size" ]]; then echo 0; return; fi
  if [[ -f "$1.part" ]]; then have="$(stat -c %s "$1.part")"; fi
  if (( have > model_size )); then have=0; fi
  echo $((model_size - have))
}

# check_disk <dir> <bytes>: <dir> (or its nearest existing parent) must have <bytes> free.
check_disk() {
  local d="$1" kb
  while [[ ! -d "$d" ]]; do d="$(dirname "$d")"; done
  kb="$(df -Pk "$d" | awk 'NR == 2 { print $4 }')"
  [[ "$kb" =~ ^[0-9]+$ ]] || die "cannot read the free disk space of $d"
  (( kb * 1024 >= $2 )) ||
    die "not enough disk space on $d: need $(gb "$2") GB for the model download, $(gb $((kb * 1024))) GB free. Free some space and run the installer again (it resumes)."
  echo "disk: $(gb $((kb * 1024))) GB free on $d, $(gb "$2") GB needed" >&2
}

# fetch_model <file>: download the model to <file> and verify its SHA-256. An interrupted
# download resumes from <file>.part; a complete <file> with the right hash is kept.
fetch_model() {
  local dest="$1" part="$1.part" have=0 got rc i progress=(-sS)
  [[ -t 2 ]] && progress=(-#)
  if [[ -f "$dest" && "$(stat -c %s "$dest")" = "$model_size" ]]; then
    echo "model: checking the existing $dest (about a minute)" >&2
    got="$(sha256sum "$dest" | cut -d' ' -f1)"
    if [[ "$got" = "$model_sha" ]]; then echo "model: $dest, sha256 $got (verified)" >&2; return 0; fi
    echo "model: $dest is not the pinned model; downloading it" >&2
  fi
  if [[ -f "$part" ]]; then have="$(stat -c %s "$part")"; fi
  if (( have > model_size )); then rm -f "$part"; have=0; fi
  check_disk "$(dirname "$dest")" $((model_size - have + reserve))
  if (( have < model_size )); then
    if (( have > 0 )); then
      echo "model: resuming at $(gb "$have") of $(gb "$model_size") GB from $model_url" >&2
    else
      echo "model: downloading $(gb "$model_size") GB from $model_url" >&2
    fi
    # Retries resume where the last attempt stopped; a stalled transfer (under 10 kB/s for
    # a minute) counts as interrupted. HTTP errors (22) and missing files (37) are final; a
    # server that cannot resume (33) gets a fresh download.
    for i in 1 2 3 4 5; do
      rc=0
      curl -fL "${progress[@]}" --proto '=https,file' --proto-redir '=https' --retry 3 --retry-delay 5 \
        --connect-timeout 30 --speed-limit 10000 --speed-time 60 -C - -o "$part" "$model_url" || rc=$?
      [[ $rc -ne 0 ]] || break
      case $rc in
        22|37) die "model download failed: $model_url (curl exit $rc)" ;;
        33) rm -f "$part" ;;
      esac
      (( i < 5 )) || die "model download failed 5 times (curl exit $rc); run the installer again to resume"
      echo "model: download interrupted (curl exit $rc); resuming in 10 s" >&2
      sleep 10
    done
  fi
  echo "model: checking sha256 (about a minute)" >&2
  got="$(sha256sum "$part" | cut -d' ' -f1)"
  if [[ "$got" != "$model_sha" ]]; then
    rm -f "$part"
    die "model checksum mismatch: got $got, want $model_sha. The download was deleted; run the installer again to download it again."
  fi
  mv -f "$part" "$dest"
  echo "model: $dest, sha256 $got (verified)" >&2
}

# install_libs <build dir> <lib dir>: llama.cpp's shared libraries, with modes set here rather
# than taken from the source. A bundle unpacked under umask 077 has owner-only files, which
# the aisathome service user then cannot load ("cannot open shared object file", 2026-10-15).
install_libs() {
  find "$1" -name '*.so*' \( -type f -o -type l \) -exec cp -P {} "$2"/ \;
  find "$2" -type f -exec chmod u=rwX,go=rX {} +
}

# pick_node_id <token> [installed id]: the node id for a generated node.json: --node-id,
# else the id that a token from the account page carries ("nt-<id>.<secret>", as
# node.TokenNodeID reads it), else the installed node.json's, else the host name. (The node
# also takes an account token's id over node_id, with a warning.)
pick_node_id() {
  [[ -z "$node_id" ]] || return 0
  if [[ "$1" =~ ^nt-(n-[0-9a-f]{8})\. ]]; then node_id="${BASH_REMATCH[1]}"; return 0; fi
  if [[ -n "${2:-}" ]]; then
    node_id="$2"
    [[ "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
      die "the installed node id '$node_id' is not valid; pass --node-id" 2
    return 0
  fi
  node_id="$(hostname -s 2>/dev/null || true)"
  [[ "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
    die "the host name '$node_id' is not a valid node id; pass --node-id" 2
}

# interactive: stdin is a terminal (a person answers the token prompt).
interactive() { [[ -t 0 ]]; }

# read_token <token.env>: the node token from stdin, never from argv. An installed token is
# kept with --keep-token, or when a person at a terminal presses Enter on an empty line; piped
# empty input is still an error, so a script never keeps a token by accident.
read_token() {
  local old=""
  [[ -r "$1" ]] && old="$(sed -n 's/^AISATHOME_NODE_TOKEN=//p' "$1" | head -n 1)"
  if [[ $keep_token = 1 ]]; then
    [[ -n "$old" ]] || die "--keep-token: no installed token in $1" 2
    token="$old"; echo "token: keeping the installed one" >&2; return 0
  fi
  if interactive; then
    if [[ -n "$old" ]]; then echo "Paste the node token (input hidden), or press Enter to keep the installed one:" >&2
    else echo "Paste the node token (input hidden), then Enter:" >&2; fi
  fi
  token=""
  IFS= read -rs token || true
  if [[ -z "$token" && -n "$old" ]] && interactive; then
    token="$old"; echo "token: keeping the installed one" >&2
  fi
  [[ -n "$token" ]] || die "no token on stdin${old:+ (to keep the installed one: --keep-token)}"
}

# node.json for the preset, when no --node-config is given (inputs validated above).
gen_node_config() {
  printf '{\n  "node_id": "%s",\n  "gateway_url": "%s",\n  "upstream": "http://127.0.0.1:8000",\n  "models": [{"name": "%s", "roles": ["deep", "agent"], "slots": %s, "ctx": %s}]\n}\n' \
    "$node_id" "$gateway_url" "$alias" "$slots" "$ctx_per_slot"
}

if [[ -n "$download_only" ]]; then
  [[ -d "$download_only" ]] || die "--download-only: no directory $download_only" 2
  [[ -z "$model" ]] || die "use --download-only or --model, not both" 2
  fetch_model "$download_only/$model_name"
  echo "Downloaded. Install with --model $download_only/$model_name (the file is moved)." >&2
  exit 0
fi

[[ $dry_run = 1 || $EUID -eq 0 ]] || die "run with sudo"
[[ $dry_run = 1 || -n "$llama_build" ]] ||
  die "no llama-server: run install.sh from the prebuilt bundle, or pass --llama-build <llama.cpp/build>" 2
[[ $bundle = 0 ]] || check_bundle
if [[ -n "$model" ]]; then
  [[ -f "$model" ]] || die "no model file: $model"
else
  echo "model: $model_url (sha256 $model_sha, $(gb "$model_size") GB) to $target" >&2
  check_disk "$(dirname "$target")" $(( $(model_missing "$target") + reserve ))
fi
[[ -z "$node_config" || -f "$node_config" ]] || die "no node config: $node_config"
[[ -z "$node_release" ]] || fetch_node_release

if [[ $dry_run = 1 ]]; then
  if [[ -z "$node_config" ]]; then
    [[ -n "$node_id$old_id" ]] || echo "node id: the one an account-page token carries, else the host name" >&2
    [[ -r "$conf/node.json" || ! -e "$conf/node.json" ]] ||
      echo "node.json: $conf/node.json is not readable without sudo; the install keeps its node id and gateway URL" >&2
    pick_node_id "" "$old_id"
    echo "node.json (generated):"; gen_node_config
  fi
  echo "preset: $preset"
  echo "$llama_exec"
  echo "local UI: $ui_port"
  node_ui_dropin
  echo "aisathome-llama.service:"; llama_unit
  exit 0
fi

[[ -x "$llama_build/bin/llama-server" ]] || die "no llama-server in $llama_build/bin"
[[ -x "$node_bin" ]] || usage
[[ -z "$node_config" ]] || python3 -m json.tool "$node_config" >/dev/null || die "invalid JSON: $node_config"

read_token "$conf/token.env"
if [[ -z "$node_config" ]]; then
  pick_node_id "$token" "$old_id"
  [[ -z "$old_id$old_gw" ]] || echo "node.json: node id $node_id, gateway $gateway_url" >&2
  workdir; node_config="$work/node.json"; gen_node_config > "$node_config"
fi

# 1. Unprivileged system user without a login shell.
id aisathome >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aisathome-node --shell /usr/sbin/nologin aisathome

# 2. Binaries and llama.cpp shared libraries. The old lib/ is removed first: no stale
# libraries from an earlier build, and a running llama-server keeps its unlinked copies
# instead of crashing on files overwritten in place. The bundle's BUILD-INFO and licences
# (llama.cpp, NVIDIA CUDA runtime and cuBLAS) go along with the libraries.
rm -rf /opt/aisathome-node/lib
install -d -m 0755 /opt/aisathome-node/bin /opt/aisathome-node/lib
install -m 0755 "$llama_build/bin/llama-server" /opt/aisathome-node/bin/
install -m 0755 "$node_bin" /opt/aisathome-node/bin/aisathome-node
install_libs "$llama_build" /opt/aisathome-node/lib
for f in BUILD-INFO THIRD_PARTY_NOTICES.md licenses; do
  if [[ $bundle = 1 && -e "$llama_build/$f" ]]; then rm -rf "/opt/aisathome-node/$f"; cp -r "$llama_build/$f" /opt/aisathome-node/; fi
done
chown -R root:root /opt/aisathome-node
chmod -R u=rwX,go=rX /opt/aisathome-node

# 3. Model (moved to save disk space, or downloaded and verified) and state directory.
install -d -o aisathome -g aisathome -m 0750 /var/lib/aisathome-node /var/lib/aisathome-node/models
if [[ -z "$model" ]]; then
  fetch_model "$target"
elif [[ "$(readlink -f "$model")" != "$target" ]]; then
  mv "$model" "$target"
fi
chown aisathome:aisathome "$target"; chmod 0640 "$target"

# 4. Config and token.
install -d -o root -g aisathome -m 0750 /etc/aisathome-node
# The token lives only in token.env; drop any token from the copied config.
python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); c.pop("token",None); json.dump(c,sys.stdout,indent=2)' \
  "$node_config" > /etc/aisathome-node/node.json.new
install -o root -g aisathome -m 0640 /etc/aisathome-node/node.json.new /etc/aisathome-node/node.json
rm -f /etc/aisathome-node/node.json.new
umask 077; printf 'AISATHOME_NODE_TOKEN=%s\n' "$token" > /etc/aisathome-node/token.env; umask 022
chown root:root /etc/aisathome-node/token.env; chmod 0600 /etc/aisathome-node/token.env

# 5. systemd units (the sandbox and llama-server's unit are defined above).

# Creates /dev/nvidia-uvm (normally done by a setuid helper on first CUDA use, which the
# sandbox blocks). Runs once, as root, outside the sandbox.
cat > /etc/systemd/system/aisathome-gpu-init.service <<UNIT
[Unit]
Description=AIs@home node: create NVIDIA device nodes for CUDA

[Service]
Type=oneshot
RemainAfterExit=yes
# Not every driver install ships nvidia-modprobe (a Vast VM with driver 535 had none, but
# already had the device nodes; 2026-10-10 install test): use it only when needed.
ExecStart=/bin/sh -c 'test -e /dev/nvidia-uvm && exit 0; command -v nvidia-modprobe >/dev/null && exec nvidia-modprobe -u -c=0; echo "no /dev/nvidia-uvm and no nvidia-modprobe: install it (e.g. apt install nvidia-modprobe)" >&2; exit 1'

[Install]
WantedBy=multi-user.target
UNIT

llama_unit > /etc/systemd/system/aisathome-llama.service

# Watchdog: restart llama-server if /health has failed for 3 checks in a row (one a minute),
# for hangs that do not end the process. Skips the first 5 minutes after a start (loading).
install -d -m 0755 /usr/local/libexec
cat > /usr/local/libexec/aisathome-node-watchdog <<'WD'
#!/bin/sh
state=/run/aisathome-node-watchdog.fails
systemctl is-active -q aisathome-llama || { rm -f "$state"; exit 0; }
since=$(systemctl show -p ActiveEnterTimestampMonotonic --value aisathome-llama)
now=$(cut -d' ' -f1 /proc/uptime | cut -d. -f1)
[ $((now - since / 1000000)) -lt 300 ] && exit 0
if curl -fs -m 10 -o /dev/null http://127.0.0.1:8000/health; then rm -f "$state"; exit 0; fi
n=$(( $(cat "$state" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$state"
if [ "$n" -ge 3 ]; then
  echo "llama-server unhealthy for $n checks; restarting" | systemd-cat -t aisathome-node-watchdog -p warning
  rm -f "$state"; systemctl restart aisathome-llama  # PartOf restarts the agent too
fi
WD
chmod 0755 /usr/local/libexec/aisathome-node-watchdog
cat > /etc/systemd/system/aisathome-node-watchdog.service <<UNIT
[Unit]
Description=AIs@home node: restart a hung llama-server
[Service]
Type=oneshot
ExecStart=/usr/local/libexec/aisathome-node-watchdog
UNIT
cat > /etc/systemd/system/aisathome-node-watchdog.timer <<UNIT
[Unit]
Description=AIs@home node: check llama-server health every minute
[Timer]
OnBootSec=2min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
UNIT

# Suspend: GPU memory is usually not preserved across suspend (NVIDIA default
# PreserveVideoMemoryAllocations=0), so a llama-server running through a suspend uses lost
# memory after resume (Xid 31 MMU fault and a broken CUDA state, 2026-10-09). Stop it before
# sleeping and start it again after resume, but only if it was running.
#
# The node must be gone before the GPU sleeps. The driver's nvidia-suspend.service (and
# nvidia-hibernate) suspends the GPU through /proc/driver/nvidia/suspend and is ordered only
# before systemd-suspend.service, so it used to run in parallel with this hook. When it won
# the race, llama-server's CUDA teardown blocked in nvidia_uvm (state D, immune to SIGKILL),
# systemctl stop gave up after 80 s and the kernel's freezer aborted the suspend ("1 tasks
# refusing to freeze", RTX 4090, driver 530, 2026-10-11 00:19 and 22:23; at 00:37 llama
# stopped first and the suspend worked). Hence Before=nvidia-suspend.service etc. below.
#
# Timing: this unit runs inside the suspend job, after logind has released its delay
# inhibitors, so logind's InhibitDelayMaxSec (5 s) does not apply, and a oneshot has no
# start timeout by default. "pre" bounds itself: SIGTERM, SIGKILL after 20 s, give up after
# 30 s (a process stuck in the driver cannot be killed; suspend then fails, as before).
# TimeoutStartSec=90 is only a backstop. "pre" always exits 0, so "post" runs on resume.
cat > /usr/local/libexec/aisathome-node-sleep <<'SL'
#!/bin/sh
# pre:  stop the node and return only when none of its processes is left (bounded).
# post: start it again, if pre stopped it (a node stopped by hand stays stopped).
# AISATHOME_SLEEP_ROOT and the *_WAIT variables exist for install_test.sh only.
root="${AISATHOME_SLEEP_ROOT:-}"
mark="$root/run/aisathome-node-was-running"
units="aisathome-node.service aisathome-llama.service"
term_wait="${AISATHOME_SLEEP_TERM_WAIT:-20}" kill_wait="${AISATHOME_SLEEP_KILL_WAIT:-10}"
log() { echo "aisathome-node-sleep: $*" >&2; }
# PIDs in the node's cgroups (cgroup v2, hybrid or v1 layout) that still exist.
node_pids() {
  for u in $units; do
    for d in "" /unified /systemd; do
      cat "$root/sys/fs/cgroup$d/system.slice/$u/cgroup.procs" 2>/dev/null
    done
  done | sort -un | while read -r p; do [ -d "/proc/$p" ] && echo "$p"; done
}
# wait_gone <seconds>: succeeds once no node process is left; fails after <seconds>.
wait_gone() {
  end=$(( $(date +%s) + $1 ))
  while [ -n "$(node_pids)" ]; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 0.2
  done
}
case "$1" in
  pre)
    if systemctl is-active -q aisathome-llama; then touch "$mark"; fi
    # Also when inactive: a failed stop can leave processes behind.
    systemctl stop --no-block $units
    wait_gone "$term_wait" && exit 0
    log "still running ${term_wait} s after SIGTERM, sending SIGKILL: $(node_pids | tr '\n' ' ')"
    systemctl kill --signal=KILL $units 2>/dev/null
    for p in $(node_pids); do kill -KILL "$p" 2>/dev/null; done
    wait_gone "$kill_wait" && exit 0
    log "still running after SIGKILL; suspend will probably fail (stuck in the GPU driver?):"
    ps -o pid=,stat=,wchan:40=,comm= -p "$(node_pids | paste -sd, -)" >&2
    exit 0 ;;
  post)
    if [ -e "$mark" ]; then rm -f "$mark"; systemctl start aisathome-llama aisathome-node; fi ;;
esac
SL
chmod 0755 /usr/local/libexec/aisathome-node-sleep
# Before= on units that do not exist (no NVIDIA systemd sleep integration) is harmless.
cat > /etc/systemd/system/aisathome-node-sleep.service <<UNIT
[Unit]
Description=AIs@home node: stop before suspend, restart after resume
Before=sleep.target
Before=nvidia-suspend.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service
StopWhenUnneeded=yes

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/aisathome-node-sleep pre
ExecStop=/usr/local/libexec/aisathome-node-sleep post
TimeoutStartSec=90

[Install]
WantedBy=sleep.target
UNIT

cat > /etc/systemd/system/aisathome-node.service <<UNIT
[Unit]
Description=AIs@home node agent (outbound connection to the gateway)
After=network-online.target aisathome-llama.service
Wants=network-online.target
# PartOf: restarting llama-server restarts the agent too. (BindsTo stopped the agent on a
# llama restart and never started it again, 2026-10-09.) The agent's own health check
# handles an engine that is up but hung.
Requires=aisathome-llama.service
PartOf=aisathome-llama.service

[Service]
User=aisathome
Group=aisathome
EnvironmentFile=/etc/aisathome-node/token.env
ExecStart=/opt/aisathome-node/bin/aisathome-node -config /etc/aisathome-node/node.json
Restart=on-failure
RestartSec=5
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
$sandbox

[Install]
WantedBy=multi-user.target
UNIT

node_ui_install
systemctl daemon-reload
systemctl enable --now aisathome-node-watchdog.timer
systemctl enable aisathome-node-sleep.service
if [[ $at_boot = 1 ]]; then systemctl enable aisathome-gpu-init aisathome-llama aisathome-node; fi
started="$(date '+%Y-%m-%d %H:%M:%S')"
systemctl restart aisathome-llama
for i in $(seq 1 90); do curl -fs http://127.0.0.1:8000/health >/dev/null && break; sleep 2; done
curl -fs http://127.0.0.1:8000/health >/dev/null || { echo "llama-server did not become healthy; see: journalctl -u aisathome-llama" >&2; exit 1; }
engine_on_gpu "$started"
systemctl restart aisathome-node
sleep 3
systemctl --no-pager --lines=3 status aisathome-node | tail -3
cat <<MSG

Installed. Pause the node and free the GPU:  sudo systemctl stop aisathome-node aisathome-llama
Resume:                                      sudo systemctl start aisathome-llama aisathome-node
Logs:                                        journalctl -u aisathome-node -u aisathome-llama -f
Start at boot: $(systemctl is-enabled --quiet aisathome-node 2>/dev/null && echo yes || echo "no (re-run with --enable-at-boot, or: sudo systemctl enable aisathome-llama aisathome-node)")
Remove:                                      sudo ./uninstall.sh
MSG
node_ui_message
