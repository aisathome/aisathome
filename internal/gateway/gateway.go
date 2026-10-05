// Package gateway is the ai@home project server: an OpenAI-compatible API for users, a
// registry of nodes connected outward over WebSocket, routing with failover, and metering.
//
// SPDX-License-Identifier: AGPL-3.0-or-later
package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"aihome/internal/protocol"
)

// Config holds gateway settings. In M1 keys and tokens come from a config file; accounts and
// payments come later.
type Config struct {
	GatewayID  string            `json:"gateway_id"`
	APIKeys    map[string]string `json:"api_keys"`    // API key → user id
	NodeTokens map[string]string `json:"node_tokens"` // node token → operator id
	UsageLog   string            `json:"usage_log"`   // JSONL file, one line per request
	// HeartbeatTimeout drops nodes that stay silent for longer than this.
	HeartbeatTimeout time.Duration `json:"-"`
}

// Gateway routes user requests to connected nodes.
type Gateway struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	node map[string]*nodeConn // by node id
	rr   int                  // round-robin cursor
	use  *usageWriter
}

// New creates a gateway.
func New(cfg Config, log *slog.Logger) (*Gateway, error) {
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 45 * time.Second
	}
	uw, err := newUsageWriter(cfg.UsageLog)
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, log: log, node: map[string]*nodeConn{}, use: uw}, nil
}

// NodeCount returns the number of connected nodes.
func (g *Gateway) NodeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.node)
}

// Handler returns the HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /v1/node/connect", g.handleNodeConnect)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

// ---------------------------------------------------------------- nodes

type nodeConn struct {
	id       string
	operator string
	models   []protocol.ModelInfo
	conn     *websocket.Conn
	wmu      sync.Mutex // serialises writes
	mu       sync.Mutex
	pending  map[string]*pendingReq
	active   int
	lastSeen time.Time
	closed   chan struct{}
}

// pendingReq is one request in flight on a node. The reader delivers messages on ch; the
// request handler closes done when it stops listening, so delivery never blocks forever.
type pendingReq struct {
	ch   chan protocol.Msg
	done chan struct{}
}

func (p *pendingReq) deliver(m protocol.Msg) {
	select {
	case p.ch <- m:
	case <-p.done:
	}
}

func (n *nodeConn) send(ctx context.Context, m protocol.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	n.wmu.Lock()
	defer n.wmu.Unlock()
	return n.conn.Write(ctx, websocket.MessageText, b)
}

func (n *nodeConn) slots(model string) int {
	for _, m := range n.models {
		if m.Name == model {
			return m.Slots
		}
	}
	return 0
}

func (g *Gateway) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	operator, ok := g.cfg.NodeTokens[token]
	if !ok {
		http.Error(w, "unknown node token", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ctx := r.Context()

	var hello protocol.Msg
	if err := readMsg(ctx, c, &hello); err != nil || hello.Type != protocol.TypeHello || hello.NodeID == "" {
		c.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	if major(hello.Protocol) != major(protocol.Version) {
		c.Close(websocket.StatusPolicyViolation, "protocol version "+hello.Protocol+" not supported")
		return
	}
	n := &nodeConn{id: hello.NodeID, operator: operator, models: hello.Models, conn: c,
		pending: map[string]*pendingReq{}, lastSeen: time.Now(), closed: make(chan struct{})}
	if err := n.send(ctx, protocol.Msg{Type: protocol.TypeWelcome, GatewayID: g.cfg.GatewayID, Protocol: protocol.Version}); err != nil {
		return
	}
	g.mu.Lock()
	if old := g.node[n.id]; old != nil { // reconnect replaces the stale connection
		old.conn.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	g.node[n.id] = n
	g.mu.Unlock()
	g.log.Info("node connected", "node", n.id, "operator", operator, "models", len(n.models))

	defer func() {
		g.mu.Lock()
		if g.node[n.id] == n {
			delete(g.node, n.id)
		}
		g.mu.Unlock()
		close(n.closed)
		// In-flight requests see n.closed and fail over or report the error themselves.
		g.log.Info("node disconnected", "node", n.id)
	}()

	for {
		rctx, cancel := context.WithTimeout(ctx, g.cfg.HeartbeatTimeout)
		var m protocol.Msg
		err := readMsg(rctx, c, &m)
		cancel()
		if err != nil {
			return
		}
		n.mu.Lock()
		n.lastSeen = time.Now()
		if m.Type == protocol.TypeHeartbeat {
			n.active = m.Active
		}
		p := n.pending[m.ReqID]
		n.mu.Unlock()
		if p != nil && (m.Type == protocol.TypeChunk || m.Type == protocol.TypeDone || m.Type == protocol.TypeError) {
			p.deliver(m) // outside the lock: a slow client must not block other requests
		}
	}
}

// pick chooses a node serving the requested model or role that has a free slot, round-robin.
// It returns the node and the local model name to request.
func (g *Gateway) pick(want string, exclude map[string]bool) (*nodeConn, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.node))
	for id := range g.node {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for i := 0; i < len(ids); i++ {
		n := g.node[ids[(g.rr+i)%len(ids)]]
		if exclude[n.id] {
			continue
		}
		for _, m := range n.models {
			if m.Name != want && !contains(m.Roles, want) {
				continue
			}
			n.mu.Lock()
			free := len(n.pending) < m.Slots
			n.mu.Unlock()
			if free {
				g.rr = (g.rr + i + 1) % len(ids)
				return n, m.Name
			}
		}
	}
	return nil, ""
}

// ---------------------------------------------------------------- user API

func (g *Gateway) user(r *http.Request) (string, bool) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u, ok := g.cfg.APIKeys[key]
	return u, ok
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.user(r); !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	seen := map[string]bool{}
	var data []map[string]any
	g.mu.Lock()
	for _, n := range g.node {
		for _, m := range n.models {
			for _, id := range append([]string{m.Name}, m.Roles...) {
				if !seen[id] {
					seen[id] = true
					data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "ai@home"})
				}
			}
		}
	}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	user, ok := g.user(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	want, _ := body["model"].(string)
	clientStream, _ := body["stream"].(bool)

	tried := map[string]bool{}
	for attempt := 0; attempt < 2; attempt++ { // one retry on another node before any output
		n, model := g.pick(want, tried)
		if n == nil {
			if attempt == 0 {
				apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("no node available for model %q", want))
			} else {
				apiError(w, http.StatusBadGateway, "request failed on all available nodes")
			}
			return
		}
		tried[n.id] = true
		started, err := g.run(w, r, user, n, model, want, body, clientStream)
		if err == nil || started {
			return // success, or failure after output began: the client sees the error
		}
		g.log.Warn("node failed before output, retrying", "node", n.id, "err", err)
	}
	apiError(w, http.StatusBadGateway, "request failed on all available nodes")
}

