// Pilot records: a pilot is one bounded serving session that
// a connector (the BOINC bridge is the first) hands to a volunteer's machine. The gateway is the
// source of truth for it: the connector asks for a pilot, gets its id ("p-" and 12 hex digits) and a
// credential, makes its own work item with the same name, and later tells the gateway who runs it
// (bind) and that it was reported. The pilot connects over the ordinary node WebSocket with the
// credential and serves as a node of the account its bind maps to (connector.go).
//
// The credential is "pc-<12 hex>.<secret>": the hex part is the pilot's id, so the node agent takes
// its node id from it as it does from an account's node token. Only its SHA-256 is stored. It works
// for one connection at a time, only after the bind, and only until the pilot ends or its deadline
// passes; a pilot that reconnects with it (a suspended BOINC task resumed) continues the same record,
// so the record's totals cover every run of the work unit.
//
// States: created (no machine yet), bound (the connector named the machine's user, host and team),
// connected, disconnected (it may come back: a suspend), ended (it said it ended for good, the
// connector reported it, or its deadline passed). A pilot ends for good when it closes with
// "pilot: time", "pilot: idle" or "pilot: engine"; "pilot: sigterm" and a dropped connection leave it
// disconnected.
//
// Credit: each answer the pilot serves is metered with the ledger's formula (credit.go,
// docs/credit.md: nothing for self-served, failed or undelivered answers) into the pilot's record,
// capped per pilot and per account and UTC day, and never into the public ledger: pilot credit is
// granted through the connector (BOINC's own statistics), not the gateway's /stats/ exports. It
// becomes final pilotCreditHold after the pilot's last disconnect, once it has ended; it is withheld
// (final 0) if a guard held the pilot, and the gateway's model check (modelcheck.go), when it withholds, the same way
// (withholdPilotCredit).
//
// The records live in the science database (science_db.go, migration 2; the tier, migration 5).
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Pilot states.
const (
	PilotCreated      = "created"
	PilotBound        = "bound"
	PilotConnected    = "connected"
	PilotDisconnected = "disconnected"
	PilotEnded        = "ended"
)

var pilotStates = []string{PilotCreated, PilotBound, PilotConnected, PilotDisconnected, PilotEnded}

const (
	// pilotCreditHold: a pilot's credit becomes final this long after its last disconnect.
	pilotCreditHold = 24 * time.Hour
	// pilotSeenEvery: how often connected pilots record that they are still connected, so that a
	// gateway restart counts their serve time up to then.
	pilotSeenEvery = time.Minute
	// pilotTimeFmt: fixed width, so the text sorts as it reads.
	pilotTimeFmt = "2006-01-02T15:04:05.000Z"
	// pilotListMax bounds a list of pilots.
	pilotListMax = 500
)

// Close reasons a pilot sends ("pilot: <reason>", node/pilot) that end it for good.
var pilotFinalReasons = map[string]bool{"pilot: time": true, "pilot: idle": true, "pilot: engine": true}

// What a connector may report about a pilot's work unit (POST .../reported).
var pilotOutcomes = []string{"success", "error", "cancelled", "expired"}

