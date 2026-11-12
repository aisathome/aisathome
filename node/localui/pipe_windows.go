//go:build windows

// SPDX-License-Identifier: Apache-2.0
package localui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrayPipe is the agent's pipe for the tray (pipe.go).
const TrayPipe = `\\.\pipe\aisathome-node`

const (
	// pipeSDDL: SYSTEM and Administrators, and any signed-in account on this computer (AU; the
	// pipe rejects remote clients) may connect, with read and write-data only: not
	// FILE_CREATE_PIPE_INSTANCE, so no user can add an instance and answer the tray in the
	// agent's place. Whether the caller gets a link is decided after it connects
	// (mayReadPassword), from its token.
	pipeSDDL         = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12008b;;;AU)"
	pipeClientAccess = windows.GENERIC_READ | windows.FILE_WRITE_DATA
	pipeTimeout      = 3 * time.Second // a client gets this long to ask and read the answer
	pipeMaxLine      = 1024
	fileReadData     = 0x1 // FILE_READ_DATA
	inheritOnlyACE   = 0x8 // INHERIT_ONLY_ACE
)

var (
	modadvapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClient = modadvapi32.NewProc("ImpersonateNamedPipeClient")
	errPipeTimeout                 = errors.New("the tray pipe's client took too long")
	errPipeStopped                 = errors.New("stopped")
)

// pipeIO is the state of the one overlapped operation in flight. It is pinned: the kernel
// writes to it while Go code runs, so it must neither move nor be freed.
type pipeIO struct {
	ov  windows.Overlapped
	buf [pipeMaxLine]byte
}

type pipeConn struct {
	h    windows.Handle
	stop windows.Handle // set when the server stops
	io   *pipeIO
}

// servePipe answers the tray on the named pipe until ctx ends: one client at a time, each
// with pipeTimeout to ask and read its answer.
func (s *Server) servePipe(ctx context.Context, name string) error {
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	n16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// FIRST_PIPE_INSTANCE: fail, rather than serve beside another program that took the name.
	h, err := windows.CreateNamedPipe(n16, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, pipeMaxLine, pipeMaxLine, 0, sa)
	if err != nil {
		return fmt.Errorf("creating %s: %w", name, err)
	}
	defer windows.CloseHandle(h)
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(stop)
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			windows.SetEvent(stop)
		case <-done:
		}
	}()
	pio := new(pipeIO)
	var pin runtime.Pinner
	pin.Pin(pio)
	defer pin.Unpin()
	pio.ov.HEvent = ev
	p := &pipeConn{h: h, stop: stop, io: pio}
	for {
		err := p.connect()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.log.Warn("tray pipe: waiting for a client failed", "err", err)
			windows.DisconnectNamedPipe(h)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		s.servePipeClient(p)
		windows.DisconnectNamedPipe(h)
	}
}

func (s *Server) servePipeClient(p *pipeConn) {
	deadline := time.Now().Add(pipeTimeout)
	line, err := p.readLine(deadline)
	if err != nil {
		if !errors.Is(err, errPipeStopped) {
			s.log.Warn("tray pipe: reading the request failed", "err", err)
		}
		return
	}
	ans := PipeAnswer{Error: "this account may not sign in to the node's page: ask an administrator, or use the local password"}
	c, err := pipeCaller(p.h)
	switch {
	case err != nil:
		s.log.Error("tray pipe: cannot tell who is asking", "err", err)
		ans.Error = "the agent cannot tell who is asking: " + err.Error()
	case s.callerMaySignIn(c):
		ans = s.pipeAnswer(line)
	default:
		s.log.Warn("tray pipe: refused a sign-in link", "user", c.User)
	}
	b, _ := json.Marshal(ans)
	if err := p.write(append(b, '\n'), deadline); err != nil {
		return
	}
	// Disconnecting throws away what the client has not read yet: wait until it closes its end.
	p.waitClosed(deadline)
}

// callerMaySignIn applies mayReadPassword to the password file's permissions. Without a
// readable permission list only administrators may.
func (s *Server) callerMaySignIn(c trayCaller) bool {
	allow, deny, err := fileReaders(s.opt.PasswordFile)
	if err != nil {
		s.log.Warn("tray pipe: cannot read the password file's permissions; only administrators may sign in from the tray", "err", err)
		allow, deny = nil, nil
	}
	return mayReadPassword(c, allow, deny)
}

func (p *pipeConn) reset() {
	ev := p.io.ov.HEvent
	p.io.ov = windows.Overlapped{HEvent: ev}
	windows.ResetEvent(ev)
}

// finish waits for the operation that returned err (pending or done) until deadline (zero:
// no limit) or the server's stop, and returns the bytes it moved.
func (p *pipeConn) finish(err error, deadline time.Time) (uint32, error) {
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	ms := uint32(windows.INFINITE)
	if !deadline.IsZero() {
		ms = uint32(max(time.Until(deadline), 0) / time.Millisecond)
	}
	r, werr := windows.WaitForMultipleObjects([]windows.Handle{p.io.ov.HEvent, p.stop}, false, ms)
	var n uint32
	if r != windows.WAIT_OBJECT_0 {
		windows.CancelIoEx(p.h, &p.io.ov)
		windows.GetOverlappedResult(p.h, &p.io.ov, &n, true) // the cancelled operation has ended
		switch {
		case werr != nil:
			return 0, werr
		case r == windows.WAIT_OBJECT_0+1:
			return 0, errPipeStopped
		}
		return 0, errPipeTimeout
	}
	err = windows.GetOverlappedResult(p.h, &p.io.ov, &n, false)
	return n, err
}

