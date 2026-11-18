// Campaigns and external results (decision 0021, point 1): a project's public record of what it ran
// and what came of it outside the network, kept in the science database (science_db.go).
//
// A campaign is a named round of a project's work (prove2me calls them trials): an id ("t8"), a
// title, a status (preparing, running, finished), when it started and ended, a few public labels,
// an optional short text, and its items (the things it works on: id, title, url, role, labels). An
// external result is what an outside service said about a piece of the work (prove2me's verdict on a
// submitted proof): its source ("prove2me"), the service's own id for it (external_id), the campaign,
// item and task it belongs to, a link, a state (pending, accepted, rejected, error), the service's
// verdict text and when it was submitted and judged. The gateway knows nothing of what a campaign or
// a result means for a project (decision 0019): that stays in labels and in the project's own code.
//
// Write, with the project's key (campaigns_http.go):
//
//	PUT    /v1/projects/{id}/campaigns/{campaign}   create or replace (idempotent)
//	PATCH  /v1/projects/{id}/campaigns/{campaign}   change some fields (finish: {"status": "finished"})
//	DELETE /v1/projects/{id}/campaigns/{campaign}   remove one no result refers to
//	POST   /v1/projects/{id}/results                create or update, idempotent by (source, external_id)
//	DELETE /v1/projects/{id}/results/{result}       remove one
//
// Read, without a key for a project with public_queue (else with its key):
//
//	GET /v1/projects/{id}/campaigns                 newest first, with items and result counts
//	GET /v1/projects/{id}/campaigns/{campaign}      one
//	GET /v1/projects/{id}/results?campaign=&item=&state=&source=
//
// Everything a read shows is public by construction: ids and labels match strict patterns, text is
// checked for key-shaped strings and e-mail addresses, and links must start with one of the
// project's public_links. A result's external_id (a service's own id, which may let others query the
// service about it) is never shown; its public id is "r-" and 16 hex characters of a hash of source
// and external_id. Never in it: prompts, workers, node ids, operators, accounts or keys.
//
// Status and times: a campaign that becomes running gets started_at now unless it has one; one that
// becomes finished gets ended_at now unless it has one; one that is not finished has no ended_at. A
// result that is not pending gets verdict_at now unless it has one; a pending one has none. A result
// with a verdict never goes back to pending: a late "pending" write (a retry that arrived after the
// verdict) changes nothing.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxCampaignBody  = 128 << 10
	maxResultBody    = 8 << 10
	maxCampaignItems = 200
	maxCampaignTitle = 200
	maxCampaignText  = 4096
	maxItemTitle     = 300
	maxResultVerdict = 1000
	maxPublicURL     = 500
	maxProjectLinks  = 8
	// maxClockSkew: how far ahead of the gateway's clock a writer's submitted_at may be.
	maxClockSkew         = 5 * time.Minute
	campaignsListDefault = 50
	campaignsListMax     = 200
	resultsListDefault   = 100
	resultsListMax       = 500
)

// The records a project may keep (variables for the tests).
var (
	maxCampaignsPerProj = 1000
	maxResultsPerProj   = 50000
)

// Campaign states and result states.
const (
	CampaignPreparing = "preparing"
	CampaignRunning   = "running"
	CampaignFinished  = "finished"

	ResultPending  = "pending"
	ResultAccepted = "accepted"
	ResultRejected = "rejected"
	ResultError    = "error"
)

var (
	campaignStates = []string{CampaignPreparing, CampaignRunning, CampaignFinished}
	resultStates   = []string{ResultPending, ResultAccepted, ResultRejected, ResultError}
	// recordIDRe: a campaign's or an item's id ("t8", "p2m-3ad60f").
	recordIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// externalIDRe: an outside service's id for a result: visible ASCII, no spaces.
	externalIDRe = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
	// resultIDRe: a result's public id.
	resultIDRe = regexp.MustCompile(`^r-[0-9a-f]{16}$`)
)

// errScience: the science database failed; the call may be repeated.
var errScience = &taskErr{code: http.StatusServiceUnavailable, msg: "science records unavailable, try again"}

