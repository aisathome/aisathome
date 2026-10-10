// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

const keysUsage = `usage: aisathome-gateway keys <command> [flags]

  add <user> [-tokens-per-day N] [-rpm N] [-concurrent N] [-min-trust trusted|any]
        create a key; it is printed once and only its hash is stored
  list  show keys by prefix (never the keys themselves)
  revoke <prefix|key>
        remove a key

The keys file comes from keys_file in -config (default gateway.json), or from -file.
A running gateway applies changes within a few seconds.
`

// runKeys implements the keys subcommand. The new key goes to out alone, so it can be
// piped; everything else goes to msg.
func runKeys(args []string, out, msg io.Writer) error {
	if len(args) == 0 {
		return errors.New(keysUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("keys "+cmd, flag.ContinueOnError)
	fs.SetOutput(msg)
	cfgPath := fs.String("config", "gateway.json", "gateway config naming keys_file")
	file := fs.String("file", "", "keys file (overrides keys_file in -config)")
	// Defaults suit one person using a coding tool (opencode): a few parallel requests, and
	// a daily budget that a heavy day with mostly cached context fits in.
	tpd := fs.Int("tokens-per-day", 2_000_000, "weighted tokens per UTC day (0 = unlimited)")
	rpm := fs.Int("rpm", 30, "requests per minute (0 = unlimited)")
	conc := fs.Int("concurrent", 4, "requests in flight (0 = unlimited)")
	trust := fs.String("min-trust", "trusted", `"trusted": only trusted nodes see this key's requests; "any": all nodes`)
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	path := *file
	if path == "" {
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		if path = cfg.KeysFile; path == "" {
			return fmt.Errorf("%s sets no keys_file; add one or pass -file", *cfgPath)
		}
	}
	kf, err := gateway.LoadKeysFile(path)
	if err != nil {
		return err
	}

	switch {
	case cmd == "add" && len(pos) == 1:
		info := gateway.KeyInfo{User: pos[0], TokensPerDay: *tpd, RPM: *rpm, MaxConcurrent: *conc}
		switch *trust {
		case "trusted":
			info.MinTrust = "trusted"
		case "any":
		default:
			return fmt.Errorf("-min-trust must be trusted or any, not %q", *trust)
		}
		// Redaction stays on (it is off only for keys bound to the user's own nodes) and
		// secrets are not restored into answers; enabling that is a deliberate edit.
		key, err := kf.Add(info, time.Now())
		if err != nil {
			return err
		}
		if err := kf.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(msg, "added key %s for %s; it is shown only now:\n", key[:11], pos[0])
		fmt.Fprintln(out, key)
	case cmd == "list" && len(pos) == 0:
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PREFIX\tUSER\tTOKENS/DAY\tRPM\tCONCURRENT\tTRUST\tCREATED")
		for _, r := range kf.Keys {
			trust := r.MinTrust
			if trust == "" {
				trust = "any"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Prefix, r.User, limit(r.TokensPerDay),
				limit(r.RPM), limit(r.MaxConcurrent), trust, r.Created.Format("2006-01-02"))
		}
		return tw.Flush()
	case cmd == "revoke" && len(pos) == 1:
		r, err := kf.Revoke(pos[0])
		if err != nil {
			return err
		}
		if err := kf.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(msg, "revoked key %s of %s\n", r.Prefix, r.User)
	default:
		return errors.New(keysUsage)
	}
	return nil
}

func limit(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// parseInterspersed allows flags after positional arguments (keys add bob -rpm 10), which
// the flag package alone stops parsing at.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
