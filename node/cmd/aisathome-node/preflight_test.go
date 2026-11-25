// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisathome/aisathome/node"
)

const (
	pfGW    = "wss://gw.example.org/v1/node/connect"
	pfOther = "wss://other.example.org/v1/node/connect"
)

var (
	pfTok    = "nt-n-0123abcd." + strings.Repeat("S", 43)
	pfNew    = "nt-n-0000000e." + strings.Repeat("N", 43)
	pfAnon   = "nt-a-0000000d." + strings.Repeat("Q", 43)
	pfLegacy = "nt-n-0123abcd.short" // an older token form: kept as it is, never accepted when given
)

// pfCase is one preflight run against a fake installed node.
type pfCase struct {
	nodeJSON, tokenEnv string // installed files ("" = none)
	args               []string
	stdin              string
	tty                bool
	state              string // what the token's gateway says ("ok" default)
}

type pfOut struct {
	code         int
	res          preflightResult
	stderr       string
	nodeJSON     map[string]any
	tokenEnv     string
	askedGateway []string
}

func (c pfCase) run(t *testing.T) pfOut {
	t.Helper()
	dir, out := t.TempDir(), t.TempDir()
	if c.nodeJSON != "" {
		os.WriteFile(filepath.Join(dir, "node.json"), []byte(c.nodeJSON), 0o640)
	}
	if c.tokenEnv != "" {
		os.WriteFile(filepath.Join(dir, "token.env"), []byte(c.tokenEnv), 0o600)
	}
	var o pfOut
	e := preflightEnv{stdin: strings.NewReader(c.stdin), interactive: c.tty,
		readSecret: func() (string, error) { return readLine(strings.NewReader(c.stdin)) },
		hostname:   func() (string, error) { return "box.local", nil },
		tokenState: func(tok, gw string) string {
			o.askedGateway = append(o.askedGateway, gw)
			if c.state == "" {
				return "ok"
			}
			return c.state
		}}
	var stdout, stderr bytes.Buffer
	args := append([]string{"-conf-dir", dir, "-out", out, "-json", "-model", "m", "-ctx", "196608"}, c.args...)
	o.code = e.main(args, &stdout, &stderr)
	o.stderr = stderr.String()
	if o.code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &o.res); err != nil {
			t.Fatalf("json: %v: %s", err, stdout.String())
		}
		b, err := os.ReadFile(filepath.Join(out, "node.json"))
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(b, &o.nodeJSON)
		if b, err := os.ReadFile(filepath.Join(out, "token.env")); err == nil {
			o.tokenEnv = string(b)
		}
	}
	return o
}

func tokenEnv(tok, gw string) string {
	s := "AISATHOME_NODE_TOKEN=" + tok + "\n"
	if gw != "" {
		s += "AISATHOME_TOKEN_GATEWAY=" + gw + "\n"
	}
	return s
}

func installedJSON(gw string) string {
	return `{"node_id": "box-1", "gateway_url": "` + gw + `", "slots": 2}`
}

func TestPreflightGateway(t *testing.T) {
	for _, tc := range []struct {
		name, installed string
		args            []string
		want            string
		code            int
	}{
		{"default", "", []string{"-account", "link"}, "wss://api.aisathome.org/v1/node/connect", 0},
		{"installed kept", installedJSON(pfGW), []string{"-account", "link"}, pfGW, 0},
		{"flag wins", installedJSON(pfGW), []string{"-account", "link", "-gateway-url", pfOther}, pfOther, 0},
		{"loopback ws", "", []string{"-account", "link", "-gateway-url", "ws://127.0.0.1:8080/v1/node/connect"}, "ws://127.0.0.1:8080/v1/node/connect", 0},
		{"localhost ws", "", []string{"-account", "link", "-gateway-url", "ws://localhost:8080/v1/node/connect"}, "ws://localhost:8080/v1/node/connect", 0},
		{"ws to another machine", "", []string{"-account", "link", "-gateway-url", "ws://gw.example.org/v1/node/connect"}, "", 2},
		{"ws to a 127 look-alike", "", []string{"-account", "link", "-gateway-url", "ws://127.0.0.1.gw.example/v1/node/connect"}, "", 2},
		{"http", "", []string{"-account", "link", "-gateway-url", "http://127.0.0.1/"}, "", 2},
		{"query", "", []string{"-account", "link", "-gateway-url", "wss://gw/x?y"}, "", 2},
		{"installed ws to another machine", installedJSON("ws://gw.example.org/v1/node/connect"), []string{"-account", "link"}, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := pfCase{nodeJSON: tc.installed, args: tc.args}.run(t)
			if o.code != tc.code {
				t.Fatalf("exit %d, want %d: %s", o.code, tc.code, o.stderr)
			}
			if tc.code == 0 && (o.res.GatewayURL != tc.want || o.nodeJSON["gateway_url"] != tc.want) {
				t.Errorf("gateway %q (node.json %v), want %q", o.res.GatewayURL, o.nodeJSON["gateway_url"], tc.want)
			}
			if tc.code == 2 && !strings.Contains(o.stderr, "ws:// only to this machine") {
				t.Errorf("message: %s", o.stderr)
			}
		})
	}
}

