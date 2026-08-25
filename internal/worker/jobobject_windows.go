//go:build windows

package worker

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	jobObjectLimitKillOnJobClose = 0x2000
	jobObjectExtendedLimitInfo   = 9
	jobObjectMsgEndOfJobTime     = 1
)

var (
	modkernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = modkernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = modkernel32.NewProc("AssignProcessToJobObject")
	procSetInformationJobObject  = modkernel32.NewProc("SetInformationJobObject")
	procTerminateJobObject       = modkernel32.NewProc("TerminateJobObject")
	procCloseHandle              = modkernel32.NewProc("CloseHandle")
	procOpenProcess              = modkernel32.NewProc("OpenProcess")
)

// jobObjectSysProcAttr is a stub on non-Windows; the Windows implementation
// attaches the child to a Job Object after start because Go's SysProcAttr
// cannot itself create one.
func jobObjectSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// jobObjectHandle is a live Windows Job Object handle.
type jobObjectHandle struct {
	handle uintptr
}

func newJobObject() (*jobObjectHandle, error) {
	// Create an anonymous/unnamed Job Object so handles are isolated per supervisor
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("create job object: %v", err)
	}

	// Configure JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so closing the handle
	// terminates every process in the job tree.
	type jobObjectExtendedLimitInformation struct {
		BasicLimitInformation struct {
			PerProcessUserTimeLimit int64
			PerJobUserTimeLimit     int64
			LimitFlags              uint32
			MinimumWorkingSetSize   uintptr
			MaximumWorkingSetSize   uintptr
			ActiveProcessLimit      uint32
			Affinity                uintptr
			PriorityClass           uint32
			SchedulingClass         uint32
		}
		IoInfo                [6]uintptr
		ProcessMemoryLimit    uintptr
		JobMemoryLimit        uintptr
		PeakProcessMemoryUsed uintptr
		PeakJobMemoryUsed     uintptr
	}
	info := jobObjectExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose

	r, _, err := procSetInformationJobObject.Call(
		h,
		uintptr(jobObjectExtendedLimitInfo),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r == 0 {
		_, _, _ = procCloseHandle.Call(h)
		return nil, fmt.Errorf("set job object kill-on-close: %v", err)
	}

	return &jobObjectHandle{handle: h}, nil
}

func (j *jobObjectHandle) assign(process *os.Process) error {
	// OpenProcess returns a real process handle; AssignProcessToJobObject
	// requires that handle rather than the PID.
	const (
		processSetQuota     = 0x0100
		processTerminate    = 0x0001
		processQueryLimited = 0x1000
	)
	access := uintptr(processSetQuota | processTerminate | processQueryLimited)
	hProcess, _, err := procOpenProcess.Call(access, 0, uintptr(process.Pid))
	if hProcess == 0 {
		return fmt.Errorf("open process %d: %v", process.Pid, err)
	}
	defer procCloseHandle.Call(hProcess)

	r, _, err := procAssignProcessToJobObject.Call(j.handle, hProcess)
	if r == 0 {
		return fmt.Errorf("assign process %d to job object: %v", process.Pid, err)
	}
	return nil
}

func (j *jobObjectHandle) terminate() {
	_, _, _ = procTerminateJobObject.Call(j.handle, 1)
}

func (j *jobObjectHandle) close() {
	if j.handle != 0 {
		_, _, _ = procCloseHandle.Call(j.handle)
		j.handle = 0
	}
}

// attachJobObject creates a job and assigns the child immediately after start.
// The handle is held by the caller (Supervisor) and closed when the worker
// exits, which enforces JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE for every
// descendant process.
func attachJobObject(cmd *exec.Cmd) (*jobObjectHandle, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("worker process has not started")
	}
	job, err := newJobObject()
	if err != nil {
		return nil, err
	}
	if err := job.assign(cmd.Process); err != nil {
		job.close()
		return nil, err
	}
	return job, nil
}

// terminateProcessTree forcibly terminates the worker and all descendants.
func terminateProcessTree(cmd *exec.Cmd, _ int) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	job, err := newJobObject()
	if err != nil {
		// Fallback: kill the direct process; descendant cleanup is then best
		// effort and should not happen on the normal Seam 2 path.
		_ = cmd.Process.Kill()
		return err
	}
	defer job.close()
	if err := job.assign(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	job.terminate()
	return nil
}
