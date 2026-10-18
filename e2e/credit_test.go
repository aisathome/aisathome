// Credit (backlog 18): tokens counted by the gateway, cache hits decided by it, no credit for
// serving oneself, the ledger across restarts, usernames as consent to appear publicly, the
// leaderboard and the BOINC-format exports.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/protocol"
)

// miniTokenizer is Qwen3.8's tokenizer cut down to a few hundred merges (gateway/testdata):
// enough to count every text, with real ids for the reference strings.
const miniTokenizer = "../gateway/testdata/qwen38-mini-tokenizer.json"

const effortXHigh = "Reasoning effort is set to xhigh. Please think carefully through the task, validate key assumptions, consider plausible alternatives, and prioritize correctness, consistency, and clarity in the final answer."

// lyingEngine answers "Hello, world" like mockEngine but claims a huge, fully cached prompt
// and a long answer.
func lyingEngine(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"Hello", ", ", "world"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", piece)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":90000,\"completion_tokens\":9000},\"timings\":{\"cache_n\":90000,\"prompt_n\":0}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func tokensOf(t *testing.T, u map[string]any) (prompt, cached, completion int) {
	t.Helper()
	tk, ok := u["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("usage record without tokens: %v", u)
	}
	return int(tk["prompt"].(float64)), int(tk["cached"].(float64)), int(tk["completion"].(float64))
}

func creditOf(u map[string]any) float64 { c, _ := u["credit"].(float64); return c }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// The gateway counts the tokens itself: the node's claims change nothing.
func TestCreditCountedByGateway(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) { c.TokenizerFile = miniTokenizer }, lyingEngine(t))
	tok, err := gateway.LoadTokenizer(miniTokenizer, "")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := ask(t, r, "user-key", chatBody); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("%d %s", code, body)
	}
	u := waitUsage(t, r, 1)[0]
	prompt, cached, completion := tokensOf(t, u)
	wantPrompt := tok.Count("<|im_start|>system\n" + effortXHigh + "<|im_end|>\n<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\n<think>\n")
	wantOut := tok.Count("\n</think>\n\nHello, world") + 1 // and <|im_end|>
	if u["counted"] != "gateway" || prompt != wantPrompt || cached != 0 || completion != wantOut {
		t.Fatalf("counted %v: prompt %d (want %d), cached %d (want 0), completion %d (want %d)",
			u["counted"], prompt, wantPrompt, cached, completion, wantOut)
	}
	if want := float64(prompt)*gateway.CreditPerPromptToken + float64(completion)*gateway.CreditPerOutputToken; !near(creditOf(u), want) {
		t.Fatalf("credit %v, want %v", creditOf(u), want)
	}
	// The node's own numbers are kept for comparison (clamped as before), not used.
	if us := u["usage"].(map[string]any); us["completion_tokens"].(float64) <= float64(completion) {
		t.Fatalf("node usage: %v", us)
	}
	// The public numbers use the gateway's counts too.
	s, raw := getStats(t, r.gw.URL)
	if s["today"].(map[string]any)["completion_tokens"].(float64) != float64(completion) {
		t.Fatalf("stats: %s", raw)
	}
}

// Without a tokenizer: the node's counts, clamped, marked counted "node"; the cache is still
// the gateway's decision.
func TestCreditWithoutTokenizer(t *testing.T) {
	r := newRig(t, mockEngine(t, false)) // reports 12 prompt tokens, 8 of them cached, 3 out
	ask(t, r, "user-key", chatBody)
	u := waitUsage(t, r, 1)[0]
	prompt, cached, completion := tokensOf(t, u)
	if u["counted"] != "node" || prompt != 12 || cached != 0 || completion != 3 {
		t.Fatalf("counted %v: %d/%d/%d", u["counted"], prompt, cached, completion)
	}
	if !near(creditOf(u), 12*gateway.CreditPerPromptToken+3*gateway.CreditPerOutputToken) {
		t.Fatalf("credit %v", creditOf(u))
	}
}

func convBody(system string, turns ...string) string {
	msgs := []map[string]any{{"role": "system", "content": system}}
	for i, t := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": t})
	}
	b, _ := json.Marshal(map[string]any{"model": "deep", "stream": true, "messages": msgs})
	return string(b)
}

