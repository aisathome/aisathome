// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"strings"
	"testing"
)

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"566.36", "527.41", true}, {"527.41", "527.41", true}, {"527.40", "527.41", false},
		{"570.65", "570.124.06", false}, {"570.124.06", "570.65", true}, {"535.183.01", "525.60.13", true},
		{"525.60.13", "525.60.13", true}, {"525.60", "525.60.13", false}, {"12.0", "5.0", true}, {"8.9", "12.0", false},
	} {
		if got := VersionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("VersionAtLeast(%s, %s) = %v", c.have, c.want, got)
		}
	}
}

func TestCheckDriverWindows(t *testing.T) {
	req := WindowsCUDA124
	for _, c := range []struct {
		name, drv, cap string
		want           string // "" = supported, else a part of the refusal
	}{
		{"RTX 4090", "566.36", "8.9", ""},
		{"RTX 3090", "528.02", "8.6", ""}, // native code: CUDA 12 minor-version compatibility
		{"RTX 3090", "522.25", "8.6", "NVIDIA driver 522.25 is too old for the node's llama-server (CUDA 12.4): it needs 527.41 or newer. Update it from nvidia.com/drivers"},
		{"Tesla T4", "538.78", "7.5", "needs NVIDIA driver 551.61 or newer for the node's llama-server (CUDA 12.4); this machine has 538.78"}, // PTX
		{"Tesla T4", "553.24", "7.5", ""},
		{"RTX 5070 Ti", "566.36", "12.0", "needs NVIDIA driver 570.65 or newer; this machine has 566.36"},
		{"RTX 5070 Ti", "576.88", "12.0", ""},
		{"GTX 780", "474.30", "3.5", "too old"},
		{"GTX 780", "566.36", "3.5", "has compute capability 3.5; the node's llama-server supports compute capability 5.0 and newer"},
		{"RTX 4090", "[N/A]", "8.9", "cannot read the driver version"},
	} {
		err := CheckDriver(GPU{Name: c.name, Driver: c.drv, Cap: c.cap}, req)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %s: %v", c.name, c.drv, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %s: got %v, want %q", c.name, c.drv, err, c.want)
		}
	}
}

// The Linux bundle's BUILD-INFO (as scripts/bundle_test.sh writes it): native code only.
func TestReqFromBuildInfo(t *testing.T) {
	info := "llama_cpp_tag=v0.5.0\ncuda_version=12.8\ncompute_caps=8.6 8.9 12.0\nmin_driver=525.60.13\nmin_driver_cc12=570.26\nmin_driver=1\n"
	req, err := ReqFromBuildInfo(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	if req.MinDriver != "525.60.13" || req.MinDriverCC12 != "570.26" || req.CUDA != "12.8" || strings.Join(req.Native, " ") != "8.6 8.9 12.0" || req.PTXFrom != "" {
		t.Fatalf("got %+v", req)
	}
	if err := CheckDriver(GPU{Name: "RTX 3090", Driver: "535.183.01", Cap: "8.6"}, req); err != nil {
		t.Error(err)
	}
	if err := CheckDriver(GPU{Name: "Tesla T4", Driver: "535.183.01", Cap: "7.5"}, req); err == nil || !strings.Contains(err.Error(), "supports 8.6 8.9 12.0") {
		t.Errorf("T4 on the Linux bundle: %v", err)
	}
	if err := CheckDriver(GPU{Name: "RTX 5090", Driver: "565.57.01", Cap: "12.0"}, req); err == nil || !strings.Contains(err.Error(), "570.26 or newer") {
		t.Errorf("RTX 50 on 565: %v", err)
	}
	if _, err := ReqFromBuildInfo(strings.NewReader("cuda_version=12.8\n")); err == nil {
		t.Error("incomplete BUILD-INFO: no error")
	}
}
