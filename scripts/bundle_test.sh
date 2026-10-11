#!/usr/bin/env bash
# Test the prebuilt node bundle's packaging without CUDA: build a release with a fake
# llama-server (scripts/build-release.sh with LLAMA_DIR), check SHA256SUMS and the archive's
# layout, and dry-run the unpacked install.sh with a fake nvidia-smi.
#
#   ./scripts/bundle_test.sh
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

# A stand-in for the output of scripts/build-llama.sh.
llama="$tmp/llama"
mkdir -p "$llama/bin" "$llama/lib" "$llama/licenses/llama.cpp"
printf '#!/bin/sh\necho fake llama-server\n' > "$llama/bin/llama-server"; chmod 0755 "$llama/bin/llama-server"
: > "$llama/lib/libggml-cuda.so"; ln -s libggml-cuda.so "$llama/lib/libggml-cuda.so.0"
echo "MIT" > "$llama/licenses/llama.cpp/LICENSE"
cat > "$llama/BUILD-INFO" <<'INFO'
llama_cpp_tag=v0.5.0
llama_cpp_commit=7fe450e19305b828c199d602c23a8337aaa1f03b
cuda_version=12.8
cuda_archs=86-real;89-real;120a-real
compute_caps=8.6 8.9 12.0
min_driver=525.60.13
min_driver_cc12=570.26
INFO

LLAMA_DIR="$llama" ./scripts/build-release.sh v0.0.0-test "$tmp/dist" >/dev/null
b=aisathome-node-bundle-linux-amd64
(cd "$tmp/dist" && sha256sum -c --quiet SHA256SUMS) || fail "SHA256SUMS does not match"
grep -q " $b.tar.xz\$" "$tmp/dist/SHA256SUMS" || fail "no $b.tar.xz in SHA256SUMS"
listing="$(tar -tvJf "$tmp/dist/$b.tar.xz")"
grep -qv ' 0/0 ' <<<"$listing" && fail "files not owned by 0/0 in the archive"
mkdir "$tmp/x" && tar -xJf "$tmp/dist/$b.tar.xz" -C "$tmp/x"
d="$tmp/x/$b"
for f in install.sh uninstall.sh bin/aisathome-node bin/llama-server; do [[ -x "$d/$f" ]] || fail "$f missing or not executable"; done
for f in LICENSE README.md THIRD_PARTY_NOTICES.md BUILD-INFO licenses/llama.cpp/LICENSE lib/libggml-cuda.so; do [[ -f "$d/$f" ]] || fail "$f missing"; done
[[ -L "$d/lib/libggml-cuda.so.0" ]] || fail "library symlink not kept"
grep -qx 'aisathome_version=v0.0.0-test' "$d/BUILD-INFO" || fail "BUILD-INFO has no aisathome_version"
cmp -s "$d/install.sh" node/packaging/linux/install.sh || fail "install.sh differs from the repository's"
if [[ "$(uname -m)" = x86_64 ]]; then
  "$d/bin/aisathome-node" -version | grep -q v0.0.0-test || fail "bundled aisathome-node is not this build"
fi

# The unpacked installer finds the bundle by itself.
mkdir "$tmp/fake"
printf '#!/bin/sh\necho "0, NVIDIA GeForce RTX 3090, 535.183.01, 8.6"\n' > "$tmp/fake/nvidia-smi"
printf '#!/bin/sh\necho "Filesystem 1024-blocks Used Available Capacity Mounted on"\necho "/dev/fake 1 1 100000000 1%% /"\n' > "$tmp/fake/df"
chmod +x "$tmp/fake/"*
if [[ "$(uname -m)" = x86_64 ]] && grep -qw avx2 /proc/cpuinfo; then
  out="$(PATH="$tmp/fake:$PATH" "$d/install.sh" --dry-run --node-id ci 2>&1)" || fail "dry run: $out"
  for want in "llama-server: prebuilt, llama.cpp v0.5.0" "GPU 0: NVIDIA GeForce RTX 3090" '"node_id": "ci"' "preset: long-context"; do
    [[ "$out" == *"$want"* ]] || fail "dry run lacks '$want': $out"
  done
fi
echo "bundle: all tests passed ($(du -h "$tmp/dist/$b.tar.xz" | cut -f1) with a fake llama-server)"
