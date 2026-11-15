// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/node/setup"
)

// TestLlamaServerSmoke runs a real llama-server under the supervisor with each tier's exact
// command (only the model file and the port differ) and checks it serves: the model name, a
// chat answer, the speed test's /tokenize and /completion, /metrics, and that the reasoning
// budget's message really reaches the model's thinking. It needs AISATHOME_LLAMA_SERVER (the
// program) and AISATHOME_SMOKE_MODEL (a small GGUF with a thinking chat template whose head
// size the q4_0 KV cache can split); CI sets both on Windows with the pinned upstream build and
// runs it on the CPU. Elsewhere it is skipped. It also runs the model check's client against the
// real server: a short reference, then a check against it.
func TestLlamaServerSmoke(t *testing.T) {
	exe, model := os.Getenv("AISATHOME_LLAMA_SERVER"), os.Getenv("AISATHOME_SMOKE_MODEL")
	if exe == "" || model == "" {
		t.Skip("AISATHOME_LLAMA_SERVER and AISATHOME_SMOKE_MODEL not set")
	}
	for _, tier := range setup.Tiers {
		t.Run(tier.Name, func(t *testing.T) {
			e := tier.Engine(exe, model)
			e.Port = freePort(t)
			s := serveEngine(t, e)
			base := e.Upstream()

			var models struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			getJSON(t, base+"/v1/models", &models)
			if len(models.Data) != 1 || models.Data[0].ID != tier.Alias {
				t.Errorf("models %+v, want %s", models, tier.Alias)
			}

			ans := chat(t, base, 16)
			if len(ans.Choices) != 1 || ans.Usage.CompletionTokens == 0 {
				t.Errorf("chat answer %+v", ans)
			}

			// The setup's speed test, with a short prompt (a CPU is slow) and no minimum.
			sp, err := setup.SpeedTest(context.Background(), setup.HTTPEngine{BaseURL: base}, 256, 8)
			if err != nil || sp.TPS <= 0 || sp.PromptTokens != 256 {
				t.Errorf("speed test %+v %v", sp, err)
			}
			t.Logf("%s: %.1f tokens/s on the CPU", tier.Name, sp.TPS)

			resp, err := http.Get(base + "/metrics")
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || !strings.Contains(string(b), "llamacpp:") {
					t.Errorf("/metrics: %d %.200s", resp.StatusCode, b)
				}
			} else {
				t.Error(err)
			}
			stopEngine(t, s)
		})
	}

	// The model check against a real llama-server: its logprob answers parse, and a second run on
	// the same engine passes against a reference recorded from the first.
	t.Run("model check", func(t *testing.T) {
		e := setup.Tiers[0].Engine(exe, model)
		e.Port = freePort(t)
		s := serveEngine(t, e)
		c := &modelcheck.Client{BaseURL: e.Upstream()}
		rq := modelcheck.Request{MaxTokens: 8, TopLogprobs: 20}
		prompts := modelcheck.Prompts()[:4]
		mk := func(kind string) *modelcheck.Reference {
			return &modelcheck.Reference{Format: modelcheck.Format, Kind: kind, Name: "smoke", Request: rq,
				Results: c.Run(context.Background(), prompts, rq, nil)}
		}
		ref := mk(modelcheck.KindReference)
		if err := ref.Validate(); err != nil {
			t.Fatalf("reference: %v (%+v)", err, ref.Results)
		}
		for _, r := range ref.Results {
			if r.Error != "" || len(r.Tokens) == 0 || len(r.Tokens[0].Top) < 2 {
				t.Fatalf("prompt %s: %q, %d tokens", r.ID, r.Error, len(r.Tokens))
			}
		}
		rep := modelcheck.Compare(ref, mk(modelcheck.KindRun), modelcheck.DefaultThresholds)
		if rep.Verdict != modelcheck.Pass || rep.Prompts != 4 {
			t.Errorf("check against itself: %s %v", rep.Verdict, rep.Reasons)
		}
		t.Logf("model check on the CPU: %d positions, largest group KL %.6f", rep.Positions, rep.CheckKLMax)
		stopEngine(t, s)
	})

	// The thinking budget: with a budget of a few tokens, the budget message (passed as one
	// argument with spaces, parentheses and a semicolon) ends the model's thinking.
	t.Run("reasoning budget", func(t *testing.T) {
		e := setup.Tiers[0].Engine(exe, model)
		e.Port = freePort(t)
		e.ReasoningBudget = 8
		s := serveEngine(t, e)
		ans := chat(t, e.Upstream(), 48)
		if len(ans.Choices) != 1 || !strings.Contains(ans.Choices[0].Message.ReasoningContent, setup.ReasoningBudgetMessage) {
			t.Errorf("the budget message is not in the thinking: %+v", ans.Choices)
		}
		stopEngine(t, s)
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type running struct {
	sup    *Supervisor
	cancel context.CancelFunc
	done   chan error
	out    *lockedWriter
}

// lockedWriter collects the engine's output while the test reads it.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// serveEngine runs e under a supervisor and waits for its /health.
func serveEngine(t *testing.T, e setup.EngineSettings) *running {
	t.Helper()
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &running{out: &lockedWriter{}, done: make(chan error, 1)}
	r.sup = New(Command{Path: e.Exe, Args: e.Args(), Env: e.Env()}, Options{Output: r.out})
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- r.sup.Run(ctx) }()
	t.Cleanup(func() { stopEngine(t, r) })
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if st := r.sup.Status(); st.State == StateRestarting {
			t.Fatalf("llama-server ended: %s\n%s", st.LastExit, r.output())
		}
		resp, err := http.Get(e.Upstream() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("llama-server not healthy after 3 minutes:\n%s", r.output())
	return nil
}

// stopEngine stops the engine and checks the process is gone.
func stopEngine(t *testing.T, r *running) {
	t.Helper()
	if r.cancel == nil {
		return
	}
	pid := r.sup.Status().PID
	r.cancel()
	r.cancel = nil
	select {
	case <-r.done:
	case <-time.After(time.Minute):
		t.Fatal("the supervisor did not stop")
	}
	if pid != 0 && !processGone(pid, 30*time.Second) {
		t.Errorf("llama-server %d still runs after the stop", pid)
	}
	if t.Failed() {
		t.Logf("llama-server's output:\n%s", r.output())
	}
}

func (r *running) output() string {
	r.out.mu.Lock()
	defer r.out.mu.Unlock()
	s := r.out.b.String()
	if len(s) > 20000 {
		s = "...\n" + s[len(s)-20000:]
	}
	return s
}

type chatAnswer struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// chat asks for a greedy answer of up to maxTokens.
func chat(t *testing.T, base string, maxTokens int) chatAnswer {
	t.Helper()
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":"Name three rivers."}],"max_tokens":%d,"temperature":0}`, maxTokens)
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a chatAnswer
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(b, &a) != nil {
		t.Fatalf("chat: %d %.300s", resp.StatusCode, b)
	}
	return a
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
}