func (p *pipeConn) connect() error {
	p.reset()
	err := windows.ConnectNamedPipe(p.h, &p.io.ov)
	if err == nil || err == windows.ERROR_PIPE_CONNECTED { // a client came before the call
		return nil
	}
	_, err = p.finish(err, time.Time{})
	return err
}

// readLine reads up to a newline, at most pipeMaxLine bytes.
func (p *pipeConn) readLine(deadline time.Time) ([]byte, error) {
	var line []byte
	for {
		p.reset()
		var n uint32
		n, err := p.finish(windows.ReadFile(p.h, p.io.buf[:], &n, &p.io.ov), deadline)
		if err != nil {
			return nil, err
		}
		line = append(line, p.io.buf[:n]...)
		if i := strings.IndexByte(string(line), '\n'); i >= 0 {
			return line[:i], nil
		}
		if len(line) >= pipeMaxLine {
			return nil, errors.New("request line too long")
		}
	}
}

func (p *pipeConn) write(b []byte, deadline time.Time) error {
	if len(b) > len(p.io.buf) {
		return errors.New("answer too long")
	}
	for len(b) > 0 {
		p.reset()
		k := copy(p.io.buf[:], b)
		var n uint32
		n, err := p.finish(windows.WriteFile(p.h, p.io.buf[:k], &n, &p.io.ov), deadline)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// waitClosed reads until the client closes its end (or deadline).
func (p *pipeConn) waitClosed(deadline time.Time) {
	for {
		p.reset()
		var n uint32
		if _, err := p.finish(windows.ReadFile(p.h, p.io.buf[:], &n, &p.io.ov), deadline); err != nil {
			return
		}
	}
}

// pipeCaller reads the account and groups of the pipe's client from its token, while this
// thread impersonates it. The client connects at the identification level (AskLoginLink), so
// the agent can read its identity but never act as it.
func pipeCaller(h windows.Handle) (trayCaller, error) {
	type result struct {
		c   trayCaller
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		if r, _, e := procImpersonateNamedPipeClient.Call(uintptr(h)); r == 0 {
			runtime.UnlockOSThread()
			ch <- result{err: fmt.Errorf("ImpersonateNamedPipeClient: %w", e)}
			return
		}
		c, err := tokenCaller(windows.GetCurrentThreadToken())
		if err != nil { // the pseudo handle failed: open the thread's token as the agent instead
			var t windows.Token
			if oerr := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &t); oerr == nil {
				c, err = tokenCaller(t)
				t.Close()
			}
		}
		if rerr := windows.RevertToSelf(); rerr != nil {
			// The thread still acts as the client: leave it locked, so it ends with this goroutine.
			ch <- result{err: fmt.Errorf("RevertToSelf: %w", rerr)}
			return
		}
		runtime.UnlockOSThread()
		ch <- result{c, err}
	}()
	r := <-ch
	return r.c, r.err
}

func tokenCaller(t windows.Token) (trayCaller, error) {
	u, err := t.GetTokenUser()
	if err != nil {
		return trayCaller{}, fmt.Errorf("the caller's account: %w", err)
	}
	gs, err := t.GetTokenGroups()
	if err != nil {
		return trayCaller{}, fmt.Errorf("the caller's groups: %w", err)
	}
	c := trayCaller{User: u.User.Sid.String()}
	for _, g := range gs.AllGroups() {
		c.Groups = append(c.Groups, tokenGroup{SID: g.Sid.String(), Attrs: g.Attributes})
	}
	return c, nil
}

// fileReaders returns the SIDs of the allow and deny entries of path's permissions that cover
// reading its data. A file without a permission list (a null DACL) lets everyone read.
func fileReaders(path string) (allow, deny []string, err error) {
	if path == "" {
		return nil, nil, errors.New("no password file")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, nil, err
	}
	if dacl == nil {
		return []string{"S-1-1-0"}, nil, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, nil, err
		}
		if ace.Header.AceFlags&inheritOnlyACE != 0 || ace.Mask&(fileReadData|windows.GENERIC_READ|windows.GENERIC_ALL) == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			allow = append(allow, sid)
		case windows.ACCESS_DENIED_ACE_TYPE:
			deny = append(deny, sid)
		}
	}
	return allow, deny, nil
}

// AskLoginLink asks the agent on the pipe name for a one-time sign-in link to its local UI on
// port, and checks that the link points there (ValidLoginLink).
func AskLoginLink(name string, port int, timeout time.Duration) (string, error) {
	n16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	var h windows.Handle
	for {
		// SECURITY_IDENTIFICATION: the agent may learn who asks, not act as this account.
		h, err = windows.CreateFile(n16, pipeClientAccess, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == windows.ERROR_PIPE_BUSY && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break
	}
	if err == windows.ERROR_FILE_NOT_FOUND {
		return "", errors.New("the node agent is not running (no pipe " + name + ")")
	}
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", name, err)
	}
	f := os.NewFile(uintptr(h), name)
	defer f.Close()
	req, _ := json.Marshal(PipeRequest{Op: OpLoginLink})
	if _, err := f.Write(append(req, '\n')); err != nil {
		return "", fmt.Errorf("asking the agent: %w", err)
	}
	// The agent ends the connection after pipeTimeout, so this read ends too.
	line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("reading the agent's answer: %w", err)
	}
	var ans PipeAnswer
	if err := json.Unmarshal(line, &ans); err != nil {
		return "", fmt.Errorf("the agent's answer: %w", err)
	}
	if ans.Error != "" {
		return "", errors.New(ans.Error)
	}
	if !ValidLoginLink(ans.URL, port) {
		return "", fmt.Errorf("the pipe answered with a link elsewhere: %q", ans.URL)
	}
	return ans.URL, nil
}
