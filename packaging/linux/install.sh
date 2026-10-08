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
ctx="${CTX:-32768}" slots="${SLOTS:-2}" alias="${MODEL_ALIAS:-qwen3.8-27b}"
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

echo "Paste the node token (input hidden), then Enter:" >&2
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

cat > /etc/systemd/system/aisathome-llama.service <<UNIT
[Unit]
Description=AIs@home node: local llama-server (127.0.0.1 only)
After=network.target

[Service]
User=aisathome
Group=aisathome
Environment=LD_LIBRARY_PATH=/opt/aisathome-node/lib
# Create /dev/nvidia-uvm as root first: the sandbox (NoNewPrivileges) blocks the setuid helper
# that CUDA would otherwise use, e.g. right after boot.
ExecStartPre=+-/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/opt/aisathome-node/bin/llama-server -m $target --alias $alias \\
  --host 127.0.0.1 --port 8000 -ngl all -fa on -c $ctx -np $slots --kv-unified \\
  -ctk q8_0 -ctv q8_0 --cache-prompt --metrics
Restart=on-failure
RestartSec=5
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
$sandbox

[Install]
WantedBy=multi-user.target
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
if [[ $at_boot = 1 ]]; then systemctl enable aisathome-llama aisathome-node; fi
systemctl restart aisathome-llama
for i in $(seq 1 90); do curl -fs http://127.0.0.1:8000/health >/dev/null && break; sleep 2; done
curl -fs http://127.0.0.1:8000/health >/dev/null || { echo "llama-server did not become healthy; see: journalctl -u aisathome-llama" >&2; exit 1; }
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
