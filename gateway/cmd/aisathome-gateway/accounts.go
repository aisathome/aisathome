// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const accountsUsage = `usage: aisathome-gateway accounts [-config gateway.json | -file accounts.json] <command>

  list                 accounts, their state, number of keys, weighted tokens used today (UTC)
                       and username, then their use over all time (requests, tokens, the day
                       of the last request); the account limit and sign-up state, and the
                       limits that apply to each account (tokens/day, requests/min, concurrent).
                       USED TODAY is the account page's "Today" number, from the budget
                       state the gateway saves every 5 s (needs -config with state_dir).
                       REQUESTS, TOKENS (prompt + completion, unweighted) and LAST USED (UTC)
                       are summed from the usage log, as far back as it goes (needs -config
                       with usage_log and accounts.session_secret, which names accounts in the
                       log; "-" when there is no log). A request retried on another node
                       counts again; records from before a change of session_secret are not
                       found. It reads the whole log, so it takes a moment on a big one
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
  contacts [id]        the addresses of accounts that asked to be emailed about important
                       changes and outages (opt-in on the sign-in or account page), with the
                       consent time and where the address came from; for writing by hand
  contact-clear <id>   delete an account's contact address (the user asked to stop)
  notice               show the notice on the account pages and in the chat
  notice set "<text>"  set it: one line of plain text, at most 300 characters; it replaces
                       the last one and shows again to people who dismissed that
  notice clear         remove it

  nodes                node tokens of all accounts, held nodes and the node settings
  nodes close | open   stop or allow new node tokens (existing nodes keep working)
  nodes limit <n>      at most n node tokens over all accounts (default 25)
  nodes per-account <n> | default
                       at most n node tokens per account (default 10)
  nodes anon close | open
                       stop or allow new nodes without an account (science only, registered by
                       the installer with no sign-in; existing ones keep working)
  nodes anon limit <n> | default
                       at most n nodes without an account (default 25), apart from the limit above
  node-revoke <node>   delete a node token (e.g. n-1a2b3c4d, or a node without an account's
                       a-1a2b3c4d); the node is dropped
  node-clear <node>    let a node that a guard held serve again
  guard-log [node]     the guards' recent stops, holds and flags (at most 200, of the past 14
                       days) with what matched: a piece of the node's answer, secrets redacted;
                       as of the gateway's last save (every 5 s; needs -config with state_dir)
  model-check [node]   the model check's result per node (pass, suspect, fail, unknown), with each
                       node's latest checks (KL, confident flips, the secret prompts' ids), as of
                       the gateway's last save (every 5 s; needs -config with state_dir)
  node-checks [days] [node]
                       answer checks and signals per node over the last days (default 7)
                       from the usage log (needs -config with usage_log): answers, failed
                       checks by rule, credit voided, output tok/s, answers cut at
                       max_tokens, bad tool calls, discarded answers, degenerate texts
  credit-adjust        the credit corrections: applied (as of the gateway's last save) and
                       pending (needs -config with state_dir)
  credit-adjust <node> <credit> "<reason>"
                       correct a node's credit and its owner's by a signed amount (negative
                       takes credit back) after a mistake, e.g. a counting bug; applied by the
                       gateway within seconds, kept in the ledger and listed with its reason on
                       the public leaderboard; it changes totals, not today's credit or the
                       recent average
  routing own-only | any
                       emergency switch: send every request only to its sender's own nodes
                       (no stranger's node serves anything, science neither), or back

  chat-on-pilots <id> on | off
                       an account's switch for its connector pilots (BOINC): off = they serve
                       science only (default on; the connector's pilots_chat_enabled and
                       no_chat_users still apply)
  link <id> <connector> <user>
                       link an account to a connector's user (a BOINC user id), so that user's
                       pilots serve as that account's nodes; a user links to one account only
  unlink <id> <connector> <user>
                       remove such a link

Ids look like gh:12345 (GitHub's numeric user id), google:<sub> (Google's account
identifier, digits), em:<32 hex digits> (email sign-in: random, not derived from the
address; "add" does not take em: ids, since only a sign-in can attach an address to one) or
cx:<32 hex digits> (unclaimed: made by a connector's pilot for a user with no account yet).
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
	// guard-log reads the gateway's state_dir only: it needs no accounts file, nor the right to
	// write it (opening the store writes it).
	if a := fs.Args(); len(a) > 0 && a[0] == "guard-log" {
		if len(a) > 2 {
			fs.Usage()
			return errors.New("guard-log takes at most one argument, a node")
		}
		if cfg == nil || cfg.StateDir == "" {
			return errors.New("guard-log needs -config with a state_dir: the gateway keeps the log there")
		}
		node := ""
		if len(a) == 2 {
			node = a[1]
		}
		es, err := gateway.ReadGuardLog(cfg.StateDir, time.Now())
		if err != nil {
			return err
		}
		return printGuardLog(out, es, node)
	}
	// model-check reads the state_dir only, like guard-log.
	if a := fs.Args(); len(a) > 0 && a[0] == "model-check" {
		if len(a) > 2 {
			fs.Usage()
			return errors.New("model-check takes at most one argument, a node")
		}
		if cfg == nil || cfg.StateDir == "" {
			return errors.New("model-check needs -config with a state_dir: the gateway keeps its results there")
		}
		st, err := gateway.ReadModelCheckState(cfg.StateDir)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no model check results: model_check is off, or the gateway has not saved any yet")
		} else if err != nil {
			return err
		}
		node := ""
		if len(a) == 2 {
			node = a[1]
		}
		return printModelCheck(out, st, node, time.Now())
	}
	if a := fs.Args(); len(a) > 0 && a[0] == "credit-adjust" {
		if cfg == nil || cfg.StateDir == "" {
			return errors.New("credit-adjust needs -config with a state_dir: the gateway keeps its ledger there")
		}
		return creditAdjust(out, cfg.StateDir, a[1:], time.Now())
	}
	if a := fs.Args(); len(a) > 0 && a[0] == "node-checks" {
		if cfg == nil || cfg.UsageLog == "" {
			return errors.New("node-checks needs -config with a usage_log")
		}
		return nodeChecks(out, cfg.UsageLog, a[1:], time.Now())
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
		return listAccounts(out, store, cfg, time.Now())
	case "limit":
		if err := need(1); err != nil {
			return err
		}
		n, err := strconv.Atoi(a[1])
		if err != nil || n < 0 {
			return fmt.Errorf("limit must be a number ≥ 0")
		}
		return store.SetMaxActive(n)
	case "close", "open":
		if err := need(0); err != nil {
			return err
		}
		return store.SetClosed(a[0] == "close")
	case "add":
		if err := need(1); err != nil {
			return err
		}
		if !strings.HasPrefix(a[1], "gh:") && !strings.HasPrefix(a[1], "google:") && !strings.HasPrefix(a[1], "test:") {
			return fmt.Errorf("an id looks like gh:12345, google:<sub> or test:<name>")
		}
		return store.AddAccount(a[1], time.Now())
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
			return listNodes(out, store, time.Now())
		}
		switch a[1] {
		case "close", "open":
			if len(a) != 2 {
				return errors.New("nodes close|open takes no argument")
			}
			return store.SetNodesClosed(a[1] == "close")
		case "limit":
			if len(a) != 3 {
				return errors.New("nodes limit takes one argument")
			}
			n, err := strconv.Atoi(a[2])
			if err != nil || n < 1 {
				return fmt.Errorf("the node limit must be a number ≥ 1 (use nodes close for none)")
			}
			return store.SetMaxNodes(n)
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
			return store.SetMaxNodesPerAccount(n)
		case "anon":
			switch {
			case len(a) == 3 && (a[2] == "close" || a[2] == "open"):
				return store.SetAnonClosed(a[2] == "close")
			case len(a) == 4 && a[2] == "limit":
				n := 0
				if a[3] != "default" {
					var err error
					if n, err = strconv.Atoi(a[3]); err != nil || n < 1 {
						return fmt.Errorf("the limit on nodes without an account must be a number ≥ 1, or default (nodes anon close for none)")
					}
				}
				return store.SetMaxAnonNodes(n)
			}
			return errors.New("nodes anon takes close, open, or limit <n>|default")
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
	case "contacts":
		if len(a) > 2 {
			fs.Usage()
			return errors.New("contacts takes at most one account id")
		}
		only := ""
		if len(a) == 2 {
			only = a[1]
		}
		return listContacts(out, store, only)
	case "contact-clear":
		if err := need(1); err != nil {
			return err
		}
		return store.ClearContact(a[1])
	case "notice":
		switch {
		case len(a) == 1:
			n := store.Settings().Notice
			if n == nil {
				fmt.Fprintln(out, "no notice")
				return nil
			}
			fmt.Fprintf(out, "%s (set %s)\n", n.Text, n.Set.Format("2006-01-02 15:04 UTC"))
			return nil
		case len(a) == 3 && a[1] == "set":
			return store.SetNotice(a[2], time.Now())
		case len(a) == 2 && a[1] == "clear":
			return store.ClearNotice()
		}
		fs.Usage()
		return errors.New(`notice takes set "<text>" (one argument: quote it) or clear`)
	case "routing":
		if err := need(1); err != nil {
			return err
		}
		if a[1] != gateway.RoutingOwnOnly && a[1] != "any" {
			return fmt.Errorf("routing is %s or any", gateway.RoutingOwnOnly)
		}
		routing := "" // any
		if a[1] == gateway.RoutingOwnOnly {
			routing = gateway.RoutingOwnOnly
		}
		return store.SetRouting(routing)
	case "chat-on-pilots":
		if err := need(2); err != nil {
			return err
		}
		if a[2] != "on" && a[2] != "off" {
			return errors.New("chat-on-pilots takes on or off")
		}
		return store.SetPilotChat(a[1], a[2] == "on")
	case "link", "unlink":
		if err := need(3); err != nil {
			return err
		}
		if a[0] == "unlink" {
			return store.Unlink(a[1], a[2], a[3])
		}
		return store.Link(a[1], a[2], a[3], time.Now())
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

// listContacts prints the opted-in addresses (of one account with only), oldest consent first.
func listContacts(out io.Writer, store *gateway.AccountStore, only string) error {
	var rows []gateway.Account
	for _, x := range store.Accounts() {
		if x.ContactEmail != "" && x.Status != gateway.StatusBanned && (only == "" || x.ID == only) {
			rows = append(rows, x)
		}
	}
	if only != "" && len(rows) == 0 {
		if _, _, ok := store.Get(only); !ok {
			return fmt.Errorf("no account %s", only)
		}
		return fmt.Errorf("%s has no contact address", only)
	}
	sort.SliceStable(rows, func(i, j int) bool { return consentTime(rows[i]).Before(consentTime(rows[j])) })
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tSTATE\tEMAIL\tFROM\tCONSENT (UTC)")
	for _, x := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", x.ID, x.Status, x.ContactEmail, x.ContactSource, consentTime(x).Format("2006-01-02 15:04"))
	}
	tw.Flush()
	if only == "" {
		fmt.Fprintf(out, "\n%d address(es). Write from contact@aisathome.org with the addresses in Bcc, and only about important changes and outages.\n", len(rows))
	}
	return nil
}

func consentTime(a gateway.Account) time.Time {
	if a.ContactConsentAt == nil {
		return time.Time{}
	}
	return *a.ContactConsentAt
}

// holdLine is a hold's rule, time and, when recorded, the guard's reason (fixed words, no content).
func holdLine(q gateway.NodeHold) string {
	s := q.Rule + " " + q.At.Format("2006-01-02 15:04")
	if q.Reason != "" {
		s += " (" + q.Reason + ")"
	}
	return s
}

// evidenceLines are a hold's or event's evidence, indented under its row: where and the host, then
// the fragment quoted (Go syntax: anything unprintable shows as an escape).
func evidenceLines(e *gateway.GuardEvidence, req string) string {
	var head []string
	if req != "" {
		head = append(head, "request "+req)
	}
	if e != nil && e.Where != "" {
		head = append(head, e.Where)
	}
	if e != nil && e.Host != "" {
		head = append(head, "host "+e.Host)
	}
	s := "    " + strings.Join(head, ", ") + "\n"
	if e == nil || e.Fragment == "" {
		return s + "    (nothing kept: older than 14 days, or held before evidence was kept)\n"
	}
	return s + "    " + strconv.Quote(e.Fragment) + "\n"
}

// printGuardLog prints the guard events, of one node if node is set, oldest first.
func printGuardLog(out io.Writer, es []gateway.GuardEvent, node string) error {
	n := 0
	for _, e := range es {
		if node != "" && e.Node != node {
			continue
		}
		n++
		fmt.Fprintf(out, "%s  %s  %s  %s", e.At.UTC().Format("2006-01-02 15:04:05"), e.Node, e.Action, e.Rule)
		if e.Reason != "" {
			fmt.Fprintf(out, " (%s)", e.Reason)
		}
		fmt.Fprintln(out)
		fmt.Fprint(out, evidenceLines(&e.GuardEvidence, e.Req))
	}
	fmt.Fprintf(out, "\n%d guard event(s) of the past 14 days (UTC); what matched is a piece of the node's answer, secrets redacted\n", n)
	return nil
}

// printModelCheck prints the model check's result per node, failing nodes first; with node set,
// that node's checks one per line.
func printModelCheck(out io.Writer, st *gateway.ModelCheckState, node string, now time.Time) error {
	th := st.Thresholds
	mode := "observe-only"
	if from := st.ObservingSince.Add(gateway.ModelCheckObserveFirst); st.Withhold && now.Before(from) {
		mode = "observe-only, withholding from " + from.UTC().Format("2006-01-02 15:04") + " UTC"
	} else if st.Withhold {
		mode = "withholding (a failing node is held)"
	}
	fmt.Fprintf(out, "model check: %s, observing since %s UTC; limits KL %.4f per check of %d prompts, %d confident flips over the window\n\n",
		mode, st.ObservingSince.UTC().Format("2006-01-02 15:04"), th.MaxCheckKL, th.GroupSize, th.MaxFlips)
	ids := make([]string, 0, len(st.Nodes))
	for id := range st.Nodes {
		if node == "" || id == node {
			ids = append(ids, id)
		}
	}
	if node != "" && len(ids) == 0 {
		return fmt.Errorf("no model check results for node %s", node)
	}
	rank := map[string]int{gateway.MCFail: 0, gateway.MCSuspect: 1, gateway.MCPass: 2, gateway.MCUnknown: 3}
	res := map[string]string{}
	for _, id := range ids {
		res[id], _, _ = st.Nodes[id].Status(th)
	}
	sort.Slice(ids, func(i, j int) bool {
		if rank[res[ids[i]]] != rank[res[ids[j]]] {
			return rank[res[ids[i]]] < rank[res[ids[j]]]
		}
		return ids[i] < ids[j]
	})
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tOPERATOR\tMODEL\tRESULT\tCHECKS\tLARGEST KL\tFLIPS\tNO ANSWER\tLAST CHECK")
	for _, id := range ids {
		n := st.Nodes[id]
		r, worst, flips := n.Status(th)
		if !n.HeldAt.IsZero() {
			r += " (held)"
		}
		last := "-"
		if len(n.Checks) > 0 {
			c := n.Checks[len(n.Checks)-1]
			last = c.Verdict + " " + c.At.UTC().Format("01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d pass, %d fail\t%.4f\t%d\t%d\t%s\n", id, n.Operator, n.Model, r, n.Pass, n.Fail, worst, flips, n.Dropped, last)
	}
	tw.Flush()
	if node != "" {
		fmt.Fprintf(out, "\nlatest checks of %s, oldest first:\n", node)
		for _, c := range st.Nodes[node].Checks {
			fmt.Fprintf(out, "%s  %s  KL %.4f  flips %d  positions %d  diverged %d  prompts %s", c.At.UTC().Format("2006-01-02 15:04:05"),
				c.Verdict, c.KL, c.Flips, c.Positions, c.Diverged, strings.Join(c.Prompts, ","))
			if len(c.Reasons) > 0 {
				fmt.Fprintf(out, "  (%s)", strings.Join(c.Reasons, "; "))
			}
			fmt.Fprintln(out)
		}
	}
	return nil
}

// listNodes prints every account's node tokens, the held nodes, what their guard matched and the
// node settings.
func listNodes(out io.Writer, store *gateway.AccountStore, now time.Time) error {
	holds, settings := store.NodeHolds(), store.Settings()
	held := map[string]gateway.NodeHold{}
	for _, p := range holds {
		held[p.Node] = p
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tACCOUNT\tNAME\tCREATED\tHELD")
	total, anon := 0, 0
	for _, a := range store.Accounts() {
		for _, n := range a.Nodes {
			if strings.HasPrefix(a.ID, "ax:") {
				anon++
			} else {
				total++
			}
			p := "-"
			if q, ok := held[n.ID]; ok {
				p = holdLine(q)
				delete(held, n.ID)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.ID, a.ID, n.Name, n.Created.Format("2006-01-02"), p)
		}
	}
	for _, q := range holds { // held nodes of config tokens, or of revoked tokens
		if _, ok := held[q.Node]; ok {
			fmt.Fprintf(tw, "%s\t%s\t\t\t%s\n", q.Node, q.Operator, holdLine(q))
		}
	}
	tw.Flush()
	if len(holds) > 0 {
		fmt.Fprintln(out, "\nWhat the guards matched (a piece of the node's answer, secrets redacted, kept 14 days):")
		for _, q := range holds {
			fmt.Fprintf(out, "  %s  %s\n", q.Node, q.Rule)
			fmt.Fprint(out, evidenceLines(q.CurrentEvidence(now), q.Req))
		}
	}
	state, routing := "open", "any"
	if settings.NodesClosed {
		state = "closed"
	}
	if settings.OwnOnly() { // also "trusted-only", its name before 2026-11-20
		routing = gateway.RoutingOwnOnly
	}
	perAccount := strconv.Itoa(settings.MaxNodesPerAccountOrDefault())
	if settings.MaxNodesPerAccount == 0 {
		perAccount += " (default)"
	}
	anonState := "open"
	if settings.AnonClosed || settings.NodesClosed {
		anonState = "closed"
	}
	fmt.Fprintf(out, "\n%d of %d nodes without an account (science only, ids a-…); new ones are %s\n", anon, settings.MaxAnonNodesOrDefault(), anonState)
	// The summary stays the last line: the browser tests (and scripts) read it there.
	fmt.Fprintf(out, "%d of %d node tokens; up to %s per account; new nodes are %s; routing %s\n", total,
		settings.MaxNodesOrDefault(), perAccount, state, routing)
	return nil
}

// listAccounts prints the accounts, the sign-up state and, with a config, the per-account
// limits, each account's use today and its use over all time.
func listAccounts(out io.Writer, store *gateway.AccountStore, cfg *gateway.Config, now time.Time) error {
	accounts, settings := store.Accounts(), store.Settings()
	ids := make([]string, len(accounts))
	for i, x := range accounts {
		ids[i] = x.ID
	}
	var used map[string]int64 // nil: unknown, the column is left out
	if cfg != nil && cfg.StateDir != "" {
		var err error
		if used, err = gateway.AccountsUsedToday(cfg.StateDir, ids, now); err != nil {
			return fmt.Errorf("reading today's use: %w", err)
		}
	}
	totals, totalsNote, err := readTotals(cfg, ids)
	if err != nil {
		return err
	}
	active := 0
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	head := []string{"ID", "STATE", "KEYS"}
	if used != nil {
		head = append(head, "USED TODAY")
	}
	head = append(head, "CREATED", "USERNAME")
	if cfg != nil { // without a config the usage log is unknown: the columns are left out
		head = append(head, "REQUESTS", "TOKENS", "LAST USED")
	}
	fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, x := range accounts {
		if x.Status == gateway.StatusActive {
			active++
		}
		row := []string{x.ID, x.Status, strconv.Itoa(len(x.Keys))}
		if used != nil {
			row = append(row, thousands(used[x.ID]))
		}
		row = append(row, x.Created.Format("2006-01-02"), strconv.Quote(x.Username))
		if cfg != nil {
			row = append(row, totalsColumns(totals, x.ID)...)
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
	state := "open"
	if settings.Closed {
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
	if totalsNote != "" {
		fmt.Fprintln(out, totalsNote)
	}
	// The summary stays the last line: the browser tests (and scripts) read it there.
	fmt.Fprintf(out, "%d of %d active, %d accounts in total; sign-up is %s\n", active, settings.MaxActive, len(accounts), state)
	return nil
}

// readTotals sums each account's use over all time from the usage log of cfg. A nil map means
// unknown (the columns show "-"); the note says where the numbers come from, or why they are
// unknown. Without a config there is neither.
func readTotals(cfg *gateway.Config, ids []string) (map[string]gateway.AccountTotals, string, error) {
	const unknown = "REQUESTS, TOKENS, LAST USED are unknown (-): "
	switch {
	case cfg == nil:
		return nil, "", nil
	case cfg.UsageLog == "":
		return nil, unknown + "the config has no usage_log", nil
	case cfg.Accounts == nil || cfg.Accounts.SessionSecret == "":
		return nil, unknown + "the config has no accounts.session_secret, which names accounts in the usage log", nil
	}
	lf, err := os.Open(cfg.UsageLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil, unknown + "there is no usage log at " + cfg.UsageLog + " (usage_log in the config)", nil
	} else if err != nil {
		return nil, "", fmt.Errorf("reading the usage log: %w", err)
	}
	defer lf.Close()
	totals, malformed, err := gateway.ReadAccountTotals(lf, cfg.Accounts.SessionSecret, ids)
	if err != nil {
		return nil, "", fmt.Errorf("reading the usage log %s: %w", cfg.UsageLog, err)
	}
	note := "REQUESTS, TOKENS, LAST USED: all time, as far back as the usage log " + cfg.UsageLog + " goes (usage_log);\n" +
		"  requests answered or attempted by a node (a retry on another node counts again), prompt + completion\n" +
		"  tokens as logged (unweighted), and the UTC day of the last one"
	if malformed > 0 {
		note += fmt.Sprintf("; %d malformed line(s) in the log skipped", malformed)
	}
	return totals, note, nil
}

// totalsColumns are an account's REQUESTS, TOKENS and LAST USED: "-" when unknown (totals nil),
// and LAST USED "-" for an account the log has no record of.
func totalsColumns(totals map[string]gateway.AccountTotals, id string) []string {
	if totals == nil {
		return []string{"-", "-", "-"}
	}
	t, last := totals[id], "-"
	if !t.Last.IsZero() {
		last = t.Last.UTC().Format("2006-01-02")
	}
	return []string{thousands(t.Requests), thousands(t.Tokens), last}
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

// creditAdjust lists the credit corrections, or asks for one (node, signed credit, reason).
func creditAdjust(out io.Writer, stateDir string, a []string, now time.Time) error {
	switch len(a) {
	case 0:
	case 3:
		credit, err := strconv.ParseFloat(a[1], 64)
		if err != nil {
			return fmt.Errorf("credit %q: not a number", a[1])
		}
		c, err := gateway.AddCreditCorrection(stateDir, a[0], credit, a[2], now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "requested %s: %s %+.3f credit (%s); the gateway applies it within seconds\n", c.ID, c.Node, c.Credit, c.Reason)
	default:
		return errors.New(`credit-adjust takes no arguments, or <node> <credit> "<reason>"`)
	}
	applied, pending, err := gateway.ReadCreditCorrections(stateDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tTIME (UTC)\tNODE\tCREDIT\tREASON")
	row := func(c gateway.CreditCorrection, state string, at time.Time) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%+.3f\t%s\n", c.ID, state, at.UTC().Format("2006-01-02 15:04"), c.Node, c.Credit, c.Reason)
	}
	for _, c := range applied {
		row(c, "applied", c.Applied)
	}
	for _, c := range pending {
		row(c, "pending", c.Time)
	}
	if len(applied)+len(pending) == 0 {
		fmt.Fprintln(tw, "(none)")
	}
	return tw.Flush()
}
