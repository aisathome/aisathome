// Package bridge joins a BOINC project to an AIs@home gateway as one of its connectors
// (docs/gateway.md, "Connectors and pilots"). It runs beside the project's stock server and holds
// the only connector key. The gateway is the source of truth; the bridge mirrors it into BOINC
// and tells it what BOINC saw:
//
//   - demand: it asks how many pilots are wanted, creates each at the gateway, stages the pilot's
//     credential as a work-unit input file and makes a work unit with the pilot's name
//     (create_work); unsent work units the gateway no longer wants, and those too close to the
//     pilot's deadline, are cancelled (cancel_jobs) and reported;
//   - binds and ends: once BOINC sends a work unit, it tells the gateway which BOINC user, host
//     and team run it; once BOINC has the result (or gave up on it), it reports the outcome, which
//     ends the pilot;
//   - credit: once the gateway's credit for a finished pilot is final (it holds it for a day), it
//     writes it into a file the project's validator reads (BOINC grants it as the result's credit);
//   - people's requests: "Run on BOINC" makes a BOINC user with create_account.php and answers
//     its id and weak (attach-only) key from am_get_info.php; a claim looks the user up by the
//     account key the person gave (am_get_info.php).
//
// It reads BOINC's database and never writes it; BOINC's own tools make and cancel work units.
// It never logs a credential, an account key, a weak key, an address or a password hash.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// orphanAfter: a created pilot with no work unit this long after its creation was never made
	// in BOINC (the bridge stopped between the two steps, or create_work failed); it is ended.
	orphanAfter = 5 * time.Minute
	// requestRetry: a request whose BOINC call failed (not a BOINC refusal) is tried again then.
	requestRetry = 2 * time.Minute
	// creditPage and creditPages: results awaiting credit are read this many at a time, at most
	// this many pages per pass (100,000 results).
	creditPage  = 500
	creditPages = 200
	// cleanupEvery: how often finished credit files are removed.
	cleanupEvery = time.Hour
	// creditFileKeep: a credit file whose work unit the bridge can no longer find is removed after
	// this long (the work unit was purged, or it belongs to another bridge's batch).
	creditFileKeep = 30 * 24 * time.Hour
)

var (
	pilotIDRe = regexp.MustCompile(`^p-[0-9a-f]{12}$`)
	// accountKeyRe: a BOINC account key (authenticator), 32 hex digits.
	accountKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Bridge is one bridge: one gateway connector, one BOINC project, one batch.
type Bridge struct {
	cfg   Config
	gw    *Gateway
	store Store
	tools Tools
	rpc   RPC
	log   *slog.Logger
	key   []byte

	// Now is the bridge's clock (tests set it).
	Now func() time.Time

	lastDemand  time.Time
	lastCleanup time.Time
	creditNext  map[string]time.Time // pilot → when to ask for its credit again
	reqNext     map[string]time.Time // request → when to try it again
	st          counters
}

// counters since the last status line.
type counters struct {
	created, cancelled, bound, reported, answered, credited, errors int
	wanted                                                          int
	open                                                            map[string]int
}

// New returns a bridge. key is the connector key. The config is checked (and its defaults
// filled in) as LoadConfig does.
func New(cfg Config, key string, store Store, tools Tools, rpc RPC, log *slog.Logger) (*Bridge, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("no connector key")
	}
	return &Bridge{cfg: cfg, gw: NewGateway(cfg.Gateway, cfg.Connector, key), store: store, tools: tools, rpc: rpc,
		log: log, key: []byte(key), Now: time.Now, creditNext: map[string]time.Time{}, reqNext: map[string]time.Time{}}, nil
}

