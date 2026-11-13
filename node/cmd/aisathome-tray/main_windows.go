// Command aisathome-tray is the AIs@home node's icon in the Windows notification area, one per
// signed-in user (the installer starts it at every login): the node's state as its icon and
// tooltip, and a menu to open the node's page signed in, stop and start the engine, collect
// diagnostics and quit. It never reads the local password: the agent's pipe gives it one-time
// sign-in links (node/localui/pipe.go).
//
//	aisathome-tray          the tray (a second start in the same session just exits)
//	aisathome-tray -open    also open the node's page, signed in (the Start-menu entry)
//	aisathome-tray -quit    close the running trays (the installer, before it replaces or removes them)
//	aisathome-tray -check   print the tray's state and test the sign-in link, then exit (CI)
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/aisathome/aisathome/node"
	"github.com/aisathome/aisathome/node/localui"
	"github.com/aisathome/aisathome/node/tray"
)

const (
	exeName      = "aisathome-tray.exe"
	mutexName    = `Local\AIsAtHome-tray`      // one tray per session
	quitName     = `Local\AIsAtHome-tray-quit` // -quit sets it; the tray then quits
	pollInterval = 5 * time.Second
	maxLogBytes  = 1 << 20
)

var logger = slog.New(slog.DiscardHandler)

func main() {
	open := flag.Bool("open", false, "open the node's page, signed in")
	quit := flag.Bool("quit", false, "close the running trays and exit")
	check := flag.Bool("check", false, "print the tray's state, test the sign-in link and exit")
	flag.Parse()
	if *quit {
		os.Exit(quitTrays(os.Stdout))
	}
	port, err := localui.PortFromEnv(localui.DefaultPort)
	if err != nil || port == 0 {
		port = localui.DefaultPort
	}
	c := tray.NewClient(port, func() (string, error) { return localui.AskLoginLink(localui.TrayPipe, port, 5*time.Second) })
	if *check {
		os.Exit(runCheck(c, os.Stdout))
	}
	closeLog := openLog()
	defer closeLog()
	m, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(mutexName))
	if err == windows.ERROR_ALREADY_EXISTS { // a tray runs in this session already
		if *open {
			openPage(c)
		}
		return
	}
	if err != nil {
		logger.Warn("cannot tell whether a tray runs already", "err", err)
	} else {
		defer windows.CloseHandle(m)
	}
	t := &app{c: c}
	if *open {
		go openPage(c)
	}
	go t.waitQuit()
	logger.Info("tray starting", "version", node.Version)
	systray.Run(t.ready, func() { logger.Info("tray quits") })
}

// openLog sends the tray's log (and the systray library's) to tray.LogFile, capped.
func openLog() func() {
	path := tray.LogFile(os.Getenv)
	if path == "" {
		return func() {}
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogBytes {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return func() {}
	}
	logger = slog.New(slog.NewTextHandler(f, nil))
	log.SetOutput(f)
	return func() { f.Close() }
}

type app struct {
	c                                        *tray.Client
	title, open, stop, start, diag, quitItem *systray.MenuItem
	refresh                                  chan struct{}
	last                                     tray.View
}

func (t *app) ready() {
	t.refresh = make(chan struct{}, 1)
	t.title = systray.AddMenuItem("AIs@home node", "")
	t.title.Disable()
	systray.AddSeparator()
	t.open = systray.AddMenuItem("Open the node page", "The node's page in your browser, signed in")
	t.stop = systray.AddMenuItem("Stop engine", "Free the GPU: the node leaves the network until Start engine")
	t.start = systray.AddMenuItem("Start engine", "Load the model again and rejoin the network")
	systray.AddSeparator()
	t.diag = systray.AddMenuItem("Collect diagnostics...", "Write a zip for the AIs@home team to the Desktop (asks for administrator rights)")
	t.quitItem = systray.AddMenuItem("Quit tray", "Close this icon; the node keeps running")
	systray.SetOnTapped(func() { go openPage(t.c) })
	t.show(tray.FromStatus(nil, fmt.Errorf("starting")))
	go t.poll()
	go t.clicks()
}

func (t *app) show(v tray.View) {
	if v == t.last {
		return
	}
	if v.State != t.last.State {
		systray.SetIcon(tray.Icon(v.State))
		logger.Info("state", "state", v.State, "tooltip", v.Tooltip)
	}
	t.last = v
	systray.SetTooltip(v.Tooltip)
	t.title.SetTitle(v.Title)
	enable(t.stop, v.CanStop)
	enable(t.start, v.CanStart)
}

func enable(m *systray.MenuItem, on bool) {
	if on {
		m.Enable()
	} else {
		m.Disable()
	}
}

func (t *app) poll() {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		st, err := t.c.Status(ctx)
		cancel()
		t.show(tray.FromStatus(st, err))
		select {
		case <-tick.C:
		case <-t.refresh:
		}
	}
}

func (t *app) poke() {
	select {
	case t.refresh <- struct{}{}:
	default:
	}
}