// CampaignItem is one thing a campaign works on.
type CampaignItem struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	URL    string            `json:"url"`
	Role   string            `json:"role"`
	Labels map[string]string `json:"labels"`
}

// Campaign is a campaign as stored and as the public reads show it.
type Campaign struct {
	Project   string            `json:"project"`
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Status    string            `json:"status"`
	StartedAt *time.Time        `json:"started_at"`
	EndedAt   *time.Time        `json:"ended_at"`
	Labels    map[string]string `json:"labels"`
	Text      string            `json:"text"`
	Items     []CampaignItem    `json:"items"`
	// Results: the campaign's external results per state (every state, 0 when none).
	Results   map[string]int `json:"results"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// campaignIn is the body of a PUT: the whole campaign. Absent fields are empty.
type campaignIn struct {
	Title     string            `json:"title"`
	Status    string            `json:"status"`
	StartedAt *time.Time        `json:"started_at"`
	EndedAt   *time.Time        `json:"ended_at"`
	Labels    map[string]string `json:"labels"`
	Text      string            `json:"text"`
	Items     []CampaignItem    `json:"items"`
}

// campaignPatch is the body of a PATCH: absent (or null) fields stay as they are.
type campaignPatch struct {
	Title     *string            `json:"title"`
	Status    *string            `json:"status"`
	StartedAt *time.Time         `json:"started_at"`
	EndedAt   *time.Time         `json:"ended_at"`
	Labels    *map[string]string `json:"labels"`
	Text      *string            `json:"text"`
	Items     *[]CampaignItem    `json:"items"`
}

// Result is an external result as the public reads show it.
type Result struct {
	Project     string            `json:"project"`
	ID          string            `json:"id"`
	Source      string            `json:"source"`
	Campaign    *string           `json:"campaign"`
	Item        *string           `json:"item"`
	Task        *string           `json:"task"`
	URL         *string           `json:"url"`
	State       string            `json:"state"`
	Verdict     string            `json:"verdict"`
	Labels      map[string]string `json:"labels"`
	SubmittedAt time.Time         `json:"submitted_at"`
	VerdictAt   *time.Time        `json:"verdict_at"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// resultIn is the body of a result's POST.
type resultIn struct {
	Source      string            `json:"source"`
	ExternalID  string            `json:"external_id"`
	Campaign    string            `json:"campaign"`
	Item        string            `json:"item"`
	Task        string            `json:"task"`
	URL         string            `json:"url"`
	State       string            `json:"state"`
	Verdict     string            `json:"verdict"`
	Labels      map[string]string `json:"labels"`
	SubmittedAt *time.Time        `json:"submitted_at"`
	VerdictAt   *time.Time        `json:"verdict_at"`
}

// publicRef: a public id or reference (a campaign's, an item's, a task's) that holds nothing
// key-shaped: the id patterns alone would take a whole API key.
func publicRef(re *regexp.Regexp, s string) bool {
	return re.MatchString(s) && !publicSecret.MatchString(s)
}

// resultID is a result's public id.
func resultID(source, externalID string) string {
	h := sha256.Sum256([]byte(source + "\x00" + externalID))
	return "r-" + hex.EncodeToString(h[:8])
}

// checkPublicText accepts public free text of at most maxLen bytes: valid UTF-8, no control
// characters but line breaks and tabs, nothing key-shaped and no e-mail address.
func checkPublicText(field, s string, maxLen int) error {
	switch {
	case len(s) > maxLen:
		return fmt.Errorf("%s: at most %d bytes", field, maxLen)
	case !utf8.ValidString(s):
		return fmt.Errorf("%s: not UTF-8", field)
	case strings.ContainsFunc(s, func(r rune) bool { return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f }):
		return fmt.Errorf("%s: control characters", field)
	case publicSecret.MatchString(s) || publicEmail.MatchString(s):
		return fmt.Errorf("%s: looks like a key or an e-mail address; it is public", field)
	}
	return nil
}

// checkPublicLink accepts "" or an https link under one of the project's public_links prefixes.
func checkPublicLink(field, s string, prefixes []string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	// No dot segments, no escapes ('%', '\\') and no spaces (a browser trims them): the link a browser
	// resolves is the one checked.
	ok := err == nil && len(s) <= maxPublicURL && u.Scheme == "https" && u.User == nil && u.RawQuery == "" &&
		u.Fragment == "" && !strings.ContainsAny(s, "#?%\\") && !strings.ContainsFunc(s, unicode.IsSpace) &&
		utf8Printable(s) && !dotSegment(u.Path) &&
		!publicSecret.MatchString(s) && !publicEmail.MatchString(s)
	if ok {
		ok = slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
	}
	if !ok {
		if len(prefixes) == 0 {
			return fmt.Errorf("%s: this project has no public_links, so it may not link anywhere", field)
		}
		return fmt.Errorf("%s: an https link without query or fragment, at most %d bytes, under %s", field, maxPublicURL, strings.Join(prefixes, " or "))
	}
	return nil
}

// dotSegment: a path with a "." or ".." segment.
func dotSegment(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(seg string) bool { return seg == "." || seg == ".." })
}