func TestPreflightCanonicalConfig(t *testing.T) {
	// The installed node.json with keys equal in any case is refused, at any level.
	for _, bad := range []string{
		`{"gateway_url": "` + pfGW + `", "Gateway_URL": "` + pfOther + `"}`,
		`{"models": [{"name": "m", "Name": "n"}]}`,
		`[1]`,
		`{broken`,
	} {
		if o := (pfCase{nodeJSON: bad, args: []string{"-account", "link"}}).run(t); o.code != 2 {
			t.Errorf("installed %s: exit %d, want 2", bad, o.code)
		}
	}
	// A supplied node.json: canonical (lower-case keys at every level, no token), its gateway checked.
	dir := t.TempDir()
	sup := filepath.Join(dir, "sup.json")
	os.WriteFile(sup, []byte(`{"Gateway_URL": "`+pfGW+`", "TOKEN": "nt-secret", "Node_ID": "mine", "Models": [{"Name": "m", "Slots": 1}]}`), 0o600)
	o := pfCase{args: []string{"-account", "link", "-node-config", sup}}.run(t)
	if o.code != 0 {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	want := map[string]any{"gateway_url": pfGW, "node_id": "mine", "models": []any{map[string]any{"name": "m", "slots": float64(1)}}}
	if b1, _ := json.Marshal(o.nodeJSON); string(b1) != string(must(json.Marshal(want))) {
		t.Errorf("canonical node.json %s", b1)
	}
	os.WriteFile(sup, []byte(`{"gateway_url": "`+pfGW+`", "Gateway_URL": "`+pfOther+`"}`), 0o600)
	if o := (pfCase{args: []string{"-account", "link", "-node-config", sup}}).run(t); o.code != 2 || !strings.Contains(o.stderr, "appears more than once") {
		t.Errorf("supplied case variants: exit %d: %s", o.code, o.stderr)
	}
	os.WriteFile(sup, []byte(`{"gateway_url": "ws://gw.example.org/v1/node/connect"}`), 0o600)
	if o := (pfCase{args: []string{"-account", "link", "-node-config", sup}}).run(t); o.code != 2 {
		t.Errorf("supplied ws:// gateway: exit %d", o.code)
	}
	if o := (pfCase{args: []string{"-account", "link", "-node-config", sup, "-node-id", "x"}}).run(t); o.code != 2 || !strings.Contains(o.stderr, "edit your --node-config") {
		t.Errorf("--node-config with --node-id: exit %d: %s", o.code, o.stderr)
	}
}

func must(b []byte, _ error) []byte { return b }

func TestPreflightNodeID(t *testing.T) {
	for _, tc := range []struct {
		name, installed, stdin string
		args                   []string
		want                   string
		code                   int
	}{
		{"flag", installedJSON(pfGW), "", []string{"-account", "link", "-node-id", "mine"}, "mine", 0},
		{"installed", installedJSON(pfGW), "", []string{"-account", "link"}, "box-1", 0},
		{"host name", "", "", []string{"-account", "link"}, "box", 0},
		{"the token's", installedJSON(pfGW), pfNew + "\n", nil, "n-0000000e", 0},
		{"a node without an account's token", installedJSON(pfGW), pfAnon + "\n", nil, "a-0000000d", 0},
		{"flag over the token's", installedJSON(pfGW), pfNew + "\n", []string{"-node-id", "mine"}, "mine", 0},
		{"bad flag", "", "", []string{"-account", "link", "-node-id", "-x"}, "", 2},
		{"bad installed", `{"node_id": "a b", "gateway_url": "` + pfGW + `"}`, "", []string{"-account", "link"}, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := pfCase{nodeJSON: tc.installed, args: tc.args, stdin: tc.stdin}.run(t)
			if o.code != tc.code {
				t.Fatalf("exit %d, want %d: %s", o.code, tc.code, o.stderr)
			}
			if tc.code == 0 && (o.res.NodeID != tc.want || o.nodeJSON["node_id"] != tc.want) {
				t.Errorf("node id %q, want %q", o.res.NodeID, tc.want)
			}
		})
	}
	o := pfCase{args: []string{"-account", "link"}}.run(t)
	m, _ := o.nodeJSON["models"].([]any)
	if len(m) != 1 || o.nodeJSON["upstream"] != "http://127.0.0.1:8000" {
		t.Fatalf("generated node.json %v", o.nodeJSON)
	}
	if mm := m[0].(map[string]any); mm["name"] != "m" || mm["ctx"] != float64(196608) || mm["slots"] != float64(1) {
		t.Errorf("model %v", mm)
	}
}

