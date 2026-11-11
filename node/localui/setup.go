// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisathome/aisathome/node/setupflow"
)

// setupRoutes are the setup page's actions; each needs the local password, like every control.
// The token travels only in Start's and ChangeToken's request bodies and is never sent back.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	step := func(do func() error) http.HandlerFunc {
		return s.control(func(w http.ResponseWriter, _ *http.Request) { s.setupResult(w, do()) })
	}
	f := s.opt.Setup
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, f.State()) })
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