// checkPublicLinks checks a project's public_links: https prefixes ending in "/".
func checkPublicLinks(links []string) error {
	if len(links) > maxProjectLinks {
		return fmt.Errorf("public_links: at most %d", maxProjectLinks)
	}
	for _, l := range links {
		u, err := url.Parse(l)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			strings.ContainsAny(l, "?#%\\") || strings.ContainsFunc(l, unicode.IsSpace) || dotSegment(u.Path) || !strings.HasSuffix(l, "/") || len(l) > 200 {
			return fmt.Errorf("public_links: %q: an https prefix ending in /, such as https://example.org/items/", l)
		}
	}
	return nil
}

// checkTime accepts a time the records keep (seconds, UTC): from 2000 to 2100.
func checkTime(field string, t *time.Time) error {
	if t != nil && (t.Year() < 2000 || t.Year() > 2100) {
		return fmt.Errorf("%s: a time from 2000 to 2100 (RFC 3339)", field)
	}
	return nil
}

func normTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC().Truncate(time.Second)
	return &u
}

// validate checks a campaign as it would be stored; links are the project's public_links.
func (c *Campaign) validate(links []string) error {
	errs := []error{checkPublicText("title", c.Title, maxCampaignTitle), checkPublicText("text", c.Text, maxCampaignText),
		checkTime("started_at", c.StartedAt), checkTime("ended_at", c.EndedAt), checkLabels(c.Labels)}
	if !slices.Contains(campaignStates, c.Status) {
		errs = append(errs, fmt.Errorf("status: %s", strings.Join(campaignStates, ", ")))
	}
	if c.EndedAt != nil && c.Status != CampaignFinished {
		errs = append(errs, errors.New("ended_at: only a finished campaign has one"))
	}
	if c.StartedAt != nil && c.EndedAt != nil && c.EndedAt.Before(*c.StartedAt) {
		errs = append(errs, errors.New("ended_at: before started_at"))
	}
	if len(c.Items) > maxCampaignItems {
		errs = append(errs, fmt.Errorf("items: at most %d", maxCampaignItems))
	} else {
		seen := map[string]bool{}
		for i, it := range c.Items {
			f := fmt.Sprintf("items[%d]", i)
			if !publicRef(recordIDRe, it.ID) {
				errs = append(errs, fmt.Errorf("%s.id: 1-64 letters, digits, '.', '_' or '-'", f))
			} else if seen[it.ID] {
				errs = append(errs, fmt.Errorf("%s.id: %s twice", f, it.ID))
			}
			seen[it.ID] = true
			errs = append(errs, checkPublicText(f+".title", it.Title, maxItemTitle), checkPublicLink(f+".url", it.URL, links))
			if it.Role != "" {
				errs = append(errs, checkKind(f+".role", it.Role))
			}
			if err := checkLabels(it.Labels); err != nil {
				errs = append(errs, fmt.Errorf("%s.%w", f, err))
			}
		}
	}
	return errors.Join(errs...)
}

