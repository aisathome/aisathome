// Evidence of the guards (guard.go): what a rule matched, so that an operator can tell an attack
// from an honest answer before clearing a hold. A hold of 2026-11-19 recorded only the rule and its
// fixed reason ("a download piped into a shell or interpreter"): nobody could say whether the node
// had piped an unknown script into a shell or suggested an official installer, and the node stayed
// held for hours.
//
// The evidence is a fragment of the node's answer, never of the request: at most maxFragment bytes
// of the text the rule read (a tool call's decoded arguments, the answer's text, or a stream chunk)
// around the match, with secrets in well-known formats redacted (secretPatterns, as in requests)
// and control and format characters escaped. It is kept with the hold record (the accounts file)
// and in a bounded log of recent stops and flags (state_dir/guard-log.json), each for evidenceKeep
// (14 days), and shown to admins only (/account/ops, `accounts nodes`, `accounts guard-log`). The
// gateway's own log lines keep no fragment and no host.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// GuardEvidence is what a guard matched in a node's answer (see the top of this file).
type GuardEvidence struct {
	Where    string `json:"where,omitempty"`    // "tool call bash", "answer text" or "stream"
	Host     string `json:"host,omitempty"`     // the host of the web address the match names, if any
	Fragment string `json:"fragment,omitempty"` // the text around the match, secrets redacted
}

// GuardEvent is one guard stop, hold or flag, with its evidence.
type GuardEvent struct {
	At       time.Time `json:"at"`
	Node     string    `json:"node"`
	Operator string    `json:"operator,omitempty"`
	Req      string    `json:"req"`
	Rule     string    `json:"rule"`
	Reason   string    `json:"reason,omitempty"`
	Action   string    `json:"action"` // GuardHeld, GuardStopped or GuardFlagged
	GuardEvidence
}

// Actions of guard events.
const (
	GuardHeld    = "held"    // the answer was stopped and the node held
	GuardStopped = "stopped" // the answer was stopped, the node not held (an official installer)
	GuardFlagged = "flagged" // the answer went out; the event is only recorded
)

const (
	// maxFragment bounds a fragment: the match with the text around it, or its start if longer.
	maxFragment = 300
	// EvidenceKeep is how long fragments are kept (GDPR, data minimisation): long enough for an
	// operator to look at a hold after a weekend away, no longer.
	EvidenceKeep = 14 * 24 * time.Hour
	// maxGuardLog bounds the log of recent guard events.
	maxGuardLog = 200
	// GuardLogFile is the log's name in state_dir.
	GuardLogFile = "guard-log.json"
	// redactReach is how far around a fragment secrets are looked for, so that one that starts
	// before the fragment or ends after it is still found whole.
	redactReach = 64 << 10
)

// evidence is the hit's evidence: a fragment of the text the rule read, around the match. Where
// (a tool's name) and the host come from the node too: they are bounded, redacted and escaped like
// the fragment (code review, 2026-11-19).
func (h guardHit) evidence() GuardEvidence {
	return GuardEvidence{Where: cleanField(h.where), Host: cleanField(h.host), Fragment: fragmentOf(h.text, h.at[0], h.at[1])}
}

// maxField bounds where and the host: a tool's name or a host name is short.
const maxField = 100

// cleanField is a short field of the node's text, redacted and escaped as a fragment is, then cut
// to maxField bytes. Redacted before it is cut: a secret cut first could fall below its pattern's
// shortest length and stay (code review, 2026-11-19). The text read for secrets is bounded by
// redactReach; what lies beyond it is never kept.
func cleanField(s string) string {
	if s == "" {
		return ""
	}
	s = escapeFragment(redactWindow(s, 0, min(len(s), redactReach)))
	if len(s) > maxField {
		end := maxField
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		s = s[:end] + "…"
	}
	return s
}

// fragmentOf is text[m0:m1] with the text around it, at most maxFragment bytes of text before
// redaction, cut at rune boundaries, "…" where it was cut, secrets redacted and control and format
// characters escaped.
func fragmentOf(text string, m0, m1 int) string {
	if m0 < 0 || m1 > len(text) || m0 >= m1 {
		return ""
	}
	start, end := m0, m1
	if end-start >= maxFragment {
		end = start + maxFragment
	} else {
		pad := (maxFragment - (end - start)) / 2
		start, end = max(0, start-pad), min(len(text), end+pad)
		if room := maxFragment - (end - start); room > 0 { // one side reached an end of the text
			if start == 0 {
				end = min(len(text), end+room)
			} else {
				start = max(0, start-room)
			}
		}
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start++
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(strings.TrimSpace(escapeFragment(redactWindow(text, start, end))))
	if end < len(text) {
		b.WriteString("…")
	}
	return b.String()
}

// redactWindow is text[start:end] with every secret that overlaps it replaced whole, also one that
// begins before start or ends after end (looked for within redactReach).
func redactWindow(text string, start, end int) string {
	lo, hi := max(0, start-redactReach), min(len(text), end+redactReach)
	region := text[lo:hi]
	type span struct{ s, e int }
	var spans []span
	for _, p := range secretPatterns {
		for _, m := range p.FindAllStringIndex(region, -1) {
			if s, e := lo+m[0], lo+m[1]; s < end && e > start {
				spans = append(spans, span{s, e})
			}
		}
	}
	if len(spans) == 0 {
		return text[start:end]
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s < spans[j].s })
	var b strings.Builder
	pos := start
	for _, sp := range spans {
		if sp.e <= pos {
			continue // inside one already replaced
		}
		if sp.s > pos {
			b.WriteString(text[pos:sp.s])
		}
		b.WriteString(redacted(text[sp.s:sp.e]))
		pos = sp.e
	}
	if pos < end {
		b.WriteString(text[pos:end])
	}
	return b.String()
}