// A prompt counts as cached only up to what the same node served the same requester just
// before.
func TestCreditCacheDecidedByGateway(t *testing.T) {
	r := newRigCfg(t, func(c *gateway.Config) { c.TokenizerFile = miniTokenizer }, lyingEngine(t))
	sys := strings.Repeat("You are a careful assistant who answers briefly. ", 40)
	ask(t, r, "user-key", convBody(sys, "first question"))
	ask(t, r, "user-key", convBody(sys, "first question", "Hello, world", "second question"))
	ask(t, r, "agent-key", convBody(sys, "first question"))
	u := waitUsage(t, r, 3)
	p1, c1, _ := tokensOf(t, u[0])
	p2, c2, _ := tokensOf(t, u[1])
	p3, c3, _ := tokensOf(t, u[2])
	if c1 != 0 || c3 != 0 {
		t.Fatalf("a first request counted cached tokens: %d, %d (the node claimed all)", c1, c3)
	}
	if c2 < p1*3/4 || c2 >= p2 {
		t.Fatalf("follow-up: %d of %d cached, first prompt %d", c2, p2, p1)
	}
	if p3 != p1 || creditOf(u[1]) >= creditOf(u[0])+float64(p2-p1)*gateway.CreditPerPromptToken+1 {
		t.Fatalf("credits %v %v %v", creditOf(u[0]), creditOf(u[1]), creditOf(u[2]))
	}
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", u, resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(v)
}

type board struct {
	Total      struct{ Credit float64 }
	Volunteers struct {
		Count  int
		Nodes  int
		Credit float64
	}
	Anonymous struct {
		Count  int
		Credit float64
	}
	Maintainer struct {
		Nodes  int
		Credit float64
	}
	Top []struct {
		Rank  int
		Name  string
		Total float64 `json:"total_credit"`
		Nodes int
	}
}

func gunzipURL(t *testing.T, u string) (string, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %d not gzip: %v", u, resp.StatusCode, err)
	}
	out, _ := io.ReadAll(z)
	return string(out), resp.Header.Get("ETag")
}

