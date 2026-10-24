// Compatibility with released node agents (decision 0013: the gateway works with the current
// and the previous node release). The protocol version: nodes without one speak version 1, the
// gateway accepts a configured range and refuses others with a message the node shows.
//
// TestReleasedNodesChat builds the node agent from the latest release tags and runs the basic
// chat through it. TestRecordedHelloV020 replays the hello frame of the v0.2.0 agent (recorded
// from that build: testdata/hello-v0.2.0.json), so the version check is covered even where the
// tags cannot be built.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aisathome/aisathome/gateway"
	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/protocol"
)

// compatReleases is how many of the latest release tags TestReleasedNodesChat builds: the
// current node release and the one before it.
const compatReleases = 2

// releaseRefs returns the git refs to build: AISATHOME_COMPAT_REFS (comma-separated, e.g.
// "v0.2.0,d48ea5b") or the latest compatReleases release tags. A checkout without tags skips
// the test, except in CI (CI is set), where the workflow fetches them and a skip would hide a
// broken promise.
func releaseRefs(t *testing.T, root string) []string {
	t.Helper()
	if v := os.Getenv("AISATHOME_COMPAT_REFS"); v != "" {
		return strings.Split(v, ",")
	}
	out, err := exec.Command("git", "-C", root, "tag", "-l", "v[0-9]*.[0-9]*.[0-9]*", "--sort=-v:refname").Output()
	tags := strings.Fields(string(out))
	if err != nil || len(tags) == 0 {
		if os.Getenv("CI") != "" {
			t.Fatalf("no release tags in this checkout (git: %v); CI must fetch them (fetch-depth: 0)", err)
		}
		t.Skip("no release tags in this checkout")
	}
	return tags[:min(compatReleases, len(tags))]
}

// buildRelease builds the node agent as it was at ref (git archive, so the working tree is
// not touched) and returns the binary.
func buildRelease(t *testing.T, root, ref string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := exec.Command("git", "-C", root, "archive", "--format=tar", ref)
	untar := exec.Command("tar", "-x", "-C", src)
	var archiveErr, untarErr bytes.Buffer // two processes: one buffer each
	archive.Stderr, untar.Stderr = &archiveErr, &untarErr
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	if err := untar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("git archive %s: %v\n%s", ref, err, archiveErr.String())
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("untar %s: %v\n%s", ref, err, untarErr.String())
	}
	bin := filepath.Join(dir, "aisathome-node")
	goBin, err := exec.LookPath("go")
	if err != nil { // go test does not need go on PATH; the toolchain that runs it does
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	build := exec.Command(goBin, "build", "-trimpath", "-ldflags", "-X github.com/aisathome/aisathome/node.Version="+ref,
		"-o", bin, "./node/cmd/aisathome-node")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the node agent at %s: %v\n%s", ref, err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runReleased starts a built node agent against the rig's gateway; it stops with the test.
func (r *rig) runReleased(t *testing.T, bin, id string, engine *httptest.Server) *syncBuf {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := json.Marshal(node.Config{NodeID: id, Token: "node-token", Upstream: engine.URL,
		GatewayURL: "ws" + strings.TrimPrefix(r.gw.URL, "http") + "/v1/node/connect",
		Models:     []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 2, Ctx: 32768}}})
	path := filepath.Join(dir, "node.json")
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "-config", path)
	cmd.Env = append(os.Environ(), "AISATHOME_UI_PORT=off", "AISATHOME_NODE_TOKEN=")
	logs := &syncBuf{} // the process writes it while the test may read it
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	return logs
}

