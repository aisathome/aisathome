// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/nodeconf"
	"github.com/aisathome/aisathome/node/setup"
	"github.com/aisathome/aisathome/node/setupflow"
	"github.com/aisathome/aisathome/protocol"
)

// Features is what `aisathome-node -features` prints, one per line: what an installer may ask of this
// binary. An older binary refuses the flag (exit 2) and runs nothing, so the installer can ask safely.
var Features = []string{"config-preflight"}

const preflightUsage = `usage: aisathome-node config preflight [flags]

For the Linux installer (install.sh), which runs it under the writers' lock (node.lock) before it
installs anything: it reads the installed node.json and token.env, decides with the node's own
rules what the install does with the node's config and token, and writes the files to install.

  - node.json: the installed one and a supplied one (-node-config) must be canonical (no two keys
    equal in any case); the written one has lower-case keys and no token.
  - The gateway: -gateway-url, else the installed node.json's, else the network's; wss://, or ws://
    only to this machine.
  - The token: one is kept or sent only to the gateway its record (AISATHOME_TOKEN_GATEWAY) names;
    any other installed token is moved aside by the installer ("park") and never written again.
  - The engine address (upstream) of a generated node.json: -upstream, else the installed one's,
    else http://127.0.0.1:8000.
  - The account (-account): link, anonymous, keep, token (read from stdin), or "": at a terminal
    an installed token for this gateway is kept (a node without an account's registers again when
    its gateway deleted or holds it), else it asks "Link this node to your account? [Y/n]" (y:
    link, n: without an account); without a terminal a token is read from stdin.

It writes node.json (and token.env when the node gets a token now) into -out, prints what it
decided as JSON on stdout (-json) and tells the person on stderr. -dry-run reads no token,
asks nothing and sends nothing. Exit 2: refused input; 1: another failure.

Flags:
`

// preflightResult is what the installer acts on (-json).
type preflightResult struct {
	GatewayURL string `json:"gateway_url"`
	NodeID     string `json:"node_id"`
	// Account is how the install ends: "keep" (the installed token stays), "token" (a token given now),
	// "link" (aisathome-node link at the end), "anonymous" (aisathome-node register at the end), or ""
	// (a dry run that does not know yet).
	Account string `json:"account"`
	// Park: the installed token is not this gateway's (or has no record): the installer moves it aside and
	// stops the node before it installs the new node.json.
	Park bool `json:"park"`
	// TokenEnv: -out/token.env was written (the token with its gateway).
	TokenEnv bool `json:"token_env"`
}

// preflightEnv is what the preflight works with; tests replace the parts that touch the system.
type preflightEnv struct {
	stdin       io.Reader
	interactive bool                                                     // stdin is a terminal: a person answers
	hidden      func(body func(read func() (string, error)) error) error // runs body with the terminal not echoing
	hostname    func() (string, error)                                   // os.Hostname
	httpClient  func() *http.Client                                      // asks a token's gateway about it
	tokenState  func(token, gw string) string                            // default: preflightEnv.askTokenState
}

// defaultUpstream is the engine the installer runs (llama-server on this machine's port 8000).
const defaultUpstream = "http://127.0.0.1:8000"

var reNodeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// usageError is refused input: exit 2.
type usageError struct{ error }

func usagef(f string, a ...any) error { return usageError{fmt.Errorf(f, a...)} }

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "preflight" {
		fmt.Fprintln(stderr, "usage: aisathome-node config preflight [flags] (see -h)")
		return 2
	}
	env := preflightEnv{stdin: os.Stdin, interactive: stdinIsTerminal(), hidden: withoutEcho, hostname: os.Hostname,
		httpClient: func() *http.Client { return node.TokenClient(20 * time.Second) }}
	return env.main(args[1:], stdout, stderr)
}

