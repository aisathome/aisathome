#!/usr/bin/env bash
# Build static release binaries and their SHA256SUMS into an empty directory.
#
#   ./scripts/build-release.sh <version> <outdir>
#
# Assets are named aisathome-<command>-<os>-<arch>[.exe]; install.sh relies on that.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

[[ $# -eq 2 ]] || { sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
version="$1" out="$2"
[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad version: $version" >&2; exit 2; }
mkdir -p "$out"
[[ -z "$(ls -A "$out")" ]] || { echo "$out is not empty" >&2; exit 1; }
out="$(cd "$out" && pwd)"
cd "$(dirname "$0")/.."

targets=(node/linux/amd64 node/linux/arm64 node/windows/amd64 gateway/linux/amd64 gateway/linux/arm64)
for t in "${targets[@]}"; do
  IFS=/ read -r cmd os arch <<<"$t"
  ext=""; [[ $os = windows ]] && ext=.exe
  name="aisathome-$cmd-$os-$arch$ext"
  echo "building $name ($version)"
  # main.version exists only where the command defines it; the linker ignores it elsewhere.
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/$name" "./$cmd/cmd/aisathome-$cmd"
done
(cd "$out" && sha256sum aisathome-* > SHA256SUMS)
cat "$out/SHA256SUMS"
