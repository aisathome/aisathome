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
//     password in the password file;
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
	"time"

	"github.com/aisathome/aisathome/node"
)

// DefaultPort is the local UI's port unless configured otherwise.
const DefaultPort = 7381

// DefaultPasswordFile is where the installer puts the UI password (root:aisathome 0640).
const DefaultPasswordFile = "/etc/aisathome-node/ui-password"

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
}

// Options configure a Server.
type Options struct {
	Port         int    // 0 = DefaultPort
	PasswordFile string // "" = controls off
	NvidiaSMI    string // nvidia-smi command; "" = "nvidia-smi"
	Log          *slog.Logger
}

// Server is the local UI.
type Server struct {
	agent Agent
	opt   Options
	auth  *auth
	gpu   *gpuProbe
	log   *slog.Logger
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
	mux.Handle("GET /", files)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/pause", s.control(func(w http.ResponseWriter) { s.agent.Pause(); s.ok(w) }))
	mux.HandleFunc("POST /api/resume", s.control(func(w http.ResponseWriter) { s.agent.Resume(); s.ok(w) }))
	mux.HandleFunc("POST /api/restart-engine", s.control(s.restart))
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
	GPU        GPUInfo  `json:"gpu"`
	Controls   Controls `json:"controls"`
	AccountURL string   `json:"account_url"`
}

// Controls tells the page which controls it may show.
type Controls struct {
	Enabled        bool   `json:"enabled"` // a password is set
	SignedIn       bool   `json:"signed_in"`
	Note           string `json:"note,omitempty"`
	RestartCommand string `json:"restart_command"` // to run by hand when the agent cannot
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.agent.Status()
	c := Controls{RestartCommand: node.RestartCommand}
	if _, err := s.auth.password(); err != nil {
		c.Note = "Controls are off: " + err.Error()
	} else {
		c.Enabled = true
		c.SignedIn = s.auth.valid(r)
	}
	writeJSON(w, http.StatusOK, StatusResponse{Status: st, GPU: s.gpu.get(r.Context()), Controls: c, AccountURL: AccountURL})
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

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.auth.clear())
	s.ok(w)
}

// control wraps a handler that changes the node: it needs a valid session.
func (s *Server) control(h func(http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.valid(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "enter the local password first"})
			return
		}
		h(w)
	}
}

func (s *Server) restart(w http.ResponseWriter) {
	switch err := s.agent.RestartEngine(); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true,
			"message": "Restarting the engine. The agent restarts with it; this page is back in a few seconds and the node reconnects once the model is loaded."})
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