func (t *app) clicks() {
	for {
		select {
		case <-t.open.ClickedCh:
			go openPage(t.c)
		case <-t.stop.ClickedCh:
			go t.control("stop-engine")
		case <-t.start.ClickedCh:
			go t.control("start-engine")
		case <-t.diag.ClickedCh:
			go diagnose()
		case <-t.quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (t *app) control(action string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg, err := t.c.Control(ctx, action)
	t.poke()
	if err != nil {
		logger.Warn("control failed", "action", action, "err", err)
		message("AIs@home node", "Could not "+strings.ReplaceAll(action, "-", " ")+": "+err.Error()+
			"\n\nOpen the node page and sign in with the local password, or ask the PC's administrator.", windows.MB_ICONWARNING)
		return
	}
	logger.Info("control", "action", action, "message", msg)
}

// openPage opens the node's page in the browser: signed in with a one-time link when the agent
// gives one, else the page itself, which asks for the local password.
func openPage(c *tray.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	_, serr := c.Status(ctx)
	cancel()
	if serr != nil {
		message("AIs@home node", "The AIs@home node agent is not running (service "+node.ServiceName+
			"). It starts with Windows; if it does not, restart the PC or start it in the Services window.", windows.MB_ICONWARNING)
		return
	}
	u, err := c.PageURL()
	if err != nil {
		logger.Warn("no sign-in link; opening the page without one", "err", err)
	}
	if err := shellExecute("open", u, ""); err != nil {
		logger.Error("cannot open the browser", "err", err)
	}
}

// diagnose runs `aisathome-node diagnose` as administrator (Windows asks) in its own window,
// writing the zip to this user's Desktop; the window stays until Enter.
func diagnose() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	agent := filepath.Join(filepath.Dir(self), "aisathome-node.exe")
	desk, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0)
	if err != nil {
		desk, _ = os.UserHomeDir()
	}
	out := filepath.Join(desk, "aisathome-diagnose-"+time.Now().Format("20060102-150405")+".zip")
	args := "diagnose -pause -out " + syscall.EscapeArg(out)
	if err := shellExecute("runas", agent, args); err != nil {
		if err == windows.ERROR_CANCELLED { // the person said no to the administrator prompt
			return
		}
		logger.Error("cannot run diagnose", "err", err)
		message("AIs@home node", "Could not run the diagnostics: "+err.Error(), windows.MB_ICONWARNING)
		return
	}
	logger.Info("diagnose started", "out", out)
}

func shellExecute(verb, file, args string) error {
	var a *uint16
	if args != "" {
		a = windows.StringToUTF16Ptr(args)
	}
	return windows.ShellExecute(0, windows.StringToUTF16Ptr(verb), windows.StringToUTF16Ptr(file), a, nil, windows.SW_SHOWNORMAL)
}

func message(title, text string, icon uint32) {
	windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr(title), windows.MB_OK|icon|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
}

// waitQuit quits the tray when -quit sets the session's quit event.
func (t *app) waitQuit() {
	ev, err := windows.CreateEvent(nil, 0, 0, windows.StringToUTF16Ptr(quitName))
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		logger.Warn("no quit event: the installer will end this tray the hard way", "err", err)
		return
	}
	windows.WaitForSingleObject(ev, windows.INFINITE)
	logger.Info("asked to quit (-quit)")
	systray.Quit()
}

// quitTrays implements -quit: it asks the trays in this session to quit, waits for them, then
// ends any tray still running (other sessions, or one that does not answer). 0 when none is left.
func quitTrays(w io.Writer) int {
	if ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, windows.StringToUTF16Ptr(quitName)); err == nil {
		windows.SetEvent(ev)
		windows.CloseHandle(ev)
	}
	var sess uint32
	windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sess)
	waitGone(func(p trayProc) bool { return p.session == sess }, 5*time.Second)
	// Another user's tray (fast user switching): an administrator may end it once the debug
	// privilege is on (an elevated token has it, switched off).
	enableDebugPrivilege()
	for _, p := range trays() {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, p.pid)
		if err != nil {
			fmt.Fprintf(w, "cannot end the tray %d (session %d): %v\n", p.pid, p.session, err)
			continue
		}
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
		fmt.Fprintf(w, "ended the tray %d (session %d)\n", p.pid, p.session)
	}
	if waitGone(func(trayProc) bool { return true }, 5*time.Second) {
		return 0
	}
	fmt.Fprintln(w, "a tray is still running")
	return 1
}

func enableDebugPrivilege() {
	var t windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &t); err != nil {
		return
	}
	defer t.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeDebugPrivilege"), &luid); err != nil {
		return
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	windows.AdjustTokenPrivileges(t, false, &tp, 0, nil, nil)
}

type trayProc struct{ pid, session uint32 }

// trays lists the other aisathome-tray processes.
func trays() []trayProc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	self := windows.GetCurrentProcessId()
	var out []trayProc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == self || !strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), exeName) {
			continue
		}
		p := trayProc{pid: e.ProcessID}
		windows.ProcessIdToSessionId(e.ProcessID, &p.session)
		out = append(out, p)
	}
	return out
}

// waitGone waits until no tray that match selects is left.
func waitGone(match func(trayProc) bool, d time.Duration) bool {
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		left := 0
		for _, p := range trays() {
			if match(p) {
				left++
			}
		}
		if left == 0 {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// runCheck implements -check: the state the tray would show, and the sign-in link's test.
func runCheck(c *tray.Client, w io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	v := tray.FromStatus(st, err)
	fmt.Fprintf(w, "state=%s\ntooltip=%q\ncan_stop=%v can_start=%v\n", v.State, v.Tooltip, v.CanStop, v.CanStart)
	if err != nil {
		fmt.Fprintln(w, "status error:", err)
		return 1
	}
	if err := c.CheckSignIn(ctx); err != nil {
		fmt.Fprintln(w, "signin=failed:", err)
		return 1
	}
	fmt.Fprintln(w, "signin=ok")
	return 0
}
