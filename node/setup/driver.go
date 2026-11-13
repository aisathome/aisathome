// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DriverReq is what the engine build needs from the NVIDIA driver and the card. Cards with
// native code for their compute capability need MinDriver (CUDA minor-version compatibility);
// cards that run from PTX (compiled by the driver at first start) need a driver for the build's
// own CUDA version, MinDriverPTX.
type DriverReq struct {
	CUDA          string   // the build's CUDA version, for messages ("12.4")
	MinDriver     string   // every card
	Native        []string // compute capabilities with native code ("8.6")
	PTXFrom       string   // the lowest compute capability its PTX runs on ("5.0"); "" = native only
	MinDriverPTX  string   // cards that run from PTX
	MinDriverCC12 string   // compute capability 12 and up (RTX 50); "" = no extra minimum
	Hint          string   // how to update the driver on this platform
}

// WindowsCUDA124 is upstream llama.cpp's Windows CUDA 12.4 build of the pinned commit (the
// plan's choice; WindowsLlama pins the zips). ggml's default architectures for CUDA 12.4:
// native code for 8.6 and 8.9, PTX for 5.0 to 9.0. Drivers (Checked 2026-11-10, NVIDIA's CUDA Toolkit release
// notes, Windows x86_64 column): 527.41 is CUDA 12.0 GA's (CUDA 12.x minor-version
// compatibility), 551.61 CUDA 12.4 GA's (PTX from 12.4 needs it), 570.65 CUDA 12.8 GA's, the
// first CUDA with RTX 50 (Blackwell, 12.0). To re-measure on the GPU checklist (slice 7).
var WindowsCUDA124 = DriverReq{
	CUDA:          "12.4",
	MinDriver:     "527.41",
	Native:        []string{"8.6", "8.9"},
	PTXFrom:       "5.0",
	MinDriverPTX:  "551.61",
	MinDriverCC12: "570.65",
	Hint:          "Update it from nvidia.com/drivers or the NVIDIA app, then restart the PC.",
}

var (
	reVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)
	reCap     = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

// ReqFromBuildInfo reads a Linux bundle's BUILD-INFO (key=value lines, as scripts/build-llama.sh
// writes them): cuda_version, min_driver, min_driver_cc12 and compute_caps (native only, no PTX).
func ReqFromBuildInfo(r io.Reader) (DriverReq, error) {
	kv := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if _, seen := kv[k]; ok && !seen {
			kv[k] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return DriverReq{}, err
	}
	req := DriverReq{
		CUDA: kv["cuda_version"], MinDriver: kv["min_driver"], MinDriverCC12: kv["min_driver_cc12"],
		Native: strings.Fields(kv["compute_caps"]),
		Hint:   "Update the driver (Ubuntu: sudo ubuntu-drivers install), then reboot.",
	}
	ok := reVersion.MatchString(req.MinDriver) && reVersion.MatchString(req.MinDriverCC12) && len(req.Native) > 0
	for _, c := range req.Native {
		ok = ok && reCap.MatchString(c)
	}
	if !ok {
		return DriverReq{}, fmt.Errorf("BUILD-INFO is incomplete; download the bundle again")
	}
	return req, nil
}

// CheckDriver refuses a card the build cannot run on, or a driver too old for it, with the
// minimum in the message.
func CheckDriver(g GPU, req DriverReq) error {
	if !reVersion.MatchString(g.Driver) || !reCap.MatchString(g.Cap) {
		return fmt.Errorf("cannot read the driver version and compute capability from nvidia-smi: %q, %q", g.Driver, g.Cap)
	}
	if !VersionAtLeast(g.Driver, req.MinDriver) {
		return fmt.Errorf("NVIDIA driver %s is too old for the node's llama-server (CUDA %s): it needs %s or newer. %s",
			g.Driver, req.CUDA, req.MinDriver, req.Hint)
	}
	native := slices.Contains(req.Native, g.Cap)
	if !native {
		if req.PTXFrom == "" || !VersionAtLeast(g.Cap, req.PTXFrom) {
			return fmt.Errorf("GPU %d (%s) has compute capability %s; the node's llama-server supports %s. This card cannot run it.",
				g.Index, g.Name, g.Cap, supported(req))
		}
		if !VersionAtLeast(g.Driver, req.MinDriverPTX) {
			return fmt.Errorf("GPU %d (%s) needs NVIDIA driver %s or newer for the node's llama-server (CUDA %s); this machine has %s. %s",
				g.Index, g.Name, req.MinDriverPTX, req.CUDA, g.Driver, req.Hint)
		}
	}
	if major, _, _ := strings.Cut(g.Cap, "."); req.MinDriverCC12 != "" && atoi(major) >= 12 && !VersionAtLeast(g.Driver, req.MinDriverCC12) {
		return fmt.Errorf("GPU %d (%s) needs NVIDIA driver %s or newer; this machine has %s. %s",
			g.Index, g.Name, req.MinDriverCC12, g.Driver, req.Hint)
	}
	return nil
}

func supported(req DriverReq) string {
	s := strings.Join(req.Native, " ")
	if req.PTXFrom != "" {
		s = "compute capability " + req.PTXFrom + " and newer"
	}
	return s
}

// VersionAtLeast compares dotted versions number by number ("570.65" >= "570.124.06" is
// false); a missing part counts as 0. Parts that are not numbers count as 0 too.
func VersionAtLeast(have, want string) bool {
	a, b := strings.Split(have, "."), strings.Split(want, ".")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = atoi(a[i])
		}
		if i < len(b) {
			y = atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
