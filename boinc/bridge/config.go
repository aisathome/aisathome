// The bridge's configuration file.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config is the bridge's JSON configuration. Paths in it are as the bridge sees them (in its
// container: the project directory mounted where BOINC's tools expect it).
type Config struct {
	// Gateway is the gateway's base URL ("https://api.example.org").
	Gateway string `json:"gateway"`
	// Connector is the connector's id at the gateway ("boinc").
	Connector string `json:"connector"`
	// KeyFile holds the connector key (one line). Only the bridge reads it.
	KeyFile string `json:"key_file"`
	// PilotGateway is the node WebSocket a pilot connects to, as its work unit's command line
	// carries it ("wss://api.example.org/v1/node/connect").
	PilotGateway string `json:"pilot_gateway"`

	// ProjectDir is the BOINC project's directory (bin/, templates/, download/, config.xml).
	ProjectDir string `json:"project_dir"`
	// App is the BOINC app of the pilots (default "pilot"), without Tiers.
	App string `json:"app,omitempty"`
	// Batch tags the bridge's work units (create_work --batch) and is how it tells them from
	// other work units in the same project: a test work unit made by hand, or a second bridge
	// that serves another gateway. 1 or more; never shared by two bridges.
	Batch int `json:"batch"`
	// WUTemplate and ResultTemplate, relative to ProjectDir (default templates/pilot_in and
	// templates/pilot_out).
	WUTemplate     string `json:"wu_template,omitempty"`
	ResultTemplate string `json:"result_template,omitempty"`
	// Model is the pilot's model, a remote sticky file (create_work --remote_file), without Tiers.
	Model RemoteFile `json:"model"`
	// Tiers, if set, replaces App and Model: one entry per pilot tier of the gateway's connector
	// (its pilot_tiers), each with its own BOINC app (whose plan classes admit only the cards that
	// hold the tier's model) and model. The bridge creates each tier's pilots in its app, so a work
	// unit's model always fits the hosts BOINC may send it to.
	Tiers []Tier `json:"tiers,omitempty"`
	// TargetHost, if set, assigns every work unit to this BOINC host (create_work --target_host;
	// the project needs enable_assignment). For a bridge that feeds one test host.
	TargetHost int64 `json:"target_host,omitempty"`
	// DeadlineHours: the deadline a new pilot gets at the gateway, after which its credential is
	// refused (default 60). An unsent work unit whose pilot has less than MinRunwayHours left is
	// cancelled, so with the defaults a credential waits unsent for at most 6 hours.
	DeadlineHours int `json:"deadline_hours,omitempty"`
	// MinRunwayHours: at least the work unit's delay_bound plus a pilot's run (default 54: 48 h
	// and 4 h, plus 2).
	MinRunwayHours int `json:"min_runway_hours,omitempty"`

	// CreditDir is where the bridge writes each pilot's final credit for the validator (default
	// ProjectDir/credit).
	CreditDir string `json:"credit_dir,omitempty"`
	// ZeroCredit writes 0 for every pilot: a bridge whose pilots serve a test gateway grants no
	// BOINC credit.
	ZeroCredit bool `json:"zero_credit,omitempty"`
	// Validator, if set, is the app's validator (boinc/validator), relative to ProjectDir
	// ("bin/pilot_validator"): the bridge runs it on each work unit right after writing its credit
	// file. It must not also run as a daemon for the app: a daemon cannot wait for the file.
	Validator string `json:"validator,omitempty"`
	// MaxGrantedCredit caps one result's credit in the validator (--max_granted_credit); needed
	// with Validator. The gateway caps a pilot's credit too (max_credit_per_pilot).
	MaxGrantedCredit float64 `json:"max_granted_credit,omitempty"`

	// WebURL is the project's web root on its own web server, for the stock RPCs
	// ("http://apache/project/"). The account RPCs take their parameters in the query string, so
	// the web server should keep them out of its access log.
	WebURL string `json:"web_url"`
	// InviteCodeFile holds the project's invitation code for account creation by RPC, if it has
	// one (INVITE_CODES_RPC).
	InviteCodeFile string `json:"invite_code_file,omitempty"`

	// DBDSNFile holds a Go MySQL driver DSN ("user:password@tcp(host:3306)/dbname") for the
	// project's database, best a user that may only SELECT. Without it, the bridge reads
	// db_host, db_name, db_user and db_passwd from ProjectDir/config.xml.
	DBDSNFile string `json:"db_dsn_file,omitempty"`

	// PollSeconds: how often people's requests, binds and ends are looked at (default 10).
	// DemandSeconds: how often demand and credit are (default 60).
	PollSeconds   int `json:"poll_seconds,omitempty"`
	DemandSeconds int `json:"demand_seconds,omitempty"`
}

