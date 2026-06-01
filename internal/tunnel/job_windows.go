//go:build windows

package tunnel

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processJob wraps a Windows Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Every `tsh proxy app` subprocess the
// supervisor spawns is assigned to this job. When the daemon process exits
// — gracefully *or* via a hard kill from `prism down` (which translates to
// TerminateProcess on Windows since there's no SIGTERM) — the kernel closes
// the daemon's open handles, including this job handle. With no remaining
// open handles, the job is destroyed and the kill-on-close flag fires,
// terminating every assigned process.
//
// This is the canonical Windows pattern for "die with my parent" and avoids
// the orphan-port-bound problem that plain detached child processes have.
//
// We additionally set JOB_OBJECT_LIMIT_BREAKAWAY_OK so grandchildren (if
// `tsh proxy app` ever spawns any) can request to escape the job, but they
// have to opt in explicitly — by default they inherit the job assignment.
type processJob struct {
	handle windows.Handle
}

func newProcessJob() (*processJob, error) {
	// CreateJobObjectW(NULL, NULL) — unnamed, default security.
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// Set the kill-on-close limit. JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// carries the basic-limits struct embedded, so we use the extended
	// info class to set the kill-on-close flag (which lives in the
	// extended struct's LimitFlags).
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return &processJob{handle: h}, nil
}

// assign places the running cmd's process into the job. There is a brief
// window between cmd.Start() and this call where the process is unassigned;
// `tsh proxy app` doesn't spawn grandchildren in that window, so the race
// doesn't matter in practice. If it ever does, switch to the CREATE_SUSPENDED
// + AssignProcessToJobObject + ResumeThread sequence.
func (j *processJob) assign(cmd *exec.Cmd) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	if cmd.Process == nil {
		return fmt.Errorf("processJob.assign: cmd has no Process; call Start first")
	}
	// We need a process handle with PROCESS_SET_QUOTA | PROCESS_TERMINATE
	// access. cmd.Process.Pid is the PID; reopen with the needed rights.
	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(pid=%d): %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(j.handle, ph); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// close releases the daemon's handle on the job. On graceful daemon
// shutdown this triggers the kill-on-close limit and kills the children
// — same effect as the daemon process dying.
func (j *processJob) close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	h := j.handle
	j.handle = 0
	return windows.CloseHandle(h)
}