// normalize applies the status rules (see the top of the file) against the stored campaign old (nil:
// none), at now.
func (c *Campaign) normalize(old *Campaign, now time.Time) {
	c.StartedAt, c.EndedAt = normTime(c.StartedAt), normTime(c.EndedAt)
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	if c.Items == nil {
		c.Items = []CampaignItem{}
	}
	for i := range c.Items {
		if c.Items[i].Labels == nil {
			c.Items[i].Labels = map[string]string{}
		}
	}
	now = now.UTC().Truncate(time.Second)
	if c.StartedAt == nil && (c.Status == CampaignRunning || c.Status == CampaignFinished) {
		if old != nil && old.StartedAt != nil {
			c.StartedAt = old.StartedAt
		} else if c.Status == CampaignRunning {
			c.StartedAt = &now
		}
	}
	if c.Status == CampaignFinished && c.EndedAt == nil {
		if old != nil && old.EndedAt != nil && old.Status == CampaignFinished {
			c.EndedAt = old.EndedAt
		} else {
			c.EndedAt = &now
		}
	}
}

// sameCampaign: a and b store the same content.
func sameCampaign(a, b *Campaign) bool {
	return a.Title == b.Title && a.Status == b.Status && sameTime(a.StartedAt, b.StartedAt) && sameTime(a.EndedAt, b.EndedAt) &&
		maps.Equal(a.Labels, b.Labels) && a.Text == b.Text && slices.EqualFunc(a.Items, b.Items, func(x, y CampaignItem) bool {
		return x.ID == y.ID && x.Title == y.Title && x.URL == y.URL && x.Role == y.Role && maps.Equal(x.Labels, y.Labels)
	})
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// Stored times: RFC 3339, UTC, seconds.
func fmtTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t.UTC()
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTime(s.String)
	return &t
}

