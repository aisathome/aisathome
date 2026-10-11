#!/usr/bin/env bash
# Install an AIs@home node on Linux (systemd): llama-server plus aisathome-node, both
# running as the unprivileged system user "aisathome", sandboxed by systemd.
#
#   sudo ./install.sh --llama-build <llama.cpp/build> --model <file.gguf> \
#        (--node-bin <aisathome-node> | --node-release <latest|vX.Y.Z>) \
#        --node-config <node.json> [--preset <name>] [--enable-at-boot] [--dry-run]
#
# --node-release downloads aisathome-node for this machine's architecture from a GitHub
# release of aisathome/aisathome and checks it against that release's SHA256SUMS.
# Presets: balanced (default), low-vram, long-context, long-context-dual (see README.md).
# --dry-run prints the llama-server command and exits without installing (no root needed).
# The node token is read from stdin (never from argv), e.g.
#   cat token.txt | sudo ./install.sh ...
#
# What it touches:
#   /opt/aisathome-node/            binaries and llama.cpp shared libraries (root-owned)
#   /var/lib/aisathome-node/models  the model (moved, not copied; owned by aisathome)
#   /etc/aisathome-node/            node.json (root:aisathome 0640), token.env (root 0600)
#   /etc/systemd/system/aisathome-{llama,node}.service
#   local UI: see the "Local node UI" section below (UI_PORT=<port>|off, default 7381)
# Nothing listens on a public port: llama-server and the node's local UI bind 127.0.0.1, and
# the node only dials out.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "$1" >&2; exit "${2:-1}"; }
llama_build="" model="" node_bin="" node_release="" node_config="" at_boot=0 dry_run=0
preset="${PRESET:-balanced}" alias="${MODEL_ALIAS:-qwen3.8-27b}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --llama-build) llama_build="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --node-bin) node_bin="$2"; shift 2 ;;
    --node-release) node_release="$2"; shift 2 ;;
    --node-config) node_config="$2"; shift 2 ;;
    --preset) preset="$2"; shift 2 ;;
    --enable-at-boot) at_boot=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    *) usage ;;
  esac
done

# Presets set slots, context per slot and KV cache type; explicit environment variables
# (SLOTS, CTX_PER_SLOT, KV_TYPE) override them. Numbers: bench/kvquant/RESULTS.md in the
# research repo (RTX 4090, Qwen3.8 27B UD-Q4_K_M, 2026-10-09). Per-slot context means one
# long request cannot starve the others (a single shared pool can: "Context size has been
# exceeded" on 2026-10-08).
case "$preset" in
  # q8 KV: mean KLD 0.0020 vs f16, no needle loss to 60k; 1 x 64k q8 used 17,858 MiB.
  balanced) p_slots=2 p_ctx=32768 p_kv=q8_0 ;;
  # q4 KV: 1.0 GB less than q8 at 64k (16,834 MiB), no needle loss, mean KLD 2x q8.
  low-vram) p_slots=2 p_ctx=32768 p_kv=q4_0 ;;
  # 1 x 128k q4: 18,306 MiB, hard needles at 120k 118/120 (q8: 119/120, 20,354 MiB).
  long-context) p_slots=1 p_ctx=131072 p_kv=q4_0 ;;
  # 2 x 128k q4: 21,272 MiB. A ~100k prefill nearly stalls the other slot's decode
  # (3.9 tok/s for about a minute): head-of-line blocking.
  long-context-dual) p_slots=2 p_ctx=131072 p_kv=q4_0 ;;
  *) die "unknown preset '$preset'; choose balanced, low-vram, long-context or long-context-dual" 2 ;;
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

target=/var/lib/aisathome-node/models/model.gguf
llama_exec="/opt/aisathome-node/bin/llama-server -m $target --alias $alias \\
  --host 127.0.0.1 --port 8000 -ngl all -fa on -np $slots --kv-unified-per-slot $ctx_per_slot \\
  --reasoning-budget $reasoning_budget \\
  --reasoning-budget-message \"(Thinking budget reached; answering now.)\" --no-reasoning-preserve \\
  -ctk $kv_type -ctv $kv_type --cache-prompt --cache-ram $cache_ram --metrics"

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
  dl_dir="$(mktemp -d)"
  trap 'rm -rf "$dl_dir"' EXIT
  for f in "$asset" SHA256SUMS; do
    curl -fsSL --proto '=https,file' --proto-redir '=https' --retry 3 -o "$dl_dir/$f" "$url/$f" ||
      die "download failed: $url/$f"
  done
  want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$dl_dir/SHA256SUMS")"
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "no checksum for $asset in $url/SHA256SUMS"
  got="$(sha256sum "$dl_dir/$asset" | cut -d' ' -f1)"
  [[ "$got" = "$want" ]] || die "checksum mismatch for $asset: got $got, SHA256SUMS says $want"
  chmod 0755 "$dl_dir/$asset"
  node_bin="$dl_dir/$asset"
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
DeviceAllow=char-nvidia-frontend rw
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

