// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const accountsUsage = `usage: aisathome-gateway accounts [-config gateway.json | -file accounts.json] <command>

  list                 accounts, their state and number of keys; the limit and sign-up state
  limit <n>            let at most n accounts be active; waiting accounts get in automatically
  close | open         stop or allow new sign-ups (existing accounts keep working)
  add <id>             create an active account (e.g. gh:12345) without a sign-in, for tests
  activate <id>        let one account in now (also beyond the limit)
  waitlist <id>        move an account back to the waitlist
  ban <id>             close an account and delete its keys and node tokens (it cannot sign
                       up again); its connected nodes are dropped within seconds

  nodes                node tokens of all accounts, held nodes and the node settings
  nodes close | open   stop or allow new node tokens (existing nodes keep working)
  nodes limit <n>      at most n node tokens over all accounts (default 10)
  node-revoke <node>   delete a node token (e.g. n-1a2b3c4d); the node is dropped
  node-clear <node>    let a node that a guard held serve again
  routing trusted-only | any
                       emergency switch: send every request to trusted nodes only, or back

Ids look like gh:12345 (GitHub's numeric user id), google:<sub> (Google's account
identifier, digits) or em:<32 hex digits> (email sign-in: random, not derived from the
address; "add" does not take em: ids, since only a sign-in can attach an address to one).
The running gateway picks up changes within seconds.`

func runAccounts(args []string, out, msg io.Writer) error {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.SetOutput(msg)
	cfgPath := fs.String("config", "gateway.json", "gateway config with an accounts section")
	file := fs.String("file", "", "accounts file (overrides accounts.file in -config)")
	fs.Usage = func() { fmt.Fprintln(msg, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *file
	if path == "" {
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		if cfg.Accounts == nil || cfg.Accounts.File == "" {
			return fmt.Errorf("%s has no accounts.file; pass -file", *cfgPath)
		}
		path = cfg.Accounts.File
	}
	store, err := gateway.OpenAccountStore(path)
	if err != nil {
		return err
	}
	a := fs.Args()
	if len(a) == 0 {
		fs.Usage()
		return errors.New("no command")
	}
	need := func(n int) error {
		if len(a) != n+1 {
			fs.Usage()
			return fmt.Errorf("%s takes %d argument(s)", a[0], n)
		}
		return nil
	}
	switch a[0] {
	case "list":
		f := store.Snapshot()
		active := 0
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tKEYS\tCREATED")
		for _, x := range f.Accounts {
			if x.Status == gateway.StatusActive {
				active++
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", x.ID, x.Status, len(x.Keys), x.Created.Format("2006-01-02"))
		}
		tw.Flush()
		state := "open"
		if f.Settings.Closed {
			state = "closed"
		}
		fmt.Fprintf(out, "\n%d of %d active, %d accounts in total; sign-up is %s\n", active, f.Settings.MaxActive, len(f.Accounts), state)
		return nil
	case "limit":
		if err := need(1); err != nil {
			return err
		}
		n, err := strconv.Atoi(a[1])
		if err != nil || n < 0 {
			return fmt.Errorf("limit must be a number ≥ 0")
		}
		return store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxActive = n; return nil })
	case "close", "open":
		if err := need(0); err != nil {
			return err
		}
		return store.Update(func(f *gateway.AccountsFile) error { f.Settings.Closed = a[0] == "close"; return nil })
	case "add":
		if err := need(1); err != nil {
			return err
		}
		if !strings.HasPrefix(a[1], "gh:") && !strings.HasPrefix(a[1], "google:") && !strings.HasPrefix(a[1], "test:") {
			return fmt.Errorf("an id looks like gh:12345, google:<sub> or test:<name>")
		}
		return store.Update(func(f *gateway.AccountsFile) error {
			if f.Find(a[1]) != nil {
				return fmt.Errorf("%s exists", a[1])
			}
			now := time.Now().UTC()
			f.Accounts = append(f.Accounts, &gateway.Account{ID: a[1], Status: gateway.StatusActive, Created: now, Activated: &now})
			return nil
		})
	case "nodes":
		if len(a) == 1 || a[1] == "list" {
			return listNodes(out, store.Snapshot())
		}
		switch a[1] {
		case "close", "open":
			if len(a) != 2 {
				return errors.New("nodes close|open takes no argument")
			}
			return store.Update(func(f *gateway.AccountsFile) error { f.Settings.NodesClosed = a[1] == "close"; return nil })
		case "limit":
			if len(a) != 3 {
				return errors.New("nodes limit takes one argument")
			}
			n, err := strconv.Atoi(a[2])
			if err != nil || n < 1 {
				return fmt.Errorf("the node limit must be a number ≥ 1 (use nodes close for none)")
			}
			return store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodes = n; return nil })
		}
		fs.Usage()
		return fmt.Errorf("unknown nodes command %q", a[1])
	case "node-revoke":
		if err := need(1); err != nil {
			return err
		}
		return store.RevokeNode("", a[1])
	case "node-clear":
		if err := need(1); err != nil {
			return err
		}
		return store.ClearNode(a[1])
	case "routing":
		if err := need(1); err != nil {
			return err
		}
		if a[1] != gateway.RoutingTrustedOnly && a[1] != "any" {
			return fmt.Errorf("routing is %s or any", gateway.RoutingTrustedOnly)
		}
		routing := "" // any
		if a[1] == gateway.RoutingTrustedOnly {
			routing = gateway.RoutingTrustedOnly
		}
		return store.Update(func(f *gateway.AccountsFile) error { f.Settings.Routing = routing; return nil })
	case "activate", "waitlist", "ban":
		if err := need(1); err != nil {
			return err
		}
		status := map[string]string{"activate": gateway.StatusActive, "waitlist": gateway.StatusWaitlisted, "ban": gateway.StatusBanned}[a[0]]
		return store.SetStatus(a[1], status)
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", a[0])
}

// listNodes prints every account's node tokens, the held nodes and the node settings.
func listNodes(out io.Writer, f gateway.AccountsFile) error {
	held := map[string]gateway.NodeHold{}
	for _, p := range f.NodeHolds {
		held[p.Node] = p
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tACCOUNT\tNAME\tCREATED\tHELD")
	total := 0
	for _, a := range f.Accounts {
		for _, n := range a.Nodes {
			total++
			p := "-"
			if q, ok := held[n.ID]; ok {
				p = q.Rule + " " + q.At.Format("2006-01-02 15:04")
				delete(held, n.ID)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.ID, a.ID, n.Name, n.Created.Format("2006-01-02"), p)
		}
	}
	for _, q := range f.NodeHolds { // held nodes of config tokens, or of revoked tokens
		if _, ok := held[q.Node]; ok {
			fmt.Fprintf(tw, "%s\t%s\t\t\t%s %s\n", q.Node, q.Operator, q.Rule, q.At.Format("2006-01-02 15:04"))
		}
	}
	tw.Flush()
	state, routing := "open", "any"
	if f.Settings.NodesClosed {
		state = "closed"
	}
	if f.Settings.Routing != "" {
		routing = f.Settings.Routing
	}
	fmt.Fprintf(out, "\n%d of %d node tokens; new nodes are %s; routing %s\n", total, f.Settings.MaxNodesOrDefault(), state, routing)
	return nil
}
