// Each account's use over all time, from the usage log (`aisathome-gateway accounts list`):
// requests, tokens and the last request, for as far back as the log goes. The log names an
// account by its pseudonym (Gateway.pseudonym), so the admin CLI needs the session secret to
// find an account's records.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// AccountTotals is one account's use in the whole usage log.
type AccountTotals struct {
	Requests int64     // records: one per answer attempt (a request retried on another node counts again)
	Tokens   int64     // prompt + completion tokens as logged (unweighted)
	Last     time.Time // the latest record's time
}

// AccountPseudonym is the name the usage log gives account id under the session secret: no
// GitHub, Google or email id in the log, yet the operator can still find an account's records.
func AccountPseudonym(secret, id string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("usage|" + id))
	return "acct-" + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// ReadAccountTotals sums the usage log r per account of ids, streaming it line by line. Records
// of anyone else (config keys, science projects, front-end users without an account, accounts not
// in ids or from before a change of the session secret) are left out. Lines that are not a record
// (a torn last line, damage, a line too long to be one) are skipped and counted in malformed.
func ReadAccountTotals(r io.Reader, secret string, ids []string) (totals map[string]AccountTotals, malformed int, err error) {
	byUser := make(map[string]string, len(ids))
	for _, id := range ids {
		byUser[AccountPseudonym(secret, id)] = id
	}
	totals = map[string]AccountTotals{}
	var rec struct {
		Time  time.Time `json:"time"`
		User  string    `json:"user"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	add := func(line []byte) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return
		}
		rec.Time, rec.User, rec.Usage.PromptTokens, rec.Usage.CompletionTokens = time.Time{}, "", 0, 0
		if json.Unmarshal(line, &rec) != nil {
			malformed++
			return
		}
		id, ok := byUser[rec.User]
		if !ok {
			return
		}
		t := totals[id]
		t.Requests++
		t.Tokens += rec.Usage.PromptTokens + rec.Usage.CompletionTokens
		if rec.Time.After(t.Last) {
			t.Last = rec.Time
		}
		totals[id] = t
	}
	br := bufio.NewReaderSize(r, 64<<10)
	tooLong := false // inside a line longer than the buffer: skipped up to its end
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if !tooLong {
				malformed++
			}
			tooLong = true
			continue
		case tooLong:
			tooLong = false // the end of the long line
		default:
			add(line)
		}
		if err == io.EOF {
			return totals, malformed, nil
		}
		if err != nil {
			return nil, malformed, err
		}
	}
}
