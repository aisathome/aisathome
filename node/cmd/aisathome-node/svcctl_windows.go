// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/aisathome/aisathome/node"
)

// serviceAccount is the virtual account the service runs as: it has no password, no rights to
// the users' profiles and only the file access the data folder gives it.
const serviceAccount = `NT SERVICE\` + node.ServiceName

func serviceConfig() mgr.Config {
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true, // after the boot rush; the node is not needed at logon
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      "AIs@home node",
		Description: "Serves open-weight language models for AIs@home on this computer's GPU. " +
			"Its page: http://127.0.0.1:7381/",
		ServiceStartName: serviceAccount,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
}

// recoveryActions restart the agent when it fails: it exits with an error when its gateway
// link stays dead (the watchdog) or it cannot start. The count resets after a day.
var recoveryActions = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	{Type: mgr.ServiceRestart, Delay: time.Minute},
	{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
}

func connectSCM() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, fmt.Errorf("the Service Control Manager refused access: run this as administrator")
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to the Service Control Manager: %w", err)
	}
	return m, nil
}

func serviceInstall(o installOptions, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	cfg := serviceConfig()
	s, err := m.OpenService(node.ServiceName)
	switch {
	case err == nil:
		// An update (or a repair): point the service at this executable, with today's settings.
		cfg.BinaryPathName = syscall.EscapeArg(exe)
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			return fmt.Errorf("updating the service: %w", err)
		}
		fmt.Fprintf(out, "updated service %s: %s\n", node.ServiceName, exe)
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		if s, err = m.CreateService(node.ServiceName, exe, cfg); err != nil {
			return fmt.Errorf("creating the service: %w", err)
		}
		fmt.Fprintf(out, "created service %s: %s\n", node.ServiceName, exe)
	default:
		return fmt.Errorf("opening the service: %w", err)
	}
	defer s.Close()
	if err := s.SetRecoveryActions(recoveryActions, 24*60*60); err != nil {
		return fmt.Errorf("setting the restart on failure: %w", err)
	}
	// The agent's own failures end the service with a service-specific error, not a crash.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("setting the restart on failure: %w", err)
	}
	// The virtual account exists once its service does.
	svcSID, err := lookupSID(serviceAccount)
	if err != nil {
		return err
	}
	self, err := tokenUser()
	if err != nil {
		return err
	}
	uiUser := uiUserFromToken(self)
	if o.UIUser != "" {
		if uiUser, err = resolveUIUser(o.UIUser); err != nil {
			return err
		}
	}
	layout := node.WindowsLayout(os.Getenv)
	if err := prepareDataDir(layout, []string{svcSID, self}, svcSID, uiUser); err != nil {
		return err
	}
	fmt.Fprintf(out, "data folder %s: administrators and %s only\n", filepath.Dir(layout.Config), serviceAccount)
	if o.Start {
		return startService(s, out)
	}
	return nil
}

func serviceUninstall(out io.Writer) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(node.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		fmt.Fprintln(out, "service", node.ServiceName, "is not installed")
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening the service: %w", err)
	}
	// Removed even when it does not stop in time: a service left registered would point at
	// the deleted program, while one marked for removal goes when its process ends (and,
	// disabled by stopService, is not started again).
	stopErr := stopService(s, out)
	err = s.Delete()
	s.Close()
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return errors.Join(stopErr, fmt.Errorf("removing the service: %w", err))
	}
	if stopErr != nil {
		return fmt.Errorf("%w; the service is marked for removal and goes when its process ends", stopErr)
	}
	// Windows removes it once every handle to it is closed (the Services window holds one).
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		s, err := m.OpenService(node.ServiceName)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			fmt.Fprintln(out, "removed service", node.ServiceName)
			return nil
		}
		if err == nil {
			s.Close()
		}
	}
	fmt.Fprintln(out, "service", node.ServiceName, "is stopped and marked for removal; Windows removes it when the Services window is closed")
	return nil
}

func serviceStop(out io.Writer) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(node.ServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		fmt.Fprintln(out, "service", node.ServiceName, "is not installed")
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening the service: %w", err)
	}
	defer s.Close()
	return stopService(s, out)
}

// stopService disables the service, stops it and waits until its process has ended, so its
// files can be replaced or removed (the engine, its child, ends with it). Disabled first: after
// a failure the Service Control Manager may have a restart queued (recovery actions), which only
// disabling cancels; otherwise it could start the agent again while an update replaces its
// files. `service install` enables it again.
func stopService(s *mgr.Service, out io.Writer) error {
	if err := windows.ChangeServiceConfig(s.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_DISABLED,
		windows.SERVICE_NO_CHANGE, nil, nil, nil, nil, nil, nil, nil); err != nil {
		return fmt.Errorf("disabling the service: %w", err)
	}
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("querying the service: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	pid := st.ProcessId
	deadline := time.Now().Add(stopWait + 20*time.Second)
	for st.State != svc.Stopped {
		if st.State == svc.Running || st.State == svc.Paused {
			// Not accepted while it starts; asked again on the next round.
			if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) &&
				!errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return fmt.Errorf("stopping the service: %w", err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not stop within %s (state %d)", stopWait+20*time.Second, st.State)
		}
		time.Sleep(250 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return fmt.Errorf("querying the service: %w", err)
		}
	}
	if err := waitProcessExit(pid, 10*time.Second); err != nil {
		return err
	}
	fmt.Fprintln(out, "stopped service", node.ServiceName)
	return nil
}

// waitProcessExit waits for a stopped service's process to end: Stopped is reported a moment
// before the process exits, and its executable cannot be replaced until it has.
func waitProcessExit(pid uint32, wait time.Duration) error {
	if pid == 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return nil // already gone
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(wait/time.Millisecond))
	if err != nil {
		return fmt.Errorf("waiting for the service's process %d: %w", pid, err)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("the service's process %d did not end within %s", pid, wait)
	}
	return nil
}

func startService(s *mgr.Service, out io.Writer) error {
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("starting the service: %w", err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying the service: %w", err)
		}
		switch st.State {
		case svc.Running:
			fmt.Fprintln(out, "started service", node.ServiceName)
			return nil
		case svc.Stopped:
			return fmt.Errorf("the service stopped right after starting (exit code %d, service code %d); its log is in %s",
				st.Win32ExitCode, st.ServiceSpecificExitCode, filepath.Dir(node.WindowsLayout(os.Getenv).LogFile))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not start within 30s (state %d)", st.State)
		}
	}
}

func lookupSID(account string) (string, error) {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return "", fmt.Errorf("looking up %s: %w", account, err)
	}
	return sid.String(), nil
}

// tokenUser is the SID of the account running this command (an administrator: the Service
// Control Manager refuses anyone else).
func tokenUser() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("reading this process's user: %w", err)
	}
	return u.User.Sid.String(), nil
}

// resolveUIUser turns -ui-user (an account name or a SID) into a SID.
func resolveUIUser(flag string) (string, error) {
	switch {
	case strings.HasPrefix(strings.ToUpper(flag), "S-1-"):
		sid, err := windows.StringToSid(flag)
		if err != nil {
			return "", fmt.Errorf("-ui-user %q: %w", flag, err)
		}
		return sid.String(), nil
	default:
		return lookupSID(flag)
	}
}

// prepareDataDir creates the agent's folder (or takes over an existing one from an earlier
// install, when its owner is trustedOwner with trusted), sets its permissions (dataDirSDDL),
// resets the config and state files to them, and makes sure the local UI has a password that
// uiUser may read.
func prepareDataDir(layout node.Layout, trusted []string, svcSID, uiUser string) error {
	dir := filepath.Dir(layout.Config)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := checkOwned(dir, trusted, true); err != nil {
		return err
	}
	// Protected: nothing inherited from %ProgramData%. Windows passes the new entries on to
	// what is already inside (an update's files, the logs, the model).
	if err := applySDDL(dir, dataDirSDDL(svcSID), windows.OWNER_SECURITY_INFORMATION|
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return fmt.Errorf("setting the permissions of %s: %w", dir, err)
	}
	for _, f := range []string{layout.Config, layout.StateFile} {
		if err := resetFile(f, trusted); err != nil {
			return err
		}
	}
	return ensurePassword(layout.PasswordFile, trusted, uiUser)
}

// resetFile drops a file's own permission entries (e.g. from an earlier, hand-made setup), so
// it has only the folder's.
func resetFile(path string, trusted []string) error {
	if err := checkOwned(path, trusted, false); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := applySDDL(path, inheritOnlySDDL, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return fmt.Errorf("setting the permissions of %s: %w", path, err)
	}
	return nil
}

// ensurePassword keeps a usable password file (an update keeps the password) or writes a new
// one, then gives uiUser read access to it.
func ensurePassword(path string, trusted []string, uiUser string) error {
	err := checkOwned(path, trusted, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !usablePassword(b) {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		pw, err := newPassword()
		if err != nil {
			return err
		}
		// Created inside the protected folder, so it has the folder's permissions from the start.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(pw + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	if err := applySDDL(path, passwordFileSDDL(uiUser), windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return fmt.Errorf("setting the permissions of %s: %w", path, err)
	}
	return nil
}

// checkOwned returns fs.ErrNotExist for a missing path, and an error when the path is a link
// or junction, the wrong kind, or owned by an account the installer must not trust.
func checkOwned(path string, trusted []string, wantDir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return fmt.Errorf("%s is a link or junction; delete it as administrator, then install again", path)
	}
	if fi.IsDir() != wantDir {
		return fmt.Errorf("%s is not a %s", path, map[bool]string{true: "folder", false: "file"}[wantDir])
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	if !trustedOwner(owner.String(), trusted...) {
		name := owner.String()
		if acc, dom, _, err := owner.LookupAccount(""); err == nil {
			name = dom + `\` + acc
		}
		return fmt.Errorf("%s belongs to %s, not to administrators or the service, so that account "+
			"could change its permissions back; delete it as administrator, then install again", path, name)
	}
	return nil
}

// applySDDL sets the parts of path's security descriptor that info names, from sddl.
func applySDDL(path, sddl string, info windows.SECURITY_INFORMATION) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	var owner *windows.SID
	if info&windows.OWNER_SECURITY_INFORMATION != 0 {
		if owner, _, err = sd.Owner(); err != nil {
			return err
		}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		// A NULL DACL would give everyone full access.
		return fmt.Errorf("no DACL in %q", sddl)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
}
