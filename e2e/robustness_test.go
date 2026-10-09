// Robustness tests: engine error text, slow clients, malformed requests, truncated streams.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/protocol"
)

const promptMarker = "PROMPT-TEXT-THAT-MUST-NOT-BE-LOGGED"

// echoErrorEngine fails with a response body that quotes the prompt, as engines may.
func echoErrorEngine() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":"failed on input: %s"}}`, strings.ReplaceAll(string(raw), `"`, `'`))
	}))
}

func TestEngineErrorTextIsNotLoggedOrForwarded(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	r := newRig(t, echoErrorEngine())
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
		`{"model":"deep","messages":[{"role":"user","content":"`+promptMarker+`"}]}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	usage, _ := os.ReadFile(r.usageLog)
	for name, s := range map[string]string{"logs": logs.String(), "client answer": string(b), "usage log": string(usage)} {
		if strings.Contains(s, promptMarker) {
			t.Fatalf("prompt text leaked into %s", name)
		}
	}
}

// A client that stops reading must not stall other requests on the same node.
func TestSlowClientDoesNotStallNode(t *testing.T) {
	r := newRig(t, floodEngine())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", r.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deep","stream":true,"max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer user-key")
	slow, err := http.DefaultClient.Do(req) // never read: the client is "slow"
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Body.Close()
	time.Sleep(2 * time.Second) // let buffers fill

	// A second request on the same node (2 slots) must still get its first bytes promptly.
	start := time.Now()
	req2, _ := http.NewRequest("POST", r.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deep","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer user-key")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := resp2.Body.Read(buf)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || n == 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("second request stalled: status %d, %d bytes after %v", resp2.StatusCode, n, time.Since(start))
	}
}

func TestMalformedRequests(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	for _, c := range []struct {
		body string
		want int
	}{
		{`null`, 400},
		{`{"model":"deep"}`, 400},
		{`{"model":"deep","messages":"hi"}`, 400},
		{`{"model":"deep","messages":[null, 42, {"role":"assistant","tool_calls":[null, 42, {"function":null}]}, {"role":"user","content":"hi"}]}`, 200},
	} {
		resp, err := http.DefaultClient.Do(func() *http.Request {
			req, _ := http.NewRequest("POST", r.gw.URL+"/v1/chat/completions", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer user-key")
			return req
		}())
		if err != nil {
			t.Fatalf("%s: connection dropped (panic?): %v", c.body, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s: got %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
}

// truncEngine sends part of an answer and then closes the stream cleanly, without [DONE].
func truncEngine() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half an ans\"}}]}\n\n")
	}))
}

func TestTruncatedStreamIsNotSuccess(t *testing.T) {
	r := newRig(t, truncEngine())
	resp := r.do(t, "POST", "/v1/chat/completions", "user-key", `{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("a truncated answer was returned as success: %s", b)
	}
}

// rawNode speaks the protocol directly, like an old or malicious node: it answers every
// request with an error whose text quotes the prompt.
func rawNode(t *testing.T, r *rig, id string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Authorization", "Bearer node-token")
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.gw.URL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	write := func(m protocol.Msg) { b, _ := json.Marshal(m); c.Write(ctx, websocket.MessageText, b) }
	write(protocol.Msg{Type: protocol.TypeHello, NodeID: id, Protocol: protocol.Version,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2}}})
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m protocol.Msg
			json.Unmarshal(b, &m)
			if m.Type == protocol.TypeRequest {
				write(protocol.Msg{Type: protocol.TypeError, ReqID: m.ReqID,
					Error: "engine returned 500: failed on input " + string(m.Body)})
			}
		}
	}()
}

// The gateway's own defence: even a node that sends prompt text in errors gets it neither
// logged nor forwarded.
func TestGatewayDoesNotLogNodeErrorText(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	r := newRigCfg(t, nil)
	rawNode(t, r, "old-node")
	r.waitNodes(t, 1)
	for _, stream := range []string{"true", "false"} {
		resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
			`{"model":"deep","stream":`+stream+`,"messages":[{"role":"user","content":"`+promptMarker+`"}]}`)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), promptMarker) || strings.Contains(logs.String(), promptMarker) {
			t.Fatalf("stream=%s: node error text leaked (client: %v, logs: %v)", stream,
				strings.Contains(string(b), promptMarker), strings.Contains(logs.String(), promptMarker))
		}
	}
}
