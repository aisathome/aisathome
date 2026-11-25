// The project's stock web RPCs (html/user/create_account.php and am_get_info.php in BOINC's
// source, https://github.com/BOINC/boinc/tree/master/html/user). Both take their parameters in the
// query string, so the calls go to the project's own web server over the local network, not
// through the public address.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// WebRPC calls a project's web RPCs.
type WebRPC struct {
	base   string // the project's web root, ending in "/"
	invite string // the invitation code for create_account.php, if the project wants one
	http   *http.Client
}

// NewWebRPC returns a client for the project at base ("http://apache/project/").
func NewWebRPC(base, invite string) *WebRPC {
	return &WebRPC{base: base, invite: invite, http: &http.Client{
		Timeout: 30 * time.Second,
		// An RPC answers in place; a redirect would carry the query (keys) somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var (
	errNumRe   = regexp.MustCompile(`<error_num>\s*(-?\d+)\s*</error_num>`)
	errMsgRe   = regexp.MustCompile(`<error_msg>([^<]*)</error_msg>`)
	authRe     = regexp.MustCompile(`<authenticator>\s*([0-9a-f]{32})\s*</authenticator>`)
	userIDRe   = regexp.MustCompile(`<id>\s*(\d+)\s*</id>`)
	weakAuthRe = regexp.MustCompile(`<weak_auth>\s*(\d+_[0-9a-f]{32})\s*</weak_auth>`)
)

func (r *WebRPC) CreateAccount(ctx context.Context, email, passwdHash, userName string) (string, error) {
	q := url.Values{"email_addr": {email}, "passwd_hash": {passwdHash}, "user_name": {userName}}
	if r.invite != "" {
		q.Set("invite_code", r.invite)
	}
	body, err := r.get(ctx, "create_account.php", q)
	if err != nil {
		return "", err
	}
	m := authRe.FindSubmatch(body)
	if m == nil {
		return "", errors.New("create_account.php: no account key in the answer")
	}
	return string(m[1]), nil
}

func (r *WebRPC) AccountInfo(ctx context.Context, accountKey string) (Account, error) {
	body, err := r.get(ctx, "am_get_info.php", url.Values{"account_key": {accountKey}})
	if err != nil {
		return Account{}, err
	}
	id, wa := userIDRe.FindSubmatch(body), weakAuthRe.FindSubmatch(body)
	if id == nil || wa == nil {
		return Account{}, errors.New("am_get_info.php: no user id or weak key in the answer")
	}
	a := Account{WeakAuth: string(wa[1])}
	a.ID, _ = strconv.ParseInt(string(id[1]), 10, 64)
	if a.ID <= 0 || string(wa[1][:len(id[1])+1]) != string(id[1])+"_" {
		return Account{}, errors.New("am_get_info.php: the weak key is not the user's")
	}
	return a, nil
}

// get calls an RPC. A BOINC error in the answer is an *RPCError; anything else that went wrong
// is a plain error (worth trying again). Errors never carry the query, which holds keys.
func (r *WebRPC) get(ctx context.Context, page string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", r.base+page+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: bad request", page)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL
		}
		return nil, fmt.Errorf("%s: %w", page, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", page, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", page, resp.StatusCode)
	}
	if m := errNumRe.FindSubmatch(body); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		e := &RPCError{Num: n}
		if mm := errMsgRe.FindSubmatch(body); mm != nil {
			e.Msg = string(mm[1])
		}
		return nil, e
	}
	return body, nil
}