// run sends one request to a node and relays the result. started reports whether any
// output reached the client (then a retry is no longer possible).
func (g *Gateway) run(w http.ResponseWriter, r *http.Request, user string, n *nodeConn, model, want string,
	body map[string]any, clientStream bool) (started bool, err error) {
	reqID := newID()
	body["model"] = model
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	raw, _ := json.Marshal(body)

	p := &pendingReq{ch: make(chan protocol.Msg, 64), done: make(chan struct{})}
	ch := p.ch
	n.mu.Lock()
	n.pending[reqID] = p
	n.mu.Unlock()
	defer func() {
		close(p.done)
		n.mu.Lock()
		delete(n.pending, reqID)
		n.mu.Unlock()
	}()

	t0 := time.Now()
	if err := n.send(r.Context(), protocol.Msg{Type: protocol.TypeRequest, ReqID: reqID, Body: raw}); err != nil {
		return false, err
	}

	var sse *bufio.Writer
	var flusher http.Flusher
	var collected []json.RawMessage
	for {
		select {
		case <-r.Context().Done(): // client went away: tell the node to stop
			_ = n.send(context.Background(), protocol.Msg{Type: protocol.TypeCancel, ReqID: reqID})
			return started, r.Context().Err()
		case <-n.closed:
			return started, errors.New("node disconnected")
		case m := <-ch:
			switch m.Type {
			case protocol.TypeChunk:
				if !clientStream {
					collected = append(collected, m.Data)
					continue
				}
				if sse == nil {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					w.WriteHeader(http.StatusOK)
					sse = bufio.NewWriter(w)
					flusher, _ = w.(http.Flusher)
				}
				started = true
				fmt.Fprintf(sse, "data: %s\n\n", m.Data)
				sse.Flush()
				if flusher != nil {
					flusher.Flush()
				}
			case protocol.TypeError:
				if started {
					fmt.Fprintf(sse, "data: %s\n\n", mustJSON(map[string]any{"error": map[string]any{"message": m.Error}}))
					sse.Flush()
				}
				return started, errors.New(m.Error)
			case protocol.TypeDone:
				u := protocol.Usage{}
				if m.Usage != nil {
					u = *m.Usage
				}
				u.DurationMs = time.Since(t0).Milliseconds()
				g.use.record(usageRecord{Time: time.Now().UTC(), User: user, Node: n.id, Operator: n.operator,
					Model: model, Requested: want, Usage: u, ReqID: reqID})
				if clientStream {
					if sse == nil { // empty stream: still answer properly
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						sse = bufio.NewWriter(w)
					}
					fmt.Fprint(sse, "data: [DONE]\n\n")
					sse.Flush()
				} else {
					writeJSON(w, http.StatusOK, assemble(reqID, model, collected, u))
				}
				return true, nil
			}
		}
	}
}

// assemble turns streamed chunks into one non-streaming chat.completion object.
func assemble(id, model string, chunks []json.RawMessage, u protocol.Usage) map[string]any {
	var content, reasoning strings.Builder
	finish := "stop"
	for _, c := range chunks {
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(c, &ch) != nil {
			continue
		}
		for _, choice := range ch.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	return map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
			"total_tokens":          u.PromptTokens + u.CompletionTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CachedTokens}},
	}
}

// ---------------------------------------------------------------- metering

type usageRecord struct {
	Time      time.Time      `json:"time"`
	ReqID     string         `json:"req_id"`
	User      string         `json:"user"`
	Node      string         `json:"node"`
	Operator  string         `json:"operator"`
	Model     string         `json:"model"`
	Requested string         `json:"requested"`
	Usage     protocol.Usage `json:"usage"`
}

type usageWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newUsageWriter(path string) (*usageWriter, error) {
	if path == "" {
		return &usageWriter{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	return &usageWriter{f: f}, nil
}

func (u *usageWriter) record(r usageRecord) {
	if u.f == nil {
		return
	}
	b, _ := json.Marshal(r)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.f.Write(append(b, '\n'))
}

// ---------------------------------------------------------------- helpers

func readMsg(ctx context.Context, c *websocket.Conn, m *protocol.Msg) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": http.StatusText(code)}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func major(v string) string { return strings.SplitN(v, ".", 2)[0] }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