var (
	pilotIDRe = regexp.MustCompile(`^p-[0-9a-f]{12}$`)
	// extIDRe: a connector's own id for a user, host or team (BOINC's are integers). Never an email
	// address: no '@'.
	extIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

func validPilotID(id string) bool { return pilotIDRe.MatchString(id) }

// splitPilotCredential returns the pilot id of a credential "pc-<12 hex>.<secret>".
func splitPilotCredential(cred string) (id string, ok bool) {
	rest, ok := strings.CutPrefix(cred, "pc-")
	if !ok {
		return "", false
	}
	h, secret, ok := strings.Cut(rest, ".")
	id = "p-" + h
	return id, ok && validPilotID(id) && len(secret) >= 40
}

// newPilotCredential makes a pilot id and its credential.
func newPilotCredential() (id, cred string, err error) {
	b := make([]byte, 6+32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	h := hex.EncodeToString(b[:6])
	return "p-" + h, "pc-" + h + "." + base64.RawURLEncoding.EncodeToString(b[6:]), nil
}

// Pilot is a pilot's record as the connector API shows it. The credential and the gateway account
// are never in it (AccountKind says whether the account is a linked or an unclaimed one).
type Pilot struct {
	ID          string       `json:"id"`
	Connector   string       `json:"connector"`
	Tier        string       `json:"tier,omitempty"` // the connector's pilot tier it was created for (connector.go)
	State       string       `json:"state"`
	Deadline    time.Time    `json:"deadline"`
	User        string       `json:"user,omitempty"`
	Host        string       `json:"host,omitempty"`
	Team        string       `json:"team,omitempty"`
	AccountKind string       `json:"account,omitempty"` // "linked" or "unclaimed", once bound
	Created     time.Time    `json:"created_at"`
	Bound       *time.Time   `json:"bound_at"`
	First       *time.Time   `json:"first_connected_at"`
	Connected   *time.Time   `json:"connected_at"`
	Disconn     *time.Time   `json:"disconnected_at"`
	Ended       *time.Time   `json:"ended_at"`
	CloseReason string       `json:"close_reason"`
	EndReason   string       `json:"end_reason"`
	Connections int64        `json:"connections"`
	ServedSec   int64        `json:"served_seconds"`
	Requests    int64        `json:"requests"`
	Failed      int64        `json:"failed"`
	SelfServed  int64        `json:"self_served"`
	Tokens      creditTokens `json:"tokens"`
	Metered     float64      `json:"metered_credit"` // by the formula, before the caps
	Credit      float64      `json:"credit"`         // after the caps; final only per GET .../credit
	Withheld    string       `json:"withheld,omitempty"`
	Agent       string       `json:"agent,omitempty"`
	account     string       // the gateway account its bind maps to
	seen        *time.Time   // last "still connected" mark
	servedMs    int64        // ServedSec in milliseconds
}

func ptime(t time.Time) string { return t.UTC().Format(pilotTimeFmt) }

func parsePTime(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(pilotTimeFmt, s.String)
	if err != nil {
		return nil
	}
	return &t
}

const pilotCols = `id, connector, state, deadline, ext_user, ext_host, ext_team, account, created_at, bound_at,
	first_connected_at, connected_at, disconnected_at, seen_at, ended_at, close_reason, end_reason, connections,
	served_ms, requests, failed, self_served, prompt_tokens, cached_tokens, completion_tokens, metered, credit,
	withheld, agent, tier`

type rowScanner interface{ Scan(dest ...any) error }

func scanPilot(r rowScanner) (*Pilot, error) {
	var p Pilot
	var deadline, created string
	var bound, first, conn, disc, seen, ended sql.NullString
	err := r.Scan(&p.ID, &p.Connector, &p.State, &deadline, &p.User, &p.Host, &p.Team, &p.account, &created, &bound,
		&first, &conn, &disc, &seen, &ended, &p.CloseReason, &p.EndReason, &p.Connections, &p.servedMs, &p.Requests,
		&p.Failed, &p.SelfServed, &p.Tokens.Prompt, &p.Tokens.Cached, &p.Tokens.Completion, &p.Metered, &p.Credit,
		&p.Withheld, &p.Agent, &p.Tier)
	if err != nil {
		return nil, err
	}
	if t := parsePTime(sql.NullString{String: deadline, Valid: true}); t != nil {
		p.Deadline = *t
	}
	if t := parsePTime(sql.NullString{String: created, Valid: true}); t != nil {
		p.Created = *t
	}
	p.Bound, p.First, p.Connected, p.Disconn, p.seen, p.Ended = parsePTime(bound), parsePTime(first), parsePTime(conn),
		parsePTime(disc), parsePTime(seen), parsePTime(ended)
	p.ServedSec = p.servedMs / 1000
	return &p, nil
}

var errNoPilot = &taskErr{code: http.StatusNotFound, msg: "no such pilot"}

// pilotCtx bounds one database call.
func pilotCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// createPilot adds a pilot of tier ("" without tiers) for connector with deadline, unless the
// connector already has max pilots that have not ended, or created mint pilots in the last hour. It
// returns the record and the credential (shown once).
func (s *scienceDB) createPilot(connector, tier string, deadline time.Time, max, mint int, now time.Time) (*Pilot, string, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", s.fail("pilot create", connector, err)
	}
	defer tx.Rollback()
	var open, recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilots WHERE connector = ? AND state != 'ended'`, connector).Scan(&open); err != nil {
		return nil, "", s.fail("pilot create", connector, err)
	}
	if open >= max {
		return nil, "", terr(http.StatusConflict, "this connector has %d pilots that have not ended (max_pilots %d)", open, max)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilots WHERE connector = ? AND created_at > ?`, connector,
		ptime(now.Add(-time.Hour))).Scan(&recent); err != nil {
		return nil, "", s.fail("pilot create", connector, err)
	}
	if recent >= mint {
		return nil, "", terr(http.StatusTooManyRequests, "at most %d pilots an hour (mint_per_hour)", mint)
	}
	var id, cred string
	for i := 0; ; i++ {
		if id, cred, err = newPilotCredential(); err != nil {
			return nil, "", err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pilots (id, connector, cred_sha256, state, deadline, created_at, tier)
			VALUES (?, ?, ?, 'created', ?, ?, ?)`, id, connector, keyHash(cred), ptime(deadline), ptime(now), tier)
		if err != nil {
			return nil, "", s.fail("pilot create", connector, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			break
		}
		if i == 5 {
			return nil, "", s.fail("pilot create", connector, errors.New("no free pilot id"))
		}
	}
	p, err := scanPilot(tx.QueryRowContext(ctx, `SELECT `+pilotCols+` FROM pilots WHERE id = ?`, id))
	if err != nil {
		return nil, "", s.fail("pilot create", connector, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", s.fail("pilot create", connector, err)
	}
	return p, cred, nil
}

// pilot returns connector's pilot id (errNoPilot if none; connector "" matches any).
func (s *scienceDB) pilot(connector, id string) (*Pilot, error) {
	if !validPilotID(id) {
		return nil, errNoPilot
	}
	ctx, cancel := pilotCtx()
	defer cancel()
	p, err := scanPilot(s.db.QueryRowContext(ctx, `SELECT `+pilotCols+` FROM pilots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && connector != "" && p.Connector != connector) {
		return nil, errNoPilot
	}
	if err != nil {
		return nil, s.fail("pilot read", connector, err)
	}
	return p, nil
}

// pilotByCredential returns the pilot whose credential has hash h.
func (s *scienceDB) pilotByCredential(h string) (*Pilot, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	p, err := scanPilot(s.db.QueryRowContext(ctx, `SELECT `+pilotCols+` FROM pilots WHERE cred_sha256 = ?`, h))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoPilot
	}
	if err != nil {
		return nil, s.fail("pilot credential", "", err)
	}
	return p, nil
}