func (e preflightEnv) main(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("config preflight", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() { fmt.Fprint(stderr, preflightUsage); fl.PrintDefaults() }
	var o preflightOpts
	fl.StringVar(&o.confDir, "conf-dir", "/etc/aisathome-node", "the installed node's folder (node.json, token.env)")
	fl.StringVar(&o.nodeConfig, "node-config", "", "a node.json to install (made canonical, token dropped) instead of a generated one")
	fl.StringVar(&o.gatewayURL, "gateway-url", "", "the gateway for a generated node.json (default: the installed one's, else "+setupflow.DefaultGatewayURL+")")
	fl.StringVar(&o.nodeID, "node-id", "", "the node id for a generated node.json (default: the token's, else the installed one's, else the host name)")
	fl.StringVar(&o.account, "account", "", "link, anonymous, keep, token (from stdin), or empty: ask at a terminal (link? [Y/n]), else a token from stdin")
	fl.StringVar(&o.model, "model", "", "a generated node.json's model name")
	fl.IntVar(&o.slots, "slots", 1, "a generated node.json's slots")
	fl.IntVar(&o.ctx, "ctx", 0, "a generated node.json's context per slot")
	fl.StringVar(&o.upstream, "upstream", "", "a generated node.json's engine address (default: the installed one's, else "+defaultUpstream+")")
	fl.StringVar(&o.out, "out", "", "the folder to write node.json (and token.env) into")
	fl.BoolVar(&o.dryRun, "dry-run", false, "read no token, ask nothing, send nothing; write node.json only")
	asJSON := fl.Bool("json", false, "print the decision as JSON on stdout")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintln(stderr, "config preflight: unexpected arguments:", strings.Join(fl.Args(), " "))
		return 2
	}
	r, err := e.preflight(o, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	if *asJSON {
		b, _ := json.Marshal(r)
		fmt.Fprintln(stdout, string(b))
	}
	return 0
}

type preflightOpts struct {
	confDir, nodeConfig, gatewayURL, nodeID, account string
	model, upstream, out                             string
	slots, ctx                                       int
	dryRun                                           bool
}

// gatewayOK is the rule for a gateway a node token may go to: a plain address (it goes into a systemd
// EnvironmentFile as the token's record), wss://, or ws:// only to this machine.
func gatewayOK(gw string) bool {
	return strings.HasPrefix(gw, "ws") && nodeconf.ValidGateway(gw) && node.CheckGatewayURL(gw) == nil
}

func (e preflightEnv) preflight(o preflightOpts, msg io.Writer) (preflightResult, error) {
	var r preflightResult
	say := func(f string, a ...any) { fmt.Fprintf(msg, f+"\n", a...) }
	switch o.account {
	case "", "link", "anonymous", "keep", "token":
	default:
		return r, usagef("-account must be link, anonymous, keep or token")
	}
	if o.out == "" {
		return r, usagef("-out: the folder for the files to install is missing")
	}

	// The installed node.json, canonical (as the agent and aisathome-node link read it). Without root a
	// dry run may not read it: then it is as if there were none, and the person is told.
	installedPath := filepath.Join(o.confDir, "node.json")
	installed, _, err := nodeconf.ReadObject(installedPath)
	if errors.Is(err, os.ErrPermission) && o.dryRun {
		installed = map[string]json.RawMessage{}
		if o.nodeConfig == "" {
			say("node.json: %s is not readable without sudo; the install keeps its node id, gateway URL and engine address", installedPath)
		}
	} else if err != nil {
		return r, usagef("node config %v", err)
	}
	oldID, oldGW := nodeconf.String(installed, "node_id"), nodeconf.String(installed, "gateway_url")
	// The engine address of a generated node.json: -upstream, else the installed one's (an update keeps an
	// address the node was given, by --node-config or by hand), else the installer's own engine.
	if o.upstream == "" {
		o.upstream = defaultUpstream
		if up := nodeconf.String(installed, "upstream"); up != "" && up != defaultUpstream {
			if u, err := url.Parse(up); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				o.upstream = up
				say("node.json: keeps the installed engine address (upstream)")
			} else {
				say("node.json: the installed upstream is not an http(s) address; the install uses %s", defaultUpstream)
			}
		}
	}

	// The node.json to install: the supplied one, canonical; or one generated later, once the node id is known.
	var supplied map[string]any
	if o.nodeConfig != "" {
		if o.nodeID != "" || o.gatewayURL != "" {
			return r, usagef("--node-id and --gateway-url are for a generated node.json; edit your --node-config instead")
		}
		b, err := os.ReadFile(o.nodeConfig)
		if err != nil {
			return r, usagef("no node config: %v", err)
		}
		if supplied, err = canonicalConfig(b); err != nil {
			return r, usagef("node config %s: %v", o.nodeConfig, err)
		}
		r.GatewayURL, _ = supplied["gateway_url"].(string)
		if !gatewayOK(r.GatewayURL) {
			return r, usagef("--node-config: its gateway_url '%s' must look like wss://host/v1/node/connect (ws:// only to this machine)", r.GatewayURL)
		}
		r.NodeID, _ = supplied["node_id"].(string)
	} else {
		if o.nodeID != "" && !reNodeID.MatchString(o.nodeID) {
			return r, usagef("--node-id '%s': use letters, digits, '.', '_' and '-' (up to 64)", o.nodeID)
		}
		from := "--gateway-url"
		r.GatewayURL = o.gatewayURL
		if r.GatewayURL == "" && oldGW != "" {
			r.GatewayURL, from = oldGW, "the installed gateway_url"
		}
		if r.GatewayURL == "" {
			r.GatewayURL = setupflow.DefaultGatewayURL
		}
		if !gatewayOK(r.GatewayURL) {
			return r, usagef("%s '%s' must look like wss://host/v1/node/connect (ws:// only to this machine; pass --gateway-url)", from, r.GatewayURL)
		}
	}
	gw := r.GatewayURL

	token := ""
	if o.dryRun {
		r.Account = o.account
		switch o.account {
		case "anonymous":
			say("account: none; the install ends with registering this node without an account (aisathome-node register); an installed token recorded for this gateway is kept instead (unless that gateway says it is deleted, or a held node without an account's)")
			say("%s", RegisterNotice)
		case "link":
			say("account: the install ends with linking this node to your account in the browser (aisathome-node link)")
		case "":
			say("account: at a terminal the install asks \"Link this node to your account? [Y/n]\" (y: link it in the browser at the end, aisathome-node link; n: without an account, science only); an installed token recorded for this gateway is kept without asking")
		case "token":
			say("account: the node token from stdin (--token-stdin)")
		}
	} else {
		tokenEnvPath := filepath.Join(o.confDir, "token.env")
		if token, err = e.decideToken(o, tokenEnvPath, gw, &r, say); err != nil {
			return r, err
		}
	}

	// The node id of a generated node.json: --node-id, else the id the token carries, else the installed
	// one's, else the host name.
	if supplied == nil {
		r.NodeID = o.nodeID
		if r.NodeID == "" {
			if id, ok := node.TokenNodeID(token); ok {
				r.NodeID = id
			}
		}
		if r.NodeID == "" && oldID != "" {
			if !reNodeID.MatchString(oldID) {
				return r, usagef("the installed node id '%s' is not valid; pass --node-id", oldID)
			}
			r.NodeID = oldID
		}
		if r.NodeID == "" {
			if o.dryRun {
				say("node id: the one an account-page token carries, else the host name")
			}
			h, _ := e.hostname()
			h, _, _ = strings.Cut(h, ".")
			if !reNodeID.MatchString(h) {
				return r, usagef("the host name '%s' is not a valid node id; pass --node-id", h)
			}
			r.NodeID = h
		}
		if !o.dryRun && (oldID != "" || oldGW != "") {
			say("node.json: node id %s, gateway %s", r.NodeID, gw)
		}
		supplied = map[string]any{"node_id": r.NodeID, "gateway_url": gw, "upstream": o.upstream,
			"models": []any{map[string]any{"name": o.model, "roles": setup.EngineRoles, "slots": o.slots, "ctx": o.ctx}}}
	}

	// A dry run's node.json is only shown (on a terminal, in a log): without credentials.
	if o.dryRun {
		supplied = redactConfig(supplied).(map[string]any)
	}
	b, err := encodeConfig(supplied)
	if err != nil {
		return r, err
	}
	if err := os.WriteFile(filepath.Join(o.out, "node.json"), b, 0o640); err != nil {
		return r, err
	}
	if token != "" {
		// A token given now must have the issued form (checkGivenToken); a kept one is written as it was read,
		// as long as it is one plain value (nothing that could add a line or a setting to the file).
		issued := func(t string) bool { _, ok := node.IssuedAnyTokenNodeID(t); return ok }
		if r.Account == "keep" {
			issued = func(t string) bool {
				return t != "" && !strings.ContainsFunc(t, func(c rune) bool { return c <= ' ' || c == 0x7f })
			}
		}
		env, err := nodeconf.TokenEnv(token, gw, issued)
		if err != nil {
			return r, err
		}
		if err := os.WriteFile(filepath.Join(o.out, "token.env"), env, 0o600); err != nil {
			return r, err
		}
		r.TokenEnv = true
	}
	return r, nil
}

