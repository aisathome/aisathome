#!/usr/bin/env bash
# Remove an AIs@home node installed by install.sh. Keeps the model unless --purge.
# AISATHOME_UNINSTALL_ROOT (a directory standing in for /) exists for install_test.sh only.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
r="${AISATHOME_UNINSTALL_ROOT:-}"
[[ -n "$r" || $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
sd="$r/etc/systemd/system"
systemctl disable aisathome-node-sleep.service 2>/dev/null || true
systemctl disable --now aisathome-node-watchdog.timer aisathome-node aisathome-llama aisathome-gpu-init 2>/dev/null || true
# Local node UI (install.sh, "Local node UI").
systemctl disable --now aisathome-node-restart.path 2>/dev/null || true
rm -f "$sd/aisathome-node-restart.path" "$sd/aisathome-node-restart.service" "$r/usr/local/bin/aisathome-node" \
  "$r/usr/local/share/applications/aisathome-node.desktop" "$r/usr/local/share/icons/hicolor/scalable/apps/aisathome-node.svg"
rm -f "$sd/aisathome-node.service" "$sd/aisathome-llama.service" "$sd/aisathome-gpu-init.service" \
  "$sd/aisathome-node-watchdog.service" "$sd/aisathome-node-watchdog.timer" "$r/usr/local/libexec/aisathome-node-watchdog" \
  "$sd/aisathome-node-sleep.service" "$r/usr/local/libexec/aisathome-node-sleep"
# Drop-ins: the installer's (local UI) and any made with "systemctl edit" for these units.
rm -rf "$sd"/aisathome-*.service.d
systemctl daemon-reload
rm -rf "$r/opt/aisathome-node" "$r/etc/aisathome-node"
if [[ "${1:-}" == "--purge" ]]; then
  rm -rf "$r/var/lib/aisathome-node"
  userdel aisathome 2>/dev/null || true
  echo "removed everything, including the model"
else
  echo "removed services, binaries and config; kept /var/lib/aisathome-node (model) and the aisathome user. Use --purge to remove them."
fi
