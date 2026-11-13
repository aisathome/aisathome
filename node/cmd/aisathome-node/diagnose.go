// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/tray"
)

// diagnose implements `aisathome-node diagnose [-out file.zip]`: one zip with what is needed
// to find out why a node does not work, without the token or the local password: version and
// platform, the service, node.json (token redacted), the local page's status and the setup's
// last state, nvidia-smi and the driver, the agent's and the engine's logs, the files in the
// data and engine folders, and whether the gateway answers. On Windows it needs an
// administrator's PowerShell (the data folder is for administrators and the service only).
func runDiagnose(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fl.SetOutput(stderr)
	layout := node.DefaultLayout()
	out := fl.String("out", "", "the zip to write (default: aisathome-diagnose-<time>.zip on the Desktop, else in the home folder)")
	cfgPath := fl.String("config", layout.Config, "config file (JSON)")
	pause := fl.Bool("pause", false, "wait for Enter before exiting (the tray runs this in a window of its own)")
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		return 2
	}
	if *pause {
		defer func() {
			fmt.Fprint(stdout, "\nPress Enter to close this window.")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}()
	}
	if *out == "" {
		*out = defaultDiagnosePath(time.Now())
	}
	exeDir, _ := ownDir()
	d := diagnosis{layout: layout, cfgPath: *cfgPath, exeDir: exeDir, uiPort: localui.DefaultPort,
		run: runCommand, smi: setup.ExecSMI{}, now: time.Now}
	if runtime.GOOS == "windows" { // the tray's log of the user running this
		d.trayLog = tray.LogFile(os.Getenv)
	}
	if p, err := localui.PortFromEnv(localui.DefaultPort); err == nil && p != 0 {
		d.uiPort = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	problems, err := d.write(ctx, *out)
	if err != nil {
		fmt.Fprintln(stderr, "diagnose:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s\n", *out)
	fmt.Fprintln(stdout, "It holds no node token and no password; it does hold the GPU's serial, this PC's paths and the logs.")
	if problems > 0 {
		fmt.Fprintf(stdout, "%d item(s) could not be collected (see errors.txt in the zip)", problems)
		if runtime.GOOS == "windows" {
			fmt.Fprint(stdout, "; run it in PowerShell as administrator for the data folder")
		}
		fmt.Fprintln(stdout, ".")
	}
	return 0
}

func defaultDiagnosePath(now time.Time) string {
	name := "aisathome-diagnose-" + now.Format("20060102-150405") + ".zip"
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	if fi, err := os.Stat(filepath.Join(home, "Desktop")); err == nil && fi.IsDir() {
		return filepath.Join(home, "Desktop", name)
	}
	return filepath.Join(home, name)
}

// diagnosis collects the files; its parts are replaceable for tests.
type diagnosis struct {
	layout  node.Layout
	cfgPath string
	exeDir  string
	uiPort  int
	run     func(ctx context.Context, name string, args ...string) (string, error)
	smi     setup.SMI
	now     func() time.Time
	client  *http.Client // the gateway check; nil = a default with timeouts
	trayLog string       // the tray's log (Windows); "" = none

	secrets []string // exact strings to redact (the token, the password)
	errs    []string
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(b), err
}

// reToken is a token from the account page anywhere in text.
var reToken = regexp.MustCompile(`nt-(n-[0-9a-f]{8})\.[A-Za-z0-9._~+/=-]+`)

// reTokenField is a "token" value in a node.json that is not valid JSON.
var reTokenField = regexp.MustCompile(`"token"\s*:\s*"([^"]+)"`)

// redact removes the known secrets and anything shaped like a node token.
func (d *diagnosis) redact(s string) string {
	for _, sec := range d.secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	s = reToken.ReplaceAllString(s, "nt-$1.<redacted>")
	return s
}

func (d *diagnosis) fail(item string, err error) {
	d.errs = append(d.errs, fmt.Sprintf("%s: %v", item, err))
}

// write collects everything into the zip at out and returns how many items failed.
func (d *diagnosis) write(ctx context.Context, out string) (int, error) {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	z := zip.NewWriter(f)
	add := func(name, content string) {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: d.now()})
		if err == nil {
			_, err = io.WriteString(w, d.redact(content))
		}
		if err != nil {
			d.fail(name, err)
		}
	}

	cfg := d.config(add)
	add("version.txt", d.version(ctx))
	for _, c := range serviceCommands(d.layout, d.exeDir) {
		o, err := d.run(ctx, c.argv[0], c.argv[1:]...)
		if err != nil {
			o += fmt.Sprintf("\n(%v)\n", err)
		}
		add("service/"+c.name+".txt", strings.Join(c.argv, " ")+"\n\n"+o)
	}
	add("status.json", d.status(ctx))
	for name, path := range map[string]string{"setup-state.json": d.layout.SetupStateFile, "state.json": d.layout.StateFile} {
		if path == "" {
			continue
		}
		if b, err := os.ReadFile(path); err == nil {
			add(name, string(b))
		} else if !errors.Is(err, fs.ErrNotExist) {
			d.fail(name, err)
		}
	}
	add("nvidia-smi.txt", d.nvidiaSMI(ctx))
	for _, l := range []string{d.layout.LogFile, d.layout.EngineLogFile, d.trayLog} {
		for _, p := range []string{l, l + ".1"} {
			if l == "" {
				continue
			}
			b, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				d.fail(filepath.Base(p), err)
				continue
			}
			add("logs/"+filepath.Base(p), string(b))
		}
	}
	add("files.txt", d.files(cfg))
	add("gateway.txt", d.gateway(ctx, cfg.GatewayURL))
	add("errors.txt", strings.Join(d.errs, "\n")+"\n")
	if err := z.Close(); err != nil {
		f.Close()
		return 0, err
	}
	return len(d.errs), f.Close()
}

