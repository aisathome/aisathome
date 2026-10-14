#!/usr/bin/env bash
# Build static release binaries and their SHA256SUMS into an empty directory.
#
#   [LLAMA_DIR=<dir>] ./scripts/build-release.sh <version> <outdir>
#
# Assets are named aisathome-<command>-<os>-<arch>[.exe]; install.sh relies on that.
# With LLAMA_DIR (the output of scripts/build-llama.sh) it also packs the prebuilt node
# bundle aisathome-node-bundle-linux-amd64.tar.xz: install.sh, aisathome-node, llama-server
# and its libraries, ready for `sudo ./install.sh`.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

[[ $# -eq 2 ]] || { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
version="$1" out="$2"
[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version: $version" >&2; exit 2; }
mkdir -p "$out"
[[ -z "$(ls -A "$out")" ]] || { echo "$out is not empty" >&2; exit 1; }
out="$(cd "$out" && pwd)"
if [[ -n "${LLAMA_DIR:-}" ]]; then LLAMA_DIR="$(cd "$LLAMA_DIR" && pwd)"; fi
cd "$(dirname "$0")/.."

targets=(node/linux/amd64 node/linux/arm64 node/windows/amd64 gateway/linux/amd64 gateway/linux/arm64)
for t in "${targets[@]}"; do
  IFS=/ read -r cmd os arch <<<"$t"
  ext=""; [[ $os = windows ]] && ext=.exe
  name="aisathome-$cmd-$os-$arch$ext"
  echo "building $name ($version)"
  # The node reports node.Version (logs, -version, the gateway's hello, the local UI). The
  # linker ignores -X for a variable that a command does not link.
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X github.com/aisathome/aisathome/node.Version=$version" \
    -o "$out/$name" "./$cmd/cmd/aisathome-$cmd"
done
if [[ -n "${LLAMA_DIR:-}" ]]; then
  [[ -x "$LLAMA_DIR/bin/llama-server" && -f "$LLAMA_DIR/BUILD-INFO" ]] ||
    { echo "LLAMA_DIR=$LLAMA_DIR: no bin/llama-server and BUILD-INFO (see scripts/build-llama.sh)" >&2; exit 1; }
  bundle=aisathome-node-bundle-linux-amd64
  stage="$(mktemp -d)"
  trap 'rm -rf "$stage"' EXIT
  mkdir -p "$stage/$bundle"
  cp -a "$LLAMA_DIR/." "$stage/$bundle/"
  install -m 0755 "$out/aisathome-node-linux-amd64" "$stage/$bundle/bin/aisathome-node"
  install -m 0755 node/packaging/linux/install.sh node/packaging/linux/uninstall.sh "$stage/$bundle/"
  install -m 0644 LICENSE node/packaging/linux/README.md node/packaging/linux/THIRD_PARTY_NOTICES.md "$stage/$bundle/"
  # One aisathome_version line, also when LLAMA_DIR is an unpacked older bundle (which has one).
  sed -i '/^aisathome_version=/d' "$stage/$bundle/BUILD-INFO"
  echo "aisathome_version=$version" >> "$stage/$bundle/BUILD-INFO"
  echo "packing $bundle.tar.xz ($(du -sh "$stage/$bundle" | cut -f1) unpacked)"
  # Reproducible archive: sorted, root-owned, timestamps from the last commit.
  mtime="$(git log -1 --format=%ct 2>/dev/null || date +%s)"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' --mtime="@$mtime" -C "$stage" -cf - "$bundle" |
    xz -T0 -9 > "$out/$bundle.tar.xz"
fi
(cd "$out" && sha256sum aisathome-* > SHA256SUMS)
cat "$out/SHA256SUMS"