// decideToken reads token.env once and decides what becomes of the installed token and which token the
// node gets now ("" for none: keep nothing, link or register at the end). It sets r.Account and r.Park.
func (e preflightEnv) decideToken(o preflightOpts, tokenEnvPath, gw string, r *preflightResult, say func(string, ...any)) (string, error) {
	var old, rec string
	if b, err := os.ReadFile(tokenEnvPath); err == nil {
		old, rec = nodeconf.ParseTokenEnv(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// A token is trusted only with its recorded issuing gateway: one without a record, or recorded for
	// another gateway, is never kept or sent anywhere; the installer moves it aside.
	parked := ""
	if old != "" && nodeconf.CheckTokenGateway(rec, gw) != nil {
		issuer := rec
		if issuer == "" {
			issuer = "an unrecorded gateway"
		}
		if o.account == "keep" {
			return "", usagef("--keep-token: the installed token was issued by %s, not %s, and is never sent there: link the node (--link), or paste a node token for %s", issuer, gw, gw)
		}
		r.Park, parked, old = true, old, ""
		say("token: the installed one was issued by %s, not %s: it is moved aside (%s.unlinked) and never sent there", issuer, gw, tokenEnvPath)
	}
	switch o.account {
	case "anonymous":
		// A usable token stays (the node is updated) unless its gateway, which issued it, says it is gone or
		// a held node without an account's.
		if old != "" {
			switch e.state(old, gw) {
			case "gone":
				r.Park = true
				say("account: %s no longer knows the installed token (deleted or revoked): it is moved aside (%s.unlinked), and this node registers without an account", gw, tokenEnvPath)
			case "held":
				r.Park = true
				say("account: the installed token is a held node's without an account: it is moved aside (%s.unlinked), and this node registers again", tokenEnvPath)
			default:
				r.Account = "keep"
				if node.Anonymous(old) {
					say("account: this node already runs without an account on %s: it keeps its token and is updated", gw)
				} else {
					say("account: this node is linked to an account on %s: it keeps its token and is updated (not made a node without an account)", gw)
				}
				return old, nil
			}
		}
		r.Account = "anonymous"
		say("account: none; the install ends with registering this node without an account (science only)")
		say("%s", RegisterNotice)
		return "", nil
	case "link":
		r.Account = "link"
		say("account: the install ends with linking this node to your account in the browser")
		return "", nil
	case "keep":
		if old == "" {
			return "", usagef("--keep-token: no installed token in %s", tokenEnvPath)
		}
		r.Account = "keep"
		say("token: keeping the installed one")
		return old, nil
	}
	return e.askToken(o, old, parked, gw, tokenEnvPath, r, say)
}

// state asks the token's gateway about it (tokenState in tests, else askTokenState).
func (e preflightEnv) state(token, gw string) string {
	if e.tokenState != nil {
		return e.tokenState(token, gw)
	}
	return e.askTokenState(token, gw)
}

// askToken is the account without a flag (or with -account token). At a terminal, an installed token for
// this gateway is kept without asking; otherwise the person answers "Link this node to your account?
// [Y/n]": y links in the browser at the end of the install, n runs the node without an account. A node
// token (advanced) comes only from stdin: with -account token (pasted, not echoed, at a terminal), or piped
// by a script that gives no flag.
func (e preflightEnv) askToken(o preflightOpts, old, parked, gw, tokenEnvPath string, r *preflightResult, say func(string, ...any)) (string, error) {
	if o.account == "" && e.interactive {
		// A node without an account whose token its gateway deleted (not seen for 90 days) or holds registers
		// again, as with --anonymous: an update must not keep a token that no longer works. (A linked node's
		// token is kept: linking it again is the person's choice, --link.)
		if old != "" && node.Anonymous(old) {
			switch e.state(old, gw) {
			case "gone", "held":
				r.Park, r.Account = true, "anonymous"
				say("account: %s no longer takes this node's token (deleted or held): it is moved aside (%s.unlinked), and this node registers again without an account (science only)", gw, tokenEnvPath)
				say("%s", RegisterNotice)
				return "", nil
			}
		}
		if old != "" {
			r.Account = "keep"
			say("token: keeping the installed one (to link this node to another account instead: --link)")
			return old, nil
		}
		return "", e.askLink(r, say)
	}
	var line string
	var err error
	if e.interactive {
		err = e.hidden(func(read func() (string, error)) error {
			say("Paste the node token (input hidden):")
			line, err = read()
			return err
		})
	} else {
		line, err = readLine(e.stdin)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	tok, err := checkGivenToken(line, parked, gw)
	if err != nil {
		return "", err
	}
	if tok == "" {
		if o.account == "token" {
			return "", usagef("--token-stdin: no token on stdin")
		}
		hint := ""
		if old != "" {
			hint = " (to keep the installed one: --keep-token)"
		}
		return "", fmt.Errorf("no token on stdin%s; to link the node to your account in the browser instead: --link (or --anonymous: without an account)", hint)
	}
	r.Account = "token"
	return tok, nil
}

// askLink asks a person at the terminal whether to link the node to an account: y (or Enter) links it in
// the browser at the end of the install, n runs it without an account (science only). The answer is read
// without echo, like a token: someone used to the older installer, which asked for a node token here, may
// paste one, and it must not end up on the screen or in a terminal log. Whatever is not y or n is dropped
// unseen; a token-like answer is told that it was neither kept nor shown, and how to give a token.
func (e preflightEnv) askLink(r *preflightResult, say func(string, ...any)) error {
	// Echo stays off for every try, also while "Answer y or n." is printed: something pasted after any line
	// of the question never shows.
	return e.hidden(func(read func() (string, error)) error {
		for range 3 {
			say("")
			say("Link this node to your account? [Y/n] (the answer is not shown as you type)")
			say("  y: link it in your browser at the end of the install (a link and a code; approve on any device)")
			say("  n: run it without an account. %s", strings.ReplaceAll(RegisterNotice, "\n", "\n     "))
			line, err := read()
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if errors.Is(err, io.EOF) && line == "" {
				break // the terminal closed: no answer (Enter is a line, not the end)
			}
			answer := strings.ToLower(strings.TrimSpace(line))
			switch answer {
			case "", "y", "yes":
				r.Account = "link"
				say("account: the install ends with linking this node to your account in the browser")
				return nil
			case "n", "no":
				r.Account = "anonymous"
				say("account: none; the install ends with registering this node without an account (science only)")
				return nil
			}
			if looksLikeToken(answer) {
				say("That is not y or n, and it looks like a node token: a token is not taken at this question. It was not stored, and it was not shown. To give the node a token: run the installer again with --token-stdin (it reads the token without showing it, or from a pipe).")
			}
			if errors.Is(err, io.EOF) {
				break
			}
			say("Answer y or n.")
		}
		return usagef("no answer to \"Link this node to your account?\": run the installer again, or use --link or --anonymous")
	})
}

// looksLikeToken is an answer to a y/n question that may be a pasted credential: a node token's prefix,
// or anything longer than a word.
func looksLikeToken(answer string) bool {
	return strings.HasPrefix(answer, "nt-") || len(answer) > 8
}

// checkGivenToken is a token a person pasted or a script piped: without surrounding whitespace (systemd
// drops it from the saved value), exactly the form the gateway issues, and never the installed token that
// is being moved aside (it would be recorded as this gateway's and sent here). It is never printed.
func checkGivenToken(line, parked, gw string) (string, error) {
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", nil
	}
	if _, ok := node.IssuedAnyTokenNodeID(tok); !ok {
		return "", usagef("the token given is not a node token (nt-n-… or nt-a-… as the account page gives it; not shown here)")
	}
	if parked != "" && tok == parked {
		return "", usagef("the token given is the installed one, which is not %s's, so it is never sent there: link the node (--link), or give a node token from %s", gw, gw)
	}
	return tok, nil
}

// readLine reads one line from r, a byte at a time (nothing after it is consumed).
func readLine(r io.Reader) (string, error) {
	var b bytes.Buffer
	one := make([]byte, 1)
	for b.Len() < 4096 {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
	return b.String(), nil
}

// askTokenState asks the token's gateway (its recorded issuer, which gets this token anyway each time the
// node connects) about it: "gone" (it does not know it: deleted or revoked), "held" (a held node without an
// account), else "ok" (also when it does not answer: the token stays). The token goes in a header only.
func (e preflightEnv) askTokenState(token, gw string) string {
	base, err := tokenAPIBase(gw)
	if err != nil {
		return "ok"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+protocol.SelfPath, nil)
	if err != nil {
		return "ok"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "aisathome-node/"+node.Version)
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return "ok"
	}
	defer resp.Body.Close()
	var s protocol.NodeSelf
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "gone"
	case resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&s) == nil &&
		s.Anonymous && s.State == protocol.SelfHeld:
		return "held"
	}
	return "ok"
}

