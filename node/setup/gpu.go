// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SMI runs nvidia-smi with args and returns its standard output. Tests use a fake.
type SMI interface {
	Query(ctx context.Context, args ...string) (string, error)
}

// ErrNoSMI means nvidia-smi was not found: no NVIDIA driver is installed.
var ErrNoSMI = errors.New("nvidia-smi not found: install the NVIDIA driver and restart")

// ExecSMI runs the nvidia-smi at Path, or the first of SMICandidates that exists.
type ExecSMI struct {
	Path    string
	Timeout time.Duration // per call; 0 means 15 s
}

// Query runs nvidia-smi. A failure's error carries what nvidia-smi printed.
func (s ExecSMI) Query(ctx context.Context, args ...string) (string, error) {
	path := s.Path
	if path == "" {
		var err error
		if path, err = findSMI(); err != nil {
			return "", err
		}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoSMI
	}
	if err != nil {
		msg := strings.TrimSpace(string(out) + " " + stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return string(out), nil
}

// findSMI is nvidia-smi on PATH, else the first platform candidate that exists.
func findSMI() (string, error) {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p, nil
	}
	for _, p := range smiCandidates() {
		if p, err := exec.LookPath(p); err == nil {
			return p, nil
		}
	}
	return "", ErrNoSMI
}

// winSMICandidates is where Windows drivers put nvidia-smi.exe: System32 with current drivers
// (DCH), NVSMI under Program Files with old ones. Pure, so it is tested on Linux too.
func winSMICandidates(getenv func(string) string) []string {
	root := strings.TrimRight(getenv("SystemRoot"), `\/`)
	if root == "" {
		root = `C:\Windows`
	}
	pf := strings.TrimRight(getenv("ProgramFiles"), `\/`)
	if pf == "" {
		pf = `C:\Program Files`
	}
	return []string{root + `\System32\nvidia-smi.exe`, pf + `\NVIDIA Corporation\NVSMI\nvidia-smi.exe`}
}

// GPU is one card as the setup sees it.
type GPU struct {
	Index    int
	UUID     string
	Name     string
	TotalMiB int
	FreeMiB  int
	Driver   string // "566.36" (Windows) or "535.183.01" (Linux)
	Cap      string // compute capability, "8.9"
}

// gpuQuery is one nvidia-smi call for both the tier and the driver check.
const gpuQuery = "index,uuid,name,memory.total,memory.free,driver_version,compute_cap"

// ReadGPUs lists the cards. With no driver it returns ErrNoSMI. A driver too old to know a
// field fails the query; then, if the driver version alone can be read, the error says which
// driver it is (CheckDriver's minimum applies).
func ReadGPUs(ctx context.Context, smi SMI) ([]GPU, error) {
	out, err := smi.Query(ctx, "--query-gpu="+gpuQuery, "--format=csv,noheader,nounits")
	if errors.Is(err, ErrNoSMI) {
		return nil, err
	}
	if err != nil {
		if v, verr := smi.Query(ctx, "--query-gpu=driver_version", "--format=csv,noheader"); verr == nil {
			if d := strings.TrimSpace(strings.SplitN(v, "\n", 2)[0]); d != "" {
				return nil, fmt.Errorf("nvidia-smi could not report the GPUs with NVIDIA driver %s, probably too old: update the driver and restart (%v)", d, err)
			}
		}
		return nil, fmt.Errorf("nvidia-smi failed, so the NVIDIA driver is not working: %v", err)
	}
	gpus, err := ParseGPUs(out)
	if err != nil {
		return nil, err
	}
	if len(gpus) == 0 {
		return nil, errors.New("nvidia-smi lists no GPU")
	}
	return gpus, nil
}

// ParseGPUs reads nvidia-smi's CSV for gpuQuery (noheader, nounits). Memory that is not a
// number ("[N/A]") is an error: the tier cannot be chosen without it.
func ParseGPUs(out string) ([]GPU, error) {
	recs, err := readCSV(out)
	if err != nil {
		return nil, fmt.Errorf("cannot read nvidia-smi's output: %v", err)
	}
	var gpus []GPU
	for _, f := range recs {
		if len(f) == 1 && f[0] == "" {
			continue
		}
		if len(f) != 7 {
			return nil, fmt.Errorf("cannot read nvidia-smi's output: %q", strings.Join(f, ", "))
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("cannot read nvidia-smi's output: %q", strings.Join(f, ", "))
		}
		total, err1 := strconv.Atoi(f[3])
		free, err2 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil || total < 0 || free < 0 {
			return nil, fmt.Errorf("cannot read the GPU memory from nvidia-smi: %q", strings.Join(f, ", "))
		}
		gpus = append(gpus, GPU{Index: idx, UUID: f[1], Name: f[2], TotalMiB: total, FreeMiB: free, Driver: f[5], Cap: f[6]})
	}
	return gpus, nil
}

// ParseComputeApps reads `--query-compute-apps=gpu_uuid,pid,used_memory --format=csv,noheader,nounits`
// and returns the memory (MiB) that process pid holds on each card, by UUID. On Windows (WDDM)
// used_memory is "[N/A]": such lines count nothing, so the agent stops its own engine before
// it measures instead.
func ParseComputeApps(out string, pid int) map[string]int {
	recs, _ := readCSV(out)
	own := map[string]int{}
	for _, f := range recs {
		if len(f) != 3 {
			continue
		}
		p, err := strconv.Atoi(f[1])
		mib, err2 := strconv.Atoi(f[2])
		if err != nil || err2 != nil || p != pid || mib < 0 {
			continue
		}
		own[f[0]] += mib
	}
	return own
}

func readCSV(out string) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(strings.ReplaceAll(out, "\r\n", "\n")))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	for _, rec := range recs {
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
	}
	return recs, err
}