// Run steps every poll interval until ctx ends.
func (b *Bridge) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.poll())
	defer t.Stop()
	for {
		b.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step does one round: requests, binds and ends every time; demand, credit and a status line
// every demand interval; the credit directory's cleanup every hour. A failed part is logged and
// tried again next time.
func (b *Bridge) Step(ctx context.Context) {
	now := b.Now()
	b.part(ctx, "requests", b.Requests)
	b.part(ctx, "sync", b.Sync)
	if now.Sub(b.lastDemand) >= b.cfg.demand() {
		b.lastDemand = now
		b.part(ctx, "demand", b.Demand)
		b.part(ctx, "credit", b.Credit)
		b.logStatus()
	}
	if now.Sub(b.lastCleanup) >= cleanupEvery {
		b.lastCleanup = now
		b.part(ctx, "cleanup", b.Cleanup)
	}
}

func (b *Bridge) part(ctx context.Context, name string, f func(context.Context) error) {
	if ctx.Err() != nil {
		return
	}
	if err := f(ctx); err != nil {
		b.st.errors++
		b.log.Error("bridge: "+name, "err", err)
	}
}

func (b *Bridge) logStatus() {
	b.log.Info("bridge status", "open", b.st.open, "wanted", b.st.wanted, "created", b.st.created,
		"cancelled", b.st.cancelled, "bound", b.st.bound, "reported", b.st.reported, "answered", b.st.answered,
		"credit_files", b.st.credited, "errors", b.st.errors)
	open := b.st.open
	b.st = counters{open: open}
}

// openPilots lists the connector's pilots that have not ended.
func (b *Bridge) openPilots(ctx context.Context) ([]Pilot, error) {
	var out []Pilot
	for _, st := range openStates {
		ps, err := b.gw.Pilots(ctx, st)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

func pilotIDs(ps []Pilot) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if pilotIDRe.MatchString(p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

// Sync tells the gateway who runs each sent work unit and which have ended, and ends pilots that
// never got a work unit.
func (b *Bridge) Sync(ctx context.Context) error {
	open, err := b.openPilots(ctx)
	if err != nil {
		return err
	}
	jobs, err := b.store.Jobs(ctx, pilotIDs(open))
	if err != nil {
		return err
	}
	now := b.Now()
	counts := map[string]int{}
	var errs []error
	for _, p := range open {
		counts[p.State]++
		j, ok := jobs[p.ID]
		if !ok {
			if p.State == pilotCreated && now.Sub(p.Created) < orphanAfter {
				continue // create_work may still be running
			}
			errs = append(errs, b.report(ctx, p.ID, "error", "no work unit in BOINC"))
			continue
		}
		if p.State == pilotCreated && j.sent() {
			team := ""
			if j.Team > 0 {
				team = strconv.FormatInt(j.Team, 10)
			}
			if err := b.gw.Bind(ctx, p.ID, strconv.FormatInt(j.User, 10), strconv.FormatInt(j.Host, 10), team); err != nil {
				errs = append(errs, fmt.Errorf("bind %s: %w", p.ID, err))
				continue
			}
			b.st.bound++
			b.log.Info("pilot bound", "pilot", p.ID, "boinc_user", j.User, "boinc_host", j.Host)
		}
		if outcome, done := j.reported(); done {
			errs = append(errs, b.report(ctx, p.ID, outcome, ""))
		}
	}
	b.st.open = counts
	return errors.Join(errs...)
}

func (b *Bridge) report(ctx context.Context, pid, outcome, why string) error {
	if err := b.gw.Reported(ctx, pid, outcome); err != nil {
		return fmt.Errorf("report %s: %w", pid, err)
	}
	b.st.reported++
	b.log.Info("pilot reported", "pilot", pid, "outcome", outcome, "why", why)
	return nil
}

// Demand cancels unsent work units the gateway no longer wants (and those whose pilot is too
// close to its deadline to be run), then creates the pilots it wants.
func (b *Bridge) Demand(ctx context.Context) error {
	d, err := b.gw.Demand(ctx)
	if err != nil {
		return err
	}
	b.st.wanted = d.Wanted
	created, err := b.gw.Pilots(ctx, pilotCreated) // newest first: the newest are cancelled first
	if err != nil {
		return err
	}
	jobs, err := b.store.Jobs(ctx, pilotIDs(created))
	if err != nil {
		return err
	}
	now := b.Now()
	runway := time.Duration(b.cfg.MinRunwayHours) * time.Hour
	var errs []error
	cancelled := 0
	for _, p := range created {
		j, ok := jobs[p.ID]
		if !ok || !j.unsent() {
			continue
		}
		short := p.Deadline.Sub(now) < runway
		if !short && cancelled >= d.CancelUnsent {
			continue
		}
		if err := b.tools.Cancel(ctx, p.ID); err != nil {
			errs = append(errs, fmt.Errorf("cancel %s: %w", p.ID, err))
			continue
		}
		cancelled++
		b.st.cancelled++
		// BOINC may have sent it between the look and cancel_jobs, which cancels the work unit
		// anyway. A host has it then: the pilot is not ended here; Sync binds it, and reports it when
		// the result is over (the client aborts it at its next scheduler request).
		if after, err := b.store.Jobs(ctx, []string{p.ID}); err != nil {
			errs = append(errs, fmt.Errorf("cancel %s: %w", p.ID, err))
			continue
		} else if j, ok := after[p.ID]; ok && j.sent() {
			b.log.Warn("pilot sent while its work unit was cancelled; not ended", "pilot", p.ID)
			continue
		}
		why := "not wanted"
		if short {
			why = "too close to its deadline"
		}
		errs = append(errs, b.report(ctx, p.ID, "cancelled", why))
	}
	for i := 0; i < d.Wanted; i++ {
		if err := b.createPilot(ctx); err != nil {
			if s := statusOf(err); s == http.StatusConflict || s == http.StatusTooManyRequests {
				b.log.Info("bridge: no more pilots now", "why", err)
				break
			}
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}

// createPilot creates one pilot and its work unit. A pilot whose work unit could not be made is
// reported as an error, which ends it (the orphan sweep in Sync would, a few minutes later).
func (b *Bridge) createPilot(ctx context.Context) error {
	p, cred, err := b.gw.CreatePilot(ctx, b.Now().Add(time.Duration(b.cfg.DeadlineHours)*time.Hour))
	if err != nil {
		return err
	}
	// The credential's file name is unguessable: input files are served by name.
	file := "pc-" + p.ID + "-" + randHex(16)
	if err := b.tools.Stage(ctx, file, []byte(cred+"\n")); err != nil {
		return errors.Join(fmt.Errorf("pilot %s: %w", p.ID, err), b.report(ctx, p.ID, "error", "credential not staged"))
	}
	if err := b.tools.CreateWork(ctx, p.ID, file); err != nil {
		// create_work may have inserted the work unit before it failed (a timeout, say). Only a work
		// unit that is not there lets go of the credential and the pilot; if the look fails, the
		// orphan sweep in Sync decides later.
		jobs, jerr := b.store.Jobs(ctx, []string{p.ID})
		if jerr != nil {
			return fmt.Errorf("pilot %s: %w (and the work unit could not be looked up: %v)", p.ID, err, jerr)
		}
		if _, made := jobs[p.ID]; made {
			b.log.Warn("create_work failed but the work unit exists; kept", "pilot", p.ID, "err", err)
			b.st.created++
			return nil
		}
		uerr := b.tools.Unstage(ctx, file)
		return errors.Join(fmt.Errorf("pilot %s: %w", p.ID, err), uerr, b.report(ctx, p.ID, "error", "no work unit made"))
	}
	b.st.created++
	b.log.Info("pilot created", "pilot", p.ID, "deadline", p.Deadline)
	return nil
}

// Credit writes the final credit of every finished pilot whose result waits for the validator.
// Until the gateway's credit is final, the validator finds no file and BOINC tries again later.
func (b *Bridge) Credit(ctx context.Context) error {
	// Every page, not only the first: a backlog of results still in the gateway's hold must not keep
	// later ones, whose credit may be final, out of the pass.
	var names []string
	var after int64
	for page := 0; page < creditPages; page++ {
		rows, err := b.store.AwaitingCredit(ctx, after, creditPage)
		if err != nil {
			return err
		}
		for _, r := range rows {
			names = append(names, r.Name)
			after = r.ResultID
		}
		if len(rows) < creditPage {
			break
		}
	}
	now := b.Now()
	waiting := make(map[string]bool, len(names))
	var errs []error
	for _, name := range names {
		if !pilotIDRe.MatchString(name) {
			continue
		}
		waiting[name] = true
		path := filepath.Join(b.cfg.CreditDir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if t, ok := b.creditNext[name]; ok && now.Before(t) {
			continue
		}
		c, err := b.gw.Credit(ctx, name)
		if err != nil {
			next := 5 * time.Minute
			if statusOf(err) == http.StatusNotFound {
				next = time.Hour
			}
			b.creditNext[name] = now.Add(next)
			errs = append(errs, fmt.Errorf("credit %s: %w", name, err))
			continue
		}
		if !c.Final {
			next := now.Add(time.Minute) // not ended yet: Sync reports it
			if c.FinalAt != nil && c.FinalAt.After(now) {
				next = c.FinalAt.Add(30 * time.Second)
			}
			b.creditNext[name] = next
			continue
		}
		v := 0.0
		if c.Credit != nil {
			v = *c.Credit
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			errs = append(errs, fmt.Errorf("credit %s: the gateway said %v", name, v))
			b.creditNext[name] = now.Add(time.Hour)
			continue
		}
		if b.cfg.ZeroCredit {
			v = 0
		}
		if err := writeCredit(b.cfg.CreditDir, name, v); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(b.creditNext, name)
		b.st.credited++
		b.log.Info("pilot credit written", "pilot", name, "credit", v)
	}
	for name := range b.creditNext {
		if !waiting[name] {
			delete(b.creditNext, name)
		}
	}
	return errors.Join(errs...)
}

// writeCredit writes a credit file whole (a temporary file renamed), so the validator never reads
// half of one.
func writeCredit(dir, name string, v float64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.FormatFloat(v, 'f', -1, 64) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("credit file %s: %w", name, err)
	}
	return nil
}

// Cleanup removes credit files the validator is done with: their result is validated (or will
// never be). A file whose work unit the bridge cannot find is kept for creditFileKeep: it may be
// another bridge's, in the same directory.
func (b *Bridge) Cleanup(ctx context.Context) error {
	ents, err := os.ReadDir(b.cfg.CreditDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now := b.Now()
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".tmp") {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
				os.Remove(filepath.Join(b.cfg.CreditDir, n))
			}
			continue
		}
		if pilotIDRe.MatchString(n) {
			names = append(names, n)
		}
	}
	jobs, err := b.store.Jobs(ctx, names)
	if err != nil {
		return err
	}
	for _, n := range names {
		path := filepath.Join(b.cfg.CreditDir, n)
		j, ok := jobs[n]
		// Done: validated (valid, invalid, too late), or over without success. Not while inconclusive:
		// the validator reads the file again.
		done := ok && j.HasResult && j.ServerState == ServerStateOver && !j.awaitingValidation()
		if !ok {
			if info, err := os.Stat(path); err == nil && now.Sub(info.ModTime()) > creditFileKeep {
				done = true
			}
		}
		if done {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// Requests carries out people's pending requests with the project's stock RPCs and answers them.
func (b *Bridge) Requests(ctx context.Context) error {
	qs, err := b.gw.Requests(ctx)
	if err != nil {
		return err
	}
	now := b.Now()
	listed := make(map[string]bool, len(qs))
	var errs []error
	for _, q := range qs {
		listed[q.ID] = true
		if t, ok := b.reqNext[q.ID]; ok && now.Before(t) {
			continue
		}
		ans, err := b.carryOut(ctx, q)
		if err != nil { // the call failed, BOINC did not refuse: try again later
			b.reqNext[q.ID] = now.Add(requestRetry)
			errs = append(errs, fmt.Errorf("request %s (%s): %w", q.ID, q.Kind, err))
			continue
		}
		if err := b.gw.Answer(ctx, q.ID, ans); err != nil {
			b.reqNext[q.ID] = now.Add(requestRetry)
			errs = append(errs, fmt.Errorf("answer %s: %w", q.ID, err))
			continue
		}
		delete(b.reqNext, q.ID)
		b.st.answered++
		b.log.Info("request answered", "request", q.ID, "kind", q.Kind, "ok", ans.OK, "boinc_user", ans.User, "error", ans.Error)
	}
	for id := range b.reqNext {
		if !listed[id] {
			delete(b.reqNext, id)
		}
	}
	return errors.Join(errs...)
}

func refuse(msg string) (Answer, error) { return Answer{OK: false, Error: msg}, nil }

// carryOut does one request. An error means try again later; a refusal is an Answer.
func (b *Bridge) carryOut(ctx context.Context, q Request) (Answer, error) {
	switch q.Kind {
	case "create_link":
		if q.Email == "" || q.UserName == "" {
			return refuse("the request has no address or name")
		}
		ph := q.PasswdHash
		if ph == "" {
			// No password chosen: one the person never sees, derived from the request, so that
			// carrying the request out again (after a crash before the answer) finds the same user.
			ph = md5Hex(b.password(q.ID) + strings.ToLower(q.Email))
		}
		if !md5Re.MatchString(ph) {
			return refuse("the password hash is not in BOINC's form")
		}
		auth, err := b.rpc.CreateAccount(ctx, q.Email, ph, q.UserName)
		if err != nil {
			return createRefusal(err)
		}
		a, err := b.rpc.AccountInfo(ctx, auth)
		if err != nil {
			return Answer{}, err // the user exists now; the next try finds it again
		}
		return Answer{OK: true, User: strconv.FormatInt(a.ID, 10), Key: a.WeakAuth}, nil
	case "claim":
		if !accountKeyRe.MatchString(q.AccountKey) {
			return refuse("that is not a BOINC account key (32 hex digits from the Account keys page)")
		}
		a, err := b.rpc.AccountInfo(ctx, q.AccountKey)
		var re *RPCError
		if errors.As(err, &re) && re.Num == errDBNotFound {
			return refuse("no BOINC account of this project has that account key")
		}
		if err != nil {
			return Answer{}, err
		}
		return Answer{OK: true, User: strconv.FormatInt(a.ID, 10), Key: a.WeakAuth}, nil
	case "set_email":
		return refuse("this project cannot change a BOINC address from here; change it on the BOINC site")
	default:
		return refuse("the bridge does not know this kind of request")
	}
}

// createRefusal turns create_account.php's error into the person's answer, or into an error to try
// again for what is not the person's (the database, the project's settings).
func createRefusal(err error) (Answer, error) {
	var re *RPCError
	if !errors.As(err, &re) {
		return Answer{}, err
	}
	switch re.Num {
	case errDBNotUnique, errNonuniqueEmail:
		return refuse("this address already has a BOINC account on this project; claim it with its account key instead")
	case errBadEmailAddr:
		return refuse("BOINC refused the address")
	case errBadUserName:
		return refuse("BOINC refused the display name")
	case errRequireConsent:
		return refuse("the BOINC project asks new accounts to accept its terms, which this page cannot do yet")
	case errDBCantConnect, errProjectDown, -1:
		// -1: the project closed account creation or refused the invitation code: a setting to fix,
		// so the request waits (it fails by itself after a day).
		return Answer{}, err
	default:
		return refuse(fmt.Sprintf("BOINC refused to make the account (error %d)", re.Num))
	}
}

// password is the password of a user made without one: never shown, never stored, and the same
// for the same request.
func (b *Bridge) password(rid string) string {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte("aisathome-bridge password\x00" + rid))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return hex.EncodeToString(b)
}

// Check checks what the bridge needs before it runs: the connector key opens the gateway's demand,
// BOINC's tools are there, and the credit directory is writable.
func (b *Bridge) Check(ctx context.Context) error {
	if _, err := b.gw.Demand(ctx); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	for _, p := range []string{"bin/create_work", "bin/cancel_jobs", "bin/dir_hier_path"} {
		if info, err := os.Stat(filepath.Join(b.cfg.ProjectDir, p)); err != nil || info.Mode()&0o111 == 0 {
			return fmt.Errorf("project_dir: no program %s", p)
		}
	}
	if err := os.MkdirAll(b.cfg.CreditDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(b.cfg.CreditDir, ".check-*.tmp")
	if err != nil {
		return fmt.Errorf("credit_dir: %w", err)
	}
	f.Close()
	return os.Remove(f.Name())
}
