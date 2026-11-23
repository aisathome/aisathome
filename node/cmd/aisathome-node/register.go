// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/accountlink"
	"github.com/aisathome/aisathome/node/nodeconf"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/setupflow"
)

const registerUsage = `usage: aisathome-node register -accept-terms [flags]

Starts this node without an account: the gateway gives it a node token of its own, and it
saves it with its settings (readable by the owner only) and starts. A node without an account:

  - does science work only, never people's chats;
  - first answers a few test prompts (the model check) and gets work once they pass;
  - earns no credit: credit starts once you link it to an account (aisathome-node link).

You must be 18 or older; running a node means you accept the terms:
https://aisathome.org/about/terms/ . -accept-terms says you read this and agree.

Installed on Linux, run it as root (sudo aisathome-node register -accept-terms): the token goes
to /etc/aisathome-node/token.env and the node service restarts. A node that has a token keeps it:
this refuses to replace it.

Flags:
`

// RegisterNotice is what the node tells a person before it registers without an account: the
// installers show the same three lines.
const RegisterNotice = `Without an account this node does science work only, never people's chats, and earns no credit
until you link it to an account (aisathome-node link).
You must be 18 or older; running it means you accept the terms: https://aisathome.org/about/terms/`

func runRegister(args []string, stdout, stderr io.Writer) int {
	env := linkEnv{layout: node.DefaultLayout(), exists: fileExists, smi: setup.ExecSMI{},
		client:  func(base string) *accountlink.Client { return &accountlink.Client{Base: base} },
		restart: restartNode}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return env.register(ctx, args, stdout, stderr)
}

func (e linkEnv) register(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("register", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() { fmt.Fprint(stderr, registerUsage); fl.PrintDefaults() }
	cfgPath := fl.String("config", "", "the node's config file (default: "+linkInstalledConfig+" when installed, else "+e.layout.Config+")")
	tokenEnv := fl.String("token-env", "", "save the token in this environment file instead of the config (default: token.env beside the config, when it exists)")
	gateway := fl.String("gateway", "", "the gateway's node address (default: the config's gateway_url, else "+setupflow.DefaultGatewayURL+")")
	accept := fl.Bool("accept-terms", false, "you are 18 or older and accept the terms (https://aisathome.org/about/terms/)")
	noStart := fl.Bool("no-start", false, "only save the token; do not start or restart the node")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintln(stderr, "register: unexpected arguments:", strings.Join(fl.Args(), " "))
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "register:", err)
		return 1
	}
	if !*accept {
		fmt.Fprintln(stderr, RegisterNotice)
		fmt.Fprintln(stderr, "register: add -accept-terms to go on")
		return 2
	}
	t := e.target(*cfgPath, *tokenEnv)
	if e.layout.Setup && !setupflow.Configured(t.config, e.layout.Model) {
		return fail(errors.New("this node is not set up yet: finish the setup on its local page, which has a \"Start without an account\" button"))
	}
	cfg, exists, err := nodeconf.ReadObject(t.config) // canonical: case variants refused
	if err != nil {
		return fail(err)
	}
	if !exists && t.tokenEnv != "" {
		return fail(fmt.Errorf("reading the config: %s does not exist", t.config))
	}
	if tok, _ := t.installedToken(cfg); tok != "" {
		what := "a node token"
		if node.Anonymous(tok) {
			what = "a token of its own (it already runs without an account)"
		}
		return fail(fmt.Errorf("this node has %s; it keeps it. To link it to an account instead: aisathome-node link", what))
	}
	gw := strings.TrimSpace(*gateway)
	if gw == "" {
		gw = nodeconf.String(cfg, "gateway_url")
	}
	if gw == "" {
		gw = setupflow.DefaultGatewayURL
	}
	base, err := accountlink.APIBase(gw)
	if err != nil {
		return fail(err)
	}
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
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := e.client(base).Register(rctx, true)
	if err != nil {
		return fail(err)
	}
	if err := t.save(r.Token, r.NodeID, *gateway != "", gw); err != nil {
		return fail(fmt.Errorf("the gateway registered node %s, but saving its token failed: %v. Run this again (the unused registration is deleted within a day)", r.NodeID, err))
	}
	where := t.config
	if t.tokenEnv != "" {
		where = t.tokenEnv
	}
	fmt.Fprintf(stdout, "\nThis is node %s, without an account (science only). Its token is saved in %s.\n", r.NodeID, where)
	fmt.Fprintln(stdout, "It answers a few test prompts first (the model check) and gets science work once they pass.")
	fmt.Fprintln(stdout, "Link it to an account later (credit starts then): aisathome-node link")
	if *noStart {
		return 0
	}
	if err := e.restart(t.tokenEnv != "", stdout); err != nil {
		fmt.Fprintln(stderr, "register: the token is saved, but the node did not start:", err)
		return 1
	}
	return 0
}
