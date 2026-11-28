// Golden wire fixtures: each message as it goes over the wire, at the current protocol version and at the
// previous one the gateway still accepts (Version and Version-1), and the link and register calls' bodies.
// A field renamed, a tag changed or a field dropped changes the bytes and fails here, instead of in a node
// or gateway of another release.
//
// The files in testdata/ are the reference, not the code: they are written by hand from PROTOCOL.md (or
// with -update from the values below, and then read). Change one only with the protocol, and raise Version
// when the other side must know (PROTOCOL.md, "Protocol versions"). When the gateway stops accepting the
// oldest version, its directory goes, and a new version gets its own.
//
// SPDX-License-Identifier: Apache-2.0
package protocol

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/ from the values in this file")

type golden struct {
	file string // under testdata/
	v    any    // a pointer to the value the file holds
}

// The chunk and the request body pass through the protocol unchanged; these are short real ones.
var (
	chatBody = json.RawMessage(`{"model":"qwen3.8-27b-q3","messages":[{"role":"user","content":"Hi"}],"stream":true,"max_tokens":64}`)
	chunk    = json.RawMessage(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1764331200,"model":"qwen3.8-27b-q3","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`)
)

// common: the messages that are the same in both versions.
func common(dir string) []golden {
	return []golden{
		{dir + "/request.json", &Msg{Type: TypeRequest, ReqID: "r-0001", Body: chatBody}},
		{dir + "/cancel.json", &Msg{Type: TypeCancel, ReqID: "r-0001"}},
		{dir + "/chunk.json", &Msg{Type: TypeChunk, ReqID: "r-0001", Data: chunk}},
		{dir + "/done.json", &Msg{Type: TypeDone, ReqID: "r-0001", Usage: &Usage{PromptTokens: 1200, CompletionTokens: 64, CachedTokens: 1024, DurationMs: 2350}}},
		{dir + "/error.json", &Msg{Type: TypeError, ReqID: "r-0001", Error: "engine: context length exceeded"}},
	}
}

func cases() []golden {
	models := []ModelInfo{{Name: "qwen3.8-27b-q3", Roles: []string{"fast", "agent"}, Slots: 2, Ctx: 81920}}
	deleteAt := time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)
	c := []golden{
		// version 2 (v0.4.0 and later)
		{"v2/hello.json", &Msg{Type: TypeHello, NodeID: "n-1a2b3c4d", Protocol: LegacyVersion, ProtocolVersion: 2, Agent: "aisathome-node/0.9.0", Models: models}},
		{"v2/welcome.json", &Msg{Type: TypeWelcome, GatewayID: "gw-eu-1", Protocol: LegacyVersion, ProtocolVersion: 2, ProtocolMin: 1, ProtocolMax: 2}},
		{"v2/heartbeat.json", &Msg{Type: TypeHeartbeat, Active: 1, Paused: true}},
		// version 1 (v0.1.0 to v0.3.0): hello without protocol_version; a heartbeat without paused (added
		// later, optional); the welcome a current gateway sends such a node, which reads gateway_id and
		// protocol only
		{"v1/hello.json", &Msg{Type: TypeHello, NodeID: "n-1a2b3c4d", Protocol: LegacyVersion, Agent: "aisathome-node/0.3.0", Models: models}},
		{"v1/welcome.json", &Msg{Type: TypeWelcome, GatewayID: "gw-eu-1", Protocol: LegacyVersion, ProtocolVersion: 1, ProtocolMin: 1, ProtocolMax: 2}},
		{"v1/heartbeat.json", &Msg{Type: TypeHeartbeat, Active: 1}},
		// the HTTPS calls (link.go, register.go): not versioned
		{"http/link.json", &LinkInfo{Name: "box", GPU: "NVIDIA GeForce RTX 4090", VRAMMiB: 24564, OS: "Ubuntu 24.04.1 LTS", Agent: "aisathome-node/0.9.0"}},
		{"http/link-start.json", &LinkStart{DeviceCode: "dev-secret", UserCode: "KXQT-4821", VerificationURI: "https://aisathome.org/link",
			VerificationURIComplete: "https://aisathome.org/link?code=KXQT-4821", ExpiresIn: 600, Interval: 5}},
		{"http/link-poll-request.json", &LinkPollRequest{DeviceCode: "dev-secret"}},
		{"http/link-poll-pending.json", &LinkPoll{Status: LinkPending, Interval: 5}},
		{"http/link-poll-approved.json", &LinkPoll{Status: LinkApproved, Token: "nt-n-1a2b3c4d.secret", NodeID: "n-1a2b3c4d"}},
		{"http/register-request.json", &RegisterRequest{AcceptTerms: true, Agent: "aisathome-node/0.9.0"}},
		{"http/register.json", &Register{Token: "nt-a-0f1e2d3c.secret", NodeID: "a-0f1e2d3c"}},
		{"http/self.json", &NodeSelf{NodeID: "a-0f1e2d3c", Anonymous: true, ScienceOnly: true, State: SelfHeld, ChecksPassed: 1, ChecksNeeded: 3,
			Message: "This node is held.", DeleteAt: &deleteAt}},
	}
	return append(append(c, common("v2")...), common("v1")...)
}

