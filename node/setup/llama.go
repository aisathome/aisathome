// SPDX-License-Identifier: Apache-2.0
package setup

// LlamaBuild is an upstream llama.cpp release's files for one platform, pinned by SHA-256 and
// size. CI downloads and checks them (node/packaging/windows/fetch-llama) and the installer
// ships what they hold; the node never downloads code.
type LlamaBuild struct {
	Tag    string  // the release ("b11146")
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
var WindowsLlama = LlamaBuild{
	Tag:    "b11146",
	Commit: "7fe450e19305b828c199d602c23a8337aaa1f03b",
	Files: []Model{
		{
			URL:    llamaRelease + "b11146/llama-b11146-bin-win-cuda-12.4-x64.zip",
			SHA256: "3c806a6ceccc3dae1c743ceb1a1fb2cce5b76f40bfbd4c6b7b8afb6ef45a5807",
			Size:   253869799,
		},
		{
			URL:    llamaRelease + "b11146/cudart-llama-bin-win-cuda-12.4-x64.zip",
			SHA256: "8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6",
			Size:   391443627,
		},
	},
	Driver: WindowsCUDA124,
}
