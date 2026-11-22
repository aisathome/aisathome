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
	stop    func() (wasOn bool, err error)           // stops the node before link -gateway saves; nil: nothing to stop
}

func runLink(args []string, stdout, stderr io.Writer) int {
	env := linkEnv{layout: node.DefaultLayout(), exists: fileExists, smi: setup.ExecSMI{},
		client:  func(base string) *accountlink.Client { return &accountlink.Client{Base: base} },
		restart: restartNode, stop: stopNode}
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
	config    string
	tokenEnv  string               // "" = the token goes into config
	stopNode  func() (bool, error) // see linkEnv.stop
	startNode func() error         // starts the node again after a failed save stopped it
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
		gw = configGateway(t.config) // the key as the agent reads it (any case)
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
	t := linkTarget{config: cfgFlag, tokenEnv: envFlag, stopNode: e.stop}
	if e.restart != nil {
		t.startNode = func() error { return e.restart(true, io.Discard) }
	}
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
// Nothing is written unless the token has the exact form the gateway issues and carries nodeID.
func (t linkTarget) save(token, nodeID string, setGateway bool, gw string) error {
	if id, ok := node.IssuedTokenNodeID(token); !ok || id != nodeID {
		return errors.New("the gateway sent a token that is not a node token")
	}
	// A lock on the config's folder: two links at once leave one link's config and token.
	unlock, err := lockDir(filepath.Dir(t.config))
	if err != nil {
		return err
	}
	defer unlock()
	if cg := configGateway(t.config); !setGateway && cg != "" && cg != gw {
		return fmt.Errorf("the config's gateway changed to %s while linking to %s", cg, gw)
	}
	set := map[string]any{"node_id": nodeID}
	if setGateway {
		set["gateway_url"] = gw
	}
	if t.tokenEnv != "" {
		if setGateway && configGateway(t.config) != gw {
			return t.saveOtherGateway(token, set)
		}
		if err := writeTokenEnv(t.tokenEnv, token); err != nil {
			return err
		}
		return updateJSON(t.config, set, nil, 0)
	}
	set["token"] = token
	return updateJSON(t.config, set, nil, 0o600)
}

// saveOtherGateway saves a token for another gateway than the config names (link -gateway) so that at
// no moment a token sits beside the other gateway's address, or reaches it: the old token.env is moved
// aside (token.env.unlinked; the unit's ConditionFileNotEmpty keeps the node off), the node is stopped
// (a start already under way may have read the old token.env), then the config gets the new gateway,
// then token.env the new token. If the node does not stop, nothing is changed. If a write fails, the
// old config comes back, then the old token beside it, and a node that was on starts again.
func (t linkTarget) saveOtherGateway(token string, set map[string]any) error {
	oldCfg, err := os.ReadFile(t.config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hadCfg := err == nil
	parked := t.tokenEnv + ".unlinked"
	moved := true
	if err := os.Rename(t.tokenEnv, parked); errors.Is(err, os.ErrNotExist) {
		moved = false
	} else if err != nil {
		return err
	}
	wasOn := false
	if t.stopNode != nil {
		on, err := t.stopNode()
		if err != nil {
			if moved {
				os.Rename(parked, t.tokenEnv)
			}
			return fmt.Errorf("the node did not stop, so nothing was changed: %v", err)
		}
		wasOn = on
	}
	rollback := func(cause error, cfgReplaced bool) error {
		if cfgReplaced {
			var err error
			if hadCfg {
				err = writeReplacing(t.config, oldCfg, 0)
			} else {
				err = os.Remove(t.config)
			}
			if err != nil {
				return fmt.Errorf("%v; putting the old config back failed too (%v), so the old token stays in %s", cause, err, parked)
			}
		}
		if moved {
			if err := os.Rename(parked, t.tokenEnv); err != nil {
				return fmt.Errorf("%v; putting the old token back failed too: %v", cause, err)
			}
		}
		if wasOn && t.startNode != nil {
			if err := t.startNode(); err != nil {
				return fmt.Errorf("%v; the old config and token are back, but the node did not start again: %v", cause, err)
			}
		}
		return cause
	}
	if err := updateJSON(t.config, set, nil, 0); err != nil {
		return rollback(err, false)
	}
	if err := saveTokenEnv(t.tokenEnv, token); err != nil {
		return rollback(err, true)
	}
	return nil
}

// configGateway is the gateway_url in a config file ("" if none or unreadable), its key matched as the
// agent matches it (encoding/json: any case, the last one wins).
func configGateway(path string) string {
	var c struct {
		GatewayURL string `json:"gateway_url"`
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &c)
	}
	return c.GatewayURL
}

// writeTokenEnv replaces a systemd EnvironmentFile with the one line AISATHOME_NODE_TOKEN=<token>,
// atomically and with mode 0600. It checks the token again right at the write: only the form the
// gateway issues (no newline, space, quote, "=" or "#") can go into the file, so a token cannot
// add environment settings for the node's service.
func writeTokenEnv(path, token string) error {
	if _, ok := node.IssuedTokenNodeID(token); !ok {
		return errors.New("not writing token.env: the token is not a node token")
	}
	return writeReplacing(path, []byte("AISATHOME_NODE_TOKEN="+token+"\n"), 0o600)
}

// saveTokenEnv is writeTokenEnv; tests replace it to make the write after the config fail.
var saveTokenEnv = writeTokenEnv

// updateJSON sets keys in a JSON object file and deletes drop, keeping everything else. A key set
// replaces the same key in another case too (the agent reads keys in any case). mode 0 keeps the file's
// mode; otherwise the file gets mode (a config holding a token: 0600).
func updateJSON(path string, set map[string]any, drop []string, mode os.FileMode) error {
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s is not valid JSON: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for k := range m {
		for s := range set {
			if k != s && strings.EqualFold(k, s) {
				delete(m, k)
			}
		}
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
	if err := f.Sync(); err != nil { // the new content is on disk before it replaces the old file
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
	case tokenEnv && fileExists(nodeUnit):
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

// nodeUnit is the Linux installer's systemd unit for the node.
const nodeUnit = "/etc/systemd/system/" + node.ServiceName + ".service"

// stopNode stops the installed node's systemd service before link -gateway saves, and waits until it is
// off: a start already under way may have read the old token.env. It reports whether the node was on
// (or starting), so that a save that fails can start it again. Without the unit there is nothing to stop.
func stopNode() (bool, error) {
	if runtime.GOOS != "linux" || !fileExists(nodeUnit) {
		return false, nil
	}
	st := nodeState()
	on := st == "active" || st == "activating" || st == "reloading"
	if b, err := exec.Command("systemctl", "stop", node.ServiceName).CombinedOutput(); err != nil {
		return on, fmt.Errorf("systemctl stop %s: %v %s", node.ServiceName, err, strings.TrimSpace(string(b)))
	}
	if st := nodeState(); st != "inactive" && st != "failed" {
		return on, fmt.Errorf("the node service is %q after systemctl stop", st)
	}
	return on, nil
}

// nodeState is the node service's ActiveState ("" if systemctl cannot tell).
func nodeState() string {
	b, _ := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", node.ServiceName).Output()
	return strings.TrimSpace(string(b))
}