// Choice is the card the engine will use and its tier.
type Choice struct {
	GPU
	Tier    Tier
	OwnMiB  int  // memory of this node's own running engine, counted as free
	Several bool // more than one GPU: pin the engine to this card (CUDA_VISIBLE_DEVICES=UUID)
}

// Free is the card's free memory with the node's own engine's memory added back.
func (c Choice) Free() int { return c.FreeMiB + c.OwnMiB }

// ChooseCard picks the largest card (on a tie, the one with more free memory) and its tier, as
// install.sh's pick_tier. own is the memory this node's running engine holds per card UUID
// (ParseComputeApps), counted as free before the cards are compared; nil when none runs.
func ChooseCard(gpus []GPU, own map[string]int) (Choice, error) {
	if len(gpus) == 0 {
		return Choice{}, errors.New("nvidia-smi lists no GPU")
	}
	var c Choice
	for i, g := range gpus {
		o := own[g.UUID]
		if i == 0 || g.TotalMiB > c.TotalMiB || (g.TotalMiB == c.TotalMiB && g.FreeMiB+o > c.Free()) {
			c = Choice{GPU: g, OwnMiB: o}
		}
	}
	c.Several = len(gpus) > 1
	t, ok := TierForTotal(c.TotalMiB)
	if !ok {
		return c, TooSmall(c.GPU)
	}
	c.Tier = t
	return c, nil
}

// CheckFit refuses when the tier does not fit in the card's free memory now; the tier is never
// shrunk to fit. On Windows the desktop usually shares the card, so the advice names it.
func CheckFit(c Choice) error {
	need := c.Tier.MinFreeMiB
	if c.Free() >= need {
		return nil
	}
	return fmt.Errorf("GPU %d (%s) has %d MiB free; the %s tier (%s) needs %d MiB free. %s",
		c.Index, c.Name, c.Free(), c.Tier.Name, c.Tier.Text(), need, FitAdvice(c.Tier))
}

// FitAdvice is what to do when the tier does not fit in the card's free memory.
func FitAdvice(t Tier) string {
	if t.Name == "standard" {
		return "On a 16 GB card the standard tier leaves room for a light desktop only (about 1.5 GB in all for the screen and other programs). Close programs that use the GPU (games, browsers, video; nvidia-smi lists them), or connect the monitor to the motherboard's graphics (integrated GPU), and run the setup again."
	}
	return "Close programs that use the GPU (games, browsers, video; nvidia-smi lists them; a desktop with a browser can take 2 GB) or connect the monitor to the motherboard's graphics, and run the setup again."
}
