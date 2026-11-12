// SPDX-License-Identifier: Apache-2.0
package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aisathome/aisathome/node/localui"
)

// Client talks to the agent's local page for the tray: the status (public), and the engine
// controls with a session that a one-time sign-in link gives it (never the password).
type Client struct {
	Base string                 // the page, e.g. http://127.0.0.1:7381
	Link func() (string, error) // a one-time sign-in link (localui.AskLoginLink over the agent's pipe)
	HTTP *http.Client           // follows no redirects

	mu     sync.Mutex
	cookie *http.Cookie
}

// NewClient is a Client for the page on 127.0.0.1:port.
func NewClient(port int, link func() (string, error)) *Client {
	return &Client{Base: "http://" + localui.Addr(port), Link: link,
		HTTP: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Status is the agent's /api/status, without the GPU (no nvidia-smi run for the tray).
func (c *Client) Status(ctx context.Context) (*localui.StatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/status?gpu=0", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status: HTTP %d", resp.StatusCode)
	}
	var st localui.StatusResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	return &st, nil
}

// PageURL is where "Open the node page" goes: a fresh sign-in link, or the page itself (which
// asks for the password) with the reason there is no link.
func (c *Client) PageURL() (string, error) {
	u, err := c.Link()
	if err != nil {
		return c.Base + "/", err
	}
	return u, nil
}

// SignIn turns a fresh sign-in link into the client's own session.
func (c *Client) SignIn(ctx context.Context) error {
	u, err := c.Link()
	if err != nil {
		return err
	}
	ck, err := c.redeem(ctx, u)
	if err != nil {
		return err
	}
	if ck == nil {
		return errors.New("the sign-in link gave no session (used or expired)")
	}
	c.mu.Lock()
	c.cookie = ck
	c.mu.Unlock()
	return nil
}

// Control runs a control of the page ("stop-engine", "start-engine") and returns the agent's
// message. It signs in when it has no session or the session has ended (a new password).
func (c *Client) Control(ctx context.Context, action string) (string, error) {
	for try := 0; ; try++ {
		c.mu.Lock()
		ck := c.cookie
		c.mu.Unlock()
		if ck == nil {
			if err := c.SignIn(ctx); err != nil {
				return "", err
			}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/"+action, strings.NewReader("{}"))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return "", err
		}
		var body struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && try == 0:
			c.mu.Lock()
			c.cookie = nil
			c.mu.Unlock()
			continue
		case resp.StatusCode/100 == 2:
			return body.Message, nil
		case body.Error != "":
			return "", errors.New(body.Error)
		}
		return "", fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
}

// CheckSignIn is the tray's self-test (aisathome-tray -check): a sign-in link signs in once,
// the same link a second time does not, and the session it gave is signed in on the page.
func (c *Client) CheckSignIn(ctx context.Context) error {
	u, err := c.Link()
	if err != nil {
		return fmt.Errorf("no sign-in link: %w", err)
	}
	once := func() (*http.Cookie, error) {
		return c.redeem(ctx, u)
	}
	ck, err := once()
	if err != nil {
		return err
	}
	if ck == nil {
		return errors.New("the sign-in link gave no session")
	}
	if again, err := once(); err != nil || again != nil {
		return fmt.Errorf("the same sign-in link worked twice (or failed: %v)", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/status?gpu=0", nil)
	if err != nil {
		return err
	}
	req.AddCookie(ck)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var st localui.StatusResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return err
	}
	if !st.Controls.SignedIn {
		return errors.New("the link's session is not signed in on the page")
	}
	return nil
}

// redeem opens a sign-in link and returns the session cookie it set, or nil.
func (c *Client) redeem(ctx context.Context, link string) (*http.Cookie, error) {
	if !strings.HasPrefix(link, "http://") {
		return nil, fmt.Errorf("not a sign-in link: %q", link)
	}
	// The link names the agent's own address; send it to Base (the same, except in tests).
	_, path, _ := strings.Cut(strings.TrimPrefix(link, "http://"), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return nil, fmt.Errorf("sign-in link: HTTP %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Value != "" && ck.MaxAge >= 0 {
			return &http.Cookie{Name: ck.Name, Value: ck.Value}, nil
		}
	}
	return nil, nil
}

// LogFile is the tray's log, per user: %LOCALAPPDATA%\AIsAtHome\tray.log ("" without
// LOCALAPPDATA).
func LogFile(getenv func(string) string) string {
	d := strings.TrimRight(strings.TrimSpace(getenv("LOCALAPPDATA")), `\/`)
	if d == "" {
		return ""
	}
	return d + `\AIsAtHome\tray.log`
}
