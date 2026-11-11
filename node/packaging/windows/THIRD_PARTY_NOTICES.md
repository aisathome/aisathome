# Third-party software in the AIs@home node for Windows

`aisathome-node.exe` is AIs@home code under the Apache License 2.0 (`LICENSE.txt`). It is
written in Go and contains the Go standard library and `golang.org/x/sys` (BSD 3-Clause) and
`github.com/coder/websocket` (ISC).

The `engine` folder holds software under other terms, which the Apache License does **not**
cover:

| Files | What | Terms |
|---|---|---|
| `llama-server.exe`, `llama-server-impl.dll`, the `llama*`, `ggml*` and `mtmd*` DLLs and the other files of llama.cpp's release | [llama.cpp](https://github.com/ggml-org/llama.cpp)'s own Windows x64 CUDA 12.4 release `b11146` (commit `7fe450e19305b828c199d602c23a8337aaa1f03b`), unmodified | MIT (below) |
| the OpenMP runtime DLL, when present | LLVM's OpenMP runtime, shipped in llama.cpp's release | Apache-2.0 with LLVM exceptions: `LICENSE-LLVM-OpenMP` in that folder |
| `cudart64_*.dll`, `cublas64_*.dll`, `cublasLt64_*.dll` | NVIDIA CUDA runtime and cuBLAS, from llama.cpp's `cudart-llama-bin-win-cuda-12.4-x64.zip`, unmodified | [NVIDIA CUDA Toolkit EULA](https://docs.nvidia.com/cuda/eula/index.html) (below) |

## llama.cpp

```
MIT License

Copyright (c) 2023-2026 The ggml authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## NVIDIA libraries

The CUDA EULA lists the CUDA Runtime and the CUDA BLAS Library (cuBLAS, cuBLASLt) among the
files that may be distributed with an application (Attachment A). Under its terms, as
summarised here (the EULA itself governs):

- they are distributed only as part of this installer and are only for use by its
  `llama-server` (1.1.2);
- they are licensed for use in systems with NVIDIA GPUs (2.1);
- they are redistributed unmodified (2.3);
- you may not reverse engineer, modify or redistribute them on their own (1.2).

They remain NVIDIA's software under NVIDIA's terms. The other licences here do not apply to
them.
