#!/usr/bin/env bash
# Build llama-server with CUDA for the prebuilt node bundle, and collect what it needs at
# run time into an empty directory: bin/llama-server, lib/ (llama.cpp's libraries, the CUDA
# runtime, cuBLAS and libgomp), licenses/ and BUILD-INFO.
#
#   scripts/build-llama.sh <outdir>
#
# CI runs it inside the nvidia/cuda devel image pinned in .github/workflows/release.yml;
# any Linux x86_64 machine with the CUDA 12 toolkit, cmake, ninja, git and g++ works too.
# Environment: CUDA_ARCHS (default "86-real;89-real;120a-real"), JOBS (default: nproc),
# LLAMA_SRC (an existing llama.cpp checkout at the pinned commit, instead of cloning),
# LLAMA_BUILD_DIR (a build directory to keep and reuse, instead of a temporary one).
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

[[ $# -eq 1 ]] || { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

# The llama.cpp release the presets were benchmarked with (bench/kvquant/RESULTS.md in the
# research repo, 2026-10-09). The commit pins the tag: a moved tag fails the build.
LLAMA_TAG=v0.5.0
LLAMA_COMMIT=7fe450e19305b828c199d602c23a8337aaa1f03b
# Real (SASS) code only, no PTX: RTX 30 (8.6), RTX 40 (8.9), RTX 50 (12.0; llama.cpp turns
# 120 into 120a anyway). Without PTX, a CUDA 12.8 build runs on any CUDA 12 driver (>= 525)
# through minor version compatibility, which does not cover PTX; see MIN_DRIVER.
archs="${CUDA_ARCHS:-86-real;89-real;120a-real}"
jobs="${JOBS:-$(nproc)}"
# Minimum driver: CUDA 12.x minor version compatibility needs >= 525 (NVIDIA, "CUDA
# Compatibility", table 1); the first CUDA 12 driver is 525.60.13 (CUDA 12.0 GA, toolkit
# release notes). Blackwell (RTX 50) needs the CUDA 12.8 driver, >= 570.26.
MIN_DRIVER=525.60.13
MIN_DRIVER_CC12=570.26

out="$1"
mkdir -p "$out"
[[ -z "$(ls -A "$out")" ]] || { echo "$out is not empty" >&2; exit 1; }
out="$(cd "$out" && pwd)"
[[ "$archs" =~ ^[0-9]+a?(-real)?(\;[0-9]+a?(-real)?)*$ ]] || { echo "bad CUDA_ARCHS: $archs" >&2; exit 2; }
[[ "$archs" != *virtual* ]] || { echo "CUDA_ARCHS: no PTX (-virtual): it breaks minor version compatibility" >&2; exit 2; }

nvcc="$(command -v nvcc || echo /usr/local/cuda/bin/nvcc)"
[[ -x "$nvcc" ]] || { echo "nvcc not found (CUDA toolkit)" >&2; exit 1; }
cuda_version="$("$nvcc" --version | sed -n 's/.*release \([0-9][0-9.]*\),.*/\1/p')"
# MIN_DRIVER and the NOTICE assume CUDA 12; CUDA 13 needs driver >= 580.
[[ "$cuda_version" == 12.* ]] || { echo "CUDA $cuda_version: this script assumes CUDA 12 (update MIN_DRIVER)" >&2; exit 1; }
cuda_lib="$(dirname "$(dirname "$(readlink -f "$nvcc")")")/lib64"
[[ -d "$cuda_lib" ]] || cuda_lib="$(dirname "$(dirname "$(readlink -f "$nvcc")")")/targets/x86_64-linux/lib"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
src="${LLAMA_SRC:-}"
if [[ -z "$src" ]]; then
  src="$work/llama.cpp"
  git -c advice.detachedHead=false clone -q --depth 1 --branch "$LLAMA_TAG" https://github.com/ggml-org/llama.cpp "$src"
fi
got="$(git -C "$src" rev-parse HEAD)"
[[ "$got" = "$LLAMA_COMMIT" ]] || { echo "llama.cpp $LLAMA_TAG is $got, pinned $LLAMA_COMMIT" >&2; exit 1; }

# GGML_NATIVE=OFF: no -march=native, which would tie the CPU code to the build machine
# (AVX2 is the baseline instead). NCCL off: one GPU per node, and it would add libnccl.
# No OpenSSL and no web UI (it is downloaded at build time): the node only uses the API.
# RPATH $ORIGIN/../lib: bin/llama-server finds lib/ without LD_LIBRARY_PATH, and a library
# in lib/ finds its neighbours the same way.
build="${LLAMA_BUILD_DIR:-$work/build}"
# GGML_CUDA_COMPRESSION_MODE=none: llama.cpp's default ("size") compresses the CUDA kernels
# with -compress-mode=size, which drivers before 570 cannot load ("device kernel image is
# invalid" on driver 535, rented RTX 3090, 2026-10-11). Uncompressed kernels run there; the
# library grows (libggml-cuda about 40 MB to 136 MB per architecture).
# shellcheck disable=SC2016 # $ORIGIN is for the linker, not the shell
cmake -S "$src" -B "$build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DGGML_CUDA=ON "-DCMAKE_CUDA_ARCHITECTURES=$archs" -DGGML_NATIVE=OFF -DGGML_CUDA_NCCL=OFF \
  -DGGML_CUDA_COMPRESSION_MODE=none \
  -DLLAMA_OPENSSL=OFF -DLLAMA_BUILD_UI=OFF -DLLAMA_USE_PREBUILT_UI=OFF \
  -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
  '-DCMAKE_INSTALL_RPATH=$ORIGIN/../lib' -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON
cmake --build "$build" --target llama-server -j "$jobs"

mkdir -p "$out/bin" "$out/lib" "$out/licenses/llama.cpp"
install -m 0755 "$build/bin/llama-server" "$out/bin/"
find "$build" -name '*.so*' \( -type f -o -type l \) -not -path '*/CMakeFiles/*' -exec cp -a {} "$out/lib/" \;

# Copy the libraries llama-server loads from the CUDA toolkit and the compiler, unmodified
# (the CUDA EULA allows redistribution of these files only unmodified: no strip). Anything
# outside the base system that is not on this list fails the build, so a new dependency
# cannot slip into the bundle unnoticed. libcuda.so.1 belongs to the driver and is never
# shipped.
ship='^(libcudart\.so\.12|libcublas\.so\.12|libcublasLt\.so\.12|libgomp\.so\.1)$'
base='^(linux-vdso\.so\.1|libc\.so\.6|libm\.so\.6|libdl\.so\.2|libpthread\.so\.0|librt\.so\.1|libstdc\+\+\.so\.6|libgcc_s\.so\.1|ld-linux-x86-64\.so\.2|libcuda\.so\.1)$'
# ldd prints paths as found through the RPATH (bin/../lib/...): compare resolved paths.
while read -r name _ path _; do
  [[ -n "$name" && "$name" != /* ]] || continue
  if [[ "$name" =~ $base ]]; then continue; fi
  if [[ "$(readlink -f "$path")" == "$out/lib/"* ]]; then continue; fi
  [[ "$name" =~ $ship ]] || { echo "unexpected dependency of llama-server: $name => $path" >&2; exit 1; }
  [[ -f "$path" ]] || { echo "$name not found (want it from $cuda_lib or the compiler)" >&2; exit 1; }
  cp -L "$path" "$out/lib/$name"
done < <(LD_LIBRARY_PATH="$cuda_lib" ldd "$out/bin/llama-server")
for want in libcudart.so.12 libcublas.so.12 libcublasLt.so.12; do
  [[ -f "$out/lib/$want" ]] || { echo "missing $want in the bundle" >&2; exit 1; }
done

# Smoke test without a GPU: the driver stub stands in for libcuda.so.1, and nothing may
# resolve from outside the bundle except the base system.
stub="$work/stub"; mkdir -p "$stub"
[[ -f "$cuda_lib/stubs/libcuda.so" ]] || { echo "no driver stub in $cuda_lib/stubs" >&2; exit 1; }
ln -s "$cuda_lib/stubs/libcuda.so" "$stub/libcuda.so.1"
deps="$(LD_LIBRARY_PATH="$stub" ldd "$out/bin/llama-server")"
if grep -q 'not found' <<<"$deps"; then echo "$deps" >&2; echo "unresolved libraries" >&2; exit 1; fi
while read -r name _ path _; do
  [[ -n "$name" && "$name" != /* && ! "$name" =~ $base ]] || continue
  [[ "$(readlink -f "$path")" == "$out/lib/"* ]] || { echo "$name resolves outside the bundle: $path" >&2; exit 1; }
done <<<"$deps"
# The stub has no GPU behind it, so the exit status says little; the version line must show.
LD_LIBRARY_PATH="$stub" "$out/bin/llama-server" --version > "$work/version.txt" 2>&1 || true
cat "$work/version.txt"
grep -q "$(cut -c1-7 <<<"$LLAMA_COMMIT")" "$work/version.txt" || { echo "llama-server --version does not name the pinned commit" >&2; exit 1; }

# Licences: llama.cpp (MIT) and the third-party code it compiles in.
cp "$src/LICENSE" "$out/licenses/llama.cpp/LICENSE"
(cd "$src" && find licenses vendor -type f -name 'LICENSE*' -print0) |
  while IFS= read -r -d '' f; do cp "$src/$f" "$out/licenses/llama.cpp/$(tr / _ <<<"$f")"; done
if [[ -f "$(dirname "$cuda_lib")/EULA.txt" ]]; then cp "$(dirname "$cuda_lib")/EULA.txt" "$out/licenses/NVIDIA-CUDA-EULA.txt"; fi

# compute_caps: what install.sh accepts, derived from the architectures (86-real -> 8.6).
caps="$(tr ';' '\n' <<<"$archs" | sed -E 's/a?(-real)?$//; s/^([0-9]+)([0-9])$/\1.\2/' | paste -sd' ')"
cat > "$out/BUILD-INFO" <<INFO
llama_cpp_tag=$LLAMA_TAG
llama_cpp_commit=$LLAMA_COMMIT
cuda_version=$cuda_version
cuda_archs=$archs
compute_caps=$caps
min_driver=$MIN_DRIVER
min_driver_cc12=$MIN_DRIVER_CC12
build_image=${BUILD_IMAGE:-unknown}
INFO
cat "$out/BUILD-INFO"
du -sh "$out"/bin/* "$out"/lib/* | sort -h
du -sh "$out"
