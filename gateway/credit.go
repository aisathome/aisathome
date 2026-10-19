// Credit (decision 0007, backlog 18): recognition for the GPU time a node gives, in BOINC's
// unit. Decided by the owner on 2026-10-11: credit follows GPU time, anchored to BOINC's
// cobblestone for decode. An output token is 0.125 credit (about 2 × 27e9 FLOPs at 4.32e11
// FLOPs per credit), a new prompt token a tenth of that (prefill is
// that much faster per token) and a cached prompt token a hundredth. Nothing is paid for it.
//
// Tokens are counted by the gateway (count.go, tokenizer.go) and cache hits decided by it
// (prefixcache.go). No credit if no value is delivered (owner, 2026-10-18): a cancelled
// answer earns only for the output that reached the client, and nothing if none did. No credit
// for answers to the node's own account (self_served), for failed or blocked answers. Volunteers' credit is kept per account and per node; nodes from
// config tokens (the maintainer's) earn for their operator name, kept apart from accounts.
// The ledger lives in the state dir (credit.json) like the budgets.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credit per token (owner, 2026-10-11).
const (
	CreditPerOutputToken = 0.125
	CreditPerPromptToken = 0.0125
	CreditPerCachedToken = 0.00125
)

// creditHalfLife is BOINC's CREDIT_HALF_LIFE: recent average credit halves in a week.
const creditHalfLife = 7 * 86400.0

// creditTokens is what credit is computed from, as the gateway counted it.
type creditTokens struct {
	Prompt     int `json:"prompt"`     // all prompt tokens, cached included
	Cached     int `json:"cached"`     // of which the same node had just served (gateway's decision)
	Completion int `json:"completion"` // generated
}

func (t creditTokens) credit() float64 {
	cached := min(max(t.Cached, 0), max(t.Prompt, 0))
	return float64(max(t.Prompt, 0)-cached)*CreditPerPromptToken + float64(cached)*CreditPerCachedToken +
		float64(max(t.Completion, 0))*CreditPerOutputToken
}

// updateAverage is BOINC's update_average (lib/util.cpp) with times in Unix seconds. avg is
// credit per day as of avgTime. On the first grant BOINC divides by the time the work took;
// callers pass a start no later than a day before now (see grant).
func updateAverage(now, workStart, work, halfLife float64, avg, avgTime *float64) {
	if *avgTime != 0 {
		diff := max(now-*avgTime, 0)
		diffDays := diff / 86400
		weight := math.Exp(-diff * math.Ln2 / halfLife)
		*avg *= weight
		if 1-weight > 1e-6 {
			*avg += (1 - weight) * (work / diffDays)
		} else {
			*avg += math.Ln2 * work * 86400 / halfLife
		}
	} else if work != 0 {
		dd := (now - workStart) / 86400
		*avg = work / dd
	}
	*avgTime = now
}

// decayAverage is what readers apply to an average from avgTime (BOINC's decay_average).
func decayAverage(avg, avgTime float64, now time.Time) float64 {
	if avgTime == 0 {
		return 0
	}
	diff := max(float64(now.UnixNano())/1e9-avgTime, 0)
	return avg * math.Exp(-diff*math.Ln2/creditHalfLife)
}

// creditEntry is the credit of one account, operator or node.
type creditEntry struct {
	ID      int64     `json:"id"` // BOINC-style numeric id (user or host), assigned on first credit
	Total   float64   `json:"total"`
	Day     string    `json:"day"`    // UTC day of Today
	Today   float64   `json:"today"`  // credit on Day
	Avg     float64   `json:"expavg"` // BOINC's expavg_credit, as of AvgTime
	AvgTime float64   `json:"expavg_time"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`

	// Nodes only: whose node it is.
	Account  string `json:"account,omitempty"`  // a volunteer's account id
	Operator string `json:"operator,omitempty"` // or a config token's operator
}

// TodayAt is the credit earned on now's UTC day.
func (e *creditEntry) TodayAt(now time.Time) float64 {
	if e == nil || e.Day != utcDay(now) {
		return 0
	}
	return e.Today
}

func (e *creditEntry) total() float64 {
	if e == nil {
		return 0
	}
	return e.Total
}

// RAC is the recent average credit per day, decayed to now.
func (e *creditEntry) RAC(now time.Time) float64 {
	if e == nil {
		return 0
	}
	return decayAverage(e.Avg, e.AvgTime, now)
}

func (e *creditEntry) add(c float64, since, now time.Time) {
	day := utcDay(now)
	if e.Day != day {
		e.Day, e.Today = day, 0
	}
	e.Today += c
	e.Total += c
	if e.First.IsZero() {
		e.First = now.UTC()
	}
	e.Last = now.UTC()
	// The first grant sets the average to credit per day since the account or node was
	// created, and never over less than a day: BOINC divides by the job's duration, which
	// for a ten-second answer would show a busy node's rate for a week.
	start := now.Add(-24 * time.Hour)
	if !since.IsZero() && since.Before(start) {
		start = since
	}
	updateAverage(float64(now.UnixNano())/1e9, float64(start.UnixNano())/1e9, c, creditHalfLife, &e.Avg, &e.AvgTime)
}

