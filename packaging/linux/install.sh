#!/usr/bin/env bash
# Install an AIs@home node on Linux (systemd): llama-server plus aisathome-node, both
# running as the unprivileged system user "aisathome", sandboxed by systemd.
#
#   sudo ./install.sh --llama-build <llama.cpp/build> --model <file.gguf> \
#        --node-bin <aisathome-node> --node-config <node.json> [--enable-at-boot]
#
# The node token is read from stdin (never from argv), e.g.
#   cat token.txt | sudo ./install.sh ...
#
# What it touches:
#   /opt/aisathome-node/            binaries and llama.cpp shared libraries (root-owned)
#   /var/lib/aisathome-node/models  the model (moved, not copied; owned by aisathome)
#   /etc/aisathome-node/            node.json (root:aisathome 0640), token.env (root 0600)
#   /etc/systemd/system/aisathome-{llama,node}.service
# Nothing listens on a public port: llama-server binds 127.0.0.1, and the node only dials out.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
llama_build="" model="" node_bin="" node_config="" at_boot=0
# Per-slot context: each request gets at most this much, and one long request cannot starve
# the others (a single shared pool can: "Context size has been exceeded" on 2026-10-08).
ctx_per_slot="${CTX_PER_SLOT:-32768}" slots="${SLOTS:-2}" alias="${MODEL_ALIAS:-qwen3.8-27b}"
# Thinking budget in tokens (-1 = unlimited). Unlimited thinking ran for 8 minutes and filled
# the context; after the budget the model is told to answer.
reasoning_budget="${REASONING_BUDGET:-8192}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --llama-build) llama_build="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --node-bin) node_bin="$2"; shift 2 ;;
    --node-config) node_config="$2"; shift 2 ;;
    --enable-at-boot) at_boot=1; shift ;;
    *) usage ;;
  esac
done
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
[[ -x "$llama_build/bin/llama-server" ]] || { echo "no llama-server in $llama_build/bin" >&2; exit 1; }
[[ -f "$model" && -x "$node_bin" && -f "$node_config" ]] || usage
python3 -m json.tool "$node_config" >/dev/null || { echo "invalid JSON: $node_config" >&2; exit 1; }

[[ -t 0 ]] && echo "Paste the node token (input hidden), then Enter:" >&2
IFS= read -rs token || true
[[ -n "$token" ]] || { echo "no token on stdin" >&2; exit 1; }

# 1. Unprivileged system user without a login shell.
id aisathome >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aisathome-node --shell /usr/sbin/nologin aisathome

# 2. Binaries and llama.cpp shared libraries.
install -d -m 0755 /opt/aisathome-node/bin /opt/aisathome-node/lib
install -m 0755 "$llama_build/bin/llama-server" "$node_bin" /opt/aisathome-node/bin/
find "$llama_build" -name '*.so*' \( -type f -o -type l \) -exec cp -a {} /opt/aisathome-node/lib/ \;
chown -R root:root /opt/aisathome-node

# 3. Model (moved to save disk space) and state directory.
install -d -o aisathome -g aisathome -m 0750 /var/lib/aisathome-node /var/lib/aisathome-node/models
target=/var/lib/aisathome-node/models/model.gguf
if [[ "$(readlink -f "$model")" != "$target" ]]; then mv "$model" "$target"; fi
chown aisathome:aisathome "$target"; chmod 0640 "$target"

# 4. Config and token.
install -d -o root -g aisathome -m 0750 /etc/aisathome-node
install -o root -g aisathome -m 0640 "$node_config" /etc/aisathome-node/node.json
umask 077; printf 'AISATHOME_NODE_TOKEN=%s\n' "$token" > /etc/aisathome-node/token.env; umask 022
chown root:root /etc/aisathome-node/token.env; chmod 0600 /etc/aisathome-node/token.env

# 5. systemd units. Shared sandbox: no access to /home, read-only system, no privileges.
sandbox='NoNewPrivileges=yes
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
ExecStart=/usr/bin/nvidia-modprobe -u -c=0

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
ExecStart=/opt/aisathome-node/bin/llama-server -m $target --alias $alias \\
  --host 127.0.0.1 --port 8000 -ngl all -fa on -np $slots --kv-unified-per-slot $ctx_per_slot \\
  --reasoning-budget $reasoning_budget \\
  --reasoning-budget-message "(Thinking budget reached; answering now.)" --no-reasoning-preserve \\
  -ctk q8_0 -ctv q8_0 --cache-prompt --metrics
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
  rm -f "$state"; systemctl restart aisathome-llama aisathome-node
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

cat > /etc/systemd/system/aisathome-node.service <<UNIT
[Unit]
Description=AIs@home node agent (outbound connection to the gateway)
After=network-online.target aisathome-llama.service
Wants=network-online.target
BindsTo=aisathome-llama.service

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

systemctl daemon-reload
systemctl enable --now aisathome-node-watchdog.timer
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
