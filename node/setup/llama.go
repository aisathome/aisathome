// SPDX-License-Identifier: Apache-2.0
package setup

// LlamaBuild is an upstream llama.cpp release's files for one platform, pinned by SHA-256 and
// size. CI downloads and checks them (node/packaging/windows/fetch-llama) and the installer
// ships what they hold; the node never downloads code.
type LlamaBuild struct {
	Tag    string  // the release ("b12342")
	Commit string  // the commit the release was built from: the Linux bundle's (drift_test.go)
	Files  []Model // the release assets, all unpacked into one folder
	Driver DriverReq
}

// llamaRelease is where the upstream release's assets are.
const llamaRelease = "https://github.com/ggml-org/llama.cpp/releases/download/"

// WindowsLlama is upstream's Windows x64 CUDA 12.4 build of the commit the Linux bundle builds
// (scripts/build-llama.sh), and the CUDA runtime DLLs it needs (cudart, cuBLAS). Checked
// 2026-11-11: the release's target commit, and the sizes and SHA-256 digests GitHub lists for its
// assets. The zips are flat: llama-server.exe beside its DLLs (ggml-cuda.dll, the CPU variants).
// b12323 (checked 2026-11-12 the same way) replaced b11146, and b12342 (checked 2026-11-14: target
// commit 0fb8f15, the assets' digests) replaced b12323; the CUDA runtime zip is byte-identical.
var WindowsLlama = LlamaBuild{
	Tag:    "b12342",
	Commit: "0fb8f157702702f33f4e018de8701b2569a1bc28",
	Files: []Model{
		{
			URL:    llamaRelease + "b12342/llama-b12342-bin-win-cuda-12.4-x64.zip",
			SHA256: "22f8c72dcc2b9a10f352acdca4cb371cf685590d1683603fa5562d93a376d28b",
			Size:   319018144,
		},
		{
			URL:    llamaRelease + "b12342/cudart-llama-bin-win-cuda-12.4-x64.zip",
			SHA256: "8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6",
			Size:   391443627,
		},
	},
	Driver: WindowsCUDA124,
}