// The basic chat (streaming and not) through each of the latest node releases, built from
// their tags: the gateway must keep working with them (decision 0013).
func TestReleasedNodesChat(t *testing.T) {
	if testing.Short() {
		t.Skip("builds old node agents")
	}
	root := repoRoot(t)
	for _, ref := range releaseRefs(t, root) {
		t.Run(ref, func(t *testing.T) {
			bin := buildRelease(t, root, ref)
			r := newRig(t)
			logs := r.runReleased(t, bin, "old-node", mockEngine(t, false))
			for deadline := time.Now().Add(20 * time.Second); r.g.NodeCount() != 1; time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the %s node agent did not connect; its log:\n%s", ref, logs)
				}
			}
			proto, _ := r.g.NodeProtocol("old-node")
			t.Logf("%s connected with protocol version %d", ref, proto)
			resp := r.do(t, "POST", "/v1/chat/completions", "user-key",
				`{"model":"deep","messages":[{"role":"user","content":"hi"}]}`)
			var out struct {
				Choices []struct{ Message struct{ Content string } } `json:"choices"`
			}
			err := json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello, world" {
				t.Fatalf("non-streaming chat through %s: status %d, %+v, %v", ref, resp.StatusCode, out, err)
			}
			if text, done := readStream(t, r.do(t, "POST", "/v1/chat/completions", "user-key",
				`{"model":"deep","stream":true,"messages":[{"role":"user","content":"hi"}]}`)); text != "Hello, world" || !done {
				t.Fatalf("streaming chat through %s: %q, done=%v", ref, text, done)
			}
			if u := waitUsage(t, r, 2); u[0]["node"] != "old-node" || u[0]["usage"].(map[string]any)["completion_tokens"] != 3.0 {
				t.Fatalf("usage of the %s node: %v", ref, u[0])
			}
		})
	}
}

