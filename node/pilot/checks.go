// The checks a pilot makes before it serves, since no installer ran on the volunteer's machine:
// which tier it serves, whether the card the BOINC client assigned can hold that tier, and
// whether the card is fast enough (the installer's speed floor).
//
// SPDX-License-Identifier: Apache-2.0
package pilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node/engine"
	"github.com/aisathome/aisathome/node/setup"
)

// TierAuto picks the tier from the model file the pilot was given (each tier has its own file),
// and from the card when the file is none of the tiers' files.
const TierAuto = "auto"

// Device is the card as the engine sees it: the first device `llama-server --list-devices`
// lists under the task's environment, which holds the CUDA_VISIBLE_DEVICES the BOINC client
// assigned (job.xml). Memory is in MiB.
type Device struct {
	Name     string // the engine's device name ("CUDA0")
	Desc     string // the card ("NVIDIA GeForce RTX 4080")
	TotalMiB int
	FreeMiB  int
}

// deviceLine is one line of llama.cpp's --list-devices ("  %s: %s (%zu MiB, %zu MiB free)",
// common/arg.cpp; the same in the node's bundles).
var deviceLine = regexp.MustCompile(`^\s+(\S+): (.+) \((\d+) MiB, (\d+) MiB free\)\s*$`)

// parseDevices returns the first device of --list-devices' output, which follows its
// "Available devices:" line.
func parseDevices(out []byte) (Device, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	listed := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "Available devices:" {
			listed = true
			continue
		}
		if !listed {
			continue
		}
		m := deviceLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		total, err1 := strconv.Atoi(m[3])
		free, err2 := strconv.Atoi(m[4])
		if err1 != nil || err2 != nil {
			continue
		}
		return Device{Name: m[1], Desc: strings.TrimSpace(m[2]), TotalMiB: total, FreeMiB: free}, nil
	}
	return Device{}, errors.New("the engine lists no GPU (is the NVIDIA driver installed, and did the BOINC client assign a GPU?)")
}

// listDevicesWait bounds `llama-server --list-devices` (it initialises CUDA, a few seconds).
const listDevicesWait = time.Minute

// listDevices runs the engine with --list-devices in the engine's own environment. Its own
// messages go to out (the engine's log, which stays on the machine), never into the error: the
// pilot's log is shown on the project's public result page, and a loader error names local paths.
func listDevices(ctx context.Context, cmd engine.Command, out io.Writer) (Device, error) {
	ctx, cancel := context.WithTimeout(ctx, listDevicesWait)
	defer cancel()
	c := exec.CommandContext(ctx, cmd.Path, "--list-devices")
	c.Env = append(os.Environ(), cmd.Env...)
	c.Dir = cmd.Dir
	var stdout bytes.Buffer
	c.Stdout, c.Stderr = &stdout, io.Discard
	if out != nil {
		c.Stderr = out
	}
	err := c.Run()
	if out != nil {
		out.Write(stdout.Bytes())
	}
	if d, perr := parseDevices(stdout.Bytes()); perr == nil {
		return d, nil
	} else if err == nil {
		return Device{}, perr
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return Device{}, fmt.Errorf("the engine could not list the GPUs (%s; its messages are in engine.log)", ee.ProcessState)
	}
	if ctx.Err() != nil {
		return Device{}, fmt.Errorf("the engine could not list the GPUs within %s", listDevicesWait)
	}
	return Device{}, errors.New("the engine could not be started to list the GPUs")
}

// pickTier chooses the tier: the one named, or with TierAuto the tier whose model file has the
// model's size, else the card's tier. It says how it chose ("flag", "file" or "card").
func pickTier(name string, modelSize int64, d Device) (setup.Tier, string, error) {
	if name != TierAuto {
		t, ok := setup.TierByName(name)
		if !ok {
			return setup.Tier{}, "", fmt.Errorf("unknown tier %q", name)
		}
		return t, "flag", nil
	}
	for _, t := range setup.Tiers {
		if t.Model.Size == modelSize {
			return t, "file", nil
		}
	}
	t, ok := setup.TierForTotal(d.TotalMiB)
	if !ok {
		return setup.Tier{}, "", fmt.Errorf("the GPU (%s) has %d MiB of memory; the network needs a card with 16 GB or more", d.Desc, d.TotalMiB)
	}
	return t, "card", nil
}

