// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/setupflow"
	"github.com/aisathome/aisathome/protocol"
)

const linkUsage = `usage: aisathome-node link [flags]

Links this node to your AIs@home account, without pasting a token. It shows a link and a
code: open the link in your browser, sign in, check that the page shows the
same code, and approve. The node then gets its own node token, saves it with its settings
(readable by the owner only) and starts.

Installed on Linux, run it as root (sudo aisathome-node link): the token goes to
/etc/aisathome-node/token.env and the node service restarts. On Windows, run it as
administrator once the setup is done (the setup page has a "Link to my account" button).

Flags:
`

// linkInstalledConfig is the Linux installer's node.json; its token lives beside it in token.env.
const linkInstalledConfig = "/etc/aisathome-node/node.json"

// linkEnv is what `aisathome-node link` works with; tests replace the parts that touch the system.
type linkEnv struct {
	layout  node.Layout
	exists  func(path string) bool
	smi     setup.SMI
	client  func(base string) *accountlink.Client
	restart func(tokenEnv bool, out io.Writer) error // starts or restarts the node after saving
}

func runLink(args []string, stdout, stderr io.Writer) int {
	env := linkEnv{layout: node.DefaultLayout(), exists: fileExists, smi: setup.ExecSMI{},
		client:  func(base string) *accountlink.Client { return &accountlink.Client{Base: base} },
		restart: restartNode}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return env.main(ctx, args, stdout, stderr)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// linkTarget is where the token goes: token.env (the Linux install; node.json gets the node id), or
// node.json itself.
type linkTarget struct {
	config   string
	tokenEnv string // "" = the token goes into config
}

func (e linkEnv) main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("link", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() { fmt.Fprint(stderr, linkUsage); fl.PrintDefaults() }
	cfgPath := fl.String("config", "", "the node's config file (default: "+linkInstalledConfig+" when installed, else "+e.layout.Config+")")
	tokenEnv := fl.String("token-env", "", "save the token in this environment file instead of the config (default: token.env beside the config, when it exists)")
	gateway := fl.String("gateway", "", "the gateway's node address (default: the config's gateway_url, else "+setupflow.DefaultGatewayURL+")")
	name := fl.String("name", "", "the node's name on your account page (default: this computer's name; you can change it when you approve)")
	noStart := fl.Bool("no-start", false, "only save the token; do not start or restart the node")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintln(stderr, "link: unexpected arguments:", strings.Join(fl.Args(), " "))
		return 2
	}
	t := e.target(*cfgPath, *tokenEnv)
	fail := func(err error) int {
		fmt.Fprintln(stderr, "link:", err)
		return 1
	}
	if e.layout.Setup && !setupflow.Configured(t.config, e.layout.Model) {
		return fail(errors.New("this node is not set up yet: finish the setup on its local page, which has a \"Link to my account\" button"))
	}
	gw := strings.TrimSpace(*gateway)
	cfg := map[string]json.RawMessage{}
	if b, err := os.ReadFile(t.config); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fail(fmt.Errorf("%s is not valid JSON: %v", t.config, err))
		}
	} else if !errors.Is(err, os.ErrNotExist) || t.tokenEnv != "" {
		return fail(fmt.Errorf("reading the config: %w", err))
	}
	if gw == "" {
		json.Unmarshal(cfg["gateway_url"], &gw)
	}
	if gw == "" {
		gw = setupflow.DefaultGatewayURL
	}
	base, err := accountlink.APIBase(gw)
	if err != nil {
		return fail(err)
	}
	// Saving must work before a node is created on the account: a token that cannot be saved would be lost.
	for _, p := range []string{t.config, t.tokenEnv} {
		if p == "" {
			continue
		}
		if err := checkWritable(p); err != nil {
			hint := ""
			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				hint = " (run it with sudo)"
			}
			return fail(fmt.Errorf("cannot save the token: %v%s", err, hint))
		}
	}

	info := accountlink.LocalInfo(ctx, e.smi)
	if *name != "" {
		info.Name = *name
	}
	c := e.client(base)
	st, err := c.Start(ctx, info)
	if err != nil {
		return fail(err)
	}
	printLinkSteps(stdout, st)
	p, err := c.Wait(ctx, st)
	if err != nil {
		if ctx.Err() != nil {
			return fail(errors.New("stopped; nothing was changed. Run it again to link the node"))
		}
		return fail(err)
	}
	if err := t.save(p.Token, p.NodeID, *gateway != "", gw); err != nil {
		return fail(fmt.Errorf("the link was approved, but saving the token failed: %v. Revoke node %s on your account page and run this again", err, p.NodeID))
	}
	where := t.config
	if t.tokenEnv != "" {
		where = t.tokenEnv
	}
	fmt.Fprintf(stdout, "\nLinked: this is node %s on your account. Its token is saved in %s.\n", p.NodeID, where)
	if *noStart {
		return 0
	}
	if err := e.restart(t.tokenEnv != "", stdout); err != nil {
		fmt.Fprintln(stderr, "link: the token is saved, but the node did not start:", err)
		return 1
	}
	return 0
}