// Tier is one pilot tier: its name at the gateway, its BOINC app and its model.
type Tier struct {
	Name  string     `json:"name"`
	App   string     `json:"app"`
	Model RemoteFile `json:"model"`
}

// RemoteFile is a file clients download from its own URL, with BOINC's size and MD5.
type RemoteFile struct {
	URL    string `json:"url"`
	NBytes int64  `json:"nbytes"`
	MD5    string `json:"md5"`
}

var (
	connectorIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	md5Re         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	appNameRe     = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	// tierNameRe: a tier's name, as the gateway's pilot_tiers allow.
	tierNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// LoadConfig reads and checks a configuration file.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) check() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if u, err := url.Parse(c.Gateway); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" {
		bad("gateway: the gateway's base URL, https://host")
	}
	c.Gateway = strings.TrimRight(c.Gateway, "/")
	if !connectorIDRe.MatchString(c.Connector) {
		bad("connector: the connector's id at the gateway")
	}
	if c.KeyFile == "" {
		bad("key_file: the file with the connector key")
	}
	if u, err := url.Parse(c.PilotGateway); err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" ||
		u.Path != "/v1/node/connect" || u.RawQuery != "" {
		bad("pilot_gateway: wss://host/v1/node/connect")
	}
	if !filepath.IsAbs(c.ProjectDir) {
		bad("project_dir: the BOINC project's directory, an absolute path")
	}
	if len(c.Tiers) > 0 {
		if c.App != "" || c.Model != (RemoteFile{}) {
			bad("tiers: replaces app and model; set either tiers or app and model")
		}
		names, apps := map[string]bool{}, map[string]bool{}
		for i, t := range c.Tiers {
			if !tierNameRe.MatchString(t.Name) {
				bad("tiers[%d].name: the tier's name at the gateway (lower-case letters, digits, '_' and '-')", i)
			}
			if !appNameRe.MatchString(t.App) {
				bad("tiers[%d].app: a BOINC app name", i)
			}
			if names[t.Name] || apps[t.App] {
				bad("tiers[%d]: every tier has its own name and app", i)
			}
			names[t.Name], apps[t.App] = true, true
			checkModel(fmt.Sprintf("tiers[%d].model", i), t.Model, bad)
		}
	} else {
		if c.App == "" {
			c.App = "pilot"
		}
		if !appNameRe.MatchString(c.App) {
			bad("app: a BOINC app name")
		}
		checkModel("model", c.Model, bad)
	}
	if c.Batch < 1 {
		bad("batch: 1 or more, this bridge's own")
	}
	if c.WUTemplate == "" {
		c.WUTemplate = "templates/pilot_in"
	}
	if c.ResultTemplate == "" {
		c.ResultTemplate = "templates/pilot_out"
	}
	if c.Validator != "" && c.MaxGrantedCredit <= 0 {
		bad("max_granted_credit: needed with validator, more than 0")
	}
	for _, t := range []string{c.WUTemplate, c.ResultTemplate, c.Validator} {
		if t == "" {
			continue
		}
		if filepath.IsAbs(t) || strings.HasPrefix(filepath.Clean(t), "..") || strings.HasPrefix(t, "-") {
			bad("templates and validator: paths relative to project_dir")
		}
	}
	if c.TargetHost < 0 {
		bad("target_host: a BOINC host id")
	}
	if c.DeadlineHours == 0 {
		c.DeadlineHours = 60
	}
	if c.MinRunwayHours == 0 {
		c.MinRunwayHours = 54
	}
	if c.DeadlineHours < 1 || c.MinRunwayHours < 1 || c.MinRunwayHours >= c.DeadlineHours {
		bad("deadline_hours must be more than min_runway_hours")
	}
	if c.CreditDir == "" && filepath.IsAbs(c.ProjectDir) {
		c.CreditDir = filepath.Join(c.ProjectDir, "credit")
	}
	if !filepath.IsAbs(c.CreditDir) {
		bad("credit_dir: an absolute path")
	}
	if u, err := url.Parse(c.WebURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" {
		bad("web_url: the project's web root, http://host/project/")
	}
	if !strings.HasSuffix(c.WebURL, "/") {
		c.WebURL += "/"
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 10
	}
	if c.DemandSeconds == 0 {
		c.DemandSeconds = 60
	}
	if c.PollSeconds < 1 || c.DemandSeconds < c.PollSeconds {
		bad("poll_seconds 1 or more, demand_seconds at least poll_seconds")
	}
	return errors.Join(errs...)
}

