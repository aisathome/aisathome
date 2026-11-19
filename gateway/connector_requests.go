// Connector requests (decision 0020 and its addendum, map section 3): what a signed-in person asks a
// connector to do for their account. The connector (the BOINC bridge, with BOINC's stock web RPCs)
// carries it out and answers; the gateway finishes it. Kinds:
//
//	create_link  "Run on BOINC": the connector creates a user (BOINC: create_account.php) with the
//	             person's display name, a pseudonymous address (or their own) and, if they chose one,
//	             their password's hash, and answers with the user id and its attach-only key; the
//	             account is linked to that user.
//	claim        "Claim a BOINC account": the person pastes their full account key (BOINC's
//	             authenticator: it logs in to the account, unlike the weak key, which BOINC means to be
//	             shared); the connector looks the user up by it (BOINC: am_get_info.php) and answers the
//	             user id and its attach-only key, and the user is linked to the account, merging the
//	             unclaimed account that user's pilots made (claimLink).
//	set_email    no longer made (stock BOINC's am_set_info.php is disabled); a request left from before
//	             is still carried out if the connector can.
//
// States: pending (the connector has not answered), verified (it said yes; the gateway is linking),
// done, and failed (it said no, the link was refused, or it did not answer within
// connectorRequestTTL). The address and password hash leave the record when it stops being pending,
// the attach key when it is done or failed (a link keeps it in the accounts file). The connector sees
// pending requests only, and never which account asked. Finished requests are deleted after
// connectorRequestKeep. The gateway never stores a connector user's address: the person's own one
// passes through a pending request only, and the pseudonymous one is derived (pseudonymousAddress).
//
// A link spans two stores, the accounts file and the science database. The steps run under
// g.linkMu (with pilot binds, so no bind slips between them) in an order that a crash anywhere
// finishes on the next sweep: the verdict and the unclaimed account to merge (verified), then the
// accounts file (claimLink), then the pilot records, their daily credit and the request's end in one
// transaction (completeRequest). While the link moves, that user's connected pilots are dropped and
// reconnect as the new account's.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Request kinds and states.
const (
	reqCreateLink = "create_link"
	reqClaim      = "claim"
	reqSetEmail   = "set_email"

	reqPending  = "pending"
	reqVerified = "verified"
	reqDone     = "done"
	reqFailed   = "failed"
)

const (
	// connectorRequestTTL: a request the connector has not answered by then fails.
	connectorRequestTTL = 24 * time.Hour
	// connectorRequestKeep: finished requests are deleted this long after their end.
	connectorRequestKeep = 30 * 24 * time.Hour
	// connectorRequestsPerDay: requests an account may make in 24 hours.
	connectorRequestsPerDay = 10
	// connectorRequestsOpen: pending requests a connector may have; more wait (people try later).
	connectorRequestsOpen = 200
	// connectorRequestList bounds GET .../requests.
	connectorRequestList = 50
	// connectorRequestSweep: how often expiry and unfinished links are looked at.
	connectorRequestSweep = time.Minute
)

