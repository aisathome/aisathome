// SPDX-License-Identifier: Apache-2.0
package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSMIEnv makes this test binary act as nvidia-smi (TestMain), so ExecSMI is tested on
// Windows too, where a shell script cannot stand in.
const fakeSMIEnv = "AISATHOME_TEST_FAKE_NVIDIA_SMI"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeSMIEnv) {
	case "4090":
		if strings.Contains(strings.Join(os.Args[1:], " "), "--query-gpu="+gpuQuery) {
			fmt.Print("0, GPU-4090, NVIDIA GeForce RTX 4090, 24564, 22987, 566.36, 8.9\r\n")
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "unexpected arguments:", os.Args[1:])
		os.Exit(2)
	case "broken":
		fmt.Fprintln(os.Stderr, "Failed to initialize NVML: Driver/library version mismatch")
		os.Exit(9)
	}
	os.Exit(m.Run())
}

func fakeExecSMI(t *testing.T, mode string) ExecSMI {
	t.Setenv(fakeSMIEnv, mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ExecSMI{Path: exe}
}

func TestExecSMI(t *testing.T) {
	gpus, err := ReadGPUs(context.Background(), fakeExecSMI(t, "4090"))
	if err != nil || len(gpus) != 1 || gpus[0].Name != "NVIDIA GeForce RTX 4090" || gpus[0].Cap != "8.9" {
		t.Fatalf("ReadGPUs = %+v, %v", gpus, err)
	}
	_, err = fakeExecSMI(t, "broken").Query(context.Background(), "--query-gpu=index")
	if err == nil || !strings.Contains(err.Error(), "Driver/library version mismatch") {
		t.Errorf("broken driver: %v", err)
	}
	_, err = ExecSMI{Path: filepath.Join(t.TempDir(), "nvidia-smi.exe")}.Query(context.Background())
	if !errors.Is(err, ErrNoSMI) {
		t.Errorf("missing nvidia-smi: %v", err)
	}
}

func TestWinSMICandidates(t *testing.T) {
	env := map[string]string{"SystemRoot": `D:\WINDOWS\`, "ProgramFiles": `D:\Program Files`}
	got := winSMICandidates(func(k string) string { return env[k] })
	want := []string{`D:\WINDOWS\System32\nvidia-smi.exe`, `D:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v", got)
	}
	got = winSMICandidates(func(string) string { return "" })
	if got[0] != `C:\Windows\System32\nvidia-smi.exe` {
		t.Errorf("defaults: %v", got)
	}
}

// fakeSMI answers each query from a map keyed by the arguments' first element.
type fakeSMI map[string]struct {
	out string
	err error
}

func (f fakeSMI) Query(_ context.Context, args ...string) (string, error) {
	r, ok := f[args[0]]
	if !ok {
		return "", fmt.Errorf("unexpected query %v", args)
	}
	return r.out, r.err
}

// Recorded nvidia-smi output (gpuQuery, noheader, nounits).
const (
	smi3090Linux = "0, GPU-3090-aaaa, NVIDIA GeForce RTX 3090, 24576, 24126, 535.183.01, 8.6\n"
	// Windows (WDDM): CRLF line ends; the desktop holds memory on the card.
	smi4080Windows = "0, GPU-4080-bbbb, NVIDIA GeForce RTX 4080, 16376, 15210, 566.36, 8.9\r\n"
	smiTwoCards    = "0, GPU-3060-cccc, NVIDIA GeForce RTX 3060, 12288, 11800, 570.86.15, 8.6\n" +
		"1, GPU-5070ti-dddd, NVIDIA GeForce RTX 5070 Ti, 16303, 15839, 570.86.15, 12.0\n"
)

func TestParseGPUs(t *testing.T) {
	gpus, err := ParseGPUs(smi4080Windows)
	if err != nil {
		t.Fatal(err)
	}
	want := GPU{Index: 0, UUID: "GPU-4080-bbbb", Name: "NVIDIA GeForce RTX 4080", TotalMiB: 16376, FreeMiB: 15210, Driver: "566.36", Cap: "8.9"}
	if len(gpus) != 1 || gpus[0] != want {
		t.Errorf("got %+v, want %+v", gpus, want)
	}
	if gpus, err := ParseGPUs(smiTwoCards); err != nil || len(gpus) != 2 || gpus[1].Cap != "12.0" {
		t.Errorf("two cards: %+v, %v", gpus, err)
	}
	for _, bad := range []string{
		"0, GPU-x, NVIDIA GeForce RTX 4080, [N/A], [N/A], 566.36, 8.9\n",
		"0, GPU-x, NVIDIA GeForce RTX 4080, 16376\n",
		"NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n",
	} {
		if _, err := ParseGPUs(bad); err == nil {
			t.Errorf("ParseGPUs(%q): no error", bad)
		}
	}
}

func TestReadGPUs(t *testing.T) {
	ctx := context.Background()
	q := "--query-gpu=" + gpuQuery
	if _, err := ReadGPUs(ctx, fakeSMI{q: {err: ErrNoSMI}}); !errors.Is(err, ErrNoSMI) {
		t.Errorf("no nvidia-smi: %v", err)
	}
	// An old driver does not know compute_cap: the error names the driver.
	old := fakeSMI{q: {err: errors.New(`Field "compute_cap" is not a valid field to query.`)},
		"--query-gpu=driver_version": {out: "472.12\r\n"}}
	if _, err := ReadGPUs(ctx, old); err == nil || !strings.Contains(err.Error(), "NVIDIA driver 472.12, probably too old") {
		t.Errorf("old driver: %v", err)
	}
	broken := fakeSMI{q: {err: errors.New("Failed to initialize NVML")}, "--query-gpu=driver_version": {err: errors.New("x")}}
	if _, err := ReadGPUs(ctx, broken); err == nil || !strings.Contains(err.Error(), "driver is not working: Failed to initialize NVML") {
		t.Errorf("broken: %v", err)
	}
	if _, err := ReadGPUs(ctx, fakeSMI{q: {out: "\n"}}); err == nil || err.Error() != "nvidia-smi lists no GPU" {
		t.Errorf("no GPU: %v", err)
	}
}

func TestParseComputeApps(t *testing.T) {
	linux := "GPU-3090-aaaa, 4242, 18060\nGPU-3090-aaaa, 999, 300\nGPU-other, 4242, 12\n"
	got := ParseComputeApps(linux, 4242)
	if got["GPU-3090-aaaa"] != 18060 || got["GPU-other"] != 12 || len(got) != 2 {
		t.Errorf("linux: %v", got)
	}
	// WDDM: per-process memory is not reported.
	if got := ParseComputeApps("GPU-4080-bbbb, 4242, [N/A]\r\n", 4242); len(got) != 0 {
		t.Errorf("wddm: %v", got)
	}
}

func mustGPUs(t *testing.T, out string) []GPU {
	t.Helper()
	g, err := ParseGPUs(out)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestChooseCard(t *testing.T) {
	c, err := ChooseCard(mustGPUs(t, smi3090Linux), nil)
	if err != nil || c.Tier.Name != "big" || c.Several || CheckFit(c) != nil {
		t.Errorf("3090: %+v, %v, fit %v", c, err, CheckFit(c))
	}
	// The larger card wins, and several cards pin the engine to it.
	c, err = ChooseCard(mustGPUs(t, smiTwoCards), nil)
	if err != nil || c.UUID != "GPU-5070ti-dddd" || c.Tier.Name != "standard" || !c.Several {
		t.Errorf("two cards: %+v, %v", c, err)
	}
	// A tie goes to the card with more free memory, counting the node's own engine as free.
	tie := mustGPUs(t, "0, GPU-a, RTX 4090, 24564, 20000, 566.36, 8.9\n1, GPU-b, RTX 4090, 24564, 6000, 566.36, 8.9\n")
	if c, _ := ChooseCard(tie, nil); c.UUID != "GPU-a" {
		t.Errorf("tie: %s", c.UUID)
	}
	c, _ = ChooseCard(tie, map[string]int{"GPU-b": 18060})
	if c.UUID != "GPU-b" || c.Free() != 24060 || CheckFit(c) != nil {
		t.Errorf("tie with own engine: %s, free %d", c.UUID, c.Free())
	}
	// Too small.
	_, err = ChooseCard(mustGPUs(t, "0, GPU-x, NVIDIA GeForce RTX 3060 Ti, 8192, 7900, 566.36, 8.6\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "has 8192 MiB of memory; the network needs a card with 16 GB or more") {
		t.Errorf("8 GB: %v", err)
	}
	if _, err := ChooseCard(nil, nil); err == nil {
		t.Error("no GPU: no error")
	}
}

func TestCheckFit(t *testing.T) {
	c, err := ChooseCard(mustGPUs(t, smi4080Windows), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFit(c); err != nil {
		t.Errorf("15,210 MiB free on a 16 GB card: %v", err)
	}
	c.FreeMiB = 14299
	err = CheckFit(c)
	if err == nil || !strings.Contains(err.Error(), "has 14299 MiB free; the standard tier (UD-Q3_K_XL, 80k context) needs 14300 MiB free. On a 16 GB card") {
		t.Errorf("does not fit: %v", err)
	}
	c.FreeMiB = 14300
	if err := CheckFit(c); err != nil {
		t.Errorf("exactly the minimum: %v", err)
	}
	big, _ := ChooseCard(mustGPUs(t, "0, GPU-a, RTX 4090, 24564, 17000, 566.36, 8.9\n"), nil)
	if err := CheckFit(big); err == nil || !strings.Contains(err.Error(), "the big tier (UD-IQ4_XS, 192k context) needs 18500 MiB free. Close programs") {
		t.Errorf("big: %v", err)
	}
}