dl_dir=""
if [[ $dry_run = 1 ]]; then
  [[ -z "$node_release" ]] || fetch_node_release
  echo "preset: $preset"
  echo "$llama_exec"
  echo "local UI: $ui_port"
  node_ui_dropin
  exit 0
fi

[[ $EUID -eq 0 ]] || die "run with sudo"
[[ -x "$llama_build/bin/llama-server" ]] || die "no llama-server in $llama_build/bin"
[[ -f "$model" && -f "$node_config" ]] || usage
[[ -z "$node_release" ]] || fetch_node_release
[[ -x "$node_bin" ]] || usage
python3 -m json.tool "$node_config" >/dev/null || die "invalid JSON: $node_config"

[[ -t 0 ]] && echo "Paste the node token (input hidden), then Enter:" >&2
IFS= read -rs token || true
[[ -n "$token" ]] || die "no token on stdin"

# 1. Unprivileged system user without a login shell.
id aisathome >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aisathome-node --shell /usr/sbin/nologin aisathome

# 2. Binaries and llama.cpp shared libraries.
install -d -m 0755 /opt/aisathome-node/bin /opt/aisathome-node/lib
install -m 0755 "$llama_build/bin/llama-server" /opt/aisathome-node/bin/
install -m 0755 "$node_bin" /opt/aisathome-node/bin/aisathome-node
find "$llama_build" -name '*.so*' \( -type f -o -type l \) -exec cp -a {} /opt/aisathome-node/lib/ \;
chown -R root:root /opt/aisathome-node

# 3. Model (moved to save disk space) and state directory.
install -d -o aisathome -g aisathome -m 0750 /var/lib/aisathome-node /var/lib/aisathome-node/models
if [[ "$(readlink -f "$model")" != "$target" ]]; then mv "$model" "$target"; fi
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

# 5. systemd units. Shared sandbox: no access to /home, read-only system, no privileges.
# Prompts must not reach disk: no core dumps (a dump holds prompts from RAM) and no swap
# (MemorySwapMax needs cgroup v2; systemd ignores it on v1).
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

cat > /etc/systemd/system/aisathome-llama.service <<UNIT
[Unit]
Description=AIs@home node: local llama-server (127.0.0.1 only)
After=network.target aisathome-gpu-init.service
Requires=aisathome-gpu-init.service

[Service]
User=aisathome
Group=aisathome
Environment=LD_LIBRARY_PATH=/opt/aisathome-node/lib
# On a crash, ggml forks a debugger to print a backtrace; inside this sandbox that can hang
# the process instead of ending it (CUDA MMU fault, 2026-10-09). Exit at once so systemd
# restarts it.
Environment=GGML_NO_BACKTRACE=1
# GPU access. The sandbox's device allowlist (implied by ProtectClock=) would otherwise
# block the GPU, and CUDA would silently fall back to the CPU.
DeviceAllow=char-nvidia-frontend rw
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
cat > /usr/local/libexec/aisathome-node-sleep <<'SL'
#!/bin/sh
mark=/run/aisathome-node-was-running
case "$1" in
  pre)  if systemctl is-active -q aisathome-llama; then touch "$mark"; systemctl stop aisathome-llama; fi ;;
  post) if [ -e "$mark" ]; then rm -f "$mark"; systemctl start aisathome-llama aisathome-node; fi ;;
esac
SL
chmod 0755 /usr/local/libexec/aisathome-node-sleep
cat > /etc/systemd/system/aisathome-node-sleep.service <<UNIT
[Unit]
Description=AIs@home node: stop before suspend, restart after resume
Before=sleep.target
StopWhenUnneeded=yes

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/aisathome-node-sleep pre
ExecStop=/usr/local/libexec/aisathome-node-sleep post

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
if journalctl -u aisathome-llama --since "$started" -o cat | grep -q "no usable GPU found"; then
  echo "llama-server is running on the CPU, not the GPU; see: journalctl -u aisathome-llama" >&2; exit 1
fi
systemctl restart aisathome-node
sleep 3
systemctl --no-pager --lines=3 status aisathome-node | tail -3
cat <<MSG

Installed. Pause the node and free the GPU:  sudo systemctl stop aisathome-node aisathome-llama
Resume:                                      sudo systemctl start aisathome-llama aisathome-node
Logs:                                        journalctl -u aisathome-node -u aisathome-llama -f
Start at boot: $([[ $at_boot = 1 ]] && echo yes || echo "no (re-run with --enable-at-boot, or: sudo systemctl enable aisathome-llama aisathome-node)")
Remove:                                      sudo ./uninstall.sh
MSG
node_ui_message