// tokenAPIBase is the gateway's HTTP base for its node address (wss:// → https://, ws:// → http://).
func tokenAPIBase(gw string) (string, error) {
	if !gatewayOK(gw) {
		return "", fmt.Errorf("not asking %s about the installed token: it is not encrypted (ws:// only to this machine)", gw)
	}
	base := gw
	if rest, ok := strings.CutPrefix(base, "wss://"); ok {
		base = "https://" + rest
	} else if rest, ok := strings.CutPrefix(base, "ws://"); ok {
		base = "http://" + rest
	}
	if i := strings.Index(base, "/v1/"); i >= 0 {
		base = base[:i]
	}
	return strings.TrimRight(base, "/"), nil
}

// canonicalConfig is a node.json as the agent reads it: no two keys equal in any case (at any level;
// nodeconf.CheckKeys), every object's keys in lower case, and no "token" (it lives in token.env).
func canonicalConfig(b []byte) (map[string]any, error) {
	if err := nodeconf.CheckKeys(b); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := lowerKeys(v).(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	delete(m, "token")
	return m, nil
}

func lowerKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[strings.ToLower(k)] = lowerKeys(e)
		}
		return m
	case []any:
		for i := range x {
			x[i] = lowerKeys(x[i])
		}
	}
	return v
}

// reSecretKey is a config key whose value is a credential.
var reSecretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|api_?key|auth)`)

// redactConfig is a config to show: values of credential-like keys, and the user and password in any
// URL (such as an upstream's http://user:pass@host), replaced by "REDACTED". The gateway fields stay
// readable (a token's record is an address, not a secret).
func redactConfig(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			if reSecretKey.MatchString(k) && k != "token_gateway" {
				if s, ok := e.(string); !ok || s != "" {
					e = "REDACTED"
				}
			}
			m[k] = redactConfig(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactConfig(e)
		}
		return out
	case string:
		if u, err := url.Parse(x); err == nil && u.User != nil && strings.Contains(x, "://") {
			u.User = url.User("REDACTED")
			return u.String()
		}
	}
	return v
}

// encodeConfig is node.json's bytes: indented, a final newline, no HTML escaping.
func encodeConfig(m map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