// The whole volunteer flow: credit for serving others, none for serving oneself, shown on the
// account page; anonymous until a username is set; then on the leaderboard and in the
// exports, and off them again when it is cleared.
func TestCreditVolunteerFlow(t *testing.T) {
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.TokenizerFile = miniTokenizer
		c.StatsRefresh = time.Nanosecond
		c.NodeTokens["home-token"] = gateway.NodeToken{Operator: "owner", Trust: "trusted"}
	})
	s1, _ := a.signIn(t, 1)
	token, nodeID, _ := a.addNode(t, s1, "my 3090")
	_, page := a.createKey(t, s1)
	key1 := keyRe.FindString(page)
	s2, _ := a.signIn(t, 2)
	_, page = a.createKey(t, s2)
	key2 := keyRe.FindString(page)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startTokenNode(ctx, token, mockEngine(t, false), 2, 32768)
	a.waitNodes(t, 1)

	ask(t, a.rig, key1, chatBody) // its own node: no credit
	ask(t, a.rig, key2, chatBody) // someone else's request: credit to account 1
	u := waitUsage(t, a.rig, 2)
	if u[0]["self_served"] != true || creditOf(u[0]) != 0 {
		t.Fatalf("self-served answer earned credit: %v", u[0])
	}
	earned := creditOf(u[1])
	if u[1]["self_served"] == true || earned <= 0 || u[1]["node"] != nodeID {
		t.Fatalf("answer for another account: %v", u[1])
	}
	// The account page shows it, per node and for the account; the requester has none.
	want := fmt.Sprintf("%.2f", earned)
	if _, page := a.get(t, "/account", s1); strings.Count(page, want) < 2 || !strings.Contains(page, "Credit today") {
		t.Fatalf("account page lacks credit %s:\n%s", want, page)
	}
	if _, page := a.get(t, "/account", s2); strings.Contains(page, "in total") {
		t.Fatal("the requester's page shows credit")
	}

	// Anonymous: in the totals only, never by name or id.
	var b board
	getJSON(t, a.gw.URL+"/v1/stats/credit", &b)
	if len(b.Top) != 0 || b.Anonymous.Count != 1 || b.Volunteers.Nodes != 1 || !near(b.Volunteers.Credit, math.Round(earned*10)/10) {
		t.Fatalf("anonymous board: %+v", b)
	}
	users, _ := gunzipURL(t, a.gw.URL+"/stats/user.gz")
	hosts, _ := gunzipURL(t, a.gw.URL+"/stats/host.gz")
	if strings.Contains(users, "<user>") || strings.Contains(hosts, "<host>") {
		t.Fatalf("an anonymous volunteer is exported:\n%s\n%s", users, hosts)
	}

	// A username is the consent to appear.
	_, page = a.get(t, "/account", s1)
	if !strings.Contains(page, "Shown publicly in the leaderboard and in the BOINC-format statistics, with your credit. Leave it empty to stay anonymous.") {
		t.Fatal("the username field lacks its explanation")
	}
	a.post(t, "/account/username", s1, "username="+url.QueryEscape("  Crunch   Queen "))
	getJSON(t, a.gw.URL+"/v1/stats/credit", &b)
	if len(b.Top) != 1 || b.Top[0].Name != "Crunch Queen" || b.Top[0].Rank != 1 || b.Top[0].Nodes != 1 || b.Anonymous.Count != 0 {
		t.Fatalf("named board: %+v", b)
	}
	users, etag := gunzipURL(t, a.gw.URL+"/stats/user.gz")
	hosts, _ = gunzipURL(t, a.gw.URL+"/stats/host.gz")
	if !strings.Contains(users, "<name>Crunch Queen</name>") || strings.Count(users, "<user>") != 1 || etag == "" ||
		strings.Count(hosts, "<host>") != 1 || strings.Contains(hosts, nodeID) || strings.Contains(hosts, "my 3090") {
		t.Fatalf("exports:\n%s\n%s", users, hosts)
	}
	for _, leak := range []string{"gh:1", "gh-1", "acct-", nodeID, "my 3090"} {
		if raw := fetch(t, a.gw.URL+"/v1/stats/credit"); strings.Contains(raw, leak) {
			t.Fatalf("leaderboard leaks %q: %s", leak, raw)
		}
	}
	// Usernames are unique case-insensitively, and no web addresses.
	for name, msg := range map[string]string{"crunch QUEEN": "taken", "example.com": "web address", "a<b>": "may use letters"} {
		_, page := a.post(t, "/account/username", s2, "username="+url.QueryEscape(name))
		if !strings.Contains(page, msg) {
			t.Fatalf("username %q: page lacks %q", name, msg)
		}
	}
	// Clearing it takes the account out again.
	a.post(t, "/account/username", s1, "username=")
	getJSON(t, a.gw.URL+"/v1/stats/credit", &b)
	users, _ = gunzipURL(t, a.gw.URL+"/stats/user.gz")
	if len(b.Top) != 0 || strings.Contains(users, "Crunch") {
		t.Fatalf("a cleared username is still public: %+v\n%s", b, users)
	}
	if raw := fetch(t, a.gw.URL+"/stats/tables.xml"); !strings.Contains(raw, "<tables>") || strings.Contains(raw, "nusers_total") {
		t.Fatalf("tables.xml: %s", raw)
	}
}