var (
	// claimKeyRe: a connector's attach-only key, "<user>_<secret>" (BOINC's weak account key is
	// "<user id>_<32 hex>"). A user id has no '_' (extIDRe), so the first one splits it.
	claimKeyRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9.:-]{0,63})_([A-Za-z0-9]{16,128})$`)
	reqIDRe    = regexp.MustCompile(`^r-[0-9a-f]{16}$`)

	errRequestOpen  = errors.New("a request is already waiting for an answer: reload this page in a few seconds")
	errRequestLimit = errors.New("too many requests today: try again tomorrow")
	errRequestBusy  = errors.New("too many requests are waiting right now: try again in a few minutes")
)

// connRequest is one request's record.
type connRequest struct {
	ID, Connector, Kind, Account, State string
	User                                string // the connector's user (claim, set_email; create_link once done)
	UserName                            string // create_link: the display name
	Email                               string // create_link, set_email: the address, while pending
	PasswdHash                          string // create_link: the password's hash, while pending ("" = none chosen)
	AttachKey                           string // claim: the account key to check, then the answer's attach key; create_link: the answer's; until done
	OwnEmail                            bool
	From                                string // the unclaimed account merged (verified, done)
	Error                               string
	Created, Updated                    time.Time
}

const reqCols = `id, connector, kind, account, state, ext_user, user_name, email, passwd_hash, attach_key, own_email,
	from_account, error, created_at, updated_at`

func scanRequest(r rowScanner) (*connRequest, error) {
	var q connRequest
	var created, updated string
	if err := r.Scan(&q.ID, &q.Connector, &q.Kind, &q.Account, &q.State, &q.User, &q.UserName, &q.Email, &q.PasswdHash,
		&q.AttachKey, &q.OwnEmail, &q.From, &q.Error, &created, &updated); err != nil {
		return nil, err
	}
	if t := parsePTime(sql.NullString{String: created, Valid: true}); t != nil {
		q.Created = *t
	}
	if t := parsePTime(sql.NullString{String: updated, Valid: true}); t != nil {
		q.Updated = *t
	}
	return &q, nil
}

var errNoRequest = &taskErr{code: http.StatusNotFound, msg: "no such request"}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "r-" + hex.EncodeToString(b)
}

// addRequest stores a new pending request unless its account has one open (pending or verified)
// for the same connector, made connectorRequestsPerDay in the last day, or the connector has
// connectorRequestsOpen pending.
func (s *scienceDB) addRequest(q *connRequest, now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.fail("request add", q.Connector, err)
	}
	defer tx.Rollback()
	var open, today, waiting int
	if err := tx.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN connector = ? AND state IN ('pending', 'verified') THEN 1 END),
		COUNT(CASE WHEN created_at > ? THEN 1 END) FROM connector_requests WHERE account = ?`,
		q.Connector, ptime(now.Add(-24*time.Hour)), q.Account).Scan(&open, &today); err != nil {
		return s.fail("request add", q.Connector, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connector_requests WHERE connector = ? AND state = 'pending'`,
		q.Connector).Scan(&waiting); err != nil {
		return s.fail("request add", q.Connector, err)
	}
	switch {
	case open > 0:
		return errRequestOpen
	case today >= connectorRequestsPerDay:
		return errRequestLimit
	case waiting >= connectorRequestsOpen:
		return errRequestBusy
	}
	q.ID, q.State, q.Created, q.Updated = newRequestID(), reqPending, now, now
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_requests (id, connector, kind, account, state, ext_user, user_name,
		email, passwd_hash, attach_key, own_email, created_at, updated_at) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.Connector, q.Kind, q.Account, q.User, q.UserName, q.Email, q.PasswdHash, q.AttachKey, q.OwnEmail,
		ptime(now), ptime(now)); err != nil {
		return s.fail("request add", q.Connector, err)
	}
	if err := tx.Commit(); err != nil {
		return s.fail("request add", q.Connector, err)
	}
	return nil
}

// request returns connector's request id (connector "" matches any).
func (s *scienceDB) request(connector, id string) (*connRequest, error) {
	if !reqIDRe.MatchString(id) {
		return nil, errNoRequest
	}
	ctx, cancel := pilotCtx()
	defer cancel()
	q, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+reqCols+` FROM connector_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && connector != "" && q.Connector != connector) {
		return nil, errNoRequest
	}
	if err != nil {
		return nil, s.fail("request read", connector, err)
	}
	return q, nil
}