// target works out where the token goes from the flags and what is installed.
func (e linkEnv) target(cfgFlag, envFlag string) linkTarget {
	t := linkTarget{config: cfgFlag, tokenEnv: envFlag}
	if t.config == "" {
		t.config = e.layout.Config
		if runtime.GOOS == "linux" && e.exists(linkInstalledConfig) {
			t.config = linkInstalledConfig
		}
	}
	if t.tokenEnv == "" {
		if p := filepath.Join(filepath.Dir(t.config), "token.env"); e.exists(p) || t.config == linkInstalledConfig {
			t.tokenEnv = p
		}
	}
	return t
}

// printLinkSteps tells the person what to do.
func printLinkSteps(w io.Writer, st protocol.LinkStart) {
	mins := (st.ExpiresIn + 59) / 60
	fmt.Fprintf(w, `
Link this node to your AIs@home account:

  1. Open this link in your browser:

       %s

     or go to %s and type the code.
  2. Sign in and check that the page shows the code  %s
  3. Approve.

`, st.VerificationURIComplete, st.VerificationURI, st.UserCode)
	fmt.Fprintf(w, "Waiting for the approval (the code works for %d minutes; Ctrl+C stops)...\n", mins)
}

// checkWritable reports whether path can be replaced: its folder takes a new file.
func checkWritable(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".link-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// save writes the token: into token.env (and the node id into the config), or into the config. The
// gateway's address goes into the config too when it was given on the command line.
func (t linkTarget) save(token, nodeID string, setGateway bool, gw string) error {
	set := map[string]any{"node_id": nodeID}
	if setGateway {
		set["gateway_url"] = gw
	}
	if t.tokenEnv != "" {
		if err := writeReplacing(t.tokenEnv, []byte("AISATHOME_NODE_TOKEN="+token+"\n"), 0o600); err != nil {
			return err
		}
		return updateJSON(t.config, set, nil, 0)
	}
	set["token"] = token
	return updateJSON(t.config, set, nil, 0o600)
}

// updateJSON sets keys in a JSON object file and deletes drop, keeping everything else. mode 0 keeps the
// file's mode; otherwise the file gets mode (a config holding a token: 0600).
func updateJSON(path string, set map[string]any, drop []string, mode os.FileMode) error {
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s is not valid JSON: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for k, v := range set {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		m[k] = b
	}
	for _, k := range drop {
		delete(m, k)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeReplacing(path, append(b, '\n'), mode)
}

// writeReplacing replaces path through a new file in the same folder, renamed over it. mode 0 keeps the
// old file's mode (0600 for a new one). The new file is made with the final mode, and it keeps the old
// file's owner and group (installed files: root and the node's group).
func writeReplacing(path string, b []byte, mode os.FileMode) error {
	old, statErr := os.Stat(path)
	if mode == 0 {
		mode = 0o600
		if statErr == nil {
			mode = old.Mode().Perm()
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after a successful rename there is nothing left to remove
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if statErr == nil {
		if err := keepOwner(tmp, old); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}

// restartNode starts the installed node with its new token: the systemd service on Linux (the agent
// starts the engine through its unit), the Windows service on Windows. Elsewhere it says how.
func restartNode(tokenEnv bool, out io.Writer) error {
	switch {
	case runtime.GOOS == "windows":
		return serviceRestart(out)
	case tokenEnv && fileExists("/etc/systemd/system/"+node.ServiceName+".service"):
		fmt.Fprintln(out, "Starting the node: systemctl restart", node.ServiceName)
		cmd := exec.Command("systemctl", "restart", node.ServiceName)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		fmt.Fprintln(out, "Check it with: aisathome-node status")
		return nil
	}
	fmt.Fprintln(out, "Start the node with its config as usual (aisathome-node -config <file>).")
	return nil
}