func TestPreflightToken(t *testing.T) {
	same, other, norec := tokenEnv(pfTok, pfGW), tokenEnv(pfTok, pfOther), tokenEnv(pfTok, "")
	nj := installedJSON(pfGW)
	for _, tc := range []struct {
		name          string
		c             pfCase
		code          int
		account       string
		park          bool
		token         string // in the written token.env ("" = none written)
		msg, notInMsg string
	}{
		{"--keep-token", pfCase{nodeJSON: nj, tokenEnv: same, args: []string{"-account", "keep"}}, 0, "keep", false, pfTok, "keeping the installed one", ""},
		{"--keep-token, a legacy token form", pfCase{nodeJSON: nj, tokenEnv: tokenEnv(pfLegacy, pfGW), args: []string{"-account", "keep"}}, 0, "keep", false, pfLegacy, "", ""},
		{"--keep-token, none", pfCase{nodeJSON: nj, args: []string{"-account", "keep"}}, 2, "", false, "", "no installed token", ""},
		{"--keep-token, another gateway's", pfCase{nodeJSON: nj, tokenEnv: other, args: []string{"-account", "keep"}}, 2, "", false, "", "never sent there", "moved aside"},
		{"--keep-token, no record", pfCase{nodeJSON: nj, tokenEnv: norec, args: []string{"-account", "keep"}}, 2, "", false, "", "unrecorded gateway", ""},
		{"--link", pfCase{nodeJSON: nj, tokenEnv: same, args: []string{"-account", "link"}, stdin: "nt-ignored\n"}, 0, "link", false, "", "linking", ""},
		{"--link, another gateway's", pfCase{nodeJSON: nj, tokenEnv: other, args: []string{"-account", "link"}}, 0, "link", true, "", "moved aside", ""},
		{"piped token", pfCase{nodeJSON: nj, tokenEnv: same, stdin: pfNew + "\n"}, 0, "token", false, pfNew, "", ""},
		{"piped token with a CR", pfCase{nodeJSON: nj, tokenEnv: same, stdin: pfNew + "\r\n"}, 0, "token", false, pfNew, "", ""},
		{"piped empty", pfCase{nodeJSON: nj, tokenEnv: same, stdin: ""}, 1, "", false, "", "--keep-token", ""},
		{"piped empty, no token", pfCase{nodeJSON: nj}, 1, "", false, "", "--link", ""},
		// At a terminal: an installed token for this gateway is kept without a question; otherwise the person
		// answers "Link this node to your account? [Y/n]".
		{"a terminal keeps the token", pfCase{nodeJSON: nj, tokenEnv: same, tty: true, stdin: "n\n"}, 0, "keep", false, pfTok, "keeping the installed one", "[Y/n]"},
		{"Enter links", pfCase{nodeJSON: nj, tty: true, stdin: "\n"}, 0, "link", false, "", "Link this node to your account? [Y/n]", "input hidden"},
		{"y links", pfCase{nodeJSON: nj, tty: true, stdin: "Y\n"}, 0, "link", false, "", "linking", ""},
		{"n runs without an account", pfCase{nodeJSON: nj, tty: true, stdin: " no\n"}, 0, "anonymous", false, "", "You must be 18 or older", ""},
		{"asked again", pfCase{nodeJSON: nj, tty: true, stdin: "maybe\nyes\n"}, 0, "link", false, "", "Answer y or n.", ""},
		{"no answer", pfCase{nodeJSON: nj, tty: true, stdin: "maybe\n"}, 2, "", false, "", "--link or --anonymous", ""},
		{"never an answer", pfCase{nodeJSON: nj, tty: true, stdin: "a\nb\nc\ny\n"}, 2, "", false, "", "no answer", ""},
		{"a token is not an answer", pfCase{nodeJSON: nj, tty: true, stdin: pfAnon + "\n"}, 2, "", false, "", "Answer y or n.", ""},
		{"Enter, another gateway's", pfCase{nodeJSON: nj, tokenEnv: other, tty: true, stdin: "\n"}, 0, "link", true, "", "moved aside", ""},
		{"n, another gateway's", pfCase{nodeJSON: nj, tokenEnv: other, tty: true, stdin: "n\n"}, 0, "anonymous", true, "", "moved aside", ""},
		// --token-stdin: pasted (not echoed) at a terminal, or piped.
		{"--token-stdin pasted", pfCase{nodeJSON: nj, tokenEnv: same, tty: true, stdin: pfAnon + "\n", args: []string{"-account", "token"}}, 0, "token", false, pfAnon, "input hidden", ""},
		{"--token-stdin piped", pfCase{nodeJSON: nj, stdin: pfNew + "\n", args: []string{"-account", "token"}}, 0, "token", false, pfNew, "", ""},
		{"--token-stdin empty", pfCase{nodeJSON: nj, tokenEnv: same, tty: true, stdin: "\n", args: []string{"-account", "token"}}, 2, "", false, "", "no token on stdin", ""},
		{"--token-stdin over an unrecorded", pfCase{nodeJSON: nj, tokenEnv: norec, tty: true, stdin: pfAnon + "\n", args: []string{"-account", "token"}}, 0, "token", true, pfAnon, "", ""},
		{"not the issued form", pfCase{nodeJSON: nj, stdin: "nt-n-0000000E." + strings.Repeat("N", 43) + "\n"}, 2, "", false, "", "is not a node token", "0000000E"},
		{"a short token", pfCase{nodeJSON: nj, stdin: pfLegacy + "\n"}, 2, "", false, "", "is not a node token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.c.run(t)
			if o.code != tc.code {
				t.Fatalf("exit %d, want %d: %s", o.code, tc.code, o.stderr)
			}
			if tc.msg != "" && !strings.Contains(o.stderr, tc.msg) {
				t.Errorf("message lacks %q: %s", tc.msg, o.stderr)
			}
			if tc.notInMsg != "" && strings.Contains(o.stderr, tc.notInMsg) {
				t.Errorf("message has %q: %s", tc.notInMsg, o.stderr)
			}
			if strings.Contains(o.stderr, pfTok) || strings.Contains(o.stderr, "SSSS") {
				t.Errorf("a token was printed: %s", o.stderr)
			}
			if tc.code != 0 {
				return
			}
			if o.res.Account != tc.account || o.res.Park != tc.park {
				t.Errorf("account %q park %v, want %q %v", o.res.Account, o.res.Park, tc.account, tc.park)
			}
			want := ""
			if tc.token != "" {
				want = tokenEnv(tc.token, pfGW)
			}
			if o.tokenEnv != want || o.res.TokenEnv != (want != "") {
				t.Errorf("token.env %q (token_env %v), want %q", o.tokenEnv, o.res.TokenEnv, want)
			}
			if _, ok := o.nodeJSON["token"]; ok {
				t.Error("node.json has a token")
			}
		})
	}
}

