// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeKey = "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

func TestForNodeStripsIdentity(t *testing.T) {
	body := map[string]any{
		"model": "deep", "temperature": 0.2, "user": "alice@example.com",
		"metadata": map[string]any{"chat_id": "c1"}, "session_id": "s1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi", "name": "Alice", "id": "m1"},
			map[string]any{"role": "tool", "content": "42", "name": "calc", "tool_call_id": "t1"},
		},
	}
	got := forNode(body)
	b, _ := json.Marshal(got)
	for _, leak := range []string{"alice@example.com", "chat_id", "session_id", "Alice", `"id":"m1"`} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("node body leaks %q: %s", leak, b)
		}
	}
	if got["temperature"] != 0.2 || !strings.Contains(string(b), `"name":"calc"`) {
		t.Fatalf("needed fields dropped: %s", b)
	}
}

func TestRedactAndRestore(t *testing.T) {
	r := newRedactor()
	r.restoreOn = true
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "my key is " + fakeKey + " and again " + fakeKey},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "write", "arguments": `{"text":"OPENAI_API_KEY=` + fakeKey + `"}`}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"}}},
	}}
	r.redactBody(body)
	b, _ := json.Marshal(body)
	// the armour lines stay (the model should recognise the format), the key material goes
	if strings.Contains(string(b), fakeKey) || strings.Contains(string(b), `\nabc\n`) {
		t.Fatalf("secret left in node body: %s", b)
	}
	if len(r.byPH) != 2 {
		t.Fatalf("want 2 distinct placeholders (same key twice -> one), got %d", len(r.byPH))
	}
	ph := r.byValue[fakeKey]
	if got := r.restore("write " + ph + " to .env"); got != "write "+fakeKey+" to .env" {
		t.Fatalf("restore: %q", got)
	}
}