// Each value marshals to its file's JSON, and the file decodes (unknown fields refused) to the value.
func TestGoldenWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join("testdata", c.file)
			got, err := json.Marshal(c.v)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// the same JSON (key order aside: receivers do not depend on it)
			var g, w any
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("the Go value marshals to\n%s\nthe wire fixture is\n%s", got, want)
			}
			back := reflect.New(reflect.TypeOf(c.v).Elem())
			dec := json.NewDecoder(bytes.NewReader(want))
			dec.DisallowUnknownFields()
			if err := dec.Decode(back.Interface()); err != nil {
				t.Fatalf("decoding the fixture: %v", err)
			}
			if !reflect.DeepEqual(back.Interface(), c.v) {
				t.Errorf("the fixture decodes to %+v, want %+v", back.Elem().Interface(), reflect.ValueOf(c.v).Elem().Interface())
			}
		})
	}
}

// Every message type has a fixture in both versions.
func TestGoldenWireCoversEveryType(t *testing.T) {
	have := map[string]bool{}
	for _, c := range cases() {
		if m, ok := c.v.(*Msg); ok {
			have[filepath.Dir(c.file)+"/"+m.Type] = true
		}
	}
	for _, v := range []string{"v1", "v2"} {
		for _, typ := range []string{TypeHello, TypeWelcome, TypeHeartbeat, TypeRequest, TypeCancel, TypeChunk, TypeDone, TypeError} {
			if !have[v+"/"+typ] {
				t.Errorf("no %s fixture for %s", v, typ)
			}
		}
	}
	if Version != 2 {
		t.Errorf("Version is %d: add testdata/v%d, and drop the oldest version the gateway no longer accepts", Version, Version)
	}
}

// The values that go over the wire as they are: message types, versions, the close code, the link and
// self states, and the paths.
func TestWireConstants(t *testing.T) {
	for _, c := range []struct{ got, want any }{
		{TypeHello, "hello"}, {TypeWelcome, "welcome"}, {TypeHeartbeat, "heartbeat"}, {TypeRequest, "request"},
		{TypeCancel, "cancel"}, {TypeChunk, "chunk"}, {TypeDone, "done"}, {TypeError, "error"},
		{LegacyVersion, "0.1"}, {Unversioned, 1}, {CloseUnsupportedProtocol, 4001},
		{LinkPending, "pending"}, {LinkSlowDown, "slow_down"}, {LinkApproved, "approved"}, {LinkDenied, "denied"}, {LinkExpired, "expired"},
		{SelfChecking, "checking"}, {SelfAdmitted, "admitted"}, {SelfHeld, "held"}, {SelfOffline, "offline"}, {SelfNoCheck, "no_check"},
		{SelfServing, "serving"},
		{LinkPath, "/v1/node/link"}, {LinkPollPath, "/v1/node/link/poll"}, {RegisterPath, "/v1/node/register"}, {SelfPath, "/v1/node/self"},
	} {
		if c.got != c.want {
			t.Errorf("%v, want %v", c.got, c.want)
		}
	}
}

// A hello without protocol_version is version 1; with it, that version.
func TestHelloVersion(t *testing.T) {
	for file, want := range map[string]int{"v1/hello.json": Unversioned, "v2/hello.json": 2} {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		var m Msg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if got := m.HelloVersion(); got != want {
			t.Errorf("%s: HelloVersion %d, want %d", file, got, want)
		}
	}
}
