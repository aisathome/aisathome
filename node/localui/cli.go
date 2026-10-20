// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
)

// Exit codes of `aisathome-node status`.
const (
	ExitConnected    = 0 // connected to the gateway (serving, idle or paused)
	ExitNotConnected = 1 // the agent runs but is not connected
	ExitUsage        = 2
	ExitUnreachable  = 3 // no agent answered on the local UI port
)

// PortFromEnv returns AISATHOME_UI_PORT, or def when unset. "off" or "0" returns 0.
func PortFromEnv(def int) (int, error) {
	v := strings.TrimSpace(os.Getenv("AISATHOME_UI_PORT"))
	switch v {
	case "":
		return def, nil
	case "off", "0":
		return 0, nil
	}
	p, err := strconv.Atoi(v)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("AISATHOME_UI_PORT must be a port number or \"off\", not %q", v)
	}
	return p, nil
}

// RunStatus implements `aisathome-node status`: it asks the running agent over its local UI
// and prints the status. It returns the exit code.
func RunStatus(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("status", flag.ContinueOnError)
	fl.SetOutput(stderr)
	envPort, err := PortFromEnv(DefaultPort)
	if err != nil || envPort == 0 {
		envPort = DefaultPort
	}
	port := fl.Int("port", envPort, "local UI port of the running agent")
	asJSON := fl.Bool("json", false, "print the raw status as JSON")
	if err := fl.Parse(args); err != nil {
		return ExitUsage
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + Addr(*port) + "/api/status")
	if err != nil {
		fmt.Fprintf(stderr, "no node agent answers on %s: is it running? (systemctl status aisathome-node)\n", Addr(*port))
		return ExitUnreachable
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var st StatusResponse
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &st) != nil || st.State == "" {
		fmt.Fprintf(stderr, "unexpected answer from %s (HTTP %d): not an AIs@home node agent?\n", Addr(*port), resp.StatusCode)
		return ExitUnreachable
	}
	if *asJSON {
		stdout.Write(b)
	} else {
		PrintStatus(stdout, st, *port, time.Now())
	}
	if !st.Connected {
		return ExitNotConnected
	}
	return ExitConnected
}

// PrintStatus writes a human-readable status.
func PrintStatus(w io.Writer, st StatusResponse, port int, now time.Time) {
	row := func(k, f string, a ...any) { fmt.Fprintf(w, "  %-10s %s\n", k, fmt.Sprintf(f, a...)) }
	fmt.Fprintf(w, "AIs@home node %s: %s\n", st.NodeID, strings.ToUpper(st.State))
	conn := "not connected"
	if st.Connected {
		conn = "connected"
		if st.ConnectedSince != nil {
			conn += " for " + ago(now.Sub(*st.ConnectedSince))
		}
	}
	if st.Paused {
		conn += "; paused (takes no new requests)"
	}
	row("gateway", "%s, %s", orDash(st.Gateway), conn)
	row("engine", "%s", st.Engine)
	for _, m := range st.Models {
		row("model", "%s, %d slot(s) x %d tokens", m.Name, m.Slots, m.Ctx)
	}
	if st.Preset != "" {
		row("preset", "%s", st.Preset)
	}
	row("slots", "%d of %d in use", st.SlotsInUse, st.SlotsTotal)
	for _, g := range st.GPU.GPUs {
		row(fmt.Sprintf("GPU %d", g.Index), "%s", gpuLine(g))
	}
	if st.GPU.Note != "" {
		row("GPU", "%s", st.GPU.Note)
	}
	row("today", "%s", counters(st.Today))
	row("total", "%s", counters(st.Total))
	for i, e := range st.Events {
		if i == 3 {
			break
		}
		k := "events"
		if i > 0 {
			k = ""
		}
		row(k, "%s", eventLine(e))
	}
	row("UI", "http://%s/  (your nodes: %s)", Addr(port), st.AccountURL)
}

func gpuLine(g GPU) string {
	parts := []string{g.Name}
	if g.MemUsedMiB != nil && g.MemTotalMiB != nil {
		parts = append(parts, fmt.Sprintf("%.1f of %.1f GiB", *g.MemUsedMiB/1024, *g.MemTotalMiB/1024))
	}
	if g.TempC != nil {
		parts = append(parts, fmt.Sprintf("%.0f °C", *g.TempC))
	}
	if g.UtilPct != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% busy", *g.UtilPct))
	}
	if g.PowerW != nil {
		parts = append(parts, fmt.Sprintf("%.0f W", *g.PowerW))
	}
	return strings.Join(parts, ", ")
}

func counters(c node.Counters) string {
	s := fmt.Sprintf("%d requests, %d tokens in, %d out", c.Requests, c.PromptTokens, c.CompletionTokens)
	if c.BusyMs > 0 {
		s += ", " + ago(time.Duration(c.BusyMs)*time.Millisecond) + " serving"
	}
	if c.Failed > 0 {
		s += fmt.Sprintf(", %d failed", c.Failed)
	}
	return s
}

func eventLine(e node.Event) string {
	s := e.Time.Local().Format("Jan 02 15:04:05") + "  " + strings.ReplaceAll(e.Kind, "_", " ")
	if e.Text != "" {
		s += ": " + e.Text
	}
	if e.Count > 1 {
		s += fmt.Sprintf(" (x%d)", e.Count)
	}
	return s
}

// ago is a short duration as the local UI writes it (app.js, dur): "<1s" for a moment that
// is not zero, so a node that served briefly does not show "0s".
func ago(d time.Duration) string {
	switch {
	case d > 0 && d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
