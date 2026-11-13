// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

const serviceUsage = `usage: aisathome-node service <command> [flags]

Registers the agent as a Windows service (the Windows installer runs these, as administrator).
On Linux, install.sh sets up systemd instead.

  install [-start] [-ui-user <account or SID>]
        create or update the service: this executable, virtual account
        NT SERVICE\aisathome-node, delayed automatic start, restart on failure; then the
        data folder under %ProgramData% with its permissions and the local UI's password.
        -ui-user is the account that may read the password (default: the account running
        this command, unless that is SYSTEM).
  uninstall
        stop and remove the service. The data folder stays.
  stop
        disable and stop the service and wait for its process to end (before an update
        replaces files); the next install enables it again.
`

// installOptions are `service install`'s flags.
type installOptions struct {
	Start  bool
	UIUser string
}

// runServiceCommand runs `aisathome-node service ...` and returns the exit code.
func runServiceCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, serviceUsage)
		return 2
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, serviceUsage) }
	var o installOptions
	if args[0] == "install" {
		fs.BoolVar(&o.Start, "start", false, "start the service after installing it")
		fs.StringVar(&o.UIUser, "ui-user", "", "the account that may read the local UI's password")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "service %s: unexpected arguments: %s\n", args[0], strings.Join(fs.Args(), " "))
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = serviceInstall(o, stdout)
	case "uninstall":
		err = serviceUninstall(stdout)
	case "stop":
		err = serviceStop(stdout)
	default:
		fmt.Fprint(stderr, serviceUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "service %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// Well-known SIDs for the data folder's permissions.
const (
	sidSystem           = "S-1-5-18"
	sidAdministrators   = "S-1-5-32-544"
	sidTrustedInstall   = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	sidLocalService     = "S-1-5-19"
	sidNetworkService   = "S-1-5-20"
	accessModify        = "0x1301bf" // read, write, execute and delete; not change permissions or owner
	sddlFileGenericRead = "FR"
)

// dataDirSDDL is the data folder's security descriptor: owned by Administrators, inheritance
// from %ProgramData% cut (it lets every user create files there), full control for SYSTEM and
// Administrators, modify for the service's own account; files and folders inside inherit it.
// So the token in node.json, the password and the state are readable only by the service and
// administrators.
func dataDirSDDL(serviceSID string) string {
	return "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;" + accessModify + ";;;" + serviceSID + ")"
}

// inheritOnlySDDL resets a file to the folder's inherited permissions and nothing else.
const inheritOnlySDDL = "D:"

// passwordFileSDDL is the local UI password file's own entry, on top of what it inherits:
// read for the installing user, so the tray app (running as that user) can sign in.
func passwordFileSDDL(userSID string) string {
	if userSID == "" {
		return inheritOnlySDDL
	}
	return "D:(A;;" + sddlFileGenericRead + ";;;" + userSID + ")"
}

// trustedOwner reports whether a data folder or file owned by sid may be taken over by the
// installer: SYSTEM, Administrators, TrustedInstaller, or one of also (the service's account,
// the administrator running the install). Anything else was made by another account (every
// user may create folders under %ProgramData%), which could change its permissions back after
// the install; the installer refuses it rather than put a token there.
func trustedOwner(sid string, also ...string) bool {
	if sid == "" {
		return false
	}
	return slices.Contains([]string{sidSystem, sidAdministrators, sidTrustedInstall}, sid) || slices.Contains(also, sid)
}

// uiUserFromToken is the default -ui-user: the account running the installer, unless it is a
// service account (an unattended install as SYSTEM has no user to give the password to).
func uiUserFromToken(sid string) string {
	if slices.Contains([]string{sidSystem, sidLocalService, sidNetworkService}, sid) {
		return ""
	}
	return sid
}

// minPasswordLen matches the local UI, which refuses a shorter password.
const minPasswordLen = 12

// newPassword returns a random local UI password (32 characters, 192 bits).
func newPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// usablePassword reports whether a password file's content is one the local UI accepts, so
// the installer keeps it (an update keeps the password the tray and the user know).
func usablePassword(content []byte) bool {
	return len(strings.TrimSpace(string(content))) >= minPasswordLen
}
