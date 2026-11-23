// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

const usageSecret = "0123456789abcdef0123456789abcdef"

// The name `accounts list` looks for is the one the gateway writes into the usage log for an
// account's requests (API key or chat).
func TestAccountPseudonymIsTheLogsUser(t *testing.T) {
	g := &Gateway{acfg: AccountsConfig{SessionSecret: usageSecret}}
	for _, id := range []string{"gh:1", "google:123", "em:0123456789abcdef0123456789abcdef"} {
		if got, want := AccountPseudonym(usageSecret, id), g.accountInfo(id).User; got != want || !strings.HasPrefix(got, "acct-") {
			t.Fatalf("AccountPseudonym(%s) = %q, the log has %q", id, got, want)
		}
	}
	if AccountPseudonym(usageSecret, "gh:1") == AccountPseudonym(usageSecret+"x", "gh:1") {
		t.Fatal("the pseudonym does not depend on the secret")
	}
}

func TestReadAccountTotals(t *testing.T) {
	p := func(id string) string { return AccountPseudonym(usageSecret, id) }
	rec := func(user, at string, prompt, completion int) string {
		return `{"time":"` + at + `","req_id":"r","user":"` + user + `","node":"n-1","operator":"op","model":"m","requested":"m",` +
			`"usage":{"prompt_tokens":` + strconv.Itoa(prompt) + `,"completion_tokens":` + strconv.Itoa(completion) + `,"cached_tokens":5,"duration_ms":10}}`
	}
	ids := []string{"gh:1", "gh:2", "gh:3"}
	for _, tc := range []struct {
		name      string
		log       string
		want      map[string]AccountTotals
		malformed int
	}{
		{name: "empty", log: "", want: map[string]AccountTotals{}},
		{
			name: "sums per account, latest time, others left out",
			log: strings.Join([]string{
				rec(p("gh:1"), "2026-11-01T10:00:00Z", 100, 20),
				rec(p("gh:2"), "2026-11-02T10:00:00Z", 7, 3),
				rec(p("gh:1"), "2026-11-20T23:59:59.5Z", 1000, 200), // the latest of gh:1
				rec(p("gh:1"), "2026-11-05T10:00:00Z", 1, 1),        // older, after it in the file
				rec("project:protein", "2026-11-21T10:00:00Z", 5000, 5000),
				rec("openwebui/u-123", "2026-11-21T10:00:00Z", 5000, 5000),
				rec(AccountPseudonym("another secret", "gh:2"), "2026-11-21T10:00:00Z", 5000, 5000),
				rec(p("gh:9"), "2026-11-21T10:00:00Z", 5000, 5000), // an account not in the file
			}, "\n") + "\n",
			want: map[string]AccountTotals{
				"gh:1": {Requests: 3, Tokens: 1322, Last: time.Date(2026, 11, 20, 23, 59, 59, 5e8, time.UTC)},
				"gh:2": {Requests: 1, Tokens: 10, Last: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)},
			},
		},
		{
			name: "malformed lines skipped and counted, blank lines not",
			log: strings.Join([]string{
				rec(p("gh:3"), "2026-11-03T10:00:00Z", 10, 10),
				`garbage`,
				``,
				`   `,
				`{"time":"yesterday","user":"` + p("gh:3") + `","usage":{"prompt_tokens":1}}`,
				`{"user":42}`,
				rec(p("gh:3"), "2026-11-04T10:00:00Z", 1, 2),
				`{"time":"2026-11-05T10:00:00Z","user":"` + p("gh:3") + `","usage":{"prompt_tok`, // torn last line
			}, "\n"),
			want:      map[string]AccountTotals{"gh:3": {Requests: 2, Tokens: 23, Last: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}},
			malformed: 4,
		},
		{
			name: "a record without usage counts no tokens",
			log:  rec(p("gh:1"), "2026-11-08T10:00:00Z", 50, 50) + "\n" + `{"time":"2026-11-09T10:00:00Z","user":"` + p("gh:1") + `"}` + "\n",
			want: map[string]AccountTotals{"gh:1": {Requests: 2, Tokens: 100, Last: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}},
		},
		{
			name: "a last line without a newline counts",
			log:  rec(p("gh:2"), "2026-11-06T10:00:00Z", 4, 5),
			want: map[string]AccountTotals{"gh:2": {Requests: 1, Tokens: 9, Last: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}},
		},
		{
			name: "a line longer than the buffer is one malformed line; the next is read",
			log: `{"user":"` + p("gh:1") + `","pad":"` + strings.Repeat("x", 200<<10) + `"}` + "\n" +
				rec(p("gh:1"), "2026-11-07T10:00:00Z", 2, 3) + "\n",
			want:      map[string]AccountTotals{"gh:1": {Requests: 1, Tokens: 5, Last: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}},
			malformed: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, malformed, err := ReadAccountTotals(strings.NewReader(tc.log), usageSecret, ids)
			if err != nil {
				t.Fatal(err)
			}
			if malformed != tc.malformed {
				t.Errorf("malformed %d, want %d", malformed, tc.malformed)
			}
			if len(got) != len(tc.want) {
				t.Errorf("totals %+v, want %+v", got, tc.want)
			}
			for id, w := range tc.want {
				if g := got[id]; g.Requests != w.Requests || g.Tokens != w.Tokens || !g.Last.Equal(w.Last) {
					t.Errorf("%s: %+v, want %+v", id, g, w)
				}
			}
		})
	}
}

// A read error is an error, not totals that look complete.
func TestReadAccountTotalsReadError(t *testing.T) {
	r := io.MultiReader(strings.NewReader(`{"user":"x"}`+"\n"), errReader{})
	if _, _, err := ReadAccountTotals(r, usageSecret, []string{"gh:1"}); err == nil {
		t.Fatal("a read error gave totals")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }
