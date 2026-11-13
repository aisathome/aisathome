// SPDX-License-Identifier: Apache-2.0
package node

import "github.com/aisathome/aisathome/node/engine"

// EngineConfig is node.json's "engine": the agent runs llama-server itself, with the command
// for the tier (package setup builds it, the same flags as the Linux installer's).
type EngineConfig struct {
	Tier        string `json:"tier"`                   // "standard" or "big"
	LlamaServer string `json:"llama_server,omitempty"` // default: Layout.EngineProgram, from the agent's folder
	Model       string `json:"model,omitempty"`        // default: Layout.Model
	// CUDADevice pins the engine to one card when the machine has several: its UUID from
	// nvidia-smi (CUDA_VISIBLE_DEVICES). Empty: no pin.
	CUDADevice string `json:"cuda_device,omitempty"`
	// Port is the engine's port on 127.0.0.1; default 8000, as on Linux.
	Port int `json:"port,omitempty"`
}

// EngineControl is the engine process the agent runs: package engine's Supervisor.
type EngineControl interface {
	Stop()
	Start()
	Restart()
	Status() engine.Status
}