func checkModel(field string, m RemoteFile, bad func(string, ...any)) {
	if u, err := url.Parse(m.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		bad("%s.url: https", field)
	}
	if m.NBytes <= 0 || !md5Re.MatchString(m.MD5) {
		bad("%s: nbytes and md5 (32 lower-case hex digits)", field)
	}
}

// tiers is the bridge's tiers: Tiers, or without them one unnamed tier of App and Model (a
// gateway connector without pilot_tiers).
func (c *Config) tiers() []Tier {
	if len(c.Tiers) > 0 {
		return c.Tiers
	}
	return []Tier{{App: c.App, Model: c.Model}}
}

// tierOfApp is the tier whose BOINC app is app.
func (c *Config) tierOfApp(app string) (Tier, bool) {
	for _, t := range c.tiers() {
		if t.App == app {
			return t, true
		}
	}
	return Tier{}, false
}

// Apps is the BOINC apps of the bridge's tiers.
func (c *Config) Apps() []string {
	var out []string
	for _, t := range c.tiers() {
		out = append(out, t.App)
	}
	return out
}

func (c *Config) poll() time.Duration   { return time.Duration(c.PollSeconds) * time.Second }
func (c *Config) demand() time.Duration { return time.Duration(c.DemandSeconds) * time.Second }

// ReadSecret reads a one-line secret file.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("%s: one line expected", path)
	}
	return s, nil
}

// dbSettings are the database settings in the project's config.xml (what BOINC's own programs
// use).
type dbSettings struct{ Host, Name, User, Passwd string }

func projectDB(projectDir string) (dbSettings, error) {
	var d dbSettings
	b, err := os.ReadFile(filepath.Join(projectDir, "config.xml"))
	if err != nil {
		return d, err
	}
	get := func(tag string) string {
		m := regexp.MustCompile(`<` + tag + `>\s*([^<]*?)\s*</` + tag + `>`).FindSubmatch(b)
		if m == nil {
			return ""
		}
		return xmlUnescape(string(m[1]))
	}
	d = dbSettings{Host: get("db_host"), Name: get("db_name"), User: get("db_user"), Passwd: get("db_passwd")}
	if d.Name == "" || d.User == "" {
		return d, errors.New("config.xml: no db_name or db_user")
	}
	if d.Host == "" {
		d.Host = "localhost"
	}
	if !strings.Contains(d.Host, ":") {
		d.Host += ":3306"
	}
	return d, nil
}

func xmlUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&").Replace(s)
}
