//go:build !windows

package worker

import (
	"os"
	"os/exec"
	"syscall"
)

type jobObjectHandle struct{}

func (j *jobObjectHandle) terminate() {}
func (j *jobObjectHandle) close()     {}
func jobObjectSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func attachJobObject(cmd *exec.Cmd) (*jobObjectHandle, error) {
	return &jobObjectHandle{}, nil
}

func terminateProcessTree(cmd *exec.Cmd, pid int) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Kill the process group (negative pid sends to group).
	pgid := pid
	if pgid <= 0 {
		pgid = cmd.Process.Pid
	}
	p, err := os.FindProcess(-pgid)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return p.Signal(syscall.SIGKILL)
}
