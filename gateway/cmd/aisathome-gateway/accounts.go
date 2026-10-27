// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const accountsUsage = `usage: aisathome-gateway accounts [-config gateway.json | -file accounts.json] <command>

  list                 accounts, their state, number of keys, weighted tokens used today (UTC)
                       and username; the account limit and sign-up state, and the limits
                       that apply to each account (tokens/day, requests/min, concurrent).
                       USED TODAY is the account page's "Today" number, from the budget
                       state the gateway saves every 5 s (needs -config with state_dir)
  limit <n>            let at most n accounts be active; waiting accounts get in automatically
  close | open         stop or allow new sign-ups (existing accounts keep working)
  add <id>             create an active account (e.g. gh:12345) without a sign-in, for tests
  signup <id>          sign up like a first sign-in (test:<name> ids only): active if there is
                       room, otherwise on the waitlist; for end-to-end tests of the queue
  activate <id>        let one account in now (also beyond the limit)
  waitlist <id>        move an account back to the waitlist
  ban <id>             close an account and delete its keys and node tokens (it cannot sign
                       up again); its connected nodes are dropped within seconds
  username-clear <id>  remove an account's public username (offensive or impersonating);
                       it leaves the leaderboard and exports at their next regeneration

  nodes                node tokens of all accounts, held nodes and the node settings
  nodes close | open   stop or allow new node tokens (existing nodes keep working)
  nodes limit <n>      at most n node tokens over all accounts (default 25)
  nodes per-account <n> | default
                       at most n node tokens per account (default 10)
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
	configSet := false
	fs.Visit(func(f *flag.Flag) { configSet = configSet || f.Name == "config" })
	path := *file
	var cfg *gateway.Config // nil with -file alone: list then shows no limits or use
	if path == "" || configSet {
		c, err := loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		cfg = &c
	}
	if path == "" {
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
		return listAccounts(out, store.Snapshot(), cfg, time.Now())
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
	case "signup":
		if err := need(1); err != nil {
			return err
		}
		if !strings.HasPrefix(a[1], "test:") {
			return fmt.Errorf("signup takes test:<name> ids only; real accounts sign in themselves")
		}
		acct, err := store.SignIn(a[1], time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintln(out, acct.Status)
		return nil
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
		case "per-account":
			if len(a) != 3 {
				return errors.New("nodes per-account takes one argument")
			}
			n := 0 // "default": the gateway's default, which may change with a release
			if a[2] != "default" {
				var err error
				if n, err = strconv.Atoi(a[2]); err != nil || n < 1 {
					return fmt.Errorf("the per-account node limit must be a number ≥ 1, or default")
				}
			}
			return store.Update(func(f *gateway.AccountsFile) error { f.Settings.MaxNodesPerAccount = n; return nil })
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
	case "username-clear":
		if err := need(1); err != nil {
			return err
		}
		return store.ClearUsername(a[1])
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
	perAccount := strconv.Itoa(f.Settings.MaxNodesPerAccountOrDefault())
	if f.Settings.MaxNodesPerAccount == 0 {
		perAccount += " (default)"
	}
	fmt.Fprintf(out, "\n%d of %d node tokens; up to %s per account; new nodes are %s; routing %s\n", total,
		f.Settings.MaxNodesOrDefault(), perAccount, state, routing)
	return nil
}

// listAccounts prints the accounts, the sign-up state and, with a config, the per-account
// limits and each account's use today.
func listAccounts(out io.Writer, f gateway.AccountsFile, cfg *gateway.Config, now time.Time) error {
	var used map[string]int64 // nil: unknown, the column is left out
	if cfg != nil && cfg.StateDir != "" {
		ids := make([]string, len(f.Accounts))
		for i, x := range f.Accounts {
			ids[i] = x.ID
		}
		var err error
		if used, err = gateway.AccountsUsedToday(cfg.StateDir, ids, now); err != nil {
			return fmt.Errorf("reading today's use: %w", err)
		}
	}
	active := 0
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if used != nil {
		fmt.Fprintln(tw, "ID\tSTATE\tKEYS\tUSED TODAY\tCREATED\tUSERNAME")
	} else {
		fmt.Fprintln(tw, "ID\tSTATE\tKEYS\tCREATED\tUSERNAME")
	}
	for _, x := range f.Accounts {
		if x.Status == gateway.StatusActive {
			active++
		}
		if used != nil {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%q\n", x.ID, x.Status, len(x.Keys), thousands(used[x.ID]),
				x.Created.Format("2006-01-02"), x.Username)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%q\n", x.ID, x.Status, len(x.Keys), x.Created.Format("2006-01-02"), x.Username)
		}
	}
	tw.Flush()
	state := "open"
	if f.Settings.Closed {
		state = "closed"
	}
	fmt.Fprintln(out)
	if cfg != nil && cfg.Accounts != nil {
		fmt.Fprintln(out, limitsLine(cfg.Accounts.Limits))
	}
	switch {
	case used != nil:
		fmt.Fprintf(out, "USED TODAY: weighted tokens since 00:00 UTC, chat and API together (the account page's \"Today\"),\n"+
			"  as of the gateway's last save of %s (every 5 s)\n", filepath.Join(cfg.StateDir, gateway.BudgetsFile))
	case cfg != nil:
		fmt.Fprintln(out, "today's use is not shown: the config has no state_dir, so it lives only in the gateway's memory")
	}
	// The summary stays the last line: the browser tests (and scripts) read it there.
	fmt.Fprintf(out, "%d of %d active, %d accounts in total; sign-up is %s\n", active, f.Settings.MaxActive, len(f.Accounts), state)
	return nil
}

// limitsLine states the limits every account has, each on its own (0 = unlimited).
func limitsLine(l gateway.UserLimits) string {
	n := func(v int, unit string) string {
		if v <= 0 {
			return "unlimited " + unit
		}
		return thousands(int64(v)) + " " + unit
	}
	return "limits per account, each account separately: " + n(l.TokensPerDay, "tokens/day (weighted)") + ", " +
		n(l.RPM, "requests/min") + ", " + n(l.MaxConcurrent, "concurrent") + "; the day resets 00:00 UTC"
}

// thousands formats n with commas: 5000000 → 5,000,000.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
