// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisathome/aisathome/node/setupflow"
)

// setupRoutes are the setup page's actions; each needs the local password, like every control.
// The token travels only in Start's and ChangeToken's request bodies and is never sent back; a link to
// an account brings it from the gateway to the agent directly.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	step := func(do func() error) http.HandlerFunc {
		return s.control(func(w http.ResponseWriter, _ *http.Request) { s.setupResult(w, do()) })
	}
	f := s.opt.Setup
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.setupState(r)) })
	mux.HandleFunc("POST /api/setup/check", step(f.CheckCard))
	mux.HandleFunc("POST /api/setup/download", step(f.Download))
	mux.HandleFunc("POST /api/setup/speed", step(f.SpeedTest))
	mux.HandleFunc("POST /api/setup/cancel", step(func() error { f.Cancel(); return nil }))
	mux.HandleFunc("POST /api/setup/enter", step(f.Enter))
	mux.HandleFunc("POST /api/setup/leave", step(f.Leave))
	token := func(do func(token, gateway string) error) http.HandlerFunc {
		return s.control(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Token   string `json:"token"`
				Gateway string `json:"gateway"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send {\"token\": \"...\"}"})
				return
			}
			s.setupResult(w, do(body.Token, body.Gateway))
		})
	}
	mux.HandleFunc("POST /api/setup/start", token(f.Start))
	mux.HandleFunc("POST /api/setup/token", token(f.ChangeToken))
	// Linking to an account: the answer's state carries the code and the approval page's link; the
	// token comes from the gateway to the agent, never through the page.
	mux.HandleFunc("POST /api/setup/link", token(func(_, gateway string) error { return f.Link(gateway) }))
	mux.HandleFunc("POST /api/setup/link/cancel", step(func() error { f.LinkCancel(); return nil }))
}

// setupState is the setup's state for r. A live link code (and the approval link that carries it) goes
// only to a signed-in page: whoever approves it adds this computer to their account, and anyone on this
// computer can read the status without the local password.
func (s *Server) setupState(r *http.Request) setupflow.State {
	st := s.opt.Setup.State()
	if st.Link != nil && !s.auth.valid(r) {
		l := *st.Link
		l.Code, l.URL = "", ""
		st.Link = &l
	}
	return st
}

func (s *Server) setupResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, s.opt.Setup.State())
	case errors.Is(err, setupflow.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
}
