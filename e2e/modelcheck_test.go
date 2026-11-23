// The gateway's model check end to end (gateway/modelcheck.go): real node agents in front of fake
// engines, one that computes like the reference ("the right model"), one whose probabilities are
// another model's, one that answers without logprobs (a proxy). Probes earn nothing and are in no
// usage record; withholding holds a failing node only after the observing period.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node/modelcheck"
)

const mcModel = "qwen3.8-27b-q4" // the rig's nodes announce this model

// mcReference writes a synthetic secret reference of 20 prompts and returns its config entry.
func mcReference(t *testing.T) (gateway.ModelCheckReference, *modelcheck.Reference) {
	t.Helper()
	return mcReferenceOf(t, "p", "Tell me about secret topic %d.")
}

// mcReferenceOf is mcReference with its own prompt ids (id prefix) and texts (a format with %d).
func mcReferenceOf(t *testing.T, idPrefix, textFormat string) (gateway.ModelCheckReference, *modelcheck.Reference) {
	t.Helper()
	ref := &modelcheck.Reference{Format: modelcheck.Format, Kind: modelcheck.KindReference, Name: "e2e-secret",
		Created: "2026-11-20", Engine: modelcheck.Engine{Name: "llama.cpp", Build: "b12342"},
		Model: modelcheck.Model{File: "Qwen3.8-27B-UD-IQ4_XS.gguf"}, GPU: modelcheck.GPU{Name: "test"}, Request: modelcheck.DefaultRequest}
	for i := 0; i < 20; i++ {
		var toks []modelcheck.Token
		for p := 0; p < 32; p++ {
			var top []modelcheck.Alt
			for k := 0; k < 20; k++ {
				lp := math.Log(0.3 / 19)
				if k == 0 {
					lp = math.Log(0.7)
				}
				top = append(top, modelcheck.Alt{Key: hex.EncodeToString(fmt.Appendf(nil, " w%d_%d_%d", i, p, k)), LP: lp})
			}
			toks = append(toks, modelcheck.Token{Key: top[0].Key, Top: top})
		}
		ref.Results = append(ref.Results, modelcheck.Result{ID: fmt.Sprintf("%s%02d", idPrefix, i), Text: fmt.Sprintf(textFormat, i), Tokens: toks})
	}
	b, err := ref.Encode(true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secret.json.gz")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return gateway.ModelCheckReference{Model: mcModel, File: path, SHA256: modelcheck.SHA256(b)}, ref
}

// mcEngine imitates llama-server: a prompt of the reference is answered with its recorded
// tokens (then a few more, as an answer with a large max_tokens goes on), streamed one token per
// chunk with top-20 logprobs when asked; skew makes the probabilities another model's, noLP drops
// them. Anything else (people's requests) gets "Hello, world". It records the bodies it got.
type mcEngine struct {
	ref       *modelcheck.Reference
	skew      bool
	noLP      bool
	mu        sync.Mutex
	probes    []map[string]any
	people    atomic.Int32
	coverSeen atomic.Int32 // people's requests that asked for logprobs
	toolCmd   string       // if set, a request that is no probe gets a bash tool call running it
}

func bytesList(key string) []int {
	b, _ := hex.DecodeString(key)
	out := make([]int, len(b))
	for i, c := range b {
		out[i] = int(c)
	}
	return out
}

func (e *mcEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if health(w, r) {
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	text := ""
	if msgs, _ := body["messages"].([]any); len(msgs) > 0 {
		m, _ := msgs[len(msgs)-1].(map[string]any)
		text, _ = m["content"].(string)
	}
	var res *modelcheck.Result
	for i := range e.ref.Results {
		if e.ref.Results[i].Text == text {
			res = &e.ref.Results[i]
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flush := func() { w.(http.Flusher).Flush() }
	if res == nil && e.toolCmd != "" {
		e.people.Add(1)
		a, _ := json.Marshal(map[string]string{"command": e.toolCmd})
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": string(a)}}}}}}})
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":null},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
		return
	}
	if res == nil {
		e.people.Add(1)
		if body["logprobs"] == true {
			e.coverSeen.Add(1)
		}
		for _, piece := range []string{"Hello", ", ", "world"} {
			lp := ""
			if body["logprobs"] == true {
				lp = fmt.Sprintf(`,"logprobs":{"content":[{"id":1,"token":%q,"bytes":[72],"logprob":-0.1,"top_logprobs":[{"id":1,"token":%q,"bytes":[72],"logprob":-0.1}]}]}`, piece, piece)
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null%s}]}\n\n", piece, lp)
			flush()
		}
	} else {
		e.mu.Lock()
		e.probes = append(e.probes, body)
		e.mu.Unlock()
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":null},\"finish_reason\":null}]}\n\n")
		toks := append(append([]modelcheck.Token(nil), res.Tokens...), res.Tokens[:5]...) // goes on past the reference
		for p, tk := range toks {
			raw, _ := hex.DecodeString(tk.Key)
			chunk := map[string]any{"index": 0, "delta": map[string]any{"content": string(raw)}, "finish_reason": nil}
			if !e.noLP {
				var top []map[string]any
				lp := 0.0
				for k, a := range tk.Top {
					v := a.LP
					if e.skew && p < 32 { // another model: same tokens, far less sure
						v = math.Log(0.6 / 19)
						if k == 0 {
							v = math.Log(0.4)
						}
					}
					if a.Key == tk.Key {
						lp = v
					}
					top = append(top, map[string]any{"id": k, "token": "", "bytes": bytesList(a.Key), "logprob": v})
				}
				chunk["logprobs"] = map[string]any{"content": []any{map[string]any{"id": 1, "token": string(raw), "bytes": bytesList(tk.Key), "logprob": lp, "top_logprobs": top}}}
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{chunk}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flush()
		}
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func (e *mcEngine) probeBodies() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.probes...)
}

// mcRig starts a gateway with the model check and one node per engine (node-0, node-1, ...).
func mcRig(t *testing.T, mod func(*gateway.ModelCheckConfig), engines ...*mcEngine) *rig {
	t.Helper()
	entry, _ := mcReference(t)
	servers := make([]*httptest.Server, len(engines))
	for i, e := range engines {
		servers[i] = httptest.NewServer(e)
		t.Cleanup(servers[i].Close)
	}
	return newRigCfg(t, func(c *gateway.Config) {
		share := 1.0
		mc := &gateway.ModelCheckConfig{Enabled: true, References: []gateway.ModelCheckReference{entry},
			Tick: 10 * time.Millisecond, Interval: 20 * time.Millisecond, Recheck: 20 * time.Millisecond, CoverShare: &share}
		if mod != nil {
			mod(mc)
		}
		c.ModelCheck = mc
	}, servers...)
}

// waitChecks waits until every node named has at least n checks.
func waitChecks(t *testing.T, r *rig, n int, nodes ...string) gateway.ModelCheckState {
	t.Helper()
	var st gateway.ModelCheckState
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		st, _ = r.g.ModelCheckSnapshot()
		ok := true
		for _, id := range nodes {
			if s := st.Nodes[id]; s == nil || len(s.Checks) < n {
				ok = false
			}
		}
		if ok {
			return st
		}
	}
	t.Fatalf("fewer than %d checks per node in time: %+v", n, st.Nodes)
	return st
}

func TestModelCheckCatchesTheWrongModel(t *testing.T) {
	_, ref := mcReference(t)
	right, wrong, proxy := &mcEngine{ref: ref}, &mcEngine{ref: ref, skew: true}, &mcEngine{ref: ref, noLP: true}
	r := mcRig(t, nil, right, wrong, proxy)
	// mcRig's reference is a fresh file with the same content as ref (same prompts and answers).
	st := waitChecks(t, r, 3, "node-0", "node-1", "node-2")
	th := st.Thresholds
	if res, worst, flips := st.Nodes["node-0"].Status(th); res != gateway.MCPass || worst > 1e-9 || flips != 0 {
		t.Errorf("the right model: %s, KL %v, flips %d: %+v", res, worst, flips, st.Nodes["node-0"].Checks)
	}
	if res, worst, _ := st.Nodes["node-1"].Status(th); res != gateway.MCFail || worst < th.MaxCheckKL {
		t.Errorf("another model: %s, KL %v", res, worst)
	}
	if res, _, _ := st.Nodes["node-2"].Status(th); res != gateway.MCFail || !strings.Contains(strings.Join(st.Nodes["node-2"].Checks[0].Reasons, " "), "without logprobs") {
		t.Errorf("a proxy without logprobs: %s %+v", res, st.Nodes["node-2"].Checks)
	}
	// Observe-only: nothing is held, everyone still serves.
	for _, id := range []string{"node-0", "node-1", "node-2"} {
		if !st.Nodes[id].HeldAt.IsZero() {
			t.Errorf("%s held while observing", id)
		}
	}
	// A probe looks like a request without max_tokens: the gateway's default, streamed, no tools,
	// one user message; it asks for greedy top-20 logprobs and no thinking. Nothing else.
	allowed := map[string]bool{"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true,
		"temperature": true, "logprobs": true, "top_logprobs": true, "chat_template_kwargs": true}
	bodies := right.probeBodies()
	if len(bodies) < 12 {
		t.Fatalf("%d probes", len(bodies))
	}
	seen := map[string]int{}
	for _, b := range bodies {
		for k := range b {
			if !allowed[k] {
				t.Errorf("probe body has %q: %v", k, b)
			}
		}
		if b["stream"] != true || b["max_tokens"] != float64(16384) || b["temperature"] != float64(0) || b["top_logprobs"] != float64(20) ||
			b["model"] != mcModel || len(b["messages"].([]any)) != 1 {
			t.Errorf("probe body %v", b)
		}
		if kw, _ := b["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
			t.Errorf("probe thinking: %v", b["chat_template_kwargs"])
		}
		seen[b["messages"].([]any)[0].(map[string]any)["content"].(string)]++
	}
	// No prompt twice to one node before the pool of 20 is used up.
	if len(bodies) <= 20 {
		for p, n := range seen {
			if n > 1 {
				t.Errorf("prompt %q sent %d times to one node within the pool", p, n)
			}
		}
	}
	// Probes earn nothing and are in no usage record.
	if u := r.usage(t); len(u) != 0 {
		t.Errorf("probes made %d usage records: %v", len(u), u)
	}
	// People's requests still work beside the check; they are metered as before. A streamed one
	// without tools carries the cover logprobs to the node (cover_share 1 here), never to the client.
	for i := 0; i < 6; i++ {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(sb.String(), "Hello") || strings.Contains(sb.String(), "logprobs") {
			t.Fatalf("a person's request: %d %s", resp.StatusCode, sb.String())
		}
	}
	waitUsage(t, r, 6)
	// Cover goes only to a node that passed a check (its engine answers with logprobs): here the
	// right one, on every request it got at share 1, and never to the failing ones.
	if right.people.Load() == 0 || right.coverSeen.Load() != right.people.Load() || wrong.coverSeen.Load()+proxy.coverSeen.Load() != 0 {
		t.Errorf("cover logprobs on %d of %d, %d, %d requests", right.coverSeen.Load(), right.people.Load(), wrong.coverSeen.Load(), proxy.coverSeen.Load())
	}
	// A request that asks for logprobs itself gets them, as before.
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","messages":[{"role":"user","content":"hi"}],"logprobs":true,"top_logprobs":1}`)
	var out struct {
		Choices []struct {
			Logprobs struct {
				Content []map[string]any `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Choices) == 0 || len(out.Choices[0].Logprobs.Content) != 3 {
		t.Errorf("logprobs asked for did not arrive: %+v", out)
	}
}

func TestModelCheckWithholds(t *testing.T) {
	_, ref := mcReference(t)
	var offset atomic.Int64 // the check's clock runs ahead by this much
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	right, wrong := &mcEngine{ref: ref}, &mcEngine{ref: ref, skew: true}
	r := mcRig(t, func(c *gateway.ModelCheckConfig) { c.Withhold = true; c.Now = clock }, right, wrong)
	st := waitChecks(t, r, 3, "node-0", "node-1")
	if res, _, _ := st.Nodes["node-1"].Status(st.Thresholds); res != gateway.MCFail || !st.Nodes["node-1"].HeldAt.IsZero() {
		t.Fatalf("before 3 days of observing the failing node must be recorded, not held: %s %v", res, st.Nodes["node-1"].HeldAt)
	}
	// Three days later the next failed check holds it: no more requests reach it.
	offset.Store(int64(gateway.ModelCheckObserveFirst + time.Minute))
	waitFor(t, func() bool {
		st, _ := r.g.ModelCheckSnapshot()
		return !st.Nodes["node-1"].HeldAt.IsZero()
	})
	st, _ = r.g.ModelCheckSnapshot()
	if !st.Nodes["node-0"].HeldAt.IsZero() {
		t.Error("the right model was held")
	}
	time.Sleep(100 * time.Millisecond) // the hold is applied off the check's lock
	before := wrong.people.Load()
	for i := 0; i < 4; i++ {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if wrong.people.Load() != before {
		t.Errorf("a held node got %d people's requests", wrong.people.Load()-before)
	}
	// A held node gets no more probes either.
	n := len(wrong.probeBodies())
	time.Sleep(300 * time.Millisecond)
	if got := len(wrong.probeBodies()); got != n {
		t.Errorf("a held node got %d more probes", got-n)
	}
}

// Admins see each node's result on /account/ops; others do not get the page.
func TestModelCheckOnTheOpsPage(t *testing.T) {
	entry, ref := mcReference(t)
	right, wrong := httptest.NewServer(&mcEngine{ref: ref}), httptest.NewServer(&mcEngine{ref: ref, skew: true})
	t.Cleanup(right.Close)
	t.Cleanup(wrong.Close)
	a := newAcctRigNodes(t, 100, nil, func(c *gateway.Config) {
		c.ModelCheck = &gateway.ModelCheckConfig{Enabled: true, References: []gateway.ModelCheckReference{entry},
			Tick: 10 * time.Millisecond, Interval: 20 * time.Millisecond, Recheck: 20 * time.Millisecond}
	}, right, wrong)
	waitChecks(t, a.rig, 2, "node-0", "node-1")
	admin, _ := a.signIn(t, 1)
	user, _ := a.signIn(t, 2)
	_, page := a.get(t, "/account/ops", admin)
	for _, want := range []string{`id="model-check"`, `data-mode>observe-only<`, `data-mc-node="node-0"`, `data-mc-node="node-1"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("ops page lacks %s:\n%s", want, page)
		}
	}
	// The failing node comes first, marked fail; the right one passes.
	i0, i1 := strings.Index(page, `data-mc-node="node-0"`), strings.Index(page, `data-mc-node="node-1"`)
	row1 := page[i1:]
	if i1 > i0 || !strings.Contains(row1[:strings.Index(row1, "</tr>")], `data-result>fail<`) {
		t.Errorf("the failing node is not first or not marked fail")
	}
	row0 := page[i0:]
	if !strings.Contains(row0[:strings.Index(row0, "</tr>")], `data-result>pass<`) {
		t.Errorf("the right model is not marked pass")
	}
	// No prompt text on the page: only results.
	if strings.Contains(page, "secret topic") {
		t.Error("the ops page shows a secret prompt")
	}
	if resp, _ := a.get(t, "/account/ops", user); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a user got the ops page: %d", resp.StatusCode)
	}
}
