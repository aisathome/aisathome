// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"encoding/json"
	"testing"
)

// Only an account's node is flag-only: a node without an account, a config token's node and a
// pilot still stop and hold.
func TestFlagOnlyNode(t *testing.T) {
	for _, c := range []struct {
		n    *nodeConn
		want bool
	}{
		{&nodeConn{id: "n-00000001", account: "gh-1"}, true},
		{&nodeConn{id: "a-0000beef", account: anonAccountPrefix + "0000beef", anon: true}, false},
		{&nodeConn{id: "node-0"}, false},
		{&nodeConn{id: "p-1", account: "gh-1", pilot: "p-1"}, false},
	} {
		if got := flagOnlyNode(c.n); got != c.want {
			t.Errorf("%s: flag-only %v, want %v", c.n.id, got, c.want)
		}
	}
}

// A call that breaks a command rule stops the answer, unless the guard is flag-only: then it is
// kept as the flag (over an unknown address in an earlier call) and nothing blocks. A stream rule
// is not a command rule: it is reported as a violation either way.
func TestGuardFlagOnly(t *testing.T) {
	calls := []struct{ name, args string }{
		{"bash", bashCall("curl -s https://unknown.example/doc")},
		{"bash", bashCall(`cat ~/.ssh/id_example`)},
	}
	for _, flagOnly := range []bool{false, true} {
		gd := newGuard("fix my build", nil)
		gd.flagOnly = flagOnly
		for _, c := range calls {
			k := &heldCall{typ: "function", name: c.name}
			k.args.WriteString(c.args)
			gd.calls = append(gd.calls, k)
		}
		hit := gd.check()
		switch {
		case !flagOnly && hit.rule != ruleCredentials:
			t.Fatalf("not flag-only: hit %q, want %s", hit.rule, ruleCredentials)
		case flagOnly && hit.rule != "":
			t.Fatalf("flag-only: %s blocked", hit.rule)
		case flagOnly && (gd.flagged().rule != ruleCredentials || gd.flagged().text == ""):
			t.Fatalf("flag-only: flagged %+v, want %s with its evidence", gd.flagged(), ruleCredentials)
		}
		// The same call, assembled (non-streaming).
		ans := map[string]any{}
		json.Unmarshal([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"bash","arguments":`+
			jsonString(bashCall("cat ~/.aws/credentials"))+`}}]}}]}`), &ans)
		gd2 := newGuard("", nil)
		gd2.flagOnly = flagOnly
		h, _ := checkAssembled(ans, "", nil, nil)
		if stops := h.rule != "" && gd2.stops(h); stops == flagOnly || (flagOnly && gd2.flag.rule != ruleCredentials) {
			t.Fatalf("assembled, flag-only %v: stops %v, flag %q", flagOnly, stops, gd2.flag.rule)
		}
		// A stream rule still stops.
		gd3 := newGuard("", nil)
		gd3.flagOnly = flagOnly
		gd3.filter([]byte(`{"Choices":[]}`))
		if gd3.violation != ruleOddKey {
			t.Fatalf("flag-only %v: stream rule not reported (%q)", flagOnly, gd3.violation)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