// checkCard refuses a card that cannot hold the tier: too small a card (the tier came from the
// model file the task brought), or too little free memory now (the installer's check: other
// programs hold memory on the card, such as a desktop).
func checkCard(t setup.Tier, d Device) error {
	if d.TotalMiB < t.MinTotalMiB {
		return fmt.Errorf("the GPU (%s) has %d MiB of memory; the %s tier (%s) needs a card with at least %d MiB",
			d.Desc, d.TotalMiB, t.Name, t.Text(), t.MinTotalMiB)
	}
	if d.FreeMiB < t.MinFreeMiB {
		return fmt.Errorf("the GPU (%s) has %d MiB free of %d; the %s tier (%s) needs %d MiB free: other programs hold memory on it (a desktop, a game, another GPU task)",
			d.Desc, d.FreeMiB, d.TotalMiB, t.Name, t.Text(), t.MinFreeMiB)
	}
	return nil
}

// speedTest is the installer's speed test against the engine at base (setup.SpeedTest).
func speedTest(ctx context.Context, base string) (setup.Speed, error) {
	return setup.SpeedTest(ctx, setup.HTTPEngine{BaseURL: base}, setup.SpeedPromptTokens, setup.SpeedPredict)
}

// waitHealthy waits until the engine at base answers /health with 200 (it answers 503 while it
// loads the model), at most d. It stops when ctx ends.
func waitHealthy(ctx context.Context, base string, d, poll time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// speedCacheFile, in the engine folder (the BOINC project folder), remembers passed speed tests
// for speedCacheFor, so that a task the client restarts, and the next pilots on the same card,
// do not test again. Failures are never remembered.
const (
	speedCacheFile = "speed-checks.json"
	speedCacheFor  = 7 * 24 * time.Hour
	speedCacheMax  = 32
)

type speedPass struct {
	TPS float64   `json:"tps"`
	At  time.Time `json:"at"`
}

// speedKey names one card, tier and engine for the cache.
func speedKey(d Device, tier, engineID string) string {
	return fmt.Sprintf("%s|%d|%s|%s", d.Desc, d.TotalMiB, tier, engineID)
}

// engineID names the engine for the cache: the unpacked bundle's folder (named after the archive,
// which is named after its SHA-256), else the program's path, size and time.
func engineID(o Options, exe string) string {
	if o.LlamaServer == "" && o.EngineArchive != "" {
		return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(o.EngineArchive), ".xz"), ".tar")
	}
	if fi, err := os.Stat(exe); err == nil {
		return fmt.Sprintf("%s|%d|%d", exe, fi.Size(), fi.ModTime().UnixNano())
	}
	return exe
}

func readSpeedCache(dir string) map[string]speedPass {
	m := map[string]speedPass{}
	if dir == "" {
		return m
	}
	if b, err := os.ReadFile(filepath.Join(dir, speedCacheFile)); err == nil {
		if json.Unmarshal(b, &m) != nil || m == nil { // junk, or "null"
			m = map[string]speedPass{}
		}
	}
	return m
}

// cachedPass returns the pass remembered for key, if it is recent.
func cachedPass(dir, key string, now time.Time) (speedPass, bool) {
	p, ok := readSpeedCache(dir)[key]
	if !ok || now.Sub(p.At) >= speedCacheFor || now.Before(p.At.Add(-time.Hour)) || p.TPS <= 0 {
		return speedPass{}, false
	}
	return p, true
}

// rememberPass records a pass for key, dropping expired entries (and the oldest beyond
// speedCacheMax). The file is replaced whole: pilots on other GPUs of the same machine may write
// at the same time, and the last one wins (losing another's entry costs one more test).
func rememberPass(dir, key string, p speedPass) error {
	if dir == "" {
		return nil
	}
	m := readSpeedCache(dir)
	for k, v := range m {
		if p.At.Sub(v.At) >= speedCacheFor {
			delete(m, k)
		}
	}
	m[key] = p
	for len(m) > speedCacheMax {
		oldest := ""
		for k, v := range m {
			if oldest == "" || v.At.Before(m[oldest].At) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".speed-*")
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, speedCacheFile))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// slowError is the refusal of a card below the speed floor.
func slowError(d Device, s setup.Speed, minTPS float64) error {
	return fmt.Errorf("the GPU (%s) writes %.1f tokens/s after a %d-token prompt; the network needs at least %g. "+
		"If other programs used the GPU during the test, or its clocks or power limit are lowered, undo that",
		d.Desc, s.TPS, s.PromptTokens, minTPS)
}
