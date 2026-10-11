# Third-party software in the prebuilt node bundle

`install.sh`, `uninstall.sh` and `bin/aisathome-node` are AIs@home code under the Apache
License 2.0 (`LICENSE`). The bundle also contains software under other terms, which the
Apache License does **not** cover:

| Files | What | Terms |
|---|---|---|
| `bin/llama-server`, `lib/libllama*`, `lib/libggml*`, `lib/libmtmd*` and the other libraries built from llama.cpp | [llama.cpp](https://github.com/ggml-org/llama.cpp) at the tag and commit in `BUILD-INFO`, built by `scripts/build-llama.sh` | MIT, plus the licences of the code it compiles in: `licenses/llama.cpp/` |
| `lib/libcudart.so.12`, `lib/libcublas.so.12`, `lib/libcublasLt.so.12` | NVIDIA CUDA runtime and cuBLAS, from the CUDA toolkit named in `BUILD-INFO`, unmodified | [NVIDIA CUDA Toolkit EULA](https://docs.nvidia.com/cuda/eula/index.html) (`licenses/NVIDIA-CUDA-EULA.txt` when the build image includes it) |
| `lib/libgomp.so.1` | GNU OpenMP runtime, from GCC | GPL-3.0 with the GCC Runtime Library Exception 3.1 |

## NVIDIA libraries

The CUDA EULA lists `libcudart.so` ("CUDA Runtime") and `libcublas.so`, `libcublasLt.so`
("CUDA BLAS Library") as files that may be distributed with an application (Attachment A,
checked against EULA v13.4, last updated 26 January 2026). Under its terms, as summarised
here (the EULA itself governs):

- they are distributed only as part of this bundle and are only for use by its
  `llama-server` (1.1.2);
- they are licensed for use in systems with NVIDIA GPUs (2.1);
- they are redistributed unmodified, not even stripped (2.3);
- you may not reverse engineer, modify or redistribute them on their own (1.2).

They remain NVIDIA's software under NVIDIA's terms. The rest of the bundle's licences do not
apply to them.