type creditFile struct {
	Accounts   map[string]*creditEntry `json:"accounts"`  // by account id
	Operators  map[string]*creditEntry `json:"operators"` // by config operator name
	Nodes      map[string]*creditEntry `json:"nodes"`     // by node id
	NextUserID int64                   `json:"next_user_id"`
	NextHostID int64                   `json:"next_host_id"`
}

// creditLedger is the credit of everyone, saved like the budgets.
type creditLedger struct {
	mu     sync.Mutex
	f      creditFile
	path   string
	dirty  bool
	saveMu sync.Mutex
}

func newCreditLedger() *creditLedger {
	return &creditLedger{f: creditFile{Accounts: map[string]*creditEntry{}, Operators: map[string]*creditEntry{},
		Nodes: map[string]*creditEntry{}, NextUserID: 1, NextHostID: 1}}
}

// grant adds credit c for an answer by node nodeID, to its account (a volunteer) or, for a
// config token, its operator. accountSince and nodeSince are when they were created (zero
// if unknown).
func (l *creditLedger) grant(nodeID, account, operator string, c float64, accountSince, nodeSince, now time.Time) {
	if c <= 0 || math.IsNaN(c) || math.IsInf(c, 0) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	owner := l.f.Operators
	key := operator
	if account != "" {
		owner, key = l.f.Accounts, account
	}
	e := owner[key]
	if e == nil {
		e = &creditEntry{}
		if account != "" {
			e.ID = l.f.NextUserID
			l.f.NextUserID++
		}
		owner[key] = e
	}
	e.add(c, accountSince, now)
	n := l.f.Nodes[nodeID]
	if n == nil {
		n = &creditEntry{ID: l.f.NextHostID}
		l.f.NextHostID++
		l.f.Nodes[nodeID] = n
	}
	n.Account, n.Operator = account, ""
	if account == "" {
		n.Operator = operator
	}
	n.add(c, nodeSince, now)
	l.dirty = true
}

// snapshot returns a deep copy of the ledger.
func (l *creditLedger) snapshot() creditFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := func(m map[string]*creditEntry) map[string]*creditEntry {
		out := make(map[string]*creditEntry, len(m))
		for k, v := range m {
			e := *v
			out[k] = &e
		}
		return out
	}
	return creditFile{Accounts: cp(l.f.Accounts), Operators: cp(l.f.Operators), Nodes: cp(l.f.Nodes),
		NextUserID: l.f.NextUserID, NextHostID: l.f.NextHostID}
}

// account and node return copies of one entry (nil if none).
func (l *creditLedger) account(id string) *creditEntry {
	return l.get(func() *creditEntry { return l.f.Accounts[id] })
}
func (l *creditLedger) node(id string) *creditEntry {
	return l.get(func() *creditEntry { return l.f.Nodes[id] })
}

func (l *creditLedger) get(f func() *creditEntry) *creditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := f()
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// load reads the ledger; a missing file is an empty ledger, a broken one is logged and kept
// aside (credit.json.bad) rather than overwritten.
func (l *creditLedger) load(log *slog.Logger) {
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f creditFile
	if err == nil {
		err = json.Unmarshal(raw, &f)
	}
	if err != nil {
		log.Error("credit ledger unreadable; starting empty and keeping the old file as .bad", "err", err)
		os.Rename(l.path, l.path+".bad")
		return
	}
	n := newCreditLedger().f
	for _, m := range []*map[string]*creditEntry{&f.Accounts, &f.Operators, &f.Nodes} {
		if *m == nil {
			*m = map[string]*creditEntry{}
		}
	}
	f.NextUserID, f.NextHostID = max(f.NextUserID, n.NextUserID), max(f.NextHostID, n.NextHostID)
	l.mu.Lock()
	l.f = f
	l.mu.Unlock()
}

// save writes the ledger if it changed (atomically: temp file, then rename).
func (l *creditLedger) save() error {
	if l.path == "" {
		return nil
	}
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(l.f)
	l.dirty = false
	l.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		l.markDirty()
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		l.markDirty()
		return err
	}
	return nil
}

func (l *creditLedger) markDirty() { l.mu.Lock(); l.dirty = true; l.mu.Unlock() }

// fmtCredit writes credit for people: two decimals below 10, one below 1,000, then whole
// numbers with thousands separators.
func fmtCredit(c float64) string {
	switch {
	case c < 10:
		return strconv.FormatFloat(c, 'f', 2, 64)
	case c < 1000:
		return strconv.FormatFloat(c, 'f', 1, 64)
	}
	s := strconv.FormatFloat(math.Round(c), 'f', 0, 64)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
