package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// Cancelling the context (shutdown) interrupts an SMTP server that accepted the connection
// and then stalls; without it, the send would wait for the 30-second deadline.
func TestSMTPSendStopsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, then say nothing
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	e := newEmailSignIn(EmailConfig{SMTPHost: "127.0.0.1", SMTPPort: port, From: "Test <noreply@site.test>"},
		"https://site.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	err = e.send(ctx, emailMsg{to: "someone@site.test", code: "123456"})
	if d := time.Since(start); err == nil || d > 3*time.Second {
		t.Fatalf("send returned after %s (err %v): cancellation must close a stalled connection", d, err)
	}
}