// config adds node.json without its token and returns it (for the gateway check). It also
// learns the secrets to redact everywhere: the token and the local password.
func (d *diagnosis) config(add func(name, content string)) node.Config {
	var cfg node.Config
	if pw, err := os.ReadFile(d.layout.PasswordFile); err == nil {
		d.secrets = append(d.secrets, strings.TrimSpace(string(pw)))
	} else if d.layout.PasswordFile != "" {
		d.fail("ui-password (only its presence is checked)", err)
	}
	b, err := os.ReadFile(d.cfgPath)
	if err != nil {
		d.fail("node.json", err)
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		// Not readable as a config, so its token is unknown: the file stays out of the zip, and
		// anything that looks like a token's value is redacted from the rest (logs).
		d.fail("node.json", fmt.Errorf("not valid JSON (%v); not included", err))
		for _, m := range reTokenField.FindAllStringSubmatch(string(b), -1) {
			d.secrets = append(d.secrets, m[1])
		}
		add("node.json", fmt.Sprintf("not valid JSON: %v (%d bytes; not included, it may hold the token)\n", err, len(b)))
		return node.Config{}
	}
	if tok := strings.TrimSpace(cfg.Token); tok != "" {
		d.secrets = append(d.secrets, tok)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if tok, ok := m["token"].(string); ok {
			id, _ := node.TokenNodeID(tok)
			m["token"] = fmt.Sprintf("<redacted: %d characters, node id %q>", len(tok), id)
		}
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if enc.Encode(m) == nil {
			b = []byte(buf.String())
		}
	}
	add("node.json", string(b))
	return cfg
}

func (d *diagnosis) version(ctx context.Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent: %s\nos: %s/%s\ntime: %s\n", node.AgentVersion(), runtime.GOOS, runtime.GOARCH, d.now().UTC().Format(time.RFC3339))
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(&b, "program: %s\n", exe)
	}
	fmt.Fprintf(&b, "config: %s\nengine folder: %s\nllama.cpp: %s (%s)\n", d.cfgPath,
		filepath.Join(d.exeDir, filepath.Dir(d.layout.EngineProgram)), setup.WindowsLlama.Tag, setup.WindowsLlama.Commit)
	if o, err := d.run(ctx, osVersionCommand[0], osVersionCommand[1:]...); err == nil {
		fmt.Fprintf(&b, "system: %s\n", strings.TrimSpace(o))
	}
	return b.String()
}

// status is the running agent's /api/status, as the local page sees it.
func (d *diagnosis) status(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+localui.Addr(d.uiPort)+"/api/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.fail("status", err)
		return fmt.Sprintf(`{"error": %q}`, "no agent answered on "+localui.Addr(d.uiPort)+": "+err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b)
}

func (d *diagnosis) nvidiaSMI(ctx context.Context) string {
	var b strings.Builder
	for _, args := range [][]string{
		nil,
		{"--query-gpu=index,uuid,name,memory.total,memory.free,memory.used,driver_version,compute_cap,pstate,clocks.sm,clocks.max.sm,power.draw,power.limit,temperature.gpu", "--format=csv"},
		{"--query-compute-apps=gpu_uuid,pid,process_name,used_memory", "--format=csv"},
		{"-q"},
	} {
		fmt.Fprintf(&b, "== nvidia-smi %s\n", strings.Join(args, " "))
		o, err := d.smi.Query(ctx, args...)
		if err != nil {
			fmt.Fprintf(&b, "(%v)\n", err)
			if errors.Is(err, setup.ErrNoSMI) {
				d.fail("nvidia-smi", err)
				break
			}
		}
		b.WriteString(o)
		b.WriteString("\n")
	}
	return b.String()
}

