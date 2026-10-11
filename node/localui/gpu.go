// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"context"
	"encoding/csv"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU is one card as nvidia-smi reports it. Unknown values are nil.
type GPU struct {
	Index       int      `json:"index"`
	Name        string   `json:"name"`
	MemUsedMiB  *float64 `json:"mem_used_mib"`
	MemTotalMiB *float64 `json:"mem_total_mib"`
	TempC       *float64 `json:"temp_c"`
	UtilPct     *float64 `json:"util_pct"`
	PowerW      *float64 `json:"power_w"`
	PowerLimitW *float64 `json:"power_limit_w"`
}

// GPUInfo is the GPU panel: the cards, or why there are none.
type GPUInfo struct {
	GPUs []GPU  `json:"gpus"`
	Note string `json:"note,omitempty"`
}

const gpuQuery = "index,name,memory.used,memory.total,temperature.gpu,utilization.gpu,power.draw,power.limit"

// gpuProbe runs nvidia-smi at most every few seconds, however many pages poll.
type gpuProbe struct {
	cmd string
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	last GPUInfo
}

func newGPUProbe(cmd string) *gpuProbe {
	if cmd == "" {
		cmd = "nvidia-smi"
	}
	return &gpuProbe{cmd: cmd, ttl: 5 * time.Second}
}

func (p *gpuProbe) get(ctx context.Context) GPUInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < p.ttl {
		return p.last
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, p.cmd, "--query-gpu="+gpuQuery, "--format=csv,noheader,nounits").Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		p.last = GPUInfo{Note: "nvidia-smi not found: no GPU details"}
	case err != nil:
		p.last = GPUInfo{Note: "nvidia-smi failed: no GPU details (the agent may have no access to the GPU)"}
	default:
		gpus := parseGPUs(string(out))
		p.last = GPUInfo{GPUs: gpus}
		if len(gpus) == 0 {
			p.last.Note = "nvidia-smi reported no GPU"
		}
	}
	p.at = time.Now()
	return p.last
}

// parseGPUs reads nvidia-smi's CSV (noheader, nounits). Lines it cannot read are skipped.
func parseGPUs(out string) []GPU {
	r := csv.NewReader(strings.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	recs, _ := r.ReadAll()
	var gpus []GPU
	for _, f := range recs {
		if len(f) != 8 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		gpus = append(gpus, GPU{Index: idx, Name: strings.TrimSpace(f[1]), MemUsedMiB: num(f[2]), MemTotalMiB: num(f[3]),
			TempC: num(f[4]), UtilPct: num(f[5]), PowerW: num(f[6]), PowerLimitW: num(f[7])})
	}
	return gpus
}

// num parses a value, or nil for "[N/A]", "[Not Supported]" and the like.
func num(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &v
}