// pilots lists connector's pilots, newest first, in state (or all), at most limit.
func (s *scienceDB) pilots(connector, state string, limit int) ([]*Pilot, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	q, args := `SELECT `+pilotCols+` FROM pilots WHERE connector = ?`, []any{connector}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, s.fail("pilot list", connector, err)
	}
	defer rows.Close()
	out := []*Pilot{}
	for rows.Next() {
		p, err := scanPilot(rows)
		if err != nil {
			return nil, s.fail("pilot list", connector, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("pilot list", connector, err)
	}
	return out, nil
}

// pilotCounts counts connector's pilots that have not ended by tier ("" for one created without a
// tier) and state, and those created in the hour before now.
func (s *scienceDB) pilotCounts(connector string, now time.Time) (map[string]map[string]int, int, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT tier, state, COUNT(*) FROM pilots WHERE connector = ? AND state != 'ended'
		GROUP BY tier, state`, connector)
	if err != nil {
		return nil, 0, s.fail("pilot counts", connector, err)
	}
	defer rows.Close()
	by := map[string]map[string]int{}
	for rows.Next() {
		var tier, st string
		var n int
		if err := rows.Scan(&tier, &st, &n); err != nil {
			return nil, 0, s.fail("pilot counts", connector, err)
		}
		if by[tier] == nil {
			by[tier] = map[string]int{}
		}
		by[tier][st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, s.fail("pilot counts", connector, err)
	}
	var recent int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilots WHERE connector = ? AND created_at > ?`, connector,
		ptime(now.Add(-time.Hour))).Scan(&recent); err != nil {
		return nil, 0, s.fail("pilot counts", connector, err)
	}
	return by, recent, nil
}