// files lists the data folder (names and sizes, the model's against the tiers) and the engine
// folder beside the agent, and the free disk space.
func (d *diagnosis) files(cfg node.Config) string {
	var b strings.Builder
	list := func(dir string) {
		fmt.Fprintf(&b, "== %s\n", dir)
		var lines []string
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				lines = append(lines, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
			if e.IsDir() {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			lines = append(lines, fmt.Sprintf("%14d  %s  %s", fi.Size(), fi.ModTime().UTC().Format(time.RFC3339), rel))
			return nil
		})
		if err != nil {
			lines = append(lines, err.Error())
		}
		sort.Strings(lines)
		b.WriteString(strings.Join(lines, "\n") + "\n\n")
	}
	if dir := filepath.Dir(d.cfgPath); dir != "" {
		list(dir)
	}
	if d.exeDir != "" && d.layout.EngineProgram != "" {
		list(filepath.Join(d.exeDir, filepath.Dir(d.layout.EngineProgram)))
	}
	model := d.layout.Model
	if cfg.Engine != nil && cfg.Engine.Model != "" {
		model = cfg.Engine.Model
	}
	if model != "" {
		fmt.Fprintf(&b, "== model %s\n", model)
		if fi, err := os.Stat(model); err != nil {
			fmt.Fprintf(&b, "%v\n", err)
		} else {
			fmt.Fprintf(&b, "%d bytes\n", fi.Size())
			for _, t := range setup.Tiers {
				if t.Model.Size == fi.Size() {
					fmt.Fprintf(&b, "the size of the %s tier's file (%s); its SHA-256 is checked by the setup's download step\n", t.Name, t.Model.File())
				}
			}
		}
	}
	return b.String()
}

// gateway checks that the configured gateway answers: DNS, TCP, TLS and its /healthz.
func (d *diagnosis) gateway(ctx context.Context, raw string) string {
	if raw == "" {
		raw = "wss://api.aisathome.org/v1/node/connect"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gateway: %s\n", raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		fmt.Fprintf(&b, "cannot parse it: %v\n", err)
		return b.String()
	}
	scheme, port := "https", "443"
	if u.Scheme == "ws" {
		scheme, port = "http", "80"
	}
	if u.Port() != "" {
		port = u.Port()
	}
	host := u.Hostname()
	t0 := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	fmt.Fprintf(&b, "DNS %s: %v %v (%v)\n", host, addrs, errText(err), time.Since(t0).Round(time.Millisecond))
	t0 = time.Now()
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	fmt.Fprintf(&b, "TCP %s: %s (%v)\n", net.JoinHostPort(host, port), okOr(err), time.Since(t0).Round(time.Millisecond))
	if err == nil {
		if scheme == "https" {
			tc := tls.Client(c, &tls.Config{ServerName: host})
			tc.SetDeadline(time.Now().Add(10 * time.Second))
			err = tc.HandshakeContext(ctx)
			if err == nil {
				st := tc.ConnectionState()
				fmt.Fprintf(&b, "TLS: ok, %s", tls.VersionName(st.Version))
				if len(st.PeerCertificates) > 0 {
					fmt.Fprintf(&b, ", certificate for %v until %s", st.PeerCertificates[0].DNSNames, st.PeerCertificates[0].NotAfter.Format(time.DateOnly))
				}
				b.WriteString("\n")
			} else {
				fmt.Fprintf(&b, "TLS: %v\n", err)
			}
		}
		c.Close()
	}
	client := d.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	hu := scheme + "://" + u.Host + "/healthz"
	t0 = time.Now()
	resp, err := client.Get(hu)
	if err != nil {
		fmt.Fprintf(&b, "GET %s: %v\n", hu, err)
		return b.String()
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	resp.Body.Close()
	fmt.Fprintf(&b, "GET %s: %d %q (%v)\n", hu, resp.StatusCode, strings.TrimSpace(string(body)), time.Since(t0).Round(time.Millisecond))
	return b.String()
}

func okOr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// diagCommand is a command whose output goes into the zip.
type diagCommand struct {
	name string
	argv []string
}
