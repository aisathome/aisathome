// Package localui serves the node agent's local web UI on 127.0.0.1: status, GPU, counters
// and recent events for everyone on this computer, and pause, resume and engine restart for
// whoever knows the local password.
//
// Protections, each covered by a test:
//   - it listens on 127.0.0.1 only;
//   - the Host header must name the loopback (127.0.0.1, localhost or [::1]), which defeats
//     DNS rebinding;
//   - state-changing requests must be same-origin (Origin and Sec-Fetch-Site) and JSON,
//     which defeats cross-site requests from web pages;
//   - controls need a session cookie (HttpOnly, SameSite=Strict) that is only issued for the
//     password in the password file, or for a one-time sign-in link (GET /login-link, single
//     use, a minute) that only the agent's tray pipe hands out (Windows; pipe.go);
//   - the page loads nothing from elsewhere (embedded assets, a strict CSP).
//
// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/setupflow"
)

// DefaultPort is the local UI's port unless configured otherwise.
const DefaultPort = 7381

// AccountURL is the account page that lists a volunteer's nodes.
const AccountURL = "https://aisathome.org/account"

//go:embed assets
var assets embed.FS

// Agent is what the UI needs from the node agent.
type Agent interface {
	Status() node.Status
	Pause()
	Resume()
	RestartEngine() error
	StopEngine() error  // node.ErrNoEngineControl when the agent does not run the engine
	StartEngine() error // the same
}

// Setup is the first-run setup (package setupflow) on a node whose agent runs its engine
// (Windows); nil elsewhere, and then /setup does not exist.
type Setup interface {
	State() setupflow.State
	CheckCard() error
	Download() error
	SpeedTest() error
	Cancel()
	Start(token, gateway string) error
	ChangeToken(token, gateway string) error
	Enter() error
	Leave() error
}

// Options configure a Server.
type Options struct {
	Port         int    // 0 = DefaultPort
	PasswordFile string // "" = controls off
	NvidiaSMI    string // nvidia-smi command; "" = "nvidia-smi"
	Log          *slog.Logger
	Setup        Setup // the setup page; nil = none
	// Pipe is the named pipe where the tray asks for one-time sign-in links (TrayPipe on
	// Windows); "" = none.
	Pipe string
}

// Server is the local UI.
type Server struct {
	agentMu sync.Mutex
	agent   Agent
	opt     Options
	auth    *auth
	gpu     *gpuProbe
	log     *slog.Logger
}

// New creates the UI server.
func New(a Agent, opt Options) *Server {
	if opt.Port == 0 {
		opt.Port = DefaultPort
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{agent: a, opt: opt, auth: newAuth(opt.PasswordFile), gpu: newGPUProbe(opt.NvidiaSMI), log: opt.Log}
}

// SetAgent replaces the agent the UI shows and controls: the command builds a new one each time
// the setup writes node.json, while the UI keeps listening.
func (s *Server) SetAgent(a Agent) {
	s.agentMu.Lock()
	s.agent = a
	s.agentMu.Unlock()
}

func (s *Server) cur() Agent {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	return s.agent
}

// Addr is the only address the UI listens on.
func Addr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// ListenAndServe serves on 127.0.0.1:port until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", Addr(s.opt.Port))
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	s.log.Info("local UI listening", "url", "http://"+ln.Addr().String()+"/")
	if s.opt.Pipe != "" {
		pctx, stop := context.WithCancel(ctx)
		pipeDone := make(chan struct{})
		go func() {
			defer close(pipeDone)
			// The page works without it; only the tray's sign-in needs it.
			if err := s.servePipe(pctx, s.opt.Pipe); err != nil {
				s.log.Error("tray pipe not available: the tray cannot sign in to the page", "pipe", s.opt.Pipe, "err", err)
			}
		}()
		defer func() {
			stop()
			<-pipeDone
		}()
	}
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler returns the UI's HTTP handler with all request checks applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets")
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// The installer's shortcut opens "/": while the node is not set up (or the volunteer
		// runs the setup again) that is the setup page.
		if (r.URL.Path == "/" || r.URL.Path == "/index.html") && s.inSetup() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if (r.URL.Path == "/setup.html" || r.URL.Path == "/setup.js") && s.opt.Setup == nil {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /login-link", s.handleLoginLink)
	mux.HandleFunc("POST /api/pause", s.control(func(w http.ResponseWriter, _ *http.Request) { s.cur().Pause(); s.ok(w) }))
	mux.HandleFunc("POST /api/resume", s.control(func(w http.ResponseWriter, _ *http.Request) { s.cur().Resume(); s.ok(w) }))
	mux.HandleFunc("POST /api/restart-engine", s.control(s.restart))
	mux.HandleFunc("POST /api/stop-engine", s.control(s.engineCtl(true)))
	mux.HandleFunc("POST /api/start-engine", s.control(s.engineCtl(false)))
	if s.opt.Setup != nil {
		mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/setup.html"
			files.ServeHTTP(w, r2)
		})
		s.setupRoutes(mux)
	}
	return s.guard(mux)
}

// loopbackHost reports whether a Host header names this computer's loopback. Any port is
// fine: an SSH tunnel may forward another local port. A DNS-rebinding page has its own
// domain name here.
func loopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	switch strings.ToLower(h) {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	}
	return false
}

// guard applies the checks that every request must pass, and the security headers.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		hd.Set("Cache-Control", "no-store")
		if !loopbackHost(r.Host) {
			http.Error(w, "this UI only answers on 127.0.0.1 or localhost", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if why := crossSite(r); why != "" {
				s.log.Warn("local UI: rejected a cross-site request", "why", why, "path", r.URL.Path)
				http.Error(w, "cross-site request rejected: "+why, http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		}
		next.ServeHTTP(w, r)
	})
}

