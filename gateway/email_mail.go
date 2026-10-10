// The sign-in mail and its delivery: one plain multipart (text and HTML) message, no images,
// no tracking, sent over SMTP with STARTTLS required, from a queue after the HTTP answer.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
)

const (
	smtpDialTimeout = 10 * time.Second
	smtpTimeout     = 30 * time.Second // the whole conversation
)

// mailWorker sends queued sign-in mails one at a time until Close.
func (g *Gateway) mailWorker() {
	defer g.bg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-g.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-g.stop:
			return
		case m := <-g.email.queue:
			if err := g.email.send(ctx, m); err != nil {
				g.log.Warn("sign-in email not sent", "err", redactSMTP(err, m.to))
			}
		}
	}
}

// redactSMTP keeps the address out of the log: a server's reply may quote it.
func redactSMTP(err error, to string) string {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return fmt.Sprintf("SMTP reply %d", tp.Code)
	}
	s := strings.ReplaceAll(err.Error(), to, "<address>")
	return strings.ReplaceAll(s, strings.ToLower(to), "<address>")
}

// send delivers one mail. Without STARTTLS nothing is sent: the password and the code never
// cross the network in clear.
func (e *emailSignIn) send(ctx context.Context, m emailMsg) error {
	msg, err := e.message(m)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: smtpDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(e.cfg.SMTPHost, strconv.Itoa(e.cfg.SMTPPort)))
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(smtpTimeout))
	stop := context.AfterFunc(ctx, func() { conn.Close() }) // shutdown interrupts a stalled server
	defer stop()
	c, err := smtp.NewClient(conn, e.cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello(e.site); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("SMTP server offers no STARTTLS; refusing to send")
	}
	if err := c.StartTLS(&tls.Config{ServerName: e.cfg.SMTPHost, RootCAs: e.cfg.RootCAs, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err := c.Auth(smtp.PlainAuth("", e.cfg.SMTPUser, e.cfg.SMTPPassword, e.cfg.SMTPHost)); err != nil {
		return err
	}
	if err := c.Mail(e.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

type mailBody struct {
	Site, Code, Link, At string
}

// message builds the mail: the code in the subject and body, the link with its token in the
// fragment (so it never reaches a server or a log), and the time of the request.
func (e *emailSignIn) message(m emailMsg) ([]byte, error) {
	d := mailBody{Site: e.site, Code: m.code, Link: e.publicURL + "/account/email#t=" + m.token,
		At: m.at.UTC().Format("2006-01-02 15:04 UTC")}
	var text, html bytes.Buffer
	if err := mailText.Execute(&text, d); err != nil {
		return nil, err
	}
	if err := mailHTML.Execute(&html, d); err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	rand.Read(id)
	domain := e.from.Address[strings.LastIndexByte(e.from.Address, '@')+1:]

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	hdr := []string{
		"From: " + e.from.String(),
		"To: <" + m.to + ">",
		"Subject: " + e.site + " sign-in code: " + m.code,
		"Date: " + m.at.UTC().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id) + "@" + domain + ">",
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
		`Content-Type: multipart/alternative; boundary="` + mw.Boundary() + `"`,
	}
	var out bytes.Buffer
	out.WriteString(strings.Join(hdr, "\r\n") + "\r\n\r\n")
	for _, p := range []struct {
		typ  string
		body []byte
	}{{"text/plain", text.Bytes()}, {"text/html", html.Bytes()}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {p.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qw := quotedprintable.NewWriter(pw)
		qw.Write(p.body)
		qw.Close()
	}
	mw.Close()
	out.Write(b.Bytes())
	return out.Bytes(), nil
}

var mailText = texttemplate.Must(texttemplate.New("text").Parse(`Your sign-in code for {{.Site}}:

    {{.Code}}

Or open this link in the browser where you asked for the code:
{{.Link}}

Requested {{.At}}. The code and the link work once, for 10 minutes.

Only enter this code on {{.Site}}. If you didn't ask, ignore this email.
`))

var mailHTML = template.Must(template.New("html").Parse(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;line-height:1.5">
<p>Your sign-in code for {{.Site}}:</p>
<p style="font-size:1.6em;font-weight:bold;letter-spacing:.15em">{{.Code}}</p>
<p>Or <a href="{{.Link}}">continue signing in</a> in the browser where you asked for the code.</p>
<p>Requested {{.At}}. The code and the link work once, for 10 minutes.</p>
<p>Only enter this code on {{.Site}}. If you didn't ask, ignore this email.</p>
</body></html>
`))