func fetch(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Credit survives a restart (state_dir), and config-token nodes earn for their operator,
// apart from volunteers.
func TestCreditSurvivesRestart(t *testing.T) {
	state := t.TempDir()
	mod := func(c *gateway.Config) { c.StateDir = state; c.StatsRefresh = time.Nanosecond }
	r := newRigCfg(t, mod, mockEngine(t, false))
	for i := 0; i < 3; i++ {
		ask(t, r, "user-key", chatBody)
	}
	waitUsage(t, r, 3)
	var b board
	getJSON(t, r.gw.URL+"/v1/stats/credit", &b)
	if b.Maintainer.Credit <= 0 || b.Maintainer.Nodes != 1 || b.Volunteers.Credit != 0 || b.Total.Credit != b.Maintainer.Credit {
		t.Fatalf("operator credit: %+v", b)
	}
	before := b.Total.Credit
	r.cancel()
	r.gw.Close()
	if err := r.g.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := newRigCfg(t, func(c *gateway.Config) { mod(c); c.UsageLog = filepath.Join(t.TempDir(), "u.jsonl") })
	getJSON(t, r2.gw.URL+"/v1/stats/credit", &b)
	if b.Total.Credit != before {
		t.Fatalf("credit after restart: %v, want %v", b.Total.Credit, before)
	}
}

// An answer that breaks off on the node's side earns nothing, though its partial use is
// logged (TestGuardStopsDangerousToolCalls checks the same for a blocked answer).
func TestCreditOnlyForDeliveredAnswers(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"Hello", ", ", "wor"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", piece)
		}
		w.(http.Flusher).Flush() // and the stream ends without [DONE]
	}))
	defer broken.Close()
	r := newRigCfg(t, func(c *gateway.Config) { c.TokenizerFile = miniTokenizer }, broken)
	ask(t, r, "user-key", chatBody)
	u := waitUsage(t, r, 1)[0]
	if u["outcome"] != "failed" || u["tokens"] == nil {
		t.Fatalf("expected a metered failure: %v", u)
	}
	if _, _, c := tokensOf(t, u); c == 0 || creditOf(u) != 0 {
		t.Fatalf("a failed answer earned %v (completion %d)", creditOf(u), c)
	}
}

// No credit if no value is delivered (owner, 2026-10-18): a cancelled answer earns credit only
// for what reached the client, and nothing at all, not even for its prompt, if nothing did.
func TestCreditOnlyForWhatReachedTheClient(t *testing.T) {
	stream := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	has := func(s string) func([]byte) bool { return func(p []byte) bool { return strings.Contains(string(p), s) } }
	for _, c := range []struct {
		name      string
		tokenizer bool
		body      string
		gone      func([]byte) bool // the client is gone from this write on (it fails)
		delivered string            // the answer text that reached the client
		chunks    int               // its text chunks (credit without a tokenizer)
	}{
		{"not streamed, the answer's write fails", true, `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`,
			func([]byte) bool { return true }, "", 0},
		{"streamed, gone before the first token", true, stream, has(`"Hello"`), "", 0},
		// (Not cut before "world": "Hello, " and "Hello, world" are the same number of tokens.)
		{"streamed, gone after part of the answer", true, stream, has(`{"content":", "}`), "Hello", 1},
		{"streamed, gone after part of the answer, node counts", false, stream, has(`{"content":", "}`), "Hello", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, func(cfg *gateway.Config) {
				if c.tokenizer {
					cfg.TokenizerFile = miniTokenizer
				}
			}, mockEngine(t, false))
			cut := cutClient(t, r.g.Handler(), true, c.gone)
			req, _ := http.NewRequest("POST", cut.URL+"/v1/chat/completions", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer user-key")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			u := waitUsage(t, r, 1)[0]
			if u["outcome"] != "cancelled" {
				t.Fatalf("outcome %v, want cancelled: %v", u["outcome"], u)
			}
			prompt, _, completion := tokensOf(t, u)
			want, wantCredit := 0, 0.0
			if c.delivered != "" {
				want = c.chunks
				if c.tokenizer {
					tok, err := gateway.LoadTokenizer(miniTokenizer, "")
					if err != nil {
						t.Fatal(err)
					}
					want = tok.Count("\n</think>\n\n" + c.delivered)
				}
				wantCredit = float64(prompt)*gateway.CreditPerPromptToken + float64(want)*gateway.CreditPerOutputToken
			}
			if completion != want || !near(creditOf(u), wantCredit) {
				t.Fatalf("completion %d (want %d), credit %v (want %v): %v", completion, want, creditOf(u), wantCredit, u)
			}
		})
	}
}