// The installed token that is being moved aside is never taken back as this gateway's: piped or pasted,
// with whitespace on either side.
func TestPreflightMovedAsideTokenRefused(t *testing.T) {
	for _, installed := range []string{tokenEnv(pfTok, pfOther), tokenEnv(pfTok, ""), "AISATHOME_NODE_TOKEN=" + pfTok + "  \r\nAISATHOME_TOKEN_GATEWAY=" + pfOther + "\n"} {
		for _, given := range []string{pfTok, pfTok + " ", pfTok + "\r", "\t" + pfTok, " " + pfTok + "  "} {
			for _, tty := range []bool{false, true} {
				o := pfCase{nodeJSON: installedJSON(pfGW), tokenEnv: installed, stdin: given + "\n", tty: tty, args: []string{"-account", "token"}}.run(t)
				if o.code != 2 || !strings.Contains(o.stderr, "never sent there") {
					t.Errorf("given %q over %q (tty %v): exit %d: %s", given, installed, tty, o.code, o.stderr)
				}
			}
		}
	}
}

func TestPreflightAnonymous(t *testing.T) {
	nj := installedJSON(pfGW)
	for _, tc := range []struct {
		name, tokenEnv, state string
		account               string
		park                  bool
		token                 string
		asked                 bool
		msg                   string
	}{
		{"no token", "", "", "anonymous", false, "", false, "You must be 18 or older"},
		{"a token for this gateway", tokenEnv(pfTok, pfGW), "ok", "keep", false, pfTok, true, "linked to an account"},
		{"a node without an account's", tokenEnv(pfAnon, pfGW), "ok", "keep", false, pfAnon, true, "already runs without an account"},
		{"revoked", tokenEnv(pfTok, pfGW), "gone", "anonymous", true, "", true, "no longer knows"},
		{"held", tokenEnv(pfAnon, pfGW), "held", "anonymous", true, "", true, "registers again"},
		{"another gateway's", tokenEnv(pfTok, pfOther), "ok", "anonymous", true, "", false, "moved aside"},
		{"no record", tokenEnv(pfTok, ""), "ok", "anonymous", true, "", false, "unrecorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := pfCase{nodeJSON: nj, tokenEnv: tc.tokenEnv, state: tc.state, args: []string{"-account", "anonymous"}, stdin: "nt-ignored\n"}.run(t)
			if o.code != 0 {
				t.Fatalf("exit %d: %s", o.code, o.stderr)
			}
			if o.res.Account != tc.account || o.res.Park != tc.park {
				t.Errorf("account %q park %v, want %q %v", o.res.Account, o.res.Park, tc.account, tc.park)
			}
			if (len(o.askedGateway) > 0) != tc.asked || (tc.asked && o.askedGateway[0] != pfGW) {
				t.Errorf("gateways asked: %v", o.askedGateway)
			}
			if want := map[bool]string{true: tokenEnv(tc.token, pfGW), false: ""}[tc.token != ""]; o.tokenEnv != want {
				t.Errorf("token.env %q, want %q", o.tokenEnv, want)
			}
			if !strings.Contains(o.stderr, tc.msg) {
				t.Errorf("message lacks %q: %s", tc.msg, o.stderr)
			}
		})
	}
}