// requests lists requests: connector's in state (oldest first), or account's (newest first; connector
// and state ""), at most limit.
func (s *scienceDB) requests(connector, state, account string, limit int) ([]*connRequest, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	q, args := `SELECT `+reqCols+` FROM connector_requests WHERE connector = ? AND state = ? ORDER BY created_at, id LIMIT ?`,
		[]any{connector, state, limit}
	if account != "" {
		q, args = `SELECT `+reqCols+` FROM connector_requests WHERE account = ? ORDER BY created_at DESC, id LIMIT ?`,
			[]any{account, limit}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, s.fail("request list", connector, err)
	}
	defer rows.Close()
	var out []*connRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, s.fail("request list", connector, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("request list", connector, err)
	}
	return out, nil
}

// verifyRequest records the connector's yes on a pending request: its user, the key it answered
// (create_link; "" keeps the stored one), and the unclaimed account to merge. The address and
// password hash go. False if the request is not pending.
func (s *scienceDB) verifyRequest(id, user, key, from string, now time.Time) (bool, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE connector_requests SET state = 'verified', ext_user = ?,
		attach_key = CASE WHEN ? != '' THEN ? ELSE attach_key END, from_account = ?, email = '', passwd_hash = '', updated_at = ?
		WHERE id = ? AND state = 'pending'`, user, key, key, from, ptime(now), id)
	if err != nil {
		return false, s.fail("request verify", "", err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		s.dropWAL()
	}
	return n == 1, nil
}

// failRequest fails a request in one of states with msg (shown to the person), blanking the address,
// password hash and key. False if it was in none of them.
func (s *scienceDB) failRequest(id, msg string, now time.Time, states ...string) (bool, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	in := "'" + strings.Join(states, "', '") + "'" // the constants above, never input
	res, err := s.db.ExecContext(ctx, `UPDATE connector_requests SET state = 'failed', error = ?, email = '', passwd_hash = '',
		attach_key = '', updated_at = ? WHERE id = ? AND state IN (`+in+`)`, msg, ptime(now), id)
	if err != nil {
		return false, s.fail("request fail", "", err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		s.dropWAL()
	}
	return n == 1, nil
}

// setRequestFrom records the unclaimed account a verified request merged.
func (s *scienceDB) setRequestFrom(id, from string) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE connector_requests SET from_account = ? WHERE id = ? AND state = 'verified'`, from, id); err != nil {
		return s.fail("request from", "", err)
	}
	return nil
}

// dropWAL checkpoints the WAL and truncates it after a write that blanked a request's address, hash
// or key: secure_delete overwrites the database file's pages, but older copies of them stay in the WAL
// until it is truncated.
func (s *scienceDB) dropWAL() {
	if s.path == "" {
		return
	}
	ctx, cancel := pilotCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil && s.log != nil {
		s.log.Warn("science database: WAL not truncated after blanking a request", "err", err)
	}
}