func labelsJSON(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// readLabels reads stored labels, keeping only those that pass today's checks.
func readLabels(s string) map[string]string {
	var m map[string]string
	json.Unmarshal([]byte(s), &m)
	out := map[string]string{}
	for k, v := range m {
		if checkLabels(map[string]string{k: v}) == nil {
			out[k] = v
		}
	}
	return out
}

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const campaignCols = `id, title, status, started_at, ended_at, labels, text, created_at, updated_at`

func scanCampaign(project string, sc interface{ Scan(...any) error }) (*Campaign, error) {
	var c Campaign
	var started, ended sql.NullString
	var labels, created, updated string
	if err := sc.Scan(&c.ID, &c.Title, &c.Status, &started, &ended, &labels, &c.Text, &created, &updated); err != nil {
		return nil, err
	}
	c.Project, c.StartedAt, c.EndedAt, c.Labels = project, parseTimePtr(started), parseTimePtr(ended), readLabels(labels)
	c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
	c.Items = []CampaignItem{}
	return &c, nil
}

// fillCampaigns adds the items and result counts of the given campaigns of a project.
func fillCampaigns(ctx context.Context, q queryer, project string, cs []*Campaign) error {
	if len(cs) == 0 {
		return nil
	}
	by := map[string]*Campaign{}
	ids := make([]any, 0, len(cs)+1)
	ids = append(ids, project)
	for _, c := range cs {
		by[c.ID] = c
		c.Results = map[string]int{}
		for _, s := range resultStates {
			c.Results[s] = 0
		}
		ids = append(ids, c.ID)
	}
	in := strings.Repeat(",?", len(cs))[1:]
	rows, err := q.QueryContext(ctx, `SELECT campaign, id, title, url, role, labels FROM campaign_items
		WHERE project = ? AND campaign IN (`+in+`) ORDER BY campaign, pos`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var camp, labels string
		var it CampaignItem
		if err := rows.Scan(&camp, &it.ID, &it.Title, &it.URL, &it.Role, &labels); err != nil {
			rows.Close()
			return err
		}
		it.Labels = readLabels(labels)
		if c := by[camp]; c != nil {
			c.Items = append(c.Items, it)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT campaign, state, COUNT(*) FROM results
		WHERE project = ? AND campaign IN (`+in+`) GROUP BY campaign, state`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var camp, state string
		var n int
		if err := rows.Scan(&camp, &state, &n); err != nil {
			return err
		}
		if c := by[camp]; c != nil {
			c.Results[state] = n
		}
	}
	return rows.Err()
}

// getCampaign reads one campaign with its items and counts; nil when there is none.
func getCampaign(ctx context.Context, q queryer, project, id string) (*Campaign, error) {
	c, err := scanCampaign(project, q.QueryRowContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE project = ? AND id = ?`, project, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, fillCampaigns(ctx, q, project, []*Campaign{c})
}

func scienceCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// campaign returns one campaign of a project, or nil.
func (s *scienceDB) campaign(project, id string) (*Campaign, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	c, err := getCampaign(ctx, s.db, project, id)
	if err != nil {
		return nil, errScience
	}
	return c, nil
}

// campaignCursor names a campaign's place in the list: "<started_at or created_at>~<id>".
func campaignCursor(c *Campaign) string {
	at := c.CreatedAt
	if c.StartedAt != nil {
		at = *c.StartedAt
	}
	return fmtTime(at) + "~" + c.ID
}

func splitCursor(cur string, idRe *regexp.Regexp) (string, string, bool) {
	at, id, ok := strings.Cut(cur, "~")
	if !ok || !idRe.MatchString(id) {
		return "", "", false
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil || fmtTime(parseTime(at)) != at {
		return "", "", false
	}
	return at, id, true
}

// campaigns lists a project's campaigns, newest first (by started_at, else created_at, then id),
// at most limit after the cursor; next names the last one when more follow.
func (s *scienceDB) campaigns(project string, limit int, after string) ([]*Campaign, *string, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	q := `SELECT ` + campaignCols + ` FROM campaigns WHERE project = ?`
	args := []any{project}
	if after != "" {
		at, id, _ := splitCursor(after, recordIDRe)
		q += ` AND (COALESCE(started_at, created_at) < ? OR (COALESCE(started_at, created_at) = ? AND id < ?))`
		args = append(args, at, at, id)
	}
	q += ` ORDER BY COALESCE(started_at, created_at) DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, errScience
	}
	var out []*Campaign
	for rows.Next() {
		c, err := scanCampaign(project, rows)
		if err != nil {
			rows.Close()
			return nil, nil, errScience
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, errScience
	}
	var next *string
	if len(out) > limit {
		out = out[:limit]
		cur := campaignCursor(out[limit-1])
		next = &cur
	}
	if err := fillCampaigns(ctx, s.db, project, out); err != nil {
		return nil, nil, errScience
	}
	if out == nil {
		out = []*Campaign{}
	}
	return out, next, nil
}

// putCampaign stores c (validated and normalised by the caller against old) for a project. It
// reports whether it was new; an unchanged campaign is not written.
func (s *scienceDB) putCampaign(c *Campaign, links []string, now time.Time, patch *campaignPatch) (*Campaign, bool, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, errScience
	}
	defer tx.Rollback()
	old, err := getCampaign(ctx, tx, c.Project, c.ID)
	if err != nil {
		return nil, false, errScience
	}
	if patch != nil {
		if old == nil {
			return nil, false, &taskErr{code: http.StatusNotFound, msg: "no such campaign"}
		}
		c = old.patched(patch)
	}
	c.normalize(old, now)
	if err := c.validate(links); err != nil {
		return nil, false, &taskErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	if old != nil && sameCampaign(c, old) {
		return old, false, nil
	}
	if old == nil {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM campaigns WHERE project = ?`, c.Project).Scan(&n); err != nil {
			return nil, false, errScience
		}
		if n >= maxCampaignsPerProj {
			return nil, false, &taskErr{code: http.StatusConflict, msg: fmt.Sprintf("at most %d campaigns per project", maxCampaignsPerProj)}
		}
		c.CreatedAt = now.UTC().Truncate(time.Second)
	} else {
		c.CreatedAt = old.CreatedAt
	}
	c.UpdatedAt = now.UTC().Truncate(time.Second)
	_, err = tx.ExecContext(ctx, `INSERT INTO campaigns (project, id, title, status, started_at, ended_at, labels, text, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, id) DO UPDATE SET title = excluded.title, status = excluded.status, started_at = excluded.started_at,
		ended_at = excluded.ended_at, labels = excluded.labels, text = excluded.text, updated_at = excluded.updated_at`,
		c.Project, c.ID, c.Title, c.Status, fmtTimePtr(c.StartedAt), fmtTimePtr(c.EndedAt), labelsJSON(c.Labels), c.Text,
		fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt))
	if err != nil {
		return nil, false, errScience
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM campaign_items WHERE project = ? AND campaign = ?`, c.Project, c.ID); err != nil {
		return nil, false, errScience
	}
	for i, it := range c.Items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO campaign_items (project, campaign, pos, id, title, url, role, labels) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Project, c.ID, i, it.ID, it.Title, it.URL, it.Role, labelsJSON(it.Labels)); err != nil {
			return nil, false, errScience
		}
	}
	stored, err := getCampaign(ctx, tx, c.Project, c.ID)
	if err != nil || stored == nil {
		return nil, false, errScience
	}
	if err := tx.Commit(); err != nil {
		return nil, false, errScience
	}
	return stored, old == nil, nil
}

// patched is c with the patch's fields applied.
func (c *Campaign) patched(p *campaignPatch) *Campaign {
	n := *c
	n.Labels = maps.Clone(c.Labels)
	n.Items = slices.Clone(c.Items)
	if p.Title != nil {
		n.Title = *p.Title
	}
	if p.Status != nil {
		n.Status = *p.Status
		if n.Status != CampaignFinished && p.EndedAt == nil {
			n.EndedAt = nil // reopened
		}
	}
	if p.StartedAt != nil {
		n.StartedAt = p.StartedAt
	}
	if p.EndedAt != nil {
		n.EndedAt = p.EndedAt
	}
	if p.Labels != nil {
		n.Labels = *p.Labels
	}
	if p.Text != nil {
		n.Text = *p.Text
	}
	if p.Items != nil {
		n.Items = *p.Items
	}
	return &n
}

// deleteCampaign removes a campaign and its items; one that results refer to stays (409).
func (s *scienceDB) deleteCampaign(project, id string) error {
	ctx, cancel := scienceCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errScience
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM results WHERE project = ? AND campaign = ?`, project, id).Scan(&n); err != nil {
		return errScience
	}
	if n > 0 {
		return &taskErr{code: http.StatusConflict, msg: fmt.Sprintf("%d results refer to this campaign: delete or move them first", n)}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM campaigns WHERE project = ? AND id = ?`, project, id)
	if err != nil {
		return errScience
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return &taskErr{code: http.StatusNotFound, msg: "no such campaign"}
	}
	if err := tx.Commit(); err != nil {
		return errScience
	}
	return nil
}

// validate checks a result's POST; links are the project's public_links.
func (in *resultIn) validate(links []string) error {
	errs := []error{checkTime("submitted_at", in.SubmittedAt), checkTime("verdict_at", in.VerdictAt),
		checkPublicText("verdict", in.Verdict, maxResultVerdict), checkPublicLink("url", in.URL, links), checkLabels(in.Labels)}
	if err := checkKind("source", in.Source); err != nil {
		errs = append(errs, err)
	}
	if !externalIDRe.MatchString(in.ExternalID) {
		errs = append(errs, errors.New("external_id: 1-200 visible ASCII characters, no spaces"))
	}
	if in.Campaign != "" && !publicRef(recordIDRe, in.Campaign) {
		errs = append(errs, errors.New("campaign: a campaign's id"))
	}
	if in.Item != "" && !publicRef(recordIDRe, in.Item) {
		errs = append(errs, errors.New("item: 1-64 letters, digits, '.', '_' or '-'"))
	}
	if in.Task != "" && !publicRef(pubTaskIDRe, in.Task) {
		errs = append(errs, errors.New("task: a task's id"))
	}
	if !slices.Contains(resultStates, in.State) {
		errs = append(errs, fmt.Errorf("state: %s", strings.Join(resultStates, ", ")))
	}
	if in.SubmittedAt == nil {
		errs = append(errs, errors.New("submitted_at: required (RFC 3339)"))
	}
	if in.State == ResultPending && in.VerdictAt != nil {
		errs = append(errs, errors.New("verdict_at: not for a pending result"))
	}
	if in.SubmittedAt != nil && in.VerdictAt != nil && in.VerdictAt.Before(*in.SubmittedAt) {
		errs = append(errs, errors.New("verdict_at: before submitted_at"))
	}
	return errors.Join(errs...)
}

// storedResult is a result's row.
type storedResult struct {
	Result
	ExternalID string `json:"external_id"`
}

const resultCols = `id, source, external_id, campaign, item, task, url, state, verdict, labels, submitted_at, verdict_at, created_at, updated_at`

func scanResult(project string, sc interface{ Scan(...any) error }) (*storedResult, error) {
	var r storedResult
	var camp, verdictAt sql.NullString
	var item, task, link, labels, submitted, created, updated string
	if err := sc.Scan(&r.ID, &r.Source, &r.ExternalID, &camp, &item, &task, &link, &r.State, &r.Verdict, &labels, &submitted,
		&verdictAt, &created, &updated); err != nil {
		return nil, err
	}
	r.Project = project
	if camp.Valid {
		r.Campaign = &camp.String
	}
	r.Item, r.Task, r.URL = strOrNil(item), strOrNil(task), strOrNil(link)
	r.Labels, r.SubmittedAt, r.VerdictAt = readLabels(labels), parseTime(submitted), parseTimePtr(verdictAt)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return &r, nil
}

// sameResult: the row a holds what in (normalised) would store.
func sameResult(a *storedResult, b *storedResult) bool {
	return ptrEq(a.Campaign, b.Campaign) && ptrEq(a.Item, b.Item) && ptrEq(a.Task, b.Task) && ptrEq(a.URL, b.URL) &&
		a.State == b.State && a.Verdict == b.Verdict && maps.Equal(a.Labels, b.Labels) && a.SubmittedAt.Equal(b.SubmittedAt) &&
		sameTime(a.VerdictAt, b.VerdictAt)
}

func ptrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// putResult creates or updates a project's result by (source, external_id). It reports whether it
// was new. An unchanged result, or a pending write for one that has a verdict, writes nothing.
func (s *scienceDB) putResult(project string, in *resultIn, now time.Time) (*storedResult, bool, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	now = now.UTC().Truncate(time.Second)
	r := &storedResult{Result: Result{Project: project, ID: resultID(in.Source, in.ExternalID), Source: in.Source,
		Campaign: strOrNil(in.Campaign), Item: strOrNil(in.Item), Task: strOrNil(in.Task), URL: strOrNil(in.URL), State: in.State,
		Verdict: in.Verdict, Labels: in.Labels, SubmittedAt: *normTime(in.SubmittedAt), VerdictAt: normTime(in.VerdictAt)},
		ExternalID: in.ExternalID}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	if r.State != ResultPending && r.VerdictAt == nil {
		r.VerdictAt = &now
	}
	if r.SubmittedAt.After(now.Add(maxClockSkew)) {
		return nil, false, &taskErr{code: http.StatusBadRequest, msg: "submitted_at: in the future"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, errScience
	}
	defer tx.Rollback()
	old, err := scanResult(project, tx.QueryRowContext(ctx, `SELECT `+resultCols+` FROM results WHERE project = ? AND source = ? AND external_id = ?`,
		project, in.Source, in.ExternalID))
	if errors.Is(err, sql.ErrNoRows) {
		old, err = nil, nil
	}
	if err != nil {
		return nil, false, errScience
	}
	if old != nil {
		if old.State != ResultPending && r.State == ResultPending {
			return old, false, nil // a late retry of the submission: the verdict stands
		}
		if r.State != ResultPending && in.VerdictAt == nil && old.VerdictAt != nil && old.State == r.State {
			r.VerdictAt = old.VerdictAt // the same verdict again keeps its time
		}
		if sameResult(old, r) {
			return old, false, nil
		}
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM results WHERE project = ?`, project).Scan(&n); err != nil {
			return nil, false, errScience
		}
		if n >= maxResultsPerProj {
			return nil, false, &taskErr{code: http.StatusConflict, msg: fmt.Sprintf("at most %d results per project", maxResultsPerProj)}
		}
	}
	if r.Campaign != nil {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM campaigns WHERE project = ? AND id = ?`, project, *r.Campaign).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, &taskErr{code: http.StatusBadRequest, msg: "campaign: no such campaign in this project (create it first)"}
		}
		if err != nil {
			return nil, false, errScience
		}
	}
	r.CreatedAt, r.UpdatedAt = now, now
	if old != nil {
		r.CreatedAt = old.CreatedAt
		if old.ID != r.ID { // cannot happen: the id is a hash of the key
			return nil, false, errScience
		}
	}
	var camp any
	if r.Campaign != nil {
		camp = *r.Campaign
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO results (project, id, source, external_id, campaign, item, task, url, state, verdict, labels,
		submitted_at, verdict_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, id) DO UPDATE SET campaign = excluded.campaign, item = excluded.item, task = excluded.task,
		url = excluded.url, state = excluded.state, verdict = excluded.verdict, labels = excluded.labels,
		submitted_at = excluded.submitted_at, verdict_at = excluded.verdict_at, updated_at = excluded.updated_at`,
		project, r.ID, r.Source, r.ExternalID, camp, in.Item, in.Task, in.URL, r.State, r.Verdict, labelsJSON(r.Labels),
		fmtTime(r.SubmittedAt), fmtTimePtr(r.VerdictAt), fmtTime(r.CreatedAt), fmtTime(r.UpdatedAt))
	if err != nil {
		return nil, false, errScience
	}
	if err := tx.Commit(); err != nil {
		return nil, false, errScience
	}
	return r, old == nil, nil
}