// Streamed answers: the placeholder arrives split across chunks of any size.
func TestRestorerSplitPlaceholders(t *testing.T) {
	for _, size := range []int{1, 2, 3, 5, 7, 40} {
		r := newRedactor()
		r.restoreOn = true
		r.redactText(fakeKey)
		ph := r.byValue[fakeKey]
		text := "Set OPENAI_API_KEY=" + ph + " in the env file, then restart it."
		args := `{"path":".env","text":"KEY=` + ph + `"}`
		s := newRestorer(r)
		var gotText, gotArgs, nodeText strings.Builder
		text1 := func(raw []byte) string {
			var c struct {
				Choices []struct {
					Delta struct{ Content string } `json:"delta"`
				} `json:"choices"`
			}
			json.Unmarshal(raw, &c)
			t := ""
			for _, ch := range c.Choices {
				t += ch.Delta.Content
			}
			return t
		}
		// emit takes a chunk as the client gets it (out) and as the node wrote it, cut to what
		// was released (orig).
		emit := func(raw, orig []byte) {
			nodeText.WriteString(text1(orig))
			if strings.Contains(string(orig), fakeKey) {
				t.Fatalf("size %d: the released node text holds the secret: %s", size, orig)
			}
			var c struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Function struct{ Arguments string } `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			json.Unmarshal(raw, &c)
			for _, ch := range c.Choices {
				gotText.WriteString(ch.Delta.Content)
				for _, tc := range ch.Delta.ToolCalls {
					gotArgs.WriteString(tc.Function.Arguments)
				}
			}
		}
		for i := 0; i < len(text); i += size {
			piece := text[i:min(i+size, len(text))]
			emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": piece}}}})))
		}
		for i := 0; i < len(args); i += size {
			piece := args[i:min(i+size, len(args))]
			emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": piece}}}}}}})))
		}
		emit(s.chunk(mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})))
		if f, orig := s.flush(); f != nil {
			emit(f, orig)
		}
		want := strings.ReplaceAll(text, ph, fakeKey)
		if nodeText.String() != text { // credit counts the node's own text, all of it released
			t.Fatalf("size %d: released node text\n got %q\nwant %q", size, nodeText.String(), text)
		}
		if gotText.String() != want {
			t.Fatalf("size %d: text\n got %q\nwant %q", size, gotText.String(), want)
		}
		if gotArgs.String() != args { // tool arguments pass through with the placeholder
			t.Fatalf("size %d: args %q", size, gotArgs.String())
		}
	}
}

func TestPIIDetection(t *testing.T) {
	msg := func(s string) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": s}}}
	}
	// a syntactically valid Norwegian national identity number, constructed for the test
	fnr := ""
	for a := 0; a < 10 && fnr == ""; a++ {
		for b := 0; b < 10; b++ {
			if c := "010180123" + itoa(a) + itoa(b); fodselsnummer(c) {
				fnr = c
				break
			}
		}
	}
	if fnr == "" {
		t.Fatal("could not construct a test identity number")
	}
	for _, c := range []struct {
		text string
		want bool
	}{
		{"mail me at ola.nordmann@example.no", true},
		{"card 4111 1111 1111 1111 please", true},
		{"card 4111 1111 1111 1112 please", false}, // fails Luhn
		{"IBAN NO93 8601 1117 947", true},
		{"number " + fnr, true},
		{"order 12345678901 shipped", fodselsnummer("12345678901")},
		{"plain text with numbers 2026 and 42", false},
		{"new file mode 100644\nindex 00000000000..5ea936f9122\n--- /dev/null", false}, // git's null hash
		{"deleted file mode 100644\nindex 5ea936f9122..0000000000000\n", false},        // 13 zeros pass Luhn
		{"zeros 0000 0000 0000 0000", false},
		{"Amex 3782 822463 10005", true},
		{"Mastercard 5555555555554444", true},
		{"card 4111-1111-1111-1111", true}, // dashes between digits are separators, not a minus
		// the review of 21f1ddd: a card next to a short "/" operand, a path, and networks an
		// issuer table missed (Diners 3095, 13-digit Maestro), all Luhn-valid
		{"card 4111111111111111/12/27", true},
		{"GET /cards/4111111111111111", true},
		{"Diners 30950000000000", true},
		{"Maestro 6759000000005", true},
		// groups are joined only in written layouts; a card after a short number still counts
		{"page 3 4111 1111 1111 1111", true},
		{"Diners 3095 000000 0000", true},
		{"number " + fnr[:6] + " " + fnr[6:], true},
		{"number " + fnr[:2] + " " + fnr[2:], false},
	} {
		if got := hasPII(msg(c.text)); got != c.want {
			t.Errorf("%q: got %v want %v", c.text, got, c.want)
		}
	}
}

// Numbers in mathematics and code are not personal data (2026-11-07: a prove2me definition's
// exact rationals sent a whole agent session to trusted nodes only, and with none online every
// request after it failed). Each case passes Luhn, so only the new rules keep it out.
func TestPIIIgnoresArithmeticAndIDs(t *testing.T) {
	msg := func(s string) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "tool", "content": s}}}
	}
	// Luhn-valid numbers from start upwards: timestamps in ms, µs and ns (2026), and a UATP-like
	// 15-digit number starting with 1
	luhnFrom := func(start int64) string {
		for v := start; v < start+1000; v++ {
			if s := fmt.Sprint(v); luhn(s) {
				return s
			}
		}
		return ""
	}
	ts, us, ns, uatp := luhnFrom(1794069970000), luhnFrom(1794069970000000), luhnFrom(1794069970000000000), luhnFrom(135410014004000)
	if us == "" || ns == "" || uatp == "" {
		t.Fatal("could not construct the timestamps")
	}
	if !cardNumber("3100010004096") {
		t.Fatal("the ls -l columns below no longer pass the card check joined; the case tests nothing")
	}
	if k := piiKind(msg("UATP " + uatp)); k != piiCard {
		t.Errorf("a 15-digit number starting with 1 (UATP): kind %q", k)
	}
	// an 11-digit number with valid mod-11 control digits but no birth date (day 99)
	nodate := ""
	for v := 990180000; v < 990190000 && nodate == ""; v++ {
		for k := 0; k < 100; k++ {
			if s := fmt.Sprintf("%09d%02d", v, k); fodselsnummer11(s) {
				nodate = s
				break
			}
		}
	}
	if ts == "" || nodate == "" {
		t.Fatal("could not construct the test numbers")
	}
	for _, c := range []string{
		"(33445726437815 / 116459807114612), (82894103493547 / 116459807114612)", // the prove2me Gram data
		"x = 4111111111111111 / 1164598071146",                                   // a numerator with a card's form, over a long number
		"y = 1164598071146/4111111111111111",                                     // and a denominator
		"pi is about 3.4111111111111111",                                         // a decimal's digits
		"v = 4111111111111111.5",                                                 // the digits before a decimal point
		"offset -4111111111111111 bytes",                                         // a negative number
		"step_start timestamp " + ts + " ok",                                     // a millisecond timestamp: no card starts with 1
		"timestamp " + us,                                                        // in microseconds (the review of 9c299ec)
		"timestamp " + ns,                                                        // and nanoseconds
		"id " + nodate,                                                           // no birth date
		"drwxr-xr-x 3 1000 1000 4096 Nov  8 16:55 .cache",                        // ls -l columns (2026-11-08, a prove2me session)
		"total 411 111 111 111 116 bytes",                                        // thousands separated by spaces
		"-rw-r--r-- 1 65534 65534 4109 Nov  8 16:55 output.log",                  // and with owner nobody (review of 073c340)
	} {
		if k := piiKind(msg(c)); k != "" {
			t.Errorf("%q: flagged as %s", c, k)
		}
	}
	if k := piiKind(msg("card 4111111111111111")); k != piiCard {
		t.Errorf("a card number: kind %q", k)
	}
	if k := piiKind(msg("mail ola.nordmann@example.no")); k != piiEmail {
		t.Errorf("an email address: kind %q", k)
	}
}

// fodselsnummer11 is the mod-11 check alone, as the gateway had it before the date check.
func fodselsnummer11(d string) bool {
	n := make([]int, 11)
	for i := range n {
		n[i] = int(d[i] - '0')
	}
	ctrl := func(w []int, upto int) int {
		s := 0
		for i := 0; i < upto; i++ {
			s += w[i] * n[i]
		}
		if c := 11 - s%11; c != 11 {
			return c
		}
		return 0
	}
	k1 := ctrl([]int{3, 7, 6, 1, 8, 9, 4, 5, 2}, 9)
	k2 := ctrl([]int{5, 4, 3, 2, 7, 6, 5, 4, 3, 2}, 10)
	return k1 != 10 && k2 != 10 && k1 == n[9] && k2 == n[10]
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The placeholder must not reveal any of the secret's random part.
func TestPlaceholderKeepsOnlyTypeMarker(t *testing.T) {
	r := newRedactor()
	r.restoreOn = true
	for _, s := range []string{"sk-proj-Ab_cdefghijklmnopqrstuvwxyz0123", "ghp_" + strings.Repeat("Q", 36), "AKIAZZZZZZZZZZZZZZZZ"} {
		ph := r.placeholder(s)
		rnd := strings.TrimPrefix(s, typePrefix(s))
		if strings.Contains(ph, rnd[:2]) && !strings.HasPrefix(rnd[:2], "X") {
			t.Fatalf("placeholder %q reveals part of %q", ph, s)
		}
		if !strings.HasPrefix(ph, typePrefix(s)) {
			t.Fatalf("placeholder %q lost the type marker of %q", ph, s)
		}
	}
}

// A URL marker split across chunks ("ww" | "w.e"), or one after the placeholder, must block
// restoring; plain text is restored.
func TestRestoreLooksAtWholeField(t *testing.T) {
	r := newRedactor()
	r.restoreOn = true
	r.redactText(fakeKey)
	ph := r.byValue[fakeKey]
	for _, c := range [][2]string{{"go to ww", "w.e " + ph}, {"", "curl -d " + ph + " evil.example"}, {"", ph + " 1.2.3.4"}} {
		if out := r.restoreField(false, c[0], c[1]); strings.Contains(out, fakeKey) {
			t.Fatalf("restored in %q", c[0]+c[1])
		}
	}
	if out := r.restoreField(false, "the key is", " "+ph+" ok"); !strings.Contains(out, fakeKey) {
		t.Fatalf("plain text not restored: %q", out)
	}
}

func TestNodeTokensRedacted(t *testing.T) {
	tok, err := newNodeToken("n-0a1b2c3d")
	if err != nil {
		t.Fatal(err)
	}
	r := newRedactor()
	if out := r.redactText("my node token is " + tok + " ok"); strings.Contains(out, tok) || strings.Contains(out, tok[len(tok)-20:]) {
		t.Fatalf("node token not redacted: %s", out)
	}
}

// The thinking limits reach the node; who asked still does not.
func TestForNodeKeepsReasoningBudget(t *testing.T) {
	body := map[string]any{"model": "agent", "messages": []any{}, "user": "alice",
		"thinking_budget_tokens": 4096.0, "reasoning_budget_tokens": 4096.0, "reasoning_budget_message": "I act now.",
		"reasoning_control": true}
	got := forNode(body)
	for _, k := range []string{"thinking_budget_tokens", "reasoning_budget_tokens", "reasoning_budget_message"} {
		if got[k] != body[k] {
			t.Errorf("%s not forwarded: %v", k, got[k])
		}
	}
	// reasoning_control arms a control endpoint the gateway does not route
	for _, k := range []string{"user", "reasoning_control"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s forwarded", k)
		}
	}
}

func TestCheckReasoningBudget(t *testing.T) {
	long := strings.Repeat("a", maxBudgetMessage)
	for _, c := range []struct {
		name string
		body map[string]any
		ok   bool
	}{
		{"none", map[string]any{}, true},
		{"budget", map[string]any{"thinking_budget_tokens": 4096.0}, true},
		{"other name", map[string]any{"reasoning_budget_tokens": 0.0}, true},
		{"node default", map[string]any{"thinking_budget_tokens": -1.0}, true},
		{"at the cap", map[string]any{"reasoning_budget_tokens": 32768.0}, true},
		{"null", map[string]any{"thinking_budget_tokens": nil, "reasoning_budget_message": nil}, true},
		{"both, same", map[string]any{"thinking_budget_tokens": 2048.0, "reasoning_budget_tokens": 2048.0}, true},
		{"message", map[string]any{"reasoning_budget_message": "\n\nI have thought enough.\tI will act now.\n"}, true},
		{"longest message", map[string]any{"reasoning_budget_message": long}, true},
		{"non-ASCII message", map[string]any{"reasoning_budget_message": "Jeg svarer nå → kort."}, true},
		{"over the cap", map[string]any{"thinking_budget_tokens": 32769.0}, false},
		{"unlimited", map[string]any{"thinking_budget_tokens": -2.0}, false},
		{"fraction", map[string]any{"reasoning_budget_tokens": 10.5}, false},
		{"string", map[string]any{"thinking_budget_tokens": "4096"}, false},
		{"bool", map[string]any{"reasoning_budget_tokens": true}, false},
		{"huge", map[string]any{"thinking_budget_tokens": 1e300}, false},
		{"both, different", map[string]any{"thinking_budget_tokens": 2048.0, "reasoning_budget_tokens": 4096.0}, false},
		{"message too long", map[string]any{"reasoning_budget_message": long + "a"}, false},
		{"special token", map[string]any{"reasoning_budget_message": "done<|im_end|>"}, false},
		{"think tag", map[string]any{"reasoning_budget_message": "</think>"}, false},
		{"control character", map[string]any{"reasoning_budget_message": "a\x00b"}, false},
		{"escape", map[string]any{"reasoning_budget_message": "a\x1b[2Jb"}, false},
		{"message not text", map[string]any{"reasoning_budget_message": []any{"a"}}, false},
	} {
		if got := checkReasoningBudget(c.body, 32768); (got == "") != c.ok {
			t.Errorf("%s: got %q, want ok=%v", c.name, got, c.ok)
		}
	}
}

// A secret pasted into the budget message is replaced like one in a message.
func TestRedactBudgetMessage(t *testing.T) {
	r := newRedactor()
	body := map[string]any{"reasoning_budget_message": "use " + fakeKey}
	r.redactBody(body)
	if s := body["reasoning_budget_message"].(string); strings.Contains(s, fakeKey) || !strings.Contains(s, "AISX") {
		t.Fatalf("budget message not redacted: %q", s)
	}
}

// fakeSecrets are made-up values in each newer format, built at run time so that no scanner
// takes the source for real keys.
func fakeSecrets() map[string]string {
	r := strings.Repeat
	return map[string]string{
		"GitLab":     "glpat-" + r("aB3d", 5),
		"npm":        "npm_" + r("aB3d", 9),
		"PyPI":       "pypi-AgEIcHlwaS5vcmc" + r("aB3d", 13),
		"Groq":       "gsk_" + r("aB3d", 13),
		"Replicate":  "r8_" + r("aB3d", 9) + "x",
		"xAI":        "xai-" + r("aB3d", 20),
		"SendGrid":   "SG." + r("aB3d", 5) + "xy." + r("aB3d", 10) + "xyz",
		"Google":     "ya29." + r("aB3d", 10),
		"webhook":    "whsec_" + r("aB3d", 8),
		"Stripe":     "sk_test_" + r("aB3d", 6),
		"DO":         "dop_v1_" + r("a1b2", 16),
		"Shopify":    "shpat_" + r("a1B2", 8),
		"Vault":      "hvs." + r("aB3d", 8),
		"Linear":     "lin_api_" + r("aB3d", 10),
		"Supabase":   "sbp_" + r("a1b2", 10),
		"Tailscale":  "tskey-auth-" + r("aB3d", 3) + "-" + r("aB3d", 6),
		"age":        "AGE-SECRET-KEY-1" + r("QPZR", 14) + "QP",
		"Telegram":   "123456789:AA" + r("aB3d", 8) + "x",
		"Slack hook": "https://hooks.slack.com/services/T" + r("A1", 5) + "/B" + r("A1", 5) + "/" + r("aB3d", 6),
		"Discord":    "https://discord.com/api/webhooks/" + r("1", 18) + "/" + r("aB3d", 17),
		"PGP": "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBF" + r("aB3d", 10) + "\n=abcd\n" +
			"-----END PGP PRIVATE KEY BLOCK-----",
	}
}

// Each newer format is replaced before a node sees it, and its placeholder keeps only the
// format's marker.
func TestNewerSecretFormatsRedacted(t *testing.T) {
	for name, s := range fakeSecrets() {
		r := newRedactor()
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "export TOKEN=" + s + " # ok"}}}
		r.redactBody(body)
		b, _ := json.Marshal(body)
		if strings.Contains(string(b), s) || len(r.byPH) != 1 {
			t.Errorf("%s: not redacted (%d placeholders): %s", name, len(r.byPH), b)
			continue
		}
		ph := r.byValue[s]
		if p := typePrefix(s); !strings.HasPrefix(ph, p) || (name != "Telegram" && name != "PGP" && p == "") {
			t.Errorf("%s: placeholder %q lost the marker %q", name, ph, p)
		}
		if rnd := strings.TrimPrefix(s, typePrefix(s)); name != "PGP" && strings.Contains(ph, rnd[:6]) {
			t.Errorf("%s: placeholder %q shows the random part", name, ph)
		}
	}
}

// Ordinary code and prose near those markers stays as it is.
func TestNewerSecretFormatsLeaveCodeAlone(t *testing.T) {
	for _, s := range []string{
		"npm_config_cache=/tmp/npm", "SG.send(msg)", "sk_test_mode = True", "lin_api_url = base + '/graphql'",
		"see https://hooks.slack.com/services/ for setup", "the time was 12:34:56, AA battery",
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nabc\n-----END PGP PUBLIC KEY BLOCK-----",
		"tskey-auth is the prefix", "hvs.example", "gsk_short", "r8_ok",
	} {
		r := newRedactor()
		if out := r.redactText(s); out != s {
			t.Errorf("changed %q to %q", s, out)
		}
	}
}

// Only the template switches the gateway renders reach a node; the client's body keeps the rest.
func TestForNodeKeepsOnlyTemplateSwitches(t *testing.T) {
	kw := map[string]any{"enable_thinking": false, "preserve_thinking": true, "reasoning_effort": "low",
		"preserve_reasoning": false, "user_name": "Alice", "session": "s-1"}
	body := map[string]any{"messages": []any{}, "chat_template_kwargs": kw}
	got := forNode(body)["chat_template_kwargs"].(map[string]any)
	if len(got) != 4 || got["enable_thinking"] != false || got["preserve_thinking"] != true ||
		got["reasoning_effort"] != "low" || got["preserve_reasoning"] != false {
		t.Fatalf("template switches: %v", got)
	}
	if len(kw) != 6 {
		t.Fatalf("forNode changed the request itself: %v", kw)
	}
}

// Every credential format the gateway issues is redacted, also glued to a word where its form
// has an id (node tokens, pilot credentials), in text and in JSON keys.
func TestIssuedCredentialsRedacted(t *testing.T) {
	apiKey, _ := newAPIKey()
	keysFile := &KeysFile{}
	fileKey, _ := keysFile.Add(KeyInfo{User: "u"}, time.Now())
	accountNode, _ := newNodeToken("n-0a1b2c3d")
	anonNode, _ := newNodeToken(anonNodePrefix + "0a1b2c3d")
	store, err := OpenAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	registered, _, err := store.RegisterAnon(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, pilot, _ := newPilotCredential()
	for name, c := range map[string]struct {
		tok   string
		glued bool
	}{
		"API key": {apiKey, false}, "keys file key": {fileKey, false},
		"account node token": {accountNode, true}, "anonymous node token": {anonNode, true},
		"registered anonymous node token": {registered, true}, "pilot credential": {pilot, true},
	} {
		secret := c.tok[len(c.tok)-30:]
		contexts := []string{"my token is " + c.tok + " ok", "TOKEN=" + c.tok, `{"token":"` + c.tok + `"}`, "https://x.example/?t=" + c.tok + "&a=1"}
		if c.glued {
			contexts = append(contexts, "token"+c.tok)
		}
		for _, text := range contexts {
			if out := newRedactor().redactText(text); strings.Contains(out, secret) {
				t.Errorf("%s not redacted in %q: %s", name, text, out)
			}
		}
		r := newRedactor()
		body := map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{c.tok: map[string]any{"type": "string"}}}}}}}
		r.redactBody(body)
		if b, _ := json.Marshal(body); strings.Contains(string(b), secret) {
			t.Errorf("%s not redacted in a JSON key: %s", name, b)
		}
	}
}