// readStream reads an SSE answer: its text and whether it ended with [DONE].
func readStream(t *testing.T, resp *http.Response) (string, bool) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var text strings.Builder
	for _, line := range strings.Split(readAll(t, resp), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			return text.String(), true
		}
		var c struct {
			Choices []struct{ Delta struct{ Content string } } `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &c) == nil {
			for _, ch := range c.Choices {
				text.WriteString(ch.Delta.Content)
			}
		}
	}
	return text.String(), false
}

func readAll(t *testing.T, resp *http.Response) string {
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// helloV020 is the hello the v0.2.0 node agent sent (recorded from that build, see
// TestRecordHello), with the node id and models of the config it ran with.
func helloV020(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/hello-v0.2.0.json")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// dialHello connects as a node with token, sends one hello frame and returns the gateway's
// first answer: a welcome, or the close error.
func dialHello(t *testing.T, gwURL, token string, hello []byte) (protocol.Msg, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gwURL, "http")+"/v1/node/connect", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	var m protocol.Msg
	_, b, err := c.Read(ctx)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// The recorded v0.2.0 hello has no protocol_version: the gateway takes it as version 1, which
// the default range (the gateway's version and the one before) includes. A gateway whose range
// starts above 1 refuses it with the close code and a message that says what to do.
func TestRecordedHelloV020(t *testing.T) {
	hello := helloV020(t)
	var h protocol.Msg
	if err := json.Unmarshal(hello, &h); err != nil || h.ProtocolVersion != 0 || h.Protocol != "0.1" || h.Agent != "aisathome-node/0.2.0" {
		t.Fatalf("the fixture is not the v0.2.0 hello: %+v %v", h, err)
	}
	r := newRig(t)
	w, err := dialHello(t, r.gw.URL, "node-token", hello)
	if err != nil || w.Type != protocol.TypeWelcome || w.Protocol != protocol.LegacyVersion || w.ProtocolVersion != 1 ||
		w.ProtocolMin != protocol.Version-1 || w.ProtocolMax != protocol.Version {
		t.Fatalf("the v0.2.0 hello must be welcomed as protocol 1 (range %d-%d): %+v %v", protocol.Version-1, protocol.Version, w, err)
	}
	r.waitNodes(t, 1)
	if p, ok := r.g.NodeProtocol(h.NodeID); !ok || p != 1 {
		t.Fatalf("protocol of the v0.2.0 node: %d %v", p, ok)
	}

	strict := newRigCfg(t, func(c *gateway.Config) { c.NodeProtocolMin = 2 })
	_, err = dialHello(t, strict.gw.URL, "node-token", hello)
	var ce websocket.CloseError
	want := "node protocol version 1 not supported (gateway supports 2): update the node agent"
	if !errors.As(err, &ce) || ce.Code != protocol.CloseUnsupportedProtocol || ce.Reason != want {
		t.Fatalf("got %v, want close %d %q", err, protocol.CloseUnsupportedProtocol, want)
	}
	if strict.g.NodeCount() != 0 {
		t.Fatal("a refused node was registered")
	}
}

// The current agent announces protocol.Version and is welcomed with it; a version above the
// gateway's range is refused: the gateway does not speak it yet.
func TestCurrentAndNewerProtocol(t *testing.T) {
	r := newRig(t, mockEngine(t, false))
	if p, _ := r.g.NodeProtocol("node-0"); p != protocol.Version {
		t.Fatalf("the current agent connected with protocol %d, want %d", p, protocol.Version)
	}
	b, _ := json.Marshal(protocol.Msg{Type: protocol.TypeHello, NodeID: "future", Protocol: protocol.LegacyVersion,
		ProtocolVersion: protocol.Version + 1, Models: []protocol.ModelInfo{{Name: "m", Slots: 1}}})
	_, err := dialHello(t, r.gw.URL, "node-token", b)
	var ce websocket.CloseError
	want := fmt.Sprintf("node protocol version %d not supported (gateway supports %d-%d): this gateway does not speak it yet",
		protocol.Version+1, protocol.Version-1, protocol.Version)
	if !errors.As(err, &ce) || ce.Code != protocol.CloseUnsupportedProtocol || ce.Reason != want {
		t.Fatalf("got %v, want close %q", err, want)
	}
	if len(want) > 123 {
		t.Fatalf("close reasons are at most 123 bytes: %d", len(want))
	}
}

// A refused agent stops reconnecting every few seconds, logs why, and shows the gateway's
// message in its status (local UI and `aisathome-node status`) until it connects again.
func TestRefusedAgentShowsWhy(t *testing.T) {
	logs := &syncBuf{}
	logSink = logs
	defer func() { logSink = io.Discard }()
	// Only version 1: the current agent (protocol.Version > 1) is refused, as by a gateway that
	// was not updated yet (a gateway that retired the agent's version refuses the same way).
	r := newRigCfg(t, func(c *gateway.Config) { c.NodeProtocolMin, c.NodeProtocolMax = 1, 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := node.Config{NodeID: "new", Token: "node-token", Upstream: mockEngine(t, false).URL,
		GatewayURL:     "ws" + strings.TrimPrefix(r.gw.URL, "http") + "/v1/node/connect",
		HealthInterval: 50 * time.Millisecond, RefusedRetry: time.Hour,
		Models: []protocol.ModelInfo{{Name: "qwen3.8-27b-q4", Roles: []string{"deep"}, Slots: 1, Ctx: 32768}}}
	a := node.New(cfg, quiet())
	go a.Run(ctx)
	want := fmt.Sprintf("node protocol version %d not supported (gateway supports 1): this gateway does not speak it yet", protocol.Version)
	for deadline := time.Now().Add(5 * time.Second); a.Status().Refused != want && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	s := a.Status()
	if s.Connected || s.State != node.StateDisconnected || s.Protocol != protocol.Version ||
		len(s.Events) == 0 || s.Events[0].Kind != "refused" || s.Events[0].Text != want {
		t.Fatalf("status after a refusal: %+v\n%s", s, logs.String())
	}
	if !strings.Contains(logs.String(), "the gateway refused this node agent's protocol version") ||
		!strings.Contains(logs.String(), "node refused: unsupported protocol version") {
		t.Fatalf("refusal not logged on both sides:\n%s", logs.String())
	}
	time.Sleep(300 * time.Millisecond) // RefusedRetry: no reconnect storm
	if n := strings.Count(logs.String(), "node refused: unsupported protocol version"); n != 1 {
		t.Fatalf("the refused agent tried %d times, want once per RefusedRetry", n)
	}
}

// TestRecordHello re-records testdata/hello-v0.2.0.json from the v0.2.0 tag:
//
//	AISATHOME_RECORD_HELLO=1 go test -run TestRecordHello ./e2e
func TestRecordHello(t *testing.T) {
	if os.Getenv("AISATHOME_RECORD_HELLO") == "" {
		t.Skip("set AISATHOME_RECORD_HELLO=1 to re-record the fixture")
	}
	bin := buildRelease(t, repoRoot(t), "v0.2.0")
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		_, b, err := c.Read(req.Context())
		if err == nil {
			select {
			case got <- b:
			default:
			}
		}
		c.Close(websocket.StatusNormalClosure, "recorded")
	}))
	defer srv.Close()
	r := &rig{gw: srv}
	r.runReleased(t, bin, "old-node", mockEngine(t, false))
	select {
	case b := <-got:
		if err := os.WriteFile("testdata/hello-v0.2.0.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded: %s", b)
	case <-time.After(30 * time.Second):
		t.Fatal("no hello from the v0.2.0 agent")
	}
}