// wantDelivered checks a cancelled record's credit: delivered is the answer text that reached
// the client ("" = none, and then no credit at all).
func wantDelivered(t *testing.T, u map[string]any, delivered string) {
	t.Helper()
	if u["outcome"] != "cancelled" {
		t.Fatalf("outcome %v, want cancelled: %v", u["outcome"], u)
	}
	prompt, _, completion := tokensOf(t, u)
	want, wantCredit := 0, 0.0
	if delivered != "" {
		tok, err := gateway.LoadTokenizer(miniTokenizer, "")
		if err != nil {
			t.Fatal(err)
		}
		want = tok.Count("\n</think>\n\n" + delivered)
		wantCredit = float64(prompt)*gateway.CreditPerPromptToken + float64(want)*gateway.CreditPerOutputToken
	}
	if completion != want || !near(creditOf(u), wantCredit) {
		t.Fatalf("completion %d (want %d), credit %v (want %v): %v", completion, want, creditOf(u), wantCredit, u)
	}
}

// Text the secret restorer holds back (it could be the start of a placeholder) has not reached
// the client: a cancelled answer earns nothing for it, and for text it released in part, only
// for that part, counted as the node wrote it (external review, 2026-10-18).
func TestCreditOnlyForTextTheRestorerReleased(t *testing.T) {
	for _, c := range []struct{ name, first, delivered string }{
		{"all held back", "sk-proj-AI", ""},
		{"partly released", "Hello sk-proj-AI", "Hello "},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, func(cfg *gateway.Config) { cfg.TokenizerFile = miniTokenizer })
			scriptNode(t, r, func(_ *websocket.Conn, write func(protocol.Msg), id string) {
				write(chunkMsg(id, c.first))
				write(chunkMsg(id, "x, and more"))
				write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 20, CompletionTokens: 4}})
			})
			var writes atomic.Int32 // the client takes the first chunk and is gone at the second
			cut := cutClient(t, r.g.Handler(), true, func([]byte) bool { return writes.Add(1) == 2 })
			body, _ := json.Marshal(map[string]any{"model": "deep", "stream": true,
				"messages": []any{map[string]any{"role": "user", "content": "use " + secret}}})
			req, _ := http.NewRequest("POST", cut.URL+"/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer restore-key")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			wantDelivered(t, waitUsage(t, r, 1)[0], c.delivered)
		})
	}
}

// A client whose request ends while the end of the answer goes out, though the writes still
// succeed (a closed connection takes them for a while), did not get the answer: it is recorded
// as cancelled, credited for what was written, without the end of turn (external review,
// 2026-10-18). With secrets restored, text held back to the end of the stream counts once it
// was written.
func TestClientGoneDuringTheEnd(t *testing.T) {
	stream := `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	atDone := func(p []byte) bool { return strings.Contains(string(p), "[DONE]") }
	withSecret, _ := json.Marshal(map[string]any{"model": "deep", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "use " + secret}}})
	for _, c := range []struct {
		name      string
		key, body string
		gone      func([]byte) bool
		delivered string
	}{
		{"streamed", "user-key", stream, atDone, "Hello, world"},
		{"not streamed", "user-key", `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`,
			func([]byte) bool { return true }, "Hello, world"},
		{"streamed, held-back text flushed at the end", "restore-key", string(withSecret), atDone, "Hello sk-proj-AI"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRigCfg(t, func(cfg *gateway.Config) { cfg.TokenizerFile = miniTokenizer })
			if c.key == "restore-key" {
				scriptNode(t, r, func(_ *websocket.Conn, write func(protocol.Msg), id string) {
					write(chunkMsg(id, "Hello sk-proj-AI")) // and no finish_reason: flushed after the stream
					write(protocol.Msg{Type: protocol.TypeDone, ReqID: id, Usage: &protocol.Usage{PromptTokens: 20, CompletionTokens: 4}})
				})
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				r.startNode(ctx, "node-0", "node-token", mockEngine(t, false), 1)
				r.waitNodes(t, 1)
			}
			cut := cutClient(t, r.g.Handler(), false, c.gone)
			req, _ := http.NewRequest("POST", cut.URL+"/v1/chat/completions", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+c.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(b), "Hello") { // the writes did succeed
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			wantDelivered(t, waitUsage(t, r, 1)[0], c.delivered)
		})
	}
}
