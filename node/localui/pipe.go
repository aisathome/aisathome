// The tray's way in: the agent answers a named pipe (Windows; pipe_windows.go) with one-time
// sign-in links to the local page, so the tray never needs the local password, and opens the
// page signed in. Who may ask is decided by Windows itself from the caller's account, not by a
// secret: administrators (also from a normal, not elevated, window, since they can read the
// password anyway) and the accounts that may read the password file (the installer gives the
// installing user that right). Here: the protocol and that rule, platform-independent so its
// tests run everywhere.
//
// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
)

// PipeRequest is one line the tray writes to the pipe.
type PipeRequest struct {
	Op string `json:"op"` // OpLoginLink
}

// OpLoginLink asks for a one-time sign-in link (PipeAnswer.URL).
const OpLoginLink = "login-link"

// PipeAnswer is the agent's one-line answer.
type PipeAnswer struct {
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// pipeAnswer answers a request from a caller the pipe has already let in.
func (s *Server) pipeAnswer(line []byte) PipeAnswer {
	var req PipeRequest
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		return PipeAnswer{Error: "send one JSON line, e.g. {\"op\":\"" + OpLoginLink + "\"}"}
	}
	switch req.Op {
	case OpLoginLink:
		u, err := s.NewLoginLink()
		if err != nil {
			return PipeAnswer{Error: err.Error()}
		}
		return PipeAnswer{URL: u}
	}
	return PipeAnswer{Error: "unknown op " + strconv.Quote(req.Op)}
}

// NewLoginLink returns a new one-time sign-in link to the page,
// http://127.0.0.1:<port>/login-link?t=<secret>: single use, within a minute.
func (s *Server) NewLoginLink() (string, error) {
	secret, err := s.auth.newLink()
	if err != nil {
		return "", err
	}
	return "http://" + Addr(s.opt.Port) + "/login-link?t=" + secret, nil
}

var linkSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidLoginLink reports whether u is a sign-in link to this computer's local UI on port: the
// tray opens nothing else it got from the pipe (another program could hold the pipe's name
// while the agent is down).
func ValidLoginLink(u string, port int) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" || p.Host != Addr(port) || p.Path != "/login-link" || p.User != nil || p.Fragment != "" {
		return false
	}
	q := p.Query()
	return len(q) == 1 && len(q["t"]) == 1 && linkSecret.MatchString(q.Get("t"))
}

// Well-known SIDs and token group attributes used by the pipe's rule.
const (
	sidAdministrators = "S-1-5-32-544"
	groupEnabled      = 0x4 // SE_GROUP_ENABLED
)

// tokenGroup is one group in the caller's token, with its attributes (SE_GROUP_*).
type tokenGroup struct {
	SID   string
	Attrs uint32
}

// trayCaller is the account on the other end of the pipe, from its token.
type trayCaller struct {
	User   string
	Groups []tokenGroup
}

// mayReadPassword applies the pipe's rule (package comment): Administrators in any state
// (UAC's filtered token keeps it as deny-only: an elevation away), or the user or one of its
// enabled groups among the password file's readers and not denied. allow and deny are the
// SIDs of the file's allow and deny entries that cover reading.
func mayReadPassword(c trayCaller, allow, deny []string) bool {
	for _, g := range c.Groups {
		if g.SID == sidAdministrators {
			return true
		}
	}
	ids := []string{c.User}
	all := []string{c.User}
	for _, g := range c.Groups {
		all = append(all, g.SID)
		if g.Attrs&groupEnabled != 0 {
			ids = append(ids, g.SID)
		}
	}
	for _, id := range all { // a deny entry also matches deny-only groups, as Windows does
		if slices.Contains(deny, id) {
			return false
		}
	}
	for _, id := range ids {
		if slices.Contains(allow, id) {
			return true
		}
	}
	return false
}