// bindPilot records which machine runs a created pilot and the account that serves through it.
// Binding again with the same user, host and team returns the record unchanged; anything else on a
// bound pilot, or a bind of an ended one, is a conflict.
func (s *scienceDB) bindPilot(connector, id, user, host, team, account string, now time.Time) (*Pilot, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE pilots SET state = 'bound', ext_user = ?, ext_host = ?, ext_team = ?, account = ?,
		bound_at = ? WHERE id = ? AND connector = ? AND state = 'created' AND deadline > ?`,
		user, host, team, account, ptime(now), id, connector, ptime(now))
	if err != nil {
		return nil, s.fail("pilot bind", connector, err)
	}
	p, err := s.pilot(connector, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return p, nil
	}
	switch {
	case p.State == PilotEnded:
		return nil, terr(http.StatusConflict, "pilot %s has ended", id)
	case p.State == PilotCreated: // its deadline passed (the sweep ends it)
		return nil, terr(http.StatusConflict, "pilot %s is past its deadline", id)
	case p.User != user || p.Host != host || p.Team != team:
		return nil, terr(http.StatusConflict, "pilot %s is bound to another user, host or team", id)
	}
	return p, nil
}

// pilotConnected marks a bound or disconnected pilot connected (or one still marked connected: the
// caller has no live connection of it); false if it may not connect now (ended, not bound, or past
// its deadline).
func (s *scienceDB) pilotConnected(id, agent string, now time.Time) (bool, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	t := ptime(now)
	res, err := s.db.ExecContext(ctx, `UPDATE pilots SET state = 'connected', connections = connections + 1, connected_at = ?,
		seen_at = ?, first_connected_at = COALESCE(first_connected_at, ?), agent = ?
		WHERE id = ? AND state IN ('bound', 'connected', 'disconnected') AND deadline > ?`, t, t, t, agent, id, t)
	if err != nil {
		return false, s.fail("pilot connect", "", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// pilotDisconnected records the end of a connection that began at since: its serve time, its close
// reason and, for a final reason, the pilot's end. A pilot that ended meanwhile (reported, past its
// deadline) stays ended.
func (s *scienceDB) pilotDisconnected(id, reason string, since, now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	final := pilotFinalReasons[reason]
	t := ptime(now)
	_, err := s.db.ExecContext(ctx, `UPDATE pilots SET served_ms = served_ms + ?, close_reason = ?, disconnected_at = ?, seen_at = ?,
		state = CASE WHEN state = 'ended' OR ? THEN 'ended' ELSE 'disconnected' END,
		ended_at = CASE WHEN state != 'ended' AND ? THEN ? ELSE ended_at END,
		end_reason = CASE WHEN state != 'ended' AND ? THEN ? ELSE end_reason END
		WHERE id = ? AND state IN ('connected', 'ended')`,
		max(now.Sub(since).Milliseconds(), 0), reason, t, t, final, final, t, final, reason, id)
	if err != nil {
		return s.fail("pilot disconnect", "", err)
	}
	return nil
}

// pilotsSeen marks connected pilots as still connected at now.
func (s *scienceDB) pilotsSeen(ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := pilotCtx()
	defer cancel()
	args := []any{ptime(now)}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE pilots SET seen_at = ? WHERE state = 'connected' AND id IN (?`+
		strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return s.fail("pilot seen", "", err)
	}
	return nil
}