// completeRequest ends a verified request: the pilot records and daily credit of the merged account
// from (if any) move to to, and the request is done (its key blanked), in one transaction.
func (s *scienceDB) completeRequest(id, from, to string, now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.fail("request complete", "", err)
	}
	defer tx.Rollback()
	if from != "" && to != "" && from != to {
		for _, q := range []string{
			`UPDATE pilots SET account = ? WHERE account = ?`,
			`INSERT INTO pilot_credit_days (account, day, credit) SELECT ?, day, credit FROM pilot_credit_days WHERE account = ?
				ON CONFLICT (account, day) DO UPDATE SET credit = credit + excluded.credit`,
		} {
			if _, err := tx.ExecContext(ctx, q, to, from); err != nil {
				return s.fail("request complete", "", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pilot_credit_days WHERE account = ?`, from); err != nil {
			return s.fail("request complete", "", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connector_requests SET state = 'done', attach_key = '', from_account = ?, updated_at = ?
		WHERE id = ? AND state = 'verified'`, from, ptime(now), id); err != nil {
		return s.fail("request complete", "", err)
	}
	if err := tx.Commit(); err != nil {
		return s.fail("request complete", "", err)
	}
	s.dropWAL()
	return nil
}

// expireRequests fails the pending requests older than connectorRequestTTL and deletes the finished
// ones older than connectorRequestKeep.
func (s *scienceDB) expireRequests(now time.Time) error {
	ctx, cancel := pilotCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `UPDATE connector_requests SET state = 'failed', error = ?, email = '', passwd_hash = '',
		attach_key = '', updated_at = ? WHERE state = 'pending' AND created_at <= ?`,
		"no answer in time; please try again", ptime(now), ptime(now.Add(-connectorRequestTTL)))
	if err != nil {
		return s.fail("request expire", "", err)
	}
	expired, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM connector_requests WHERE state IN ('done', 'failed') AND updated_at <= ?`,
		ptime(now.Add(-connectorRequestKeep))); err != nil {
		return s.fail("request expire", "", err)
	}
	if expired > 0 {
		s.dropWAL()
	}
	return nil
}

// verifiedRequests lists the requests whose link has not finished (a crash or a write error).
func (s *scienceDB) verifiedRequests() ([]*connRequest, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+reqCols+` FROM connector_requests WHERE state = 'verified' ORDER BY updated_at LIMIT 100`)
	if err != nil {
		return nil, s.fail("request list", "", err)
	}
	defer rows.Close()
	var out []*connRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, s.fail("request list", "", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// accountPilots lists account's pilots, newest first, at most limit.
func (s *scienceDB) accountPilots(account string, limit int) ([]*Pilot, error) {
	ctx, cancel := pilotCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+pilotCols+` FROM pilots WHERE account = ? ORDER BY created_at DESC, id LIMIT ?`, account, limit)
	if err != nil {
		return nil, s.fail("pilot list", "", err)
	}
	defer rows.Close()
	var out []*Pilot
	for rows.Next() {
		p, err := scanPilot(rows)
		if err != nil {
			return nil, s.fail("pilot list", "", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- the connector's side

// requestView is a pending request as its connector sees it: what it needs to carry it out, never
// the account that asked.
type requestView struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Created time.Time `json:"created_at"`
	User    string    `json:"user,omitempty"` // set_email: the connector's user
	// AccountKey (claim): the full account key the person gave; the connector finds the user by it.
	AccountKey string `json:"account_key,omitempty"`
	UserName   string `json:"user_name,omitempty"` // create_link: the display name
	Email      string `json:"email,omitempty"`     // create_link, set_email: the address to give the user
	// PasswdHash (create_link): MD5 hex of the person's password followed by the lower-case address
	// (BOINC's passwd_hash), or none: then the connector picks a random password.
	PasswdHash string `json:"passwd_hash,omitempty"`
	OwnEmail   bool   `json:"own_email,omitempty"` // the address is the person's own (else pseudonymous)
}

func (q *connRequest) view() requestView {
	v := requestView{ID: q.ID, Kind: q.Kind, Created: q.Created, Email: q.Email, OwnEmail: q.OwnEmail}
	switch q.Kind {
	case reqCreateLink:
		v.UserName, v.PasswdHash = q.UserName, q.PasswdHash
	case reqClaim:
		v.AccountKey = q.AttachKey
	case reqSetEmail:
		v.User = q.User
	}
	return v
}

func (g *Gateway) handleRequestList(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	qs, err := g.sci.requests(c.ID, reqPending, "", connectorRequestList)
	if err != nil {
		taskError(w, err)
		return
	}
	out := make([]requestView, len(qs))
	for i, q := range qs {
		out[i] = q.view()
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

var (
	emailLikeRe = regexp.MustCompile(`[^\s@]+@[^\s@]+`)
	hexRunRe    = regexp.MustCompile(`[0-9A-Fa-f]{24,}`)
)

// redactRequestError takes out of a connector's error text what the person sent it (address, password
// hash, keys) and anything that looks like an address or a key, so that the failed record keeps none.
func redactRequestError(s string, q *connRequest) string {
	for _, v := range []string{q.Email, q.PasswdHash, q.AttachKey} {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	s = emailLikeRe.ReplaceAllString(s, "[redacted]")
	return hexRunRe.ReplaceAllString(s, "[redacted]")
}

// cleanRequestError keeps a connector's error text short, printable and on one line: the person sees it.
func cleanRequestError(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// handleRequestResult takes a connector's answer to a pending request:
//
//	{"ok": true, "user": "<id>", "key": "<user>_<secret>"}   create_link: the user it made; claim: the user
//	                                                         the account key belongs to; and its attach key
//	{"ok": true}                                             set_email (done)
//	{"ok": false, "error": "<short text>"}                   it could not: the person sees the text
//
// The same answer again is accepted (the request as it is now); a different one after the first is a
// conflict.
func (g *Gateway) handleRequestResult(w http.ResponseWriter, r *http.Request) {
	c := g.connectorFor(w, r)
	if c == nil {
		return
	}
	var body struct {
		OK    *bool  `json:"ok"`
		User  string `json:"user"`
		Key   string `json:"key"`
		Error string `json:"error"`
	}
	if !decodeTaskBody(w, r, maxConnectorBody, &body) {
		return
	}
	if body.OK == nil {
		apiError(w, http.StatusBadRequest, "ok: true or false")
		return
	}
	g.linkMu.Lock() // the request is read and changed in one step: concurrent retries see each other
	defer g.linkMu.Unlock()
	q, err := g.sci.request(c.ID, r.PathValue("rid"))
	if err != nil {
		taskError(w, err)
		return
	}
	now := g.pilotNow()
	if !*body.OK {
		msg := redactRequestError(cleanRequestError(body.Error), q)
		if msg == "" {
			apiError(w, http.StatusBadRequest, "error: say why, in a few words (the person sees it)")
			return
		}
		failed, err := g.sci.failRequest(q.ID, msg, now, reqPending) // a yes already taken stands
		g.answerRequest(w, c, q.ID, err, failed || q.State == reqFailed, "failed")
		return
	}
	user, key := q.User, ""
	switch q.Kind {
	case reqCreateLink, reqClaim:
		m := claimKeyRe.FindStringSubmatch(body.Key)
		if !extIDRe.MatchString(body.User) || m == nil || m[1] != body.User {
			apiError(w, http.StatusBadRequest, "user (the id it made, or the account key's) and key (its attach key, \"<user>_<secret>\") are needed")
			return
		}
		user, key = body.User, body.Key
	default:
		if (body.User != "" && body.User != q.User) || body.Key != "" {
			apiError(w, http.StatusBadRequest, "this request names its user and key already")
			return
		}
	}
	from := ""
	if q.Kind != reqSetEmail {
		if id, st := g.accounts.linkHolder(c.ID, user); st == StatusUnclaimed {
			from = id
		}
	}
	verified, err := g.sci.verifyRequest(q.ID, user, key, from, now)
	if err == nil && verified {
		var v *connRequest
		if v, err = g.sci.request(c.ID, q.ID); err == nil {
			g.finishRequest(v)
		}
	}
	same := q.State == reqVerified || (q.State == reqDone && (q.Kind == reqSetEmail || g.accounts.linkedWithKey(q.Account, c.ID, user, key)))
	g.answerRequest(w, c, q.ID, err, verified || same, "ok")
}

// answerRequest answers a result call with the request as it is now, or a conflict if the request
// had already ended otherwise.
func (g *Gateway) answerRequest(w http.ResponseWriter, c *connector, id string, err error, ok bool, what string) {
	if err != nil {
		taskError(w, err)
		return
	}
	q, err := g.sci.request(c.ID, id)
	if err != nil {
		taskError(w, err)
		return
	}
	if !ok {
		apiError(w, http.StatusConflict, "this request is "+q.State+" already; an answer "+what+" does not change it")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": map[string]any{"id": q.ID, "kind": q.Kind, "state": q.State,
		"error": q.Error}})
}

// finishRequest links a verified request (caller holds g.linkMu). A refused link fails it; a write
// error leaves it verified for the sweep to finish.
func (g *Gateway) finishRequest(q *connRequest) {
	now := g.pilotNow()
	name := q.Connector
	if c := g.connector(q.Connector); c != nil && c.Accounts != nil {
		name = c.Accounts.Name
	}
	switch q.Kind {
	case reqCreateLink, reqClaim:
		link := AccountLink{AttachKey: q.AttachKey, Via: LinkClaimed}
		if q.Kind == reqCreateLink {
			link.Via, link.OwnEmail = LinkCreated, q.OwnEmail
		}
		from, err := g.accounts.claimLink(q.Account, q.Connector, q.User, link, now)
		var refused string
		switch {
		case errors.Is(err, errLinkTaken):
			refused = "this " + name + " account is linked to another AIs@home account"
		case errors.Is(err, errClaimInactive):
			refused = "a closed account cannot link a " + name + " account"
		case err != nil:
			g.log.Error("connector request: accounts file not written; the sweep retries", "request", q.ID, "err", err)
			return
		}
		if refused != "" {
			if _, err := g.sci.failRequest(q.ID, refused, now, reqVerified); err != nil {
				g.log.Error("connector request not failed", "request", q.ID, "err", err)
			}
			g.log.Info("connector request refused", "connector", q.Connector, "request", q.ID, "kind", q.Kind)
			return
		}
		if from == "" {
			from = q.From // merged before a crash: the records still follow
		} else if from != q.From {
			// Kept before the records move, so a retry after a failed move still knows it.
			if err := g.sci.setRequestFrom(q.ID, from); err != nil {
				g.log.Error("connector request: merged account not recorded; the sweep retries", "request", q.ID, "err", err)
			}
		}
		if err := g.sci.completeRequest(q.ID, from, q.Account, now); err != nil {
			g.log.Error("connector request: pilot records not moved; the sweep retries", "request", q.ID, "err", err)
			return
		}
		g.log.Info("connector user linked", "connector", q.Connector, "request", q.ID, "kind", q.Kind, "merged", from != "")
		if from != "" {
			g.sweepNodes() // the merged account's pilots go now and reconnect as the account's
		}
	case reqSetEmail:
		if err := g.accounts.setLinkOwnEmail(q.Account, q.Connector, q.User); err != nil {
			g.log.Error("connector request: accounts file not written; the sweep retries", "request", q.ID, "err", err)
			return
		}
		if err := g.sci.completeRequest(q.ID, "", "", now); err != nil {
			g.log.Error("connector request not completed; the sweep retries", "request", q.ID, "err", err)
		}
	}
}

// sweepRequests fails requests nobody answered, deletes old ones and finishes interrupted links.
func (g *Gateway) sweepRequests() {
	if g.sci == nil || g.accounts == nil {
		return
	}
	g.linkMu.Lock()
	defer g.linkMu.Unlock()
	if err := g.sci.expireRequests(g.pilotNow()); err != nil {
		return
	}
	qs, err := g.sci.verifiedRequests()
	if err != nil {
		return
	}
	for _, q := range qs {
		g.finishRequest(q)
	}
}

// pseudonymousAddress is the address a connector user made for account gets unless the person gives
// their own: "b" and 20 hex digits of a keyed hash of the connector and account, at the connector's
// address domain. It is derived, never stored, so the account page can show it as the login without
// the gateway keeping an address, and the same account always gets the same one (the connector can
// recognise a retried creation). Changing session_secret changes it on the page, not at the connector.
func (g *Gateway) pseudonymousAddress(c *connector, account string) string {
	m := hmac.New(sha256.New, []byte(g.acfg.SessionSecret))
	m.Write([]byte("connector-address|" + c.ID + "|" + account))
	return "b" + hex.EncodeToString(m.Sum(nil)[:10]) + "@" + c.Accounts.AddressDomain
}