// redacted is what stands for a secret in a fragment: its kind's marker, never its random part.
func redacted(secret string) string {
	if strings.HasPrefix(secret, "-----BEGIN") {
		return "[private key redacted]"
	}
	return typePrefix(secret) + "[redacted]"
}

// escapeFragment makes a fragment one line of printable text: line breaks and tabs become spaces,
// other control and format characters (escape sequences, bidirectional overrides, invalid UTF-8)
// \u escapes, so that it shows on a page and in a terminal as what it is.
func escapeFragment(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == utf8.RuneError && !strings.HasPrefix(s[i:], "\uFFFD"): // invalid UTF-8
			b.WriteString(`\ufffd`)
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hostIn is the host of the first web address in s (urlHostRe, or a bare host given to curl or
// wget), lower case; "" if none.
func hostIn(s string) string {
	best, host := -1, ""
	for _, re := range []*regexp.Regexp{urlHostRe, bareHostRe} {
		if m := re.FindStringSubmatchIndex(s); m != nil && (best < 0 || m[0] < best) {
			best, host = m[0], s[m[2]:m[3]]
		}
	}
	return strings.ToLower(strings.Trim(strings.TrimRight(host, ".,;"), "[]"))
}

// ---------------------------------------------------------------- the log of recent events

// guardLog keeps the last maxGuardLog guard events of the past EvidenceKeep, in memory and, with a
// state_dir, in GuardLogFile (saved every few seconds when changed, and at shutdown).
type guardLog struct {
	mu     sync.Mutex
	path   string // "" without a state_dir
	events []GuardEvent
	dirty  bool
}

// add keeps e, dropping the oldest beyond maxGuardLog.
func (l *guardLog) add(e GuardEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	if n := len(l.events) - maxGuardLog; n > 0 {
		l.events = slices.Delete(l.events, 0, n)
	}
	l.dirty = true
}

// list returns the events of the past EvidenceKeep, oldest first, and drops older ones.
func (l *guardLog) list(now time.Time) []GuardEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	return slices.Clone(l.events)
}

// prune drops events older than EvidenceKeep. Caller holds l.mu.
func (l *guardLog) prune(now time.Time) {
	kept := keptEvents(l.events, now)
	if len(kept) != len(l.events) {
		l.events, l.dirty = kept, true
	}
}

// keptEvents are the events of es within EvidenceKeep of now, at most the newest maxGuardLog.
func keptEvents(es []GuardEvent, now time.Time) []GuardEvent {
	out := make([]GuardEvent, 0, len(es))
	for _, e := range es {
		if now.Sub(e.At) < EvidenceKeep {
			out = append(out, e)
		}
	}
	if n := len(out) - maxGuardLog; n > 0 {
		out = out[n:]
	}
	return out
}

// load reads the log file. A file that does not parse is deleted, not set aside: its fragments
// must not outlive their 14 days in a copy nobody prunes.
func (l *guardLog) load(log *slog.Logger, now time.Time) {
	es, err := readGuardLog(l.path)
	if err != nil {
		if log != nil {
			log.Warn("guard log unreadable, deleting it", "file", l.path, "err", err)
		}
		os.Remove(l.path)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = es
	l.prune(now)
}

// save writes the log if it changed (or old events were dropped since).
func (l *guardLog) save(now time.Time) error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	l.prune(now)
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(l.events)
	l.dirty = false
	l.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(l.path, append(raw, '\n'))
	}
	if err != nil {
		l.mu.Lock()
		l.dirty = true // try again next time
		l.mu.Unlock()
	}
	return err
}

func readGuardLog(path string) ([]GuardEvent, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []GuardEvent
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, err
	}
	return es, nil
}

// ReadGuardLog returns the guard events the gateway last saved in stateDir (every few seconds),
// those of the past EvidenceKeep, oldest first (for `accounts guard-log`).
func ReadGuardLog(stateDir string, now time.Time) ([]GuardEvent, error) {
	es, err := readGuardLog(filepath.Join(stateDir, GuardLogFile))
	if err != nil {
		return nil, fmt.Errorf("guard log: %w", err)
	}
	return keptEvents(es, now), nil
}

// guardEvent makes the event of a hit on n's answer to request reqID.
func guardEvent(n *nodeConn, hit guardHit, reqID, action string, at time.Time) GuardEvent {
	return GuardEvent{At: at, Node: n.id, Operator: n.operator, Req: reqID, Rule: hit.rule, Reason: hit.why,
		Action: action, GuardEvidence: hit.evidence()}
}

// ---------------------------------------------------------------- holds' evidence

// CurrentEvidence is the hold's evidence while it is kept (EvidenceKeep after the hold), else nil:
// a gateway that was not running to drop it must not show it either.
func (p NodeHold) CurrentEvidence(now time.Time) *GuardEvidence {
	if p.Evidence == nil || now.Sub(p.At) >= EvidenceKeep {
		return nil
	}
	return p.Evidence
}

// dropOldEvidence removes the evidence of holds older than EvidenceKeep from the accounts file;
// the holds themselves stay until the operator clears them. It writes only when there is any.
func (s *AccountStore) dropOldEvidence(now time.Time) error {
	old := func(p NodeHold) bool { return p.Evidence != nil && p.CurrentEvidence(now) == nil }
	s.mu.Lock()
	found := slices.ContainsFunc(s.f.NodeHolds, old)
	s.mu.Unlock()
	if !found {
		return nil
	}
	return s.Update(func(f *AccountsFile) error {
		for i := range f.NodeHolds {
			if old(f.NodeHolds[i]) {
				f.NodeHolds[i].Evidence = nil
			}
		}
		return nil
	})
}