// deleteResult removes a project's result by its public id.
func (s *scienceDB) deleteResult(project, id string) error {
	ctx, cancel := scienceCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE project = ? AND id = ?`, project, id)
	if err != nil {
		return errScience
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return &taskErr{code: http.StatusNotFound, msg: "no such result"}
	}
	return nil
}

// resultFilter selects a project's results.
type resultFilter struct {
	campaign, item, source string
	states                 []string
}

func (f resultFilter) where(project string) (string, []any) {
	q, args := ` WHERE project = ?`, []any{project}
	if f.campaign != "" {
		q, args = q+` AND campaign = ?`, append(args, f.campaign)
	}
	if f.item != "" {
		q, args = q+` AND item = ?`, append(args, f.item)
	}
	if f.source != "" {
		q, args = q+` AND source = ?`, append(args, f.source)
	}
	return q, args
}

// results lists a project's results, newest submitted first (then id), at most limit after the
// cursor; counts are per state for the filter without its states and the cursor.
func (s *scienceDB) results(project string, f resultFilter, limit int, after string) ([]Result, map[string]int, *string, error) {
	ctx, cancel := scienceCtx()
	defer cancel()
	where, args := f.where(project)
	counts := map[string]int{}
	for _, st := range resultStates {
		counts[st] = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM results`+where+` GROUP BY state`, args...)
	if err != nil {
		return nil, nil, nil, errScience
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, nil, nil, errScience
		}
		counts[st] = n
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, nil, errScience
	}
	q := `SELECT ` + resultCols + ` FROM results` + where
	if len(f.states) > 0 {
		q += ` AND state IN (` + strings.Repeat(",?", len(f.states))[1:] + `)`
		for _, st := range f.states {
			args = append(args, st)
		}
	}
	if after != "" {
		at, id, _ := splitCursor(after, resultIDRe)
		q += ` AND (submitted_at < ? OR (submitted_at = ? AND id < ?))`
		args = append(args, at, at, id)
	}
	q += ` ORDER BY submitted_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err = s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, nil, errScience
	}
	defer rows.Close()
	out := []Result{}
	for rows.Next() {
		r, err := scanResult(project, rows)
		if err != nil {
			return nil, nil, nil, errScience
		}
		out = append(out, r.Result)
	}
	if rows.Err() != nil {
		return nil, nil, nil, errScience
	}
	var next *string
	if len(out) > limit {
		out = out[:limit]
		cur := fmtTime(out[limit-1].SubmittedAt) + "~" + out[limit-1].ID
		next = &cur
	}
	return out, counts, next, nil
}
