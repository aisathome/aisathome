// The gateway's connector API, as the bridge uses it (docs/gateway.md, "Connectors and pilots").
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gateway calls one connector's endpoints with its key.
type Gateway struct {
	base string // https://host/v1/connectors/<id>
	key  string
	http *http.Client
}

// NewGateway returns a client for connector id at the gateway's base URL.
func NewGateway(base, id, key string) *Gateway {
	return &Gateway{base: strings.TrimRight(base, "/") + "/v1/connectors/" + url.PathEscape(id), key: key,
		http: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is an answer other than 2xx.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("gateway: HTTP %d: %s", e.Status, e.Message) }

// statusOf returns the HTTP status of a gateway error, or 0.
func statusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// Demand is GET .../demand (the fields the bridge uses).
type Demand struct {
	Wanted       int            `json:"pilots_wanted"`
	CancelUnsent int            `json:"cancel_unsent"`
	Target       int            `json:"target"`
	Pilots       map[string]int `json:"pilots"`
}

// Pilot is a pilot's record (the fields the bridge uses).
type Pilot struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	Deadline time.Time `json:"deadline"`
	User     string    `json:"user"`
	Host     string    `json:"host"`
	Team     string    `json:"team"`
	Created  time.Time `json:"created_at"`
}

// Pilot states at the gateway.
const (
	pilotCreated      = "created"
	pilotBound        = "bound"
	pilotConnected    = "connected"
	pilotDisconnected = "disconnected"
)

// openStates: every state but ended.
var openStates = []string{pilotCreated, pilotBound, pilotConnected, pilotDisconnected}

// Credit is GET .../pilots/{pid}/credit.
type Credit struct {
	Final   bool       `json:"final"`
	FinalAt *time.Time `json:"final_at"`
	Credit  *float64   `json:"credit"`
}

// Request is one of people's pending requests (GET .../requests).
type Request struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Created    time.Time `json:"created_at"`
	User       string    `json:"user"`
	AccountKey string    `json:"account_key"`
	UserName   string    `json:"user_name"`
	Email      string    `json:"email"`
	PasswdHash string    `json:"passwd_hash"`
	OwnEmail   bool      `json:"own_email"`
}

// Answer is a request's result (POST .../requests/{rid}).
type Answer struct {
	OK    bool   `json:"ok"`
	User  string `json:"user,omitempty"`
	Key   string `json:"key,omitempty"`
	Error string `json:"error,omitempty"`
}

func (g *Gateway) Demand(ctx context.Context) (Demand, error) {
	var d Demand
	return d, g.call(ctx, "GET", "/demand", nil, &d)
}

// CreatePilot creates a pilot; the credential is in the answer only.
func (g *Gateway) CreatePilot(ctx context.Context, deadline time.Time) (Pilot, string, error) {
	var out struct {
		Pilot      Pilot  `json:"pilot"`
		Credential string `json:"credential"`
	}
	err := g.call(ctx, "POST", "/pilots", map[string]any{"deadline": deadline.UTC().Format(time.RFC3339)}, &out)
	if err == nil && (out.Pilot.ID == "" || out.Credential == "") {
		err = errors.New("gateway: a pilot without id or credential")
	}
	return out.Pilot, out.Credential, err
}

// Pilots lists the connector's pilots in a state, newest first (at most 500).
func (g *Gateway) Pilots(ctx context.Context, state string) ([]Pilot, error) {
	var out struct {
		Pilots []Pilot `json:"pilots"`
	}
	return out.Pilots, g.call(ctx, "GET", "/pilots?limit=500&state="+url.QueryEscape(state), nil, &out)
}

func (g *Gateway) Bind(ctx context.Context, pid, user, host, team string) error {
	return g.call(ctx, "POST", "/pilots/"+url.PathEscape(pid)+"/bind", map[string]string{"user": user, "host": host, "team": team}, nil)
}

func (g *Gateway) Reported(ctx context.Context, pid, outcome string) error {
	return g.call(ctx, "POST", "/pilots/"+url.PathEscape(pid)+"/reported", map[string]string{"outcome": outcome}, nil)
}

func (g *Gateway) Credit(ctx context.Context, pid string) (Credit, error) {
	var c Credit
	return c, g.call(ctx, "GET", "/pilots/"+url.PathEscape(pid)+"/credit", nil, &c)
}

func (g *Gateway) Requests(ctx context.Context) ([]Request, error) {
	var out struct {
		Requests []Request `json:"requests"`
	}
	return out.Requests, g.call(ctx, "GET", "/requests", nil, &out)
}

func (g *Gateway) Answer(ctx context.Context, rid string, a Answer) error {
	return g.call(ctx, "POST", "/requests/"+url.PathEscape(rid), a, nil)
}

func (g *Gateway) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway: %s %s: %w", method, pathOnly(path), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("gateway: %s %s: %w", method, pathOnly(path), err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("gateway: %s %s: %w", method, pathOnly(path), err)
		}
	}
	return nil
}

func pathOnly(p string) string {
	p, _, _ = strings.Cut(p, "?")
	return p
}
