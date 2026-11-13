// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sysProc is the engine's Job object. The agent holds the only handle; when it is closed (by
// release, or by Windows when the agent's process ends, however it ends) the job kills every
// process in it (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE).
type sysProc struct {
	job windows.Handle
}

// createNoWindow: the engine is a console program; it gets no console window when the agent
// runs in a user's session (the tray fallback). Its output goes to the agent's pipes.
const createNoWindow = 0x08000000

// newJob creates a job that kills its processes when its last handle closes, and makes a
// crash end the process at once instead of waiting on Windows Error Reporting's dialog
// (JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION; the Linux unit's GGML_NO_BACKTRACE has the same
// aim).
func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("creating the engine's job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("setting the engine's job object limits: %w", err)
	}
	return job, nil
}

// startSys starts the engine suspended, puts it into a new job and only then lets it run, so
// it never runs outside the job (nor could anything it starts).
func (p *proc) startSys() error {
	job, err := newJob()
	if err != nil {
		return err
	}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | createNoWindow}
	if err := p.cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := assignAndResume(job, uint32(p.pid())); err != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
		windows.CloseHandle(job)
		return fmt.Errorf("starting the engine in its job object: %w", err)
	}
	p.sys.job = job
	return nil
}

func assignAndResume(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return err
	}
	return resumeThreads(pid)
}

// resumeThreads resumes the threads of a process started suspended (it has one: its main
// thread; os/exec does not return its handle, so it is found by a thread snapshot).
func resumeThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(th)
		windows.CloseHandle(th)
		if err != nil {
			return err
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if resumed == 0 {
		return errors.New("no thread to resume")
	}
	return nil
}

// stop ends every process in the engine's job at once: Windows has no stop request a console
// program without a console receives, and llama-server keeps nothing that must be saved. It
// waits for the process to be gone, at most wait (a process stuck in the GPU driver may not go).
func (p *proc) stop(wait time.Duration) {
	windows.TerminateJobObject(p.sys.job, 1)
	p.wait(wait)
}

func (p *proc) releaseSys() {
	if p.sys.job != 0 {
		windows.CloseHandle(p.sys.job)
		p.sys.job = 0
	}
}