// recoverPilots ends the connections a stopped gateway left open in the records: each counts as
// served until it was last seen, and the pilot may connect again.
func (s *scienceDB) recoverPilots() error {
	ctx, cancel := pilotCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+pilotCols+` FROM pilots WHERE state = 'connected'`)
	if err != nil {
		return s.fail("pilot recover", "", err)
	}
	var open []*Pilot
	for rows.Next() {
		p, err := scanPilot(rows)
		if err != nil {
			rows.Close()
			return s.fail("pilot recover", "", err)
		}
		open = append(open, p)
	}
	rows.Close()
	for _, p := range open {
		since, seen := p.Connected, p.seen
		if since == nil {
			since = &p.Created
		}
		if seen == nil || seen.Before(*since) {
			seen = since
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE pilots SET state = 'disconnected', served_ms = served_ms + ?, close_reason = ?,
			disconnected_at = ? WHERE id = ? AND state = 'connected'`, seen.Sub(*since).Milliseconds(), "gateway restart",
			ptime(*seen), p.ID); err != nil {
			return s.fail("pilot recover", "", err)
		}
	}
	return nil
}

// endPilot ends connector's pilot as reported (idempotent: an ended pilot is returned as it is).
func (s *scienceDB) endPilot(connector, id, reason string, now time.Time) (*Pilot, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	t := ptime(now)
	if _, err := s.db.ExecContext(ctx, `UPDATE pilots SET state = 'ended', ended_at = ?, end_reason = ?,
		disconnected_at = CASE WHEN state = 'connected' THEN ? ELSE disconnected_at END
		WHERE id = ? AND connector = ? AND state != 'ended'`, t, reason, t, id, connector); err != nil {
		return nil, s.fail("pilot end", connector, err)
	}
	return s.pilot(connector, id)
}

// expirePilots ends the pilots whose deadline has passed.
func (s *scienceDB) expirePilots(now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	t := ptime(now)
	if _, err := s.db.ExecContext(ctx, `UPDATE pilots SET state = 'ended', ended_at = ?, end_reason = 'deadline',
		disconnected_at = CASE WHEN state = 'connected' THEN ? ELSE disconnected_at END
		WHERE state != 'ended' AND deadline <= ?`, t, t, t); err != nil {
		return s.fail("pilot expire", "", err)
	}
	return nil
}

// pilotUse is one answer (or attempt) a pilot served, as relay.commit metered it.
type pilotUse struct {
	ok         bool // ok, cancelled or preempted: the answer (or part of it) went to the client; else failed
	selfServed bool
	tokens     creditTokens
	credit     float64 // by the formula; 0 when it earns nothing
}

// meterPilot adds one answer to pilot id's totals and its credit, within maxPilot for the pilot and
// maxDay for its account on now's UTC day (0: no cap). The account is the record's (account, the
// connection's, only if the record has none).
func (s *scienceDB) meterPilot(id, account string, u pilotUse, maxPilot, maxDay float64, now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.fail("pilot meter", "", err)
	}
	defer tx.Rollback()
	var have, today float64
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT credit, account FROM pilots WHERE id = ?`, id).Scan(&have, &current); err != nil {
		return s.fail("pilot meter", "", err)
	}
	if current != "" { // the record's account: a claim may have moved it while this answer ran
		account = current
	}
	day := utcDay(now)
	if err := tx.QueryRowContext(ctx, `SELECT credit FROM pilot_credit_days WHERE account = ? AND day = ?`, account, day).Scan(&today); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s.fail("pilot meter", "", err)
	}
	c := u.credit
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		c = 0
	}
	add := c
	if maxPilot > 0 {
		add = min(add, max(0, maxPilot-have))
	}
	if maxDay > 0 {
		add = min(add, max(0, maxDay-today))
	}
	add = math.Round(add*1e6) / 1e6
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pilots SET requests = requests + ?, failed = failed + ?, self_served = self_served + ?,
		prompt_tokens = prompt_tokens + ?, cached_tokens = cached_tokens + ?, completion_tokens = completion_tokens + ?,
		metered = metered + ?, credit = credit + ? WHERE id = ?`,
		b(u.ok), b(!u.ok), b(u.selfServed), u.tokens.Prompt, u.tokens.Cached, u.tokens.Completion, c, add, id); err != nil {
		return s.fail("pilot meter", "", err)
	}
	if add > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pilot_credit_days (account, day, credit) VALUES (?, ?, ?)
			ON CONFLICT (account, day) DO UPDATE SET credit = credit + excluded.credit`, account, day, add); err != nil {
			return s.fail("pilot meter", "", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return s.fail("pilot meter", "", err)
	}
	return nil
}

// withholdPilot withholds pilot id's credit for reason (the first reason stands).
func (s *scienceDB) withholdPilot(id, reason string) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE pilots SET withheld = ? WHERE id = ? AND withheld = ''`, reason, id); err != nil {
		return s.fail("pilot withhold", "", err)
	}
	return nil
}

// pilotCredit is what GET .../pilots/{pid}/credit answers: the final credit, or "not yet".
type pilotCredit struct {
	Pilot    string     `json:"pilot"`
	State    string     `json:"state"`
	Final    bool       `json:"final"`
	FinalAt  *time.Time `json:"final_at"` // when it is (or becomes) final; null while the pilot has not ended
	Credit   *float64   `json:"credit"`   // only once final: 0 when withheld
	Withheld string     `json:"withheld,omitempty"`
}

// creditView is p's credit as of now.
func (p *Pilot) creditView(now time.Time) pilotCredit {
	v := pilotCredit{Pilot: p.ID, State: p.State, Withheld: p.Withheld}
	if p.State != PilotEnded {
		return v
	}
	last := p.Disconn
	if last == nil || (p.Ended != nil && p.First == nil) {
		last = p.Ended
	}
	if last == nil {
		last = &p.Created
	}
	at := last.Add(pilotCreditHold)
	v.FinalAt = &at
	if now.Before(at) {
		return v
	}
	c := p.Credit
	if p.Withheld != "" {
		c = 0
	}
	v.Final, v.Credit = true, &c
	return v
}