// A dry run reads no token, asks nothing and writes node.json only.
func TestPreflightDryRun(t *testing.T) {
	o := pfCase{nodeJSON: installedJSON(pfGW), tokenEnv: tokenEnv(pfTok, pfOther), tty: true, stdin: pfNew + "\n",
		args: []string{"-dry-run", "-account", "anonymous"}}.run(t)
	if o.code != 0 {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	if o.res.Park || o.res.TokenEnv || o.tokenEnv != "" || o.nodeJSON["node_id"] != "box-1" || o.nodeJSON["gateway_url"] != pfGW {
		t.Errorf("dry run: %+v %v %q", o.res, o.nodeJSON, o.tokenEnv)
	}
	for _, want := range []string{"account: none", "You must be 18 or older", "recorded for this gateway is kept instead"} {
		if !strings.Contains(o.stderr, want) {
			t.Errorf("dry run lacks %q: %s", want, o.stderr)
		}
	}
	if o := (pfCase{args: []string{"-dry-run"}}).run(t); o.code != 0 || !strings.Contains(o.stderr, "node id: the one an account-page token carries") ||
		!strings.Contains(o.stderr, "Link this node to your account? [Y/n]") {
		t.Errorf("dry run, nothing installed: exit %d: %s", o.code, o.stderr)
	}
}

// A dry run shows node.json without credentials: the user and password of any URL, and the values of
// credential-like keys. The real install's node.json is untouched by that.
func TestPreflightDryRunRedacts(t *testing.T) {
	sup := filepath.Join(t.TempDir(), "sup.json")
	os.WriteFile(sup, []byte(`{"gateway_url": "`+pfGW+`", "upstream": "http://admin:hunter2@127.0.0.1:8000",
		"models": [{"name": "m", "api_key": "sk-abc"}], "extra": {"Password": "pw1", "note": "https://u@example.org/x"}}`), 0o600)
	o := pfCase{args: []string{"-dry-run", "-account", "link", "-node-config", sup}}.run(t)
	if o.code != 0 {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	b, _ := json.Marshal(o.nodeJSON)
	for _, secret := range []string{"hunter2", "admin", "sk-abc", "pw1", "u@"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("dry run shows %q: %s", secret, b)
		}
	}
	if o.nodeJSON["upstream"] != "http://REDACTED@127.0.0.1:8000" || o.nodeJSON["gateway_url"] != pfGW {
		t.Errorf("dry run node.json %s", b)
	}
	o = pfCase{args: []string{"-account", "link", "-node-config", sup}}.run(t)
	if o.code != 0 || o.nodeJSON["upstream"] != "http://admin:hunter2@127.0.0.1:8000" {
		t.Errorf("the install's node.json was changed: %v", o.nodeJSON)
	}
}

// askTokenState: the token goes in the Authorization header of GET /v1/node/self only; 401 is gone, a held
// node without an account is held, anything else (or no answer, or a redirect) keeps the token.
func TestPreflightAskTokenState(t *testing.T) {
	var bad []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method != http.MethodGet || r.URL.Path != "/v1/node/self" || strings.Contains(r.URL.String(), tok) {
			bad = append(bad, r.Method+" "+r.URL.String())
		}
		switch tok {
		case "held":
			w.Write([]byte(`{"node_id":"a-0000000b","anonymous":true,"state":"held"}`))
		case "acct-held":
			w.Write([]byte(`{"node_id":"n-0000000a","anonymous":false,"state":"held"}`))
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "gone":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Write([]byte(`{"node_id":"a-0000000c","anonymous":true,"state":"admitted"}`))
		}
	}))
	defer srv.Close()
	gw := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/node/connect"
	e := preflightEnv{httpClient: func() *http.Client { return srv.Client() }}
	for tok, want := range map[string]string{"held": "held", "acct-held": "ok", "gone": "gone", "ok": "ok"} {
		if got := e.askTokenState(tok, gw); got != want {
			t.Errorf("%s: %s, want %s", tok, got, want)
		}
	}
	e.httpClient = func() *http.Client { return node.TokenClient(5 * time.Second) }
	if got := e.askTokenState("redirect", gw); got != "ok" {
		t.Errorf("redirect: %s", got)
	}
	if got := e.askTokenState("gone", "ws://127.0.0.1:9/v1/node/connect"); got != "ok" {
		t.Errorf("no answer: %s", got)
	}
	if got := e.askTokenState("gone", "ws://gw.example.org/v1/node/connect"); got != "ok" {
		t.Errorf("ws:// to another machine was asked: %s", got)
	}
	if len(bad) > 0 {
		t.Errorf("unexpected requests: %v", bad)
	}
}
