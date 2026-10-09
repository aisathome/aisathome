#!/usr/bin/env bash
# Remove an AIs@home node installed by install.sh. Keeps the model unless --purge.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
systemctl disable --now aisathome-node-watchdog.timer aisathome-node aisathome-llama aisathome-gpu-init 2>/dev/null || true
rm -f /etc/systemd/system/aisathome-node.service /etc/systemd/system/aisathome-llama.service /etc/systemd/system/aisathome-gpu-init.service \
  /etc/systemd/system/aisathome-node-watchdog.service /etc/systemd/system/aisathome-node-watchdog.timer /usr/local/libexec/aisathome-node-watchdog
systemctl daemon-reload
rm -rf /opt/aisathome-node /etc/aisathome-node
if [[ "${1:-}" == "--purge" ]]; then
  rm -rf /var/lib/aisathome-node
  userdel aisathome 2>/dev/null || true
  echo "removed everything, including the model"
else
  echo "removed services, binaries and config; kept /var/lib/aisathome-node (model) and the aisathome user. Use --purge to remove them."
fi