// crossSite returns why a state-changing request may come from another site, or "".
// Browsers send Sec-Fetch-Site and Origin; command-line clients send neither and pass.
func crossSite(r *http.Request) string {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return "Sec-Fetch-Site " + r.Header.Get("Sec-Fetch-Site")
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		return "Origin " + o
	}
	// HTML forms cannot send JSON; anything else needs a CORS preflight, which we never grant.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return "Content-Type must be application/json"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) }

// StatusResponse is the body of GET /api/status.
type StatusResponse struct {
	node.Status
	GPU        GPUInfo          `json:"gpu"`
	Controls   Controls         `json:"controls"`
	AccountURL string           `json:"account_url"`
	Setup      *setupflow.State `json:"setup,omitempty"` // the setup's state, on a node with a setup page
}

// Controls tells the page which controls it may show, and the platform's commands.
type Controls struct {
	Enabled        bool   `json:"enabled"` // a password is set
	SignedIn       bool   `json:"signed_in"`
	Note           string `json:"note,omitempty"`
	RestartCommand string `json:"restart_command"` // to run by hand when the agent cannot
	StatusCommand  string `json:"status_command"`  // whether the agent's service runs
	PasswordHint   string `json:"password_hint"`   // how to show the local password
	// EngineControl: the agent runs the engine itself (Windows), so the page offers Stop
	// engine and Start engine, and a restart does not restart the agent.
	EngineControl bool `json:"engine_control"`
}

// inSetup reports whether "/" should be the setup page.
func (s *Server) inSetup() bool {
	if s.opt.Setup == nil {
		return false
	}
	st := s.opt.Setup.State()
	return !st.Configured || st.Mode == "setup"
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.cur().Status()
	c := Controls{RestartCommand: node.RestartCommand, StatusCommand: node.StatusCommand,
		PasswordHint: node.PasswordHint(s.opt.PasswordFile), EngineControl: st.EngineProcess != nil}
	if _, err := s.auth.password(); err != nil {
		c.Note = "Controls are off: " + err.Error()
	} else {
		c.Enabled = true
		c.SignedIn = s.auth.valid(r)
	}
	resp := StatusResponse{Status: st, Controls: c, AccountURL: AccountURL}
	if r.URL.Query().Get("gpu") != "0" { // the tray polls without it: no nvidia-smi run for it
		resp.GPU = s.gpu.get(r.Context())
	}
	if s.opt.Setup != nil {
		ss := s.opt.Setup.State()
		resp.Setup = &ss
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send {\"password\": \"...\"}"})
		return
	}
	cookie, code, err := s.auth.login(body.Password)
	if err != nil {
		if code == http.StatusUnauthorized {
			s.log.Warn("local UI: wrong password")
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	http.SetCookie(w, cookie)
	s.ok(w)
}

// handleLoginLink signs the browser in with a one-time link from the tray (newLink) and
// goes to "/". A used, expired or unknown link goes to "/" too, where the page asks for the
// password (a browser restoring its tabs reopens a used link). Only a navigation the person
// started (Sec-Fetch-Site none, as from the tray or the address bar) or a non-browser client
// may use one: another site's page cannot, even with a valid secret.
func (s *Server) handleLoginLink(w http.ResponseWriter, r *http.Request) {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "none", "same-origin":
	default:
		s.log.Warn("local UI: rejected a sign-in link from another site", "sec-fetch-site", site)
		http.Error(w, "sign-in links only work when opened directly", http.StatusForbidden)
		return
	}
	if c, ok := s.auth.redeem(r.URL.Query().Get("t")); ok {
		http.SetCookie(w, c)
		s.log.Info("local UI: signed in with a one-time link from the tray")
	} else if !s.auth.valid(r) {
		s.log.Warn("local UI: a sign-in link was used, expired or unknown")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.auth.clear())
	s.ok(w)
}

// control wraps a handler that changes the node: it needs a valid session.
func (s *Server) control(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.valid(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enter the local password first"})
			return
		}
		h(w, r)
	}
}

func (s *Server) restart(w http.ResponseWriter, _ *http.Request) {
	a := s.cur()
	switch err := a.RestartEngine(); {
	case err == nil:
		msg := "Restarting the engine. The agent restarts with it; this page is back in a few seconds and the node reconnects once the model is loaded."
		if a.Status().EngineProcess != nil { // the agent runs the engine (Windows): only the engine restarts
			msg = "Restarting the engine; requests in flight end. The node reconnects once the model is loaded again (up to a few minutes)."
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": msg})
	case errors.Is(err, node.ErrRestartUnavailable):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "The agent may not restart the engine on this machine. Run this yourself:",
			"command": node.RestartCommand})
	case errors.Is(err, node.ErrRestartTooSoon):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
	default:
		s.log.Error("local UI: engine restart failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not request the restart: " + err.Error(),
			"command": node.RestartCommand})
	}
}

// engineCtl stops (stop) or starts the engine the agent runs.
func (s *Server) engineCtl(stop bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		a := s.cur()
		var err error
		msg := "Starting the engine. The node reconnects once the model is loaded (up to a few minutes)."
		if stop {
			err = a.StopEngine()
			msg = "Engine stopped: the GPU is free and the node is away. Start engine brings it back."
		} else {
			err = a.StartEngine()
		}
		if errors.Is(err, node.ErrNoEngineControl) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "This agent does not run the engine itself; stop or start its service instead.",
				"command": node.RestartCommand})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	}
}
