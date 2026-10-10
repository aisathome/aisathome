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
  ban <id>             close an account and delete its keys (it cannot sign up again)

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
